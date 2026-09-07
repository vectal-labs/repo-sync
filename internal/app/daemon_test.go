package app

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeSyncer struct {
	mu      sync.Mutex
	results map[string]func() (syncReport, error)
	calls   map[string]int
}

func (f *fakeSyncer) sync(_ context.Context, repo repoConfig, _ bool) (syncReport, error) {
	f.mu.Lock()
	f.calls[repo.Name]++
	result := f.results[repo.Name]
	f.mu.Unlock()
	if result == nil {
		return syncReport{}, nil
	}
	return result()
}

func (f *fakeSyncer) changes(context.Context, repoConfig) ([]change, []change, error) {
	return nil, nil, nil
}

func (f *fakeSyncer) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

type testDaemon struct {
	*daemon
	syncer   *fakeSyncer
	clock    time.Time
	messages []string
	mu       sync.Mutex
}

func newTestDaemon(t *testing.T, names ...string) *testDaemon {
	t.Helper()
	cfg := newDefaultConfig()
	for _, name := range names {
		path := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(filepath.Join(path, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg.Repositories = append(cfg.Repositories, repoConfig{Name: name, Path: path, Remote: "origin"})
	}
	fake := &fakeSyncer{results: make(map[string]func() (syncReport, error)), calls: make(map[string]int)}
	td := newTestDaemonWithSyncer(t, cfg, fake)
	td.syncer = fake
	return td
}

// newGitTestDaemon drives the daemon against a real Git checkout with a fake
// clock, so backoff and notification timing can be checked without waiting.
func newGitTestDaemon(t *testing.T, name, path string) *testDaemon {
	t.Helper()
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: name, Path: path, Remote: "origin"}}
	return newTestDaemonWithSyncer(t, cfg, gitSyncer{runner: execCommandRunner{}})
}

func newTestDaemonWithSyncer(t *testing.T, cfg config, syncer syncer) *testDaemon {
	t.Helper()
	td := &testDaemon{clock: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	td.daemon = newDaemon(context.Background(), cfg, execCommandRunner{}, log.New(io.Discard, "", 0))
	td.daemon.syncer = syncer
	td.daemon.now = func() time.Time {
		td.mu.Lock()
		defer td.mu.Unlock()
		return td.clock
	}
	td.daemon.online = func(context.Context) bool { return true }
	// Tests flush alerts by hand so the coalescing timer never races them.
	td.daemon.alerts.window = time.Hour
	td.daemon.notify = func(_ context.Context, _ commandRunner, message string) error {
		td.mu.Lock()
		td.messages = append(td.messages, message)
		td.mu.Unlock()
		return nil
	}
	t.Cleanup(td.daemon.stopTimers)
	return td
}

func (td *testDaemon) advance(delta time.Duration) {
	td.mu.Lock()
	td.clock = td.clock.Add(delta)
	td.mu.Unlock()
}

func (td *testDaemon) notifications() []string {
	td.alerts.flush()
	td.mu.Lock()
	defer td.mu.Unlock()
	return append([]string(nil), td.messages...)
}

func TestSyncDoesNotCancelPendingDebounce(t *testing.T) {
	state := &repoState{}
	fired := make(chan struct{}, 1)
	state.schedule(30*time.Millisecond, func() { fired <- struct{}{} })
	if ok, _ := state.beginSync(time.Now()); !ok {
		t.Fatal("sync did not start")
	}
	state.endSync(0, nil)
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("sync canceled the pending debounce")
	}
}

func TestNewEditResetsDebounce(t *testing.T) {
	state := &repoState{}
	fired := make(chan struct{}, 2)
	state.schedule(40*time.Millisecond, func() { fired <- struct{}{} })
	time.Sleep(20 * time.Millisecond)
	state.schedule(40*time.Millisecond, func() { fired <- struct{}{} })
	select {
	case <-fired:
		t.Fatal("debounce fired before the second quiet period elapsed")
	case <-time.After(25 * time.Millisecond):
	}
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("debounce never fired")
	}
}

func TestBackoffDelay(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, expected := range want {
		if got := backoffDelay(i + 1); got != expected {
			t.Errorf("backoffDelay(%d) = %s, want %s", i+1, got, expected)
		}
	}
}

