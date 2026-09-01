package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsevents"
)

const (
	ownWriteCooldown     = time.Second
	minBackoff           = time.Minute
	maxBackoff           = 30 * time.Minute
	offBranchNotifyAfter = 24 * time.Hour
	shutdownGrace        = 30 * time.Second
)

type repoState struct {
	config       repoConfig
	mu           sync.Mutex
	syncing      bool
	timer        *time.Timer
	lastOwnWrite time.Time

	// Retry and incident tracking. Nothing here is ever persisted.
	failures       int
	nextAttempt    time.Time
	incident       string // non-empty while a failure incident is open
	incidentSince  time.Time
	incidentNoted  bool   // the open incident has already produced a popup
	lastSkip       string // last skip reason logged, to avoid repeating it
	offBranchSince time.Time
	offBranchNoted bool
	secretsNoted   map[string]bool
}

type daemon struct {
	ctx    context.Context // cancelled to stop loops
	opCtx  context.Context // cancelled only after in-flight git work is given time to finish
	cfg    config
	syncer syncer
	states map[string]*repoState
	logger *log.Logger
	notify func(context.Context, commandRunner, string) error
	runner commandRunner
	now    func() time.Time
	online func(context.Context) bool
	alerts *failureAlerts

	healthInterval time.Duration
	inflight       sync.WaitGroup
}

func newDaemon(ctx context.Context, cfg config, runner commandRunner, logger *log.Logger) *daemon {
	d := &daemon{
		ctx: ctx, opCtx: context.Background(), cfg: cfg,
		syncer: gitSyncer{runner: runner},
		states: make(map[string]*repoState),
		logger: logger,
		notify: macOSNotify,
		runner: runner,
		now:    time.Now,
		online: networkOnline,

		healthInterval: 10 * time.Second,
	}
	d.alerts = &failureAlerts{window: alertCoalesce, send: d.sendNotification}
	for _, repo := range cfg.Repositories {
		d.states[repo.Name] = &repoState{config: repo, secretsNoted: make(map[string]bool)}
	}
	return d
}

func runDaemon(ctx context.Context, configPath string) error {
	store := &configStore{path: configPath}
	cfg, err := store.load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	d := newDaemon(ctx, cfg, execCommandRunner{}, log.New(os.Stdout, "repo-sync: ", log.LstdFlags))
	return d.run()
}

func (d *daemon) run() error {
	opCtx, cancelOps := context.WithCancel(context.Background())
	defer cancelOps()
	d.opCtx = opCtx

	if len(d.states) == 0 {
		d.logger.Print("no repositories configured; run `repo-sync setup` or `repo-sync add`")
		<-d.ctx.Done()
		return nil
	}
	paths := make([]string, 0, len(d.states))
	for _, state := range d.states {
		resolved, err := filepath.EvalSymlinks(state.config.Path)
		if err != nil {
			return fmt.Errorf("repository %s: %w", state.config.Name, err)
		}
		state.config.Path = resolved
		paths = append(paths, resolved)
	}

	stream := &fsevents.EventStream{
		Paths: paths, Latency: 250 * time.Millisecond,
		Flags: fsevents.FileEvents | fsevents.NoDefer,
	}
	if err := stream.Start(); err != nil {
		// File watching is an optimisation. The health loop polls git status
		// anyway, so keep running rather than giving up.
		d.logger.Printf("file watching unavailable (%v); polling every %s instead", err, d.healthInterval)
	} else {
		defer stream.Stop()
		go d.consumeEvents(stream.Events)
	}
	d.logger.Printf("watching %d repositories", len(d.states))

	go d.periodicRemoteSync()
	go d.healthLoop()
	go d.wakeLoop()
	go d.networkLoop()
	d.healthCheck()
	d.syncAllRemote()
	<-d.ctx.Done()
	d.shutdown()
	return nil
}

// shutdown stops new work and gives running git commands time to finish so a
// rebase or push is never killed halfway.
func (d *daemon) shutdown() {
	d.stopTimers()
	done := make(chan struct{})
	go func() {
		d.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
		d.logger.Print("shutdown grace period elapsed; stopping")
	}
}

func (d *daemon) consumeEvents(events <-chan []fsevents.Event) {
	for {
		select {
		case <-d.ctx.Done():
			return
		case batch, ok := <-events:
			if !ok {
				return
			}
			for _, event := range batch {
				d.handlePath(event.Path)
			}
		}
	}
}

