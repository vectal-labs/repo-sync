package main

import (
	"context"
	"errors"
	"io"
	"log"
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
		cfg.Repositories = append(cfg.Repositories, repoConfig{Name: name, Path: filepath.Join(t.TempDir(), name), Remote: "origin"})
	}
	fake := &fakeSyncer{results: make(map[string]func() (syncReport, error)), calls: make(map[string]int)}
	td := &testDaemon{syncer: fake, clock: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
	td.daemon = newDaemon(context.Background(), cfg, execCommandRunner{}, log.New(io.Discard, "", 0))
	td.daemon.syncer = fake
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
	state.endSync()
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