func TestFailuresBackOffPerRepoAndNotifyOnce(t *testing.T) {
	td := newTestDaemon(t, "broken", "healthy")
	td.syncer.results["broken"] = func() (syncReport, error) { return syncReport{}, errors.New("push rejected") }
	broken, healthy := td.states["broken"], td.states["healthy"]

	td.syncRepo(broken, true)
	td.syncRepo(broken, true) // inside the backoff window: must not run
	if got := td.syncer.callCount("broken"); got != 1 {
		t.Fatalf("broken synced %d times during backoff, want 1", got)
	}
	td.syncRepo(healthy, true)
	if got := td.syncer.callCount("healthy"); got != 1 {
		t.Fatal("a failing repository blocked a healthy one")
	}

	td.advance(2 * time.Minute)
	td.syncRepo(broken, true)
	if got := td.syncer.callCount("broken"); got != 2 {
		t.Fatalf("broken synced %d times after backoff, want 2", got)
	}
	if got := td.notifications(); len(got) != 0 {
		t.Fatalf("a short failure must not notify yet: %q", got)
	}
	broken.mu.Lock()
	failures, next := broken.failures, broken.nextAttempt
	broken.mu.Unlock()
	if failures != 2 || next.Sub(td.now()) != 2*time.Minute {
		t.Fatalf("failures = %d, next attempt in %s", failures, next.Sub(td.now()))
	}

	td.advance(failureNotifyAfter)
	td.syncRepo(broken, true)
	td.advance(time.Hour)
	td.syncRepo(broken, true)
	if got := td.notifications(); len(got) != 1 || !strings.Contains(got[0], "broken") {
		t.Fatalf("notifications = %q, want exactly one for the incident", got)
	}

	td.syncer.results["broken"] = nil
	td.advance(time.Hour)
	td.syncRepo(broken, true)
	broken.mu.Lock()
	defer broken.mu.Unlock()
	if broken.failures != 0 || broken.incident != "" || !broken.nextAttempt.IsZero() || broken.incidentNoted {
		t.Fatalf("recovery did not reset state: %+v", broken)
	}
}

func TestOfflineFailuresNeverNotify(t *testing.T) {
	td := newTestDaemon(t, "notes")
	td.syncer.results["notes"] = func() (syncReport, error) {
		return syncReport{}, errors.New("git fetch origin: fatal: unable to access 'https://github.com/x/notes.git/': Could not resolve host: github.com")
	}
	for i := 0; i < 5; i++ {
		td.syncRepo(td.states["notes"], true)
		td.advance(time.Hour)
	}
	if got := td.notifications(); len(got) != 0 {
		t.Fatalf("offline failure produced notifications: %q", got)
	}
}

func TestBurstOfFailingReposSharesOneNotification(t *testing.T) {
	td := newTestDaemon(t, "comp", "legal", "ideas")
	tlsError := errors.New("git fetch origin: exit status 128: fatal: unable to access 'https://github.com/x/y.git/': LibreSSL/3.3.6: error:1404B42E:SSL routines:ST_CONNECT:tlsv1 alert protocol version")
	for name := range td.states {
		td.syncer.results[name] = func() (syncReport, error) { return syncReport{}, tlsError }
	}
	syncAll := func() {
		for _, state := range td.states {
			td.syncRepo(state, true)
		}
	}
	syncAll()
	td.advance(failureNotifyAfter - time.Minute)
	syncAll()
	if got := td.notifications(); len(got) != 0 {
		t.Fatalf("notified before the threshold: %q", got)
	}
	td.advance(2 * time.Minute)
	syncAll()
	got := td.notifications()
	if len(got) != 1 {
		t.Fatalf("notifications = %q, want one shared popup", got)
	}
	if !strings.Contains(got[0], "3 repositories") || !strings.Contains(got[0], "comp, ideas, legal") || !strings.Contains(got[0], "tlsv1") {
		t.Fatalf("shared popup is missing the repos or the cause: %q", got[0])
	}
}