func (d *daemon) handlePath(path string) {
	for _, state := range d.states {
		relative, err := filepath.Rel(state.config.Path, path)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		if relative == ".git" || strings.HasPrefix(relative, ".git"+string(filepath.Separator)) {
			continue
		}
		state.schedule(d.cfg.IdleDebounce.Duration, func() { d.syncRepo(state, true) })
	}
}

func (s *repoState) schedule(delay time.Duration, callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.syncing || time.Since(s.lastOwnWrite) < ownWriteCooldown {
		return
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.setTimerLocked(delay, callback)
}

func (s *repoState) scheduleIfAbsent(delay time.Duration, callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.syncing || s.timer != nil {
		return
	}
	s.setTimerLocked(delay, callback)
}

func (s *repoState) setTimerLocked(delay time.Duration, callback func()) {
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		s.mu.Lock()
		ownsTimer := s.timer == timer
		if ownsTimer {
			s.timer = nil
		}
		s.mu.Unlock()
		if ownsTimer {
			callback()
		}
	})
	s.timer = timer
}

// beginSync claims the repository. wait is non-zero while a retry backoff is
// still running, in which case the sync must not start yet.
func (s *repoState) beginSync(now time.Time) (ok bool, wait time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.syncing {
		return false, 0
	}
	if now.Before(s.nextAttempt) {
		return false, s.nextAttempt.Sub(now)
	}
	s.syncing = true
	return true, 0
}

func (s *repoState) endSync() {
	s.mu.Lock()
	s.syncing = false
	s.lastOwnWrite = time.Now()
	s.mu.Unlock()
}

func (s *repoState) isAvailable(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.syncing && !now.Before(s.nextAttempt)
}

func (d *daemon) syncRepo(state *repoState, commitLocal bool) {
	if !commitLocal {
		if !state.isAvailable(d.now()) {
			return
		}
		changed, _, err := d.syncer.changes(d.opCtx, state.config)
		if err != nil || len(changed) > 0 {
			return
		}
	}
	ok, wait := state.beginSync(d.now())
	if !ok {
		if wait > 0 {
			state.scheduleIfAbsent(wait, func() { d.syncRepo(state, true) })
		}
		return
	}
	d.inflight.Add(1)
	defer d.inflight.Done()
	report, err := d.syncer.sync(d.opCtx, state.config, commitLocal)
	state.endSync()
	if report.Scanned {
		d.reportSecrets(state, report.Blocked)
	}
	d.handleResult(state, report, err)

	changed, _, statusErr := d.syncer.changes(d.opCtx, state.config)
	if statusErr == nil && len(changed) > 0 {
		state.scheduleIfAbsent(d.cfg.IdleDebounce.Duration, func() { d.syncRepo(state, true) })
	}
}

func (d *daemon) handleResult(state *repoState, report syncReport, err error) {
	name := state.config.Name
	if err == nil {
		state.mu.Lock()
		recovered := state.incident != ""
		state.failures, state.incident, state.nextAttempt = 0, "", time.Time{}
		state.incidentSince, state.incidentNoted = time.Time{}, false
		state.lastSkip = ""
		state.offBranchSince, state.offBranchNoted = time.Time{}, false
		state.mu.Unlock()
		if recovered {
			d.logger.Printf("%s recovered", name)
		}
		if summary := report.String(); summary != "" {
			d.logger.Printf("%s: %s", name, summary)
		}
		return
	}

	var skip *skipError
	if errors.As(err, &skip) {
		state.mu.Lock()
		repeat := state.lastSkip == skip.reason
		state.lastSkip = skip.reason
		state.mu.Unlock()
		if !repeat {
			d.logger.Printf("%s skipped: %s", name, skip.reason)
		}
		if skip.offBranch {
			d.noteOffBranch(state, skip.reason)
		}
		return
	}

	now := d.now()
	state.mu.Lock()
	state.failures++
	delay := backoffDelay(state.failures)
	state.nextAttempt = now.Add(delay)
	if state.incident == "" {
		state.incidentSince = now
	}
	state.incident = err.Error()
	// Being offline is not an incident worth a popup; it resolves itself. Other
	// failures get one popup, and only once they have lasted long enough to be
	// more than a blip.
	alert := !state.incidentNoted && !isOfflineError(err) && now.Sub(state.incidentSince) >= failureNotifyAfter
	if alert {
		state.incidentNoted = true
	}
	state.mu.Unlock()
	d.logger.Printf("%s sync failed (retry in %s): %v", name, delay.Round(time.Second), err)
	state.scheduleIfAbsent(delay, func() { d.syncRepo(state, true) })
	if alert {
		d.alerts.add(name, err)
	}
}

