package main

import (
	"bytes"
	"context"
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
