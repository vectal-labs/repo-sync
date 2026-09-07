package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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

// errRepoMissing marks a sync attempt that found no repository at the
// configured path: deleted, renamed, or on a volume that is not mounted.
var errRepoMissing = errors.New("repository folder is missing")

// repoMissing reports whether the repository is absent right now. It looks
// for .git rather than the folder so an empty mount point also counts.
func repoMissing(path string) bool {
	_, err := os.Stat(filepath.Join(path, ".git"))
	return errors.Is(err, fs.ErrNotExist)
}

type repoState struct {
	config repoConfig
	// watchPath is the symlink-resolved path FSEvents reports under. It is set
	// once before any goroutine starts and stays empty when the folder was
	// missing at startup; such repositories are covered by the health poll.
	watchPath    string
	mu           sync.Mutex
	syncing      bool
	timer        *time.Timer
	lastOwnWrite time.Time

	// Retry and incident tracking. Nothing here is ever persisted.
	failures       int
	nextAttempt    time.Time
	incident       string // non-empty while a failure incident is open
	incidentSince  time.Time
	incidentNoted  bool // the open incident has already produced a popup
	lastSuccess    time.Time
	unavailable    bool   // the last attempt found the folder missing
	lastSkip       string // last skip reason logged, to avoid repeating it
	offBranchSince time.Time
	offBranchNoted bool
	secretsNoted   map[string]bool
	withheldNoted  map[string]bool // secret paths in unpublished commits already reported
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

	statusFile     string
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
		d.states[repo.Name] = &repoState{config: repo, secretsNoted: make(map[string]bool), withheldNoted: make(map[string]bool)}
	}
	return d
}

func runDaemon(ctx context.Context, configPath string) error {
	store := &configStore{path: configPath}
	cfg, err := store.load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger := log.New(os.Stdout, "repo-sync: ", log.LstdFlags)
	runner := backgroundRunner()
	runner.warn = logger.Printf
	d := newDaemon(ctx, cfg, runner, logger)
	d.statusFile = statusPath(configPath)
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
		// A folder that is not there is a failure of that one repository, never
		// a reason to stop the service. It keeps its config and the health loop
		// polls for it until it returns.
		resolved, err := filepath.EvalSymlinks(state.config.Path)
		if err != nil {
			d.logger.Printf("%s: cannot watch %s (%v); polling every %s until it returns", state.config.Name, state.config.Path, err, d.healthInterval)
			continue
		}
		state.watchPath = resolved
		paths = append(paths, resolved)
	}

	if len(paths) > 0 {
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
	}
	d.logger.Printf("watching %d repositories", len(d.states))
	if err := d.publishStatus(); err != nil {
		return fmt.Errorf("write service readiness: %w", err)
	}
	statusDone := make(chan struct{})
	go d.statusLoop(statusDone)
	defer func() {
		<-statusDone
		if d.statusFile != "" {
			_ = os.Remove(d.statusFile)
		}
	}()

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
		if state.watchPath == "" {
			continue
		}
		relative, err := filepath.Rel(state.watchPath, path)
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

// endSync releases the repository and, in the same step, arms the follow-up
// the finished cycle decided on (a retry backoff or a debounce for leftover
// changes) unless a timer is already pending. Doing both under one lock means
// no other cycle can slip in between the release and the scheduling decision.
func (s *repoState) endSync(next time.Duration, callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncing = false
	s.lastOwnWrite = time.Now()
	if next > 0 && s.timer == nil {
		s.setTimerLocked(next, callback)
	}
}

func (s *repoState) isAvailable(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.syncing && !now.Before(s.nextAttempt)
}

func (s *repoState) isUnavailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.unavailable
}

// resumeNow drops the pending retry and its backoff so the repository syncs at
// once, e.g. the moment a missing folder is back.
func (s *repoState) resumeNow(callback func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unavailable, s.nextAttempt = false, time.Time{}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.setTimerLocked(0, callback)
}

