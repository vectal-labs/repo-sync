package app

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEndToEndConflictIsVisibleImmediately(t *testing.T) {
	remote, local := makeGitFixture(t)
	writeAndCommit(t, local, "shared.txt", "local\n", "local work")
	pushFromTeammate(t, remote, "shared.txt", "remote\n")
	d, logs, notifications, done, cancel := startDaemon(t, []repoConfig{{Name: "notes", Path: local, Remote: "origin"}})
	defer stopDaemon(t, done, cancel)
	waitForOrStop(t, "conflict detected", func() bool { return strings.Contains(logs.String(), "rebase aborted") }, done)
	status := d.statusSnapshot().Repositories[0]
	if status.State != "conflict" {
		t.Fatalf("conflict must be visible, got %s (%s); notifications=%q", status.State, status.Detail, notifications())
	}
	if got := notifications(); len(got) != 1 || !strings.Contains(got[0], "Conflict in notes") {
		t.Fatalf("want one immediate conflict notification, got %q", got)
	}
}

func TestEndToEndConflictRestartGuideAndManualRepair(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	remote, local := makeGitFixture(t)
	healthyRemote, healthy := makeGitFixture(t)
	writeAndCommit(t, local, "shared.txt", "local\n", "local work")
	pushFromTeammate(t, remote, "shared.txt", "remote\n")
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config{IdleDebounce: duration{100 * time.Millisecond}, FetchInterval: duration{200 * time.Millisecond}, Repositories: []repoConfig{
		{Name: "notes", Path: local, Remote: "origin"}, {Name: "healthy", Path: healthy, Remote: "origin"},
	}}
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	start := func() (*daemon, func() []string, chan error, context.CancelFunc) {
		ctx, cancel := context.WithCancel(context.Background())
		d := newDaemon(ctx, cfg, execCommandRunner{}, log.New(io.Discard, "", 0))
		d.configPath, d.conflictDir, d.statusFile = configPath, conflictsPath(configPath), statusPath(configPath)
		d.healthInterval = 100 * time.Millisecond
		d.online = func(context.Context) bool { return true }
		var mu sync.Mutex
		var messages []string
		d.notify = func(_ context.Context, _ commandRunner, message string) error {
			mu.Lock()
			defer mu.Unlock()
			messages = append(messages, message)
			return nil
		}
		done := make(chan error, 1)
		go func() { done <- d.run() }()
		t.Cleanup(cancel)
		return d, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), messages...) }, done, cancel
	}
	_, messages, done, cancel := start()
	waitForOrStop(t, "first conflict notification", func() bool { return len(messages()) == 1 }, done)
	stopDaemon(t, done, cancel)
	d, messages, done, cancel := start()
	defer stopDaemon(t, done, cancel)
	waitForOrStop(t, "restored conflict status", func() bool {
		data, err := os.ReadFile(statusPath(configPath))
		if err != nil {
			return false
		}
		return strings.Contains(string(data), `"state": "conflict"`)
	}, done)
	write(t, healthy, "still-syncs.txt", "other repository continues\n")
	waitForOrStop(t, "healthy repository sync during conflict", func() bool { return strings.Contains(remoteTree(t, healthy, healthyRemote), "still-syncs.txt") }, done)
	binary := filepath.Join(t.TempDir(), "repo-sync")
	if err := os.WriteFile(binary, uninstallBinaryBytes(t), 0o755); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(binary, "conflicts", "--config", configPath, local).CombinedOutput()
	if err != nil || !strings.Contains(string(output), "Repair guide with both versions:") {
		t.Fatalf("CLI: %v\n%s", err, output)
	}
	// Start the manual rebase when the daemon is between cycles. A genuine
	// Git lock refusal is retried, never removed.
	waitForOrStop(t, "manual rebase starts", func() bool {
		err := humanGit(local, "rebase", "origin/main")
		return err != nil && strings.Contains(err.Error(), "CONFLICT") && (gitSyncer{runner: execCommandRunner{}}).rebaseInProgress(context.Background(), local)
	}, done)
	write(t, local, "shared.txt", "local and remote\n")
	manualIndex := mustRead(t, filepath.Join(local, ".git", "index"))
	write(t, healthy, "during-repair.txt", "still syncing\n")
	waitForOrStop(t, "another repo syncs during manual repair", func() bool { return strings.Contains(remoteTree(t, healthy, healthyRemote), "during-repair.txt") }, done)
	if string(mustRead(t, filepath.Join(local, "shared.txt"))) != "local and remote\n" || string(mustRead(t, filepath.Join(local, ".git", "index"))) != string(manualIndex) {
		t.Fatalf("daemon changed manual repair: contents=%q, index changed=%v", string(mustRead(t, filepath.Join(local, "shared.txt"))), string(mustRead(t, filepath.Join(local, ".git", "index"))) != string(manualIndex))
	}
	gitRun(t, local, "add", "shared.txt")
	gitRun(t, local, "-c", "core.editor=true", "rebase", "--continue")
	waitForOrStop(t, "full recovery", func() bool {
		state := d.states["notes"]
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.conflict == nil && !state.lastSuccess.IsZero()
	}, done)
	if got := gitOutput(t, local, "--git-dir", remote, "show", "main:shared.txt"); got != "local and remote\n" {
		t.Fatalf("remote resolution: %q", got)
	}
	if len(messages()) != 0 {
		t.Fatalf("restart/retries sent duplicate alert: %q", messages())
	}
	if incident, err := readConflict(conflictsPath(configPath), cfg.Repositories[0]); err != nil || incident != nil {
		t.Fatalf("stale conflict record: %+v %v", incident, err)
	}
}
