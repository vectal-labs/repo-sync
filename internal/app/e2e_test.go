package app

import (
	"bytes"
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

// TestEndToEndTwoClonesStayInSync runs the real daemon (FSEvents, git, timers)
// against a bare remote and two clones, the way a user experiences it.
func TestEndToEndTwoClonesStayInSync(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	remote, alpha := makeGitFixture(t)
	beta := filepath.Join(t.TempDir(), "beta")
	gitRun(t, "", "clone", "-q", remote, beta)
	configureGitUser(t, beta)

	cfg := config{
		IdleDebounce:  duration{300 * time.Millisecond},
		FetchInterval: duration{500 * time.Millisecond},
		Repositories: []repoConfig{
			{Name: "alpha", Path: alpha, Remote: "origin"},
			{Name: "beta", Path: beta, Remote: "origin"},
		},
	}
	var logs syncBuffer
	var mu sync.Mutex
	var messages []string
	ctx, cancel := context.WithCancel(context.Background())
	d := newDaemon(ctx, cfg, execCommandRunner{}, log.New(io.MultiWriter(&logs, testWriter{t}), "", log.Ltime))
	d.healthInterval = 500 * time.Millisecond
	d.online = func(context.Context) bool { return true }
	d.notify = func(_ context.Context, _ commandRunner, message string) error {
		mu.Lock()
		messages = append(messages, message)
		mu.Unlock()
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- d.run() }()
	// Fail fast with the real error if the daemon stops before we cancel it.
	waitFor := func(t *testing.T, what string, condition func() bool) {
		t.Helper()
		waitForOrStop(t, what, condition, done)
	}

	// 1. A local edit in alpha shows up in beta without anyone touching git.
	write(t, alpha, "note.md", "hello from alpha\n")
	waitFor(t, "beta receives note.md", func() bool {
		data, err := os.ReadFile(filepath.Join(beta, "note.md"))
		return err == nil && string(data) == "hello from alpha\n"
	})

	// 2. A secret file never reaches the remote, and the user hears about it once.
	write(t, alpha, ".env", "TOKEN=hunter2\n")
	write(t, alpha, "public.md", "fine\n")
	waitFor(t, "public.md is pushed", func() bool {
		return strings.Contains(gitOutput(t, alpha, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"), "public.md")
	})
	if tree := gitOutput(t, alpha, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"); strings.Contains(tree, ".env") {
		t.Fatalf(".env reached the remote:\n%s", tree)
	}
	waitFor(t, "secret notification", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(messages) == 1 && strings.Contains(messages[0], ".env")
	})

	// 3. Conflicting edits: one side wins, the other aborts its rebase, keeps its
	// commit, is never force-pushed over, and the service keeps going.
	write(t, alpha, "shared.txt", "alpha\n")
	write(t, beta, "shared.txt", "beta\n")
	waitFor(t, "one side lands on the remote", func() bool {
		_, err := os.Stat(filepath.Join(remote, "refs", "heads", "main"))
		if err != nil {
			return false
		}
		tree := gitOutput(t, alpha, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main")
		return strings.Contains(tree, "shared.txt")
	})
	waitFor(t, "conflict is aborted and logged", func() bool {
		return strings.Contains(logs.String(), "rebase aborted")
	})
	winner := strings.TrimSpace(gitOutput(t, alpha, "--git-dir", remote, "show", "main:shared.txt"))
	loser := beta
	if winner == "beta" {
		loser = alpha
	}
	if data, _ := os.ReadFile(filepath.Join(loser, "shared.txt")); strings.TrimSpace(string(data)) == winner {
		t.Fatalf("loser's local edit was overwritten by %q", winner)
	}
	for _, path := range []string{alpha, beta} {
		gitDir := strings.TrimSpace(gitOutput(t, path, "rev-parse", "--git-dir"))
		if _, err := os.Stat(filepath.Join(path, gitDir, "rebase-merge")); !os.IsNotExist(err) {
			t.Fatalf("%s left a rebase in progress", path)
		}
	}
	winnerPath := alpha
	if winner == "beta" {
		winnerPath = beta
	}
	write(t, winnerPath, "after-conflict.md", "still running\n")
	waitFor(t, "service still syncs after the conflict", func() bool {
		return strings.Contains(gitOutput(t, alpha, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"), "after-conflict.md")
	})

	// 4. Shutdown is graceful and bounded.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon exited with error: %v", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("daemon did not stop")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(messages) != 1 {
		t.Fatalf("expected exactly one notification (the secret), got %q", messages)
	}
}

func write(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitForOrStop(t *testing.T, what string, condition func() bool, stopped <-chan error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-stopped:
			t.Fatalf("daemon stopped early while waiting for %s: %v", what, err)
		default:
		}
		if condition() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// syncBuffer is a bytes.Buffer safe to read while the daemon writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// failPushOnce makes the first push in `dir` fail, the way a rejected push or
// a broken remote would, so a cycle ends with a scanned report and an error.
type failPushOnce struct {
	inner commandRunner
	dir   string
	mu    sync.Mutex
	fired bool
}

func (f *failPushOnce) run(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	if name == "git" && dir == f.dir && len(args) > 0 && args[0] == "push" {
		f.mu.Lock()
		first := !f.fired
		f.fired = true
		f.mu.Unlock()
		if first {
			return "", errors.New("git push origin: remote: simulated outage")
		}
	}
	return f.inner.run(ctx, dir, stdin, name, args...)
}

// TestEndToEndCycleFinishesBeforeNextStarts runs the real daemon and holds a
// cycle in its secret notification after its push failed. No newer cycle may
// touch that repository until the older result is applied, while another
// repository keeps syncing.
func TestEndToEndCycleFinishesBeforeNextStarts(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	alphaRemote, alpha := makeGitFixture(t)
	betaRemote, beta := makeGitFixture(t)
	// Commands use the configured path; only file watching resolves symlinks.
	cfg := config{
		IdleDebounce:  duration{300 * time.Millisecond},
		FetchInterval: duration{500 * time.Millisecond},
		Repositories: []repoConfig{
			{Name: "alpha", Path: alpha, Remote: "origin"},
			{Name: "beta", Path: beta, Remote: "origin"},
		},
	}
	var logs syncBuffer
	notifying := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newDaemon(ctx, cfg, &failPushOnce{inner: execCommandRunner{}, dir: alpha}, log.New(io.MultiWriter(&logs, testWriter{t}), "", log.Ltime))
	d.healthInterval = 500 * time.Millisecond
	d.online = func(context.Context) bool { return true }
	d.notify = func(_ context.Context, _ commandRunner, message string) error {
		if strings.Contains(message, ".env") {
			once.Do(func() { close(notifying) })
			<-release
		}
		return nil
	}
	// Cycle 1 commits public.md, leaves .env out, fails its push, then blocks
	// inside the secret notification with its failure not yet applied. The files
	// exist before start so the first cycle is the commit cycle.
	write(t, alpha, ".env", "TOKEN=hunter2\n")
	write(t, alpha, "public.md", "fine\n")
	done := make(chan error, 1)
	go func() { done <- d.run() }()
	waitFor := func(t *testing.T, what string, condition func() bool) {
		t.Helper()
		waitForOrStop(t, what, condition, done)
	}
	remoteTree := func(remote string) string {
		return gitOutput(t, alpha, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main")
	}
	select {
	case <-notifying:
	case <-time.After(30 * time.Second):
		t.Fatal("timed out waiting for the secret notification")
	}
	write(t, alpha, "second.md", "written while cycle 1 is finishing\n")
	write(t, beta, "other.md", "beta keeps going\n")
	waitFor(t, "beta syncs while alpha's cycle is finishing", func() bool {
		return strings.Contains(remoteTree(betaRemote), "other.md")
	})
	time.Sleep(3 * time.Second) // long enough for a debounce or health check to start cycle 2
	if tree := remoteTree(alphaRemote); strings.Contains(tree, "public.md") || strings.Contains(tree, "second.md") {
		t.Fatalf("a newer cycle pushed while the previous cycle was still finishing:\n%s", tree)
	}

	close(release)
	waitFor(t, "cycle 1's failure is applied", func() bool {
		return strings.Contains(logs.String(), "alpha sync failed")
	})
	state := d.states["alpha"]
	state.mu.Lock()
	failures := state.failures
	state.mu.Unlock()
	if failures != 1 {
		t.Fatalf("failures = %d after the failed cycle, want 1", failures)
	}

	// The retry (here forced, as the network-return path does) syncs in order,
	// pushes everything, and recovers.
	d.resetBackoff()
	d.syncRepo(state, true)
	waitFor(t, "retry pushes both files", func() bool {
		tree := remoteTree(alphaRemote)
		return strings.Contains(tree, "public.md") && strings.Contains(tree, "second.md")
	})
	if tree := remoteTree(alphaRemote); strings.Contains(tree, ".env") {
		t.Fatalf(".env reached the remote:\n%s", tree)
	}
	if !strings.Contains(logs.String(), "alpha recovered") {
		t.Fatal("recovery was not logged")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon exited with error: %v", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("daemon did not stop")
	}
}

// TestEndToEndMissingFolderNeverStopsTheService runs the real daemon with one
// healthy clone and one configured folder that does not exist. The healthy
// repository must keep syncing, the missing one must resume on its own when
// its folder appears, and a folder that vanishes mid-run must come back the
// same way. No restart, no `add`, no manual command.
func TestEndToEndMissingFolderNeverStopsTheService(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	alphaRemote, alpha := makeGitFixture(t)
	betaRemote, betaClone := makeGitFixture(t)
	beta := filepath.Join(t.TempDir(), "beta") // configured, but not there yet

	d, logs, notifications, done, cancel := startDaemon(t, []repoConfig{
		{Name: "alpha", Path: alpha, Remote: "origin"},
		{Name: "beta", Path: beta, Remote: "origin"},
	})
	waitFor := func(t *testing.T, what string, condition func() bool) {
		t.Helper()
		waitForOrStop(t, what, condition, done)
	}
	remoteHas := func(remote, file string) bool {
		return strings.Contains(gitOutput(t, "", "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"), file)
	}

	// 1. The healthy repository syncs although beta's folder is missing.
	write(t, alpha, "one.md", "alpha keeps going\n")
	waitFor(t, "alpha's edit reaches its remote", func() bool { return remoteHas(alphaRemote, "one.md") })
	waitFor(t, "beta is reported as missing", func() bool {
		return strings.Contains(logs.String(), "beta sync failed") && d.states["beta"].isUnavailable()
	})

	// 2. beta's folder appears. It syncs without a restart or a manual command.
	if err := os.Rename(betaClone, beta); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "beta recovers", func() bool { return strings.Contains(logs.String(), "beta recovered") })
	write(t, beta, "two.md", "beta is back\n")
	waitFor(t, "beta's edit reaches its remote", func() bool { return remoteHas(betaRemote, "two.md") })

	// 3. alpha vanishes while running: beta still syncs, alpha resumes on return.
	away := alpha + ".away"
	if err := os.Rename(alpha, away); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alpha is reported as missing", func() bool {
		return strings.Contains(logs.String(), "alpha sync failed") && d.states["alpha"].isUnavailable()
	})
	write(t, beta, "three.md", "beta while alpha is away\n")
	waitFor(t, "beta syncs while alpha is away", func() bool { return remoteHas(betaRemote, "three.md") })
	if err := os.Rename(away, alpha); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "alpha recovers", func() bool { return strings.Contains(logs.String(), "alpha recovered") })
	write(t, alpha, "four.md", "alpha is back\n")
	waitFor(t, "alpha's edit reaches its remote after returning", func() bool { return remoteHas(alphaRemote, "four.md") })

	// 4. Shutdown is graceful. Short absences never produce a popup.
	stopDaemon(t, done, cancel)
	if got := notifications(); len(got) != 0 {
		t.Fatalf("expected no notifications, got %q", got)
	}
}

// TestEndToEndAllFoldersMissingThenOneReturns starts the daemon with nothing
// to watch at all. It must idle and pick up the first folder that shows up.
func TestEndToEndAllFoldersMissingThenOneReturns(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	remote, clone := makeGitFixture(t)
	root := t.TempDir()
	notes, ideas := filepath.Join(root, "notes"), filepath.Join(root, "ideas")

	_, logs, notifications, done, cancel := startDaemon(t, []repoConfig{
		{Name: "notes", Path: notes, Remote: "origin"},
		{Name: "ideas", Path: ideas, Remote: "origin"},
	})
	waitForOrStop(t, "both repositories are reported as missing", func() bool {
		return strings.Contains(logs.String(), "notes sync failed") && strings.Contains(logs.String(), "ideas sync failed")
	}, done)

	if err := os.Rename(clone, notes); err != nil {
		t.Fatal(err)
	}
	write(t, notes, "hello.md", "first folder to return\n")
	waitForOrStop(t, "the returned folder syncs", func() bool {
		return strings.Contains(gitOutput(t, "", "--git-dir", remote, "ls-tree", "-r", "--name-only", "main"), "hello.md")
	}, done)

	stopDaemon(t, done, cancel)
	if got := notifications(); len(got) != 0 {
		t.Fatalf("expected no notifications, got %q", got)
	}
}

// startDaemon runs the real daemon on the given repositories with fast timers.
func startDaemon(t *testing.T, repos []repoConfig) (d *daemon, logs *syncBuffer, notifications func() []string, done chan error, cancel context.CancelFunc) {
	t.Helper()
	cfg := config{
		IdleDebounce:  duration{300 * time.Millisecond},
		FetchInterval: duration{500 * time.Millisecond},
		Repositories:  repos,
	}
	logs = &syncBuffer{}
	var mu sync.Mutex
	var messages []string
	ctx, cancel := context.WithCancel(context.Background())
	d = newDaemon(ctx, cfg, execCommandRunner{}, log.New(io.MultiWriter(logs, testWriter{t}), "", log.Ltime))
	d.healthInterval = 500 * time.Millisecond
	d.online = func(context.Context) bool { return true }
	d.notify = func(_ context.Context, _ commandRunner, message string) error {
		mu.Lock()
		messages = append(messages, message)
		mu.Unlock()
		return nil
	}
	done = make(chan error, 1)
	go func() { done <- d.run() }()
	t.Cleanup(cancel)
	notifications = func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), messages...)
	}
	return d, logs, notifications, done, cancel
}

