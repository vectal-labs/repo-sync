package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEndToEndEmptyDaemonReportsReadiness(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	d := newDaemon(ctx, newDefaultConfig(), execCommandRunner{}, log.New(io.Discard, "", 0))
	d.statusFile = filepath.Join(t.TempDir(), "status.json")
	done := make(chan error, 1)
	go func() { done <- d.run() }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("empty daemon did not stop")
		}
	})
	started := time.Now()
	deadline := started.Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(d.statusFile)
		var status serviceStatus
		if err == nil && json.Unmarshal(data, &status) == nil && status.UpdatedAt.After(started.Add(time.Second)) {
			if status.PID != os.Getpid() || len(status.Repositories) != 0 || status.ConfigHash != configHash(d.cfg) {
				t.Fatalf("incorrect empty-daemon status: %+v", status)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("empty daemon did not publish readiness and a fresh heartbeat")
}

// This test uses only temporary clones and a unique LaunchAgent label.
func TestLaunchdRemoveKeepsOtherReposSyncing(t *testing.T) {
	if os.Getenv("REPO_SYNC_LAUNCHD_TEST") != "1" {
		t.Skip("set REPO_SYNC_LAUNCHD_TEST=1 on a logged-in Mac")
	}
	moduleCache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOMODCACHE", strings.TrimSpace(string(moduleCache)))
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	binary := filepath.Join(t.TempDir(), "repo-sync")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	alphaRemote, alpha := makeGitFixture(t)
	betaRemote, beta := makeGitFixture(t)
	cfg := newDefaultConfig()
	cfg.IdleDebounce.Duration = 300 * time.Millisecond
	cfg.FetchInterval.Duration = 500 * time.Millisecond
	cfg.Repositories = []repoConfig{{Name: "alpha", Path: alpha, Remote: "origin"}, {Name: "beta", Path: beta, Remote: "origin"}}
	configPath := filepath.Join(home, "custom.json")
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	service := defaultService()
	service.label = fmt.Sprintf("com.vectal-labs.repo-sync-remove-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		if err := updaterService(service).stop(context.Background()); err != nil {
			t.Error(err)
		}
		if err := service.stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	var out strings.Builder
	if err := runSetup(context.Background(), setupOptions{configPath: configPath, binary: binary, in: strings.NewReader("\n"), out: &out, runner: execCommandRunner{}, service: service}); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	write(t, alpha, "before-remove.md", "synced\n")
	waitRemote := func(remote, file, want string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			output, err := (execCommandRunner{}).run(context.Background(), "", "", "git", "--git-dir", remote, "show", "main:"+file)
			if err == nil && output == want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("%s did not sync", file)
	}
	waitRemote(alphaRemote, "before-remove.md", "synced\n")
	// Readiness must use the service's config spelling, even when remove is
	// invoked through a different symlink to the same config.
	alias := filepath.Join(home, "config-alias.json")
	if err := os.Symlink(configPath, alias); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runRemove(context.Background(), alias, alpha, service, &out); err != nil {
		t.Fatalf("remove first repo: %v\n%s", err, out.String())
	}
	status, err := service.readStatus(context.Background(), configPath)
	if err != nil || len(status.Repositories) != 1 || status.Repositories[0].Name != "beta" {
		t.Fatalf("remaining service status: %+v %v", status, err)
	}
	alphaHead := gitOutput(t, alpha, "rev-parse", "HEAD")
	alphaRemoteHead := gitOutput(t, alpha, "--git-dir", alphaRemote, "rev-parse", "main")
	write(t, alpha, "local-only.md", "staged\n")
	gitRun(t, alpha, "add", "local-only.md")
	write(t, alpha, "local-only.md", "unstaged revision\n")
	alphaIndex := gitOutput(t, alpha, "diff", "--cached", "--binary")
	write(t, beta, "after-remove.md", "still synced\n")
	waitRemote(betaRemote, "after-remove.md", "still synced\n")
	if err := runRemove(context.Background(), configPath, beta, service, &out); err != nil {
		t.Fatalf("remove last repo: %v\n%s", err, out.String())
	}
	status, err = service.readStatus(context.Background(), configPath)
	if err != nil || len(status.Repositories) != 0 {
		t.Fatalf("empty service status: %+v %v", status, err)
	}
	if got := gitOutput(t, alpha, "rev-parse", "HEAD"); got != alphaHead {
		t.Fatal("removed repo still committed changes")
	}
	if got := gitOutput(t, alpha, "--git-dir", alphaRemote, "rev-parse", "main"); got != alphaRemoteHead {
		t.Fatal("removed repo still pushed changes")
	}
	if got := gitOutput(t, alpha, "diff", "--cached", "--binary"); got != alphaIndex || string(mustRead(t, filepath.Join(alpha, "local-only.md"))) != "unstaged revision\n" {
		t.Fatal("removed repo's staged or unstaged work changed")
	}
	if string(mustRead(t, filepath.Join(beta, "after-remove.md"))) != "still synced\n" {
		t.Fatal("last repo's files were lost")
	}
}