func backoffDelay(failures int) time.Duration {
	delay := minBackoff
	for i := 1; i < failures && delay < maxBackoff; i++ {
		delay *= 2
	}
	if delay > maxBackoff {
		delay = maxBackoff
	}
	return delay
}

func (d *daemon) noteOffBranch(state *repoState, reason string) {
	now := d.now()
	state.mu.Lock()
	if state.offBranchSince.IsZero() {
		state.offBranchSince = now
	}
	notify := !state.offBranchNoted && now.Sub(state.offBranchSince) >= offBranchNotifyAfter
	if notify {
		state.offBranchNoted = true
	}
	state.mu.Unlock()
	if notify {
		d.sendNotification(fmt.Sprintf("%s has not synced for %s: %s", state.config.Name, offBranchNotifyAfter, reason))
	}
}

// reportSecrets notifies once per blocked file while it stays blocked.
func (d *daemon) reportSecrets(state *repoState, blocked []string) {
	current := make(map[string]bool, len(blocked))
	for _, path := range blocked {
		current[path] = true
	}
	var fresh []string
	state.mu.Lock()
	for path := range state.secretsNoted {
		if !current[path] {
			delete(state.secretsNoted, path)
		}
	}
	for _, path := range blocked {
		if !state.secretsNoted[path] {
			state.secretsNoted[path] = true
			fresh = append(fresh, path)
		}
	}
	state.mu.Unlock()
	if len(fresh) == 0 {
		return
	}
	d.logger.Printf("%s: left secret file(s) out of sync: %s", state.config.Name, strings.Join(fresh, ", "))
	d.sendNotification(fmt.Sprintf("%s: %d secret file(s) were not synced (%s). Run `repo-sync allow <path>` inside the repo to include one.",
		state.config.Name, len(fresh), strings.Join(fresh, ", ")))
}

func (d *daemon) sendNotification(message string) {
	if err := d.notify(d.opCtx, d.runner, message); err != nil {
		d.logger.Printf("notification failed: %v", err)
	}
}

func (d *daemon) periodicRemoteSync() {
	ticker := time.NewTicker(d.cfg.FetchInterval.Duration)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.syncAllRemote()
		}
	}
}

func (d *daemon) syncAllRemote() {
	for _, state := range d.states {
		go d.syncRepo(state, false)
	}
}

// resetBackoff lets every repository retry immediately, e.g. after the
// network comes back or the machine wakes up.
func (d *daemon) resetBackoff() {
	for _, state := range d.states {
		state.mu.Lock()
		state.nextAttempt = time.Time{}
		state.mu.Unlock()
	}
}

func (d *daemon) healthLoop() {
	ticker := time.NewTicker(d.healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.healthCheck()
		}
	}
}

// healthCheck catches local changes whose file events were missed.
func (d *daemon) healthCheck() {
	for _, state := range d.states {
		if !state.isAvailable(d.now()) {
			continue
		}
		changed, _, err := d.syncer.changes(d.opCtx, state.config)
		if err == nil && len(changed) > 0 {
			state.scheduleIfAbsent(d.cfg.IdleDebounce.Duration, func() { d.syncRepo(state, true) })
		}
	}
}

func (d *daemon) wakeLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	last := time.Now()
	for {
		select {
		case <-d.ctx.Done():
			return
		case now := <-ticker.C:
			if now.Sub(last) > 45*time.Second {
				d.logger.Print("wake detected; syncing")
				d.resetBackoff()
				d.syncAllRemote()
			}
			last = now
		}
	}
}

func (d *daemon) networkLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	wasOnline := d.online(d.ctx)
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			online := d.online(d.ctx)
			if online && !wasOnline {
				d.logger.Print("network return detected; syncing")
				d.resetBackoff()
				d.syncAllRemote()
			}
			wasOnline = online
		}
	}
}

func networkOnline(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", "github.com:443")
	if err != nil {
		return false
	}
	connection.Close()
	return true
}

func (d *daemon) stopTimers() {
	d.alerts.stop()
	for _, state := range d.states {
		state.mu.Lock()
		if state.timer != nil {
			state.timer.Stop()
		}
		state.mu.Unlock()
	}
}
