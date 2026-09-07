package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in test registers only a unique temporary LaunchAgent. Run it on a
// logged-in Mac with REPO_SYNC_LAUNCHD_TEST=1; it never uses the real service label.
func TestLaunchdInstallSyncRestartAndUninstall(t *testing.T) {
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
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "repo-sync")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, "", "init", "--bare", "--initial-branch=main", remote)
	repo := filepath.Join(home, "code", "notes")
	gitRun(t, "", "clone", remote, repo)
	configureGitUser(t, repo)
	writeAndCommit(t, repo, "note.md", "initial\n", "initial")
	gitRun(t, repo, "push", "-u", "origin", "main")
	// Custom config inside a repository must never put runtime heartbeats there.
	configPath := filepath.Join(repo, "sync-settings.json")
	cfg := newDefaultConfig()
	cfg.IdleDebounce.Duration = 300 * time.Millisecond
	cfg.FetchInterval.Duration = 500 * time.Millisecond
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}}
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	service := defaultService()
	service.label = fmt.Sprintf("com.vectal-labs.repo-sync-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		if err := service.stop(context.Background()); err != nil {
			t.Errorf("clean up temporary service: %v", err)
		}
	})
	opts := setupOptions{configPath: configPath, binary: binary, in: strings.NewReader("\n"), runner: execCommandRunner{}, service: service}
	var out strings.Builder
	opts.out = &out
	if err := runSetup(context.Background(), opts); err != nil {
		t.Fatalf("install: %v\n%s", err, out.String())
	}
	if err := runStatus(context.Background(), configPath, service, &out); err != nil {
		t.Fatalf("status: %v\n%s", err, out.String())
	}
	write(t, repo, "after-install.md", "synced by launchd\n")
	deadline := time.Now().Add(20 * time.Second)
	for {
		data, err := (execCommandRunner{}).run(context.Background(), "", "", "git", "--git-dir", remote, "show", "main:after-install.md")
		if err == nil && data == "synced by launchd\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("launchd did not sync edit: %v\n%s", err, mustRead(t, filepath.Join(home, "Library", "Logs", "repo-sync", "stdout.log")))
		}
		time.Sleep(100 * time.Millisecond)
	}
	tree := gitOutput(t, repo, "--git-dir", remote, "ls-tree", "-r", "--name-only", "main")
	if strings.Contains(tree, "status-") || strings.Contains(tree, ".status.json") {
		t.Fatalf("runtime status was synced: %s", tree)
	}
	before, err := service.inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	opts.in = strings.NewReader("\n")
	if err := runSetup(context.Background(), opts); err != nil {
		t.Fatalf("repeat setup: %v\n%s", err, out.String())
	}
	after, err := service.inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.pid == before.pid || after.pid == 0 {
		t.Fatal("repeat setup did not restart service")
	}
	actual, err := loadConfig(configPath)
	if err != nil || len(actual.Repositories) != 1 || actual.IdleDebounce.Duration != cfg.IdleDebounce.Duration {
		t.Fatalf("repeat setup changed settings: %+v %v", actual, err)
	}
	broken := opts
	broken.binary = filepath.Join(home, "missing-repo-sync")
	broken.in = strings.NewReader("\n")
	if err := runSetup(context.Background(), broken); err == nil || !strings.Contains(err.Error(), "previous setup restored") {
		t.Fatalf("failed startup did not restore previous setup: %v", err)
	}
	if err := service.waitReady(context.Background(), configPath, 15*time.Second); err != nil {
		t.Fatalf("previous service did not recover: %v", err)
	}
	if installed, err := installedConfigPath(service.plistPath(home)); err != nil || installed != configPath {
		t.Fatalf("previous plist not restored: %s %v", installed, err)
	}
	if err := runUninstall(context.Background(), uninstallOptions{binaryPaths: []string{}, configPath: configPath, binary: binary, yes: true, in: strings.NewReader(""), out: &out, service: service}); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out.String())
	}
	final, err := service.inspect(context.Background())
	if err != nil || final.loaded {
		t.Fatalf("service remains: %+v %v", final, err)
	}
	for _, path := range []string{binary, configPath, statusPath(configPath), service.plistPath(home), filepath.Join(home, "Library", "Logs", "repo-sync")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("uninstall left %s: %v", path, err)
		}
	}
	if got := string(mustRead(t, filepath.Join(repo, "after-install.md"))); got != "synced by launchd\n" {
		t.Fatal("repository data was changed")
	}
}
