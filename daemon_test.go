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
	if got := td.notifications(); len(got) != 1 || !strings.Contains(got[0], "broken") {
		t.Fatalf("notifications = %q, want exactly one for the incident", got)
	}
	broken.mu.Lock()
	failures, next := broken.failures, broken.nextAttempt
	broken.mu.Unlock()
	if failures != 2 || next.Sub(td.now()) != 2*time.Minute {
		t.Fatalf("failures = %d, next attempt in %s", failures, next.Sub(td.now()))
	}

	td.syncer.results["broken"] = nil
	td.advance(time.Hour)
	td.syncRepo(broken, true)
	broken.mu.Lock()
	defer broken.mu.Unlock()
	if broken.failures != 0 || broken.incident != "" || !broken.nextAttempt.IsZero() {
		t.Fatalf("recovery did not reset state: %+v", broken)
	}
}

func TestOfflineFailuresDoNotNotify(t *testing.T) {
	td := newTestDaemon(t, "notes")
	td.online = func(context.Context) bool { return false }
	td.syncer.results["notes"] = func() (syncReport, error) { return syncReport{}, errors.New("could not resolve host") }
	td.syncRepo(td.states["notes"], true)
	if got := td.notifications(); len(got) != 0 {
		t.Fatalf("offline failure produced notifications: %q", got)
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