func TestOffBranchNotifiesOnceAfterThreshold(t *testing.T) {
	td := newTestDaemon(t, "notes")
	td.syncer.results["notes"] = func() (syncReport, error) {
		return syncReport{}, &skipError{reason: "branch is feature; only main is synced", offBranch: true}
	}
	state := td.states["notes"]
	td.syncRepo(state, true)
	td.advance(offBranchNotifyAfter - time.Minute)
	td.syncRepo(state, true)
	if got := td.notifications(); len(got) != 0 {
		t.Fatalf("notified too early: %q", got)
	}
	td.advance(2 * time.Minute)
	td.syncRepo(state, true)
	td.syncRepo(state, true)
	if got := td.notifications(); len(got) != 1 || !strings.Contains(got[0], "feature") {
		t.Fatalf("notifications = %q, want exactly one", got)
	}
	if got := td.syncer.callCount("notes"); got != 4 {
		t.Fatalf("skips must never back off; synced %d times, want 4", got)
	}

	td.syncer.results["notes"] = nil
	td.syncRepo(state, true)
	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.offBranchSince.IsZero() || state.offBranchNoted {
		t.Fatal("returning to the default branch did not reset the tracker")
	}
}

func TestSecretFilesNotifyOncePerIncident(t *testing.T) {
	td := newTestDaemon(t, "notes")
	blocked := []string{".env"}
	td.syncer.results["notes"] = func() (syncReport, error) {
		return syncReport{Scanned: true, Blocked: blocked}, nil
	}
	state := td.states["notes"]
	td.syncRepo(state, true)
	td.syncRepo(state, true)
	if got := td.notifications(); len(got) != 1 || !strings.Contains(got[0], ".env") {
		t.Fatalf("notifications = %q, want one", got)
	}
	blocked = nil
	td.syncRepo(state, true)
	blocked = []string{".env"}
	td.syncRepo(state, true)
	if got := td.notifications(); len(got) != 2 {
		t.Fatalf("a new incident for the same file must notify again: %q", got)
	}
}

// TestPersistentRemoteLockBacksOffNotifiesOnceAndRecovers runs the daemon's
// cycle against a real remote whose default branch is stuck behind a leftover
// lock file. That is not a teammate racing us, so it must follow the normal
// failure path: backoff, one delayed popup, and a clean recovery.
func TestPersistentRemoteLockBacksOffNotifiesOnceAndRecovers(t *testing.T) {
	remote, local := makeGitFixture(t)
	write(t, local, "public.txt", "public\n")
	lock := filepath.Join(remote, "refs", "heads", "main.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	td := newGitTestDaemon(t, "notes", local)
	state := td.states["notes"]

	td.syncRepo(state, true)
	state.mu.Lock()
	failures, incident, next := state.failures, state.incident, state.nextAttempt
	state.mu.Unlock()
	if failures != 1 || !strings.Contains(incident, "cannot lock ref") || next.Sub(td.now()) != minBackoff {
		t.Fatalf("failures = %d, incident = %q, next attempt in %s; want one normal failure with backoff", failures, incident, next.Sub(td.now()))
	}
	if got := td.notifications(); len(got) != 0 {
		t.Fatalf("a fresh failure must stay silent: %q", got)
	}

	td.advance(2 * time.Minute)
	td.syncRepo(state, true)
	td.advance(failureNotifyAfter)
	td.syncRepo(state, true)
	td.advance(time.Hour)
	td.syncRepo(state, true)
	got := td.notifications()
	if len(got) != 1 || !strings.Contains(got[0], "notes") || !strings.Contains(got[0], "cannot lock ref") {
		t.Fatalf("notifications = %q, want exactly one naming the repo and the cause", got)
	}
	if tree := gitOutput(t, local, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"); strings.Contains(tree, "public.txt") {
		t.Fatalf("remote moved while locked:\n%s", tree)
	}

	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	td.advance(time.Hour)
	td.syncRepo(state, true)
	state.mu.Lock()
	failures, incident, noted := state.failures, state.incident, state.incidentNoted
	state.mu.Unlock()
	if failures != 0 || incident != "" || noted {
		t.Fatalf("recovery did not reset state: failures = %d, incident = %q, noted = %v", failures, incident, noted)
	}
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:public.txt"); got != "public\n" {
		t.Fatalf("kept commit was not pushed after unlock: %q", got)
	}
	if got := td.notifications(); len(got) != 1 {
		t.Fatalf("recovery must not add a popup: %q", got)
	}
}