func (d *daemon) syncRepo(state *repoState, commitLocal bool) {
	missing := repoMissing(state.config.Path)
	if !commitLocal && !missing {
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
	// The cycle owns the repository until its result and retry decision are
	// applied. Releasing right after the Git work let a newer cycle succeed in
	// between, only to have this cycle's older failure overwrite it.
	var report syncReport
	var err error
	if missing {
		// Never run git in a folder that is gone. A missing folder is handled
		// like any other failure: backoff, one delayed popup, automatic recovery.
		err = fmt.Errorf("%w: %s", errRepoMissing, state.config.Path)
	} else {
		report, err = d.syncer.sync(d.opCtx, state.config, commitLocal)
	}
	if report.Scanned {
		d.reportSecrets(state, report.Blocked)
	}
	if report.Checked {
		d.reportWithheld(state, report)
	}
	next := d.handleResult(state, report, err)
	if next == 0 {
		changed, _, statusErr := d.syncer.changes(d.opCtx, state.config)
		if statusErr == nil && len(changed) > 0 {
			next = d.cfg.IdleDebounce.Duration
		}
	}
	state.endSync(next, func() { d.syncRepo(state, true) })
}

// handleResult applies the cycle's outcome and returns the retry backoff, or
// zero when the repository may sync again as soon as there is work.
func (d *daemon) handleResult(state *repoState, report syncReport, err error) time.Duration {
	name := state.config.Name
	if err == nil {
		state.mu.Lock()
		recovered := state.incident != ""
		state.failures, state.incident, state.nextAttempt = 0, "", time.Time{}
		state.incidentSince, state.incidentNoted, state.unavailable = time.Time{}, false, false
		state.lastSkip = ""
		state.lastSuccess = d.now()
		state.offBranchSince, state.offBranchNoted = time.Time{}, false
		state.mu.Unlock()
		if recovered {
			d.logger.Printf("%s recovered", name)
		}
		if summary := report.String(); summary != "" {
			d.logger.Printf("%s: %s", name, summary)
		}
		return 0
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
		return 0
	}

	// The folder may also vanish in the middle of a sync, which surfaces as a
	// plain git error. Either way the health loop must poll for its return.
	unavailable := errors.Is(err, errRepoMissing) || repoMissing(state.config.Path)
	now := d.now()
	state.mu.Lock()
	state.failures++
	delay := backoffDelay(state.failures)
	state.nextAttempt = now.Add(delay)
	if state.incident == "" {
		state.incidentSince = now
	}
	state.incident = err.Error()
	state.unavailable = unavailable
	// Being offline is not an incident worth a popup; it resolves itself. Other
	// failures get one popup, and only once they have lasted long enough to be
	// more than a blip.
	alert := !state.incidentNoted && !isOfflineError(err) && now.Sub(state.incidentSince) >= failureNotifyAfter
	if alert {
		state.incidentNoted = true
	}
	state.mu.Unlock()
	d.logger.Printf("%s sync failed (retry in %s): %v", name, delay.Round(time.Second), err)
	if alert {
		d.alerts.add(name, err)
	}
	return delay
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

// reportWithheld notifies once per secret path while a push stays held back
// because an unpublished commit adds or changes it. repo-sync never edits the
// user's commits to remove it; the message explains the manual way out, which
// differs for a file the remote already tracks.
func (d *daemon) reportWithheld(state *repoState, report syncReport) {
	current := make(map[string]bool, len(report.Withheld))
	for _, entry := range report.Withheld {
		current[entry.Path] = true
	}
	var fresh []withheldSecret
	state.mu.Lock()
	for path := range state.withheldNoted {
		if !current[path] {
			delete(state.withheldNoted, path)
		}
	}
	for _, entry := range report.Withheld {
		if !state.withheldNoted[entry.Path] {
			state.withheldNoted[entry.Path] = true
			fresh = append(fresh, entry)
		}
	}
	state.mu.Unlock()
	if len(fresh) == 0 {
		return
	}
	name := state.config.Name
	d.logger.Printf("%s: push held back; unpublished commits add or change secret file(s): %s", name, withheldList(fresh))
	d.sendNotification(fmt.Sprintf("%s: push held back. Unpublished commits add or change %d secret file(s): %s. %s Or run `repo-sync allow <path>` inside the repo. It retries automatically.",
		name, len(fresh), withheldList(fresh), withheldAdvice(fresh, state.config.Remote+"/"+report.Branch)))
}

// withheldAdvice explains how to get a held push moving again without
// repo-sync touching the user's commits. A commit that is already on the fetch
// source cannot be removed by any local reset, so that case is named as such.
func withheldAdvice(withheld []withheldSecret, upstream string) string {
	var local, fresh, tracked, onSource []string
	for _, entry := range withheld {
		switch {
		case entry.OnSource:
			onSource = append(onSource, entry.Path)
		case entry.Tracked:
			local, tracked = append(local, entry.Path), append(tracked, entry.Path)
		default:
			local, fresh = append(local, entry.Path), append(fresh, entry.Path)
		}
	}
	var advice []string
	if len(local) > 0 {
		advice = append(advice, fmt.Sprintf("Drop them from your unpublished commits, e.g. `git reset --soft %s`; repo-sync then recommits everything else.", upstream))
	}
	if len(fresh) > 0 {
		advice = append(advice, fmt.Sprintf("It leaves %s out.", strings.Join(fresh, ", ")))
	}
	if len(tracked) > 0 {
		advice = append(advice, fmt.Sprintf("The remote already tracks %s, so local changes to it never publish: save them outside the repo, then restore the published version with `git checkout %s -- <path>`.", strings.Join(tracked, ", "), upstream))
	}
	if len(onSource) > 0 {
		advice = append(advice, fmt.Sprintf("%s is already on %s but not at the separate push destination; no local reset removes it. Either point the push URL at a destination that has it (`git remote set-url --push`), or run `repo-sync allow <path>` to publish it there too.", strings.Join(onSource, ", "), upstream))
	}
	return strings.Join(advice, " ")
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

// healthCheck catches local changes whose file events were missed and brings
// back repositories whose folder has returned.
func (d *daemon) healthCheck() {
	for _, state := range d.states {
		if state.isUnavailable() {
			if !repoMissing(state.config.Path) {
				state.resumeNow(func() { d.syncRepo(state, true) })
			}
			continue
		}
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