func stopDaemon(t *testing.T, done <-chan error, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon exited with error: %v", err)
		}
	case <-time.After(shutdownGrace + 5*time.Second):
		t.Fatal("daemon did not stop")
	}
}

// TestEndToEndUntrackedIgnoredFileSurvivesSync runs the real daemon after a
// user untracks a secret but keeps it on disk and ignored. Root and nested
// paths, an unrelated remote edit and a conflicting one: the daemon must
// refuse the rebase, report a failure, keep running, and never touch or
// publish the retained copy.
func TestEndToEndUntrackedIgnoredFileSurvivesSync(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	for _, test := range []struct {
		name       string
		file       string
		remoteFile string
	}{
		{"unrelated remote edit", ".env", "teammate.txt"},
		{"conflicting remote edit", ".env", ".env"},
		{"nested path", "config/.env", "teammate.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			remote, local := makeGitFixture(t)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(local, test.file)), 0o755); err != nil {
				t.Fatal(err)
			}
			writeAndCommit(t, local, test.file, "old\n", "tracked baseline")
			gitRun(t, local, "push", "-q", "origin", "main")
			other := filepath.Join(t.TempDir(), "other")
			gitRun(t, "", "clone", "-q", remote, other)
			configureGitUser(t, other)
			if err := os.MkdirAll(filepath.Dir(filepath.Join(other, test.remoteFile)), 0o755); err != nil {
				t.Fatal(err)
			}
			writeAndCommit(t, other, test.remoteFile, "remote\n", "remote edit")
			gitRun(t, other, "push", "-q", "origin", "main")
			remoteBefore := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main"))

			gitRun(t, local, "rm", "-q", "--cached", test.file)
			want := "kept locally, never committed\n"
			write(t, local, test.file, want)
			write(t, local, ".gitignore", test.file+"\n")

			_, logs, _, done, cancel := startDaemon(t, []repoConfig{{Name: "notes", Path: local, Remote: "origin"}})
			waitForOrStop(t, "daemon refuses the rebase", func() bool {
				return strings.Contains(logs.String(), "sync failed") && strings.Contains(logs.String(), "ignored file "+test.file+" would be overwritten")
			}, done)
			stopDaemon(t, done, cancel)

			if got, err := os.ReadFile(filepath.Join(local, test.file)); err != nil || string(got) != want {
				t.Fatalf("daemon removed or altered the retained file: %q, %v", got, err)
			}
			if got := strings.TrimSpace(gitOutput(t, local, "--git-dir", remote, "rev-parse", "main")); got != remoteBefore {
				t.Fatalf("remote main moved to %s; nothing may be pushed", got)
			}
			if got := gitOutput(t, local, "status", "--porcelain"); got != "" {
				t.Fatalf("status = %q, want clean", got)
			}
		})
	}
}