// A cycle owns its repository until its result and retry decision are applied.
// Releasing it after the Git work but before the bookkeeping let a newer cycle
// succeed in between, only to be overwritten by the older failure.
func TestCycleOwnsRepoUntilResultApplied(t *testing.T) {
	td := newTestDaemon(t, "notes", "other")
	state := td.states["notes"]
	td.syncer.results["notes"] = func() (syncReport, error) {
		return syncReport{Scanned: true, Blocked: []string{".env"}}, errors.New("push rejected")
	}
	notifying := make(chan struct{})
	release := make(chan struct{})
	td.daemon.notify = func(context.Context, commandRunner, string) error {
		close(notifying)
		<-release
		return nil
	}
	firstDone := make(chan struct{})
	go func() {
		td.syncRepo(state, true)
		close(firstDone)
	}()
	<-notifying

	// While the first cycle is still reporting, the same repository must not
	// start another cycle, but other repositories keep syncing.
	td.syncer.results["notes"] = nil
	secondDone := make(chan struct{})
	go func() {
		td.syncRepo(state, true)
		close(secondDone)
	}()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("second cycle waited on the first instead of being rejected")
	}
	if got := td.syncer.callCount("notes"); got != 1 {
		t.Fatalf("notes synced %d times while a cycle was still finishing, want 1", got)
	}
	td.syncRepo(td.states["other"], true)
	if got := td.syncer.callCount("other"); got != 1 {
		t.Fatal("a finishing cycle in one repository blocked another repository")
	}

	close(release)
	<-firstDone
	state.mu.Lock()
	failures, next, retryPending := state.failures, state.nextAttempt, state.timer != nil
	state.mu.Unlock()
	if failures != 1 || next.Sub(td.now()) != time.Minute {
		t.Fatalf("first cycle's failure was not applied: failures=%d next attempt in %s", failures, next.Sub(td.now()))
	}
	if !retryPending {
		t.Fatal("retry was not scheduled once the cycle released the repository")
	}

	// The retry runs the next cycle in order and recovers.
	td.advance(2 * time.Minute)
	td.syncRepo(state, true)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.failures != 0 || state.incident != "" || !state.nextAttempt.IsZero() {
		t.Fatalf("recovery did not reset state: failures=%d incident=%q", state.failures, state.incident)
	}
	if got := td.syncer.callCount("notes"); got != 2 {
		t.Fatalf("notes synced %d times, want 2", got)
	}
}

func TestMissingFolderBacksOffNotifiesOnceAndResumes(t *testing.T) {
	td := newTestDaemon(t, "gone", "healthy")
	gone := td.states["gone"]
	if err := os.RemoveAll(gone.config.Path); err != nil {
		t.Fatal(err)
	}

	td.syncRepo(gone, true)
	td.syncRepo(gone, true) // inside the backoff window: must not run
	td.syncRepo(td.states["healthy"], true)
	if got := td.syncer.callCount("gone"); got != 0 {
		t.Fatalf("git ran %d times in a missing folder", got)
	}
	if got := td.syncer.callCount("healthy"); got != 1 {
		t.Fatal("a missing folder blocked a healthy repository")
	}
	td.healthCheck() // still missing: nothing to resume
	gone.mu.Lock()
	failures, next, unavailable := gone.failures, gone.nextAttempt, gone.unavailable
	gone.mu.Unlock()
	if failures != 1 || !unavailable || next.Sub(td.now()) != time.Minute {
		t.Fatalf("failures = %d, unavailable = %v, next attempt in %s", failures, unavailable, next.Sub(td.now()))
	}

	td.advance(2 * time.Minute)
	td.syncRepo(gone, true)
	gone.mu.Lock()
	failures, next = gone.failures, gone.nextAttempt
	gone.mu.Unlock()
	if failures != 2 || next.Sub(td.now()) != 2*time.Minute {
		t.Fatalf("retries are not backing off: failures = %d, next attempt in %s", failures, next.Sub(td.now()))
	}
	if got := td.notifications(); len(got) != 0 {
		t.Fatalf("a short absence must not notify: %q", got)
	}

	td.advance(failureNotifyAfter)
	td.syncRepo(gone, true)
	td.advance(time.Hour)
	td.syncRepo(gone, true)
	if got := td.notifications(); len(got) != 1 || !strings.Contains(got[0], "gone") || !strings.Contains(got[0], "missing") {
		t.Fatalf("notifications = %q, want exactly one for the missing folder", got)
	}

	// The folder returns: the health check syncs it right away, ahead of the
	// pending backoff, and the incident is closed.
	if err := os.MkdirAll(filepath.Join(gone.config.Path, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	td.healthCheck()
	deadline := time.Now().Add(2 * time.Second)
	for td.syncer.callCount("gone") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := td.syncer.callCount("gone"); got != 1 {
		t.Fatalf("returned folder synced %d times, want 1", got)
	}
	gone.mu.Lock()
	defer gone.mu.Unlock()
	if gone.failures != 0 || gone.incident != "" || gone.unavailable || !gone.nextAttempt.IsZero() {
		t.Fatalf("recovery did not reset state: failures=%d incident=%q unavailable=%v", gone.failures, gone.incident, gone.unavailable)
	}
}

func TestWithheldPushNotifiesOncePerIncident(t *testing.T) {
	td := newTestDaemon(t, "notes")
	withheld := []withheldSecret{{Path: ".env", Commit: "abc1234"}}
	td.syncer.results["notes"] = func() (syncReport, error) {
		return syncReport{Branch: "main", Checked: true, Withheld: withheld}, &skipError{reason: "push withheld"}
	}
	state := td.states["notes"]
	td.syncRepo(state, true)
	td.syncRepo(state, true)
	got := td.notifications()
	if len(got) != 1 || !strings.Contains(got[0], ".env (abc1234)") || !strings.Contains(got[0], "repo-sync allow") || !strings.Contains(got[0], "git reset --soft origin/main") {
		t.Fatalf("notifications = %q, want one with file, commit, and the way out", got)
	}
	if got := td.syncer.callCount("notes"); got != 2 {
		t.Fatalf("a held push must keep retrying without backoff; synced %d times", got)
	}
	// A cycle that never reached the push check keeps the incident open.
	td.syncer.results["notes"] = func() (syncReport, error) {
		return syncReport{}, errors.New("git fetch origin: could not resolve host: github.com")
	}
	td.syncRepo(state, true)
	td.advance(time.Hour)
	td.syncer.results["notes"] = func() (syncReport, error) {
		return syncReport{Branch: "main", Checked: true, Withheld: withheld}, &skipError{reason: "push withheld"}
	}
	td.syncRepo(state, true)
	if got := td.notifications(); len(got) != 1 {
		t.Fatalf("an offline blip must not repeat the popup: %q", got)
	}
	// Resolved, then held again: a new incident notifies again.
	td.syncer.results["notes"] = func() (syncReport, error) { return syncReport{Branch: "main", Checked: true, Pushed: true}, nil }
	td.syncRepo(state, true)
	td.syncer.results["notes"] = func() (syncReport, error) {
		return syncReport{Branch: "main", Checked: true, Withheld: withheld}, &skipError{reason: "push withheld"}
	}
	td.syncRepo(state, true)
	if got := td.notifications(); len(got) != 2 {
		t.Fatalf("notifications = %q, want two", got)
	}
}

// TestDaemonHoldsSecretPushWhileOtherRepoKeepsSyncing drives the real git
// syncer through the daemon: one repository with a hand-made secret commit,
// one healthy.
func TestDaemonHoldsSecretPushWhileOtherRepoKeepsSyncing(t *testing.T) {
	leakyRemote, leaky := makeGitFixture(t)
	cleanRemote, clean := makeGitFixture(t)
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{
		{Name: "leaky", Path: leaky, Remote: "origin"},
		{Name: "clean", Path: clean, Remote: "origin"},
	}
	var mu sync.Mutex
	var messages []string
	d := newDaemon(context.Background(), cfg, execCommandRunner{}, log.New(io.Discard, "", 0))
	d.notify = func(_ context.Context, _ commandRunner, message string) error {
		mu.Lock()
		messages = append(messages, message)
		mu.Unlock()
		return nil
	}
	t.Cleanup(d.stopTimers)

	writeAndCommit(t, leaky, ".env", "TOKEN=1\n", "by hand")
	write(t, clean, "note.md", "fine\n")
	d.syncRepo(d.states["leaky"], true)
	d.syncRepo(d.states["leaky"], true)
	d.syncRepo(d.states["clean"], true)

	if tree := gitOutput(t, leaky, "--git-dir", leakyRemote, "ls-tree", "-r", "--name-only", "main"); strings.Contains(tree, ".env") {
		t.Fatalf(".env reached the remote:\n%s", tree)
	}
	if tree := gitOutput(t, clean, "--git-dir", cleanRemote, "ls-tree", "-r", "--name-only", "main"); !strings.Contains(tree, "note.md") {
		t.Fatalf("the healthy repository stopped syncing:\n%s", tree)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(messages) != 1 || !strings.Contains(messages[0], "leaky") || !strings.Contains(messages[0], ".env") || strings.Contains(messages[0], "TOKEN") {
		t.Fatalf("messages = %q, want one for leaky naming .env without contents", messages)
	}
}

// failingRunner fails one git subcommand and passes everything else through.
type failingRunner struct {
	inner      commandRunner
	subcommand string
}

func (f failingRunner) run(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	for _, arg := range args {
		if arg == f.subcommand {
			return "", errors.New("git " + f.subcommand + ": temporary failure")
		}
	}
	return f.inner.run(ctx, dir, stdin, name, args...)
}

func TestFailedScanKeepsWithheldIncidentOpen(t *testing.T) {
	_, local := makeGitFixture(t)
	writeAndCommit(t, local, ".env", "TOKEN=1\n", "by hand")
	td := newGitTestDaemon(t, "notes", local)
	state := td.states["notes"]
	td.syncRepo(state, true)
	if got := td.notifications(); len(got) != 1 {
		t.Fatalf("notifications = %q, want one", got)
	}
	// A cycle whose inspection fails must not count as "nothing withheld".
	td.daemon.syncer = gitSyncer{runner: failingRunner{inner: execCommandRunner{}, subcommand: "diff-tree"}}
	td.syncRepo(state, true)
	td.advance(time.Hour)
	td.daemon.syncer = gitSyncer{runner: execCommandRunner{}}
	td.syncRepo(state, true)
	if got := td.notifications(); len(got) != 1 {
		t.Fatalf("a failed scan re-notified the same incident: %q", got)
	}
}

// TestDaemonNeverLogsPushURLCredentials runs the real daemon against a push
// URL carrying a synthetic password that fails to connect; neither the log nor
// the stored incident may contain it.
func TestDaemonNeverLogsPushURLCredentials(t *testing.T) {
	_, local := makeGitFixture(t)
	write(t, local, "safe.txt", "safe\n")
	gitRun(t, local, "remote", "set-url", "--push", "origin", "http://review-user:review-credential@127.0.0.1:1/repo.git")
	cfg := config{IdleDebounce: duration{100 * time.Millisecond}, FetchInterval: duration{500 * time.Millisecond},
		Repositories: []repoConfig{{Name: "notes", Path: local, Remote: "origin"}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &syncBuffer{}
	d := newDaemon(ctx, cfg, execCommandRunner{}, log.New(logs, "", 0))
	d.healthInterval = 100 * time.Millisecond
	d.online = func(context.Context) bool { return true }
	d.notify = func(context.Context, commandRunner, string) error { return nil }
	done := make(chan error, 1)
	go func() { done <- d.run() }()
	waitForOrStop(t, "remote failure is logged", func() bool { return strings.Contains(logs.String(), "sync failed") }, done)
	stopDaemon(t, done, cancel)
	if strings.Contains(logs.String(), "review-credential") {
		t.Fatal("the daemon wrote the push URL password to its log")
	}
	state := d.states["notes"]
	state.mu.Lock()
	incident := state.incident
	state.mu.Unlock()
	if incident == "" || strings.Contains(incident, "review-credential") {
		t.Fatalf("incident must be recorded without the password (empty=%v)", incident == "")
	}
}
