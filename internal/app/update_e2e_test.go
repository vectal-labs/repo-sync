package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestUpdateCLIReleaseVersionAndPersistentControls(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and executes a release binary")
	}
	home := isolateUpdateE2EHome(t)
	binary := buildUpdateE2ERelease(t, "v1.0.0")
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "network is unavailable", http.StatusServiceUnavailable)
	}))
	defer proxy.Close()
	toolDir := filepath.Join(home, "unavailable-tools")
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toolMarker := filepath.Join(home, "unexpected-tool")
	// A setting change must remain local even when Homebrew and launchctl fail.
	for _, tool := range []string{"brew", "gh", "curl", "launchctl"} {
		script := "#!/bin/sh\nprintf invoked > " + preflightShellQuote(toolMarker) + "\nexit 99\n"
		if err := os.WriteFile(filepath.Join(toolDir, tool), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Env = append(os.Environ(), "PATH="+toolDir, "HTTP_PROXY="+proxy.URL, "HTTPS_PROXY="+proxy.URL, "NO_PROXY=")
		data, err := cmd.CombinedOutput()
		return string(data), err
	}
	if output, err := run("version"); err != nil || output != "repo-sync v1.0.0\n" {
		t.Fatalf("release version = %q, %v", output, err)
	}
	if output, err := run("updates", "off"); err != nil || !strings.Contains(output, "notifications stay enabled") {
		t.Fatalf("disable automatic updates = %q, %v", output, err)
	}
	settings, err := loadUpdateSettings()
	if err != nil || settings.Automatic {
		t.Fatalf("off was not persisted across processes: %+v, %v", settings, err)
	}
	if output, err := run("updates", "invalid"); err == nil || !strings.Contains(output, "usage:") {
		t.Fatalf("invalid toggle = %q, %v", output, err)
	}
	settings, err = loadUpdateSettings()
	if err != nil || settings.Automatic {
		t.Fatalf("invalid toggle changed settings: %+v, %v", settings, err)
	}
	if output, err := run("updates", "on"); err != nil || !strings.Contains(output, "Automatic updates enabled") {
		t.Fatalf("enable automatic updates = %q, %v", output, err)
	}
	settings, err = loadUpdateSettings()
	if err != nil || !settings.Automatic {
		t.Fatalf("on was not persisted across processes: %+v, %v", settings, err)
	}
	if info, err := os.Stat(updateSettingsPath()); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("update settings permissions: %v, %v", info, err)
	}
	if _, err := os.Stat(toolMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("controls invoked an external tool: %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("controls made %d network requests", got)
	}
	t.Run("release command supervisor", func(t *testing.T) {
		lock, err := lockUpdateFileHandle(filepath.Join(home, "supervisor.lock"), syscall.LOCK_EX)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		runner := backgroundRunner()
		runner.timeout = 5 * time.Second
		output, err := runSupervisedUpdateCommandWith(context.Background(), runner, []*os.File{lock},
			[]string{binary, "update-command-supervisor"}, "/bin/echo", "supervisor ready")
		if err != nil || output != "supervisor ready\n" {
			t.Fatalf("release supervisor = %q, %v", output, err)
		}
	})
}

// Only a unique temporary LaunchAgent is registered. Homebrew is replaced at
// the command boundary; Git, release binaries, process shutdown, status, and
// launchd restarts are real. No installed package or real user service is used.
func TestLaunchdUpdateWaitsForGitRecoversAndRunsNewRelease(t *testing.T) {
	if os.Getenv("REPO_SYNC_LAUNCHD_TEST") != "1" {
		t.Skip("set REPO_SYNC_LAUNCHD_TEST=1 on a logged-in Mac")
	}
	home := isolateUpdateE2EHome(t)
	oldBinary := buildUpdateE2ERelease(t, "v1.0.0")
	newBinary := buildUpdateE2ERelease(t, "v1.1.0")
	binary := filepath.Join(home, "bin", "repo-sync")
	if err := writeFileAtomic(binary, mustRead(t, oldBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	remote, repo := makeGitFixture(t)
	configPath := filepath.Join(home, "settings with spaces.json")
	cfg := newDefaultConfig()
	cfg.IdleDebounce.Duration = 200 * time.Millisecond
	cfg.FetchInterval.Duration = 500 * time.Millisecond
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}}
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	originalConfig := string(mustRead(t, configPath))
	service := defaultService()
	service.label = fmt.Sprintf("com.vectal-labs.repo-sync-update-e2e-%d-%d", os.Getpid(), time.Now().UnixNano())
	logDir := filepath.Join(home, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatal(err)
	}
	plist := strings.Replace(launchAgentPlist(binary, configPath, logDir), launchAgentLabel, service.label, 1)
	if err := writeFileAtomic(service.plistPath(home), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	entered := filepath.Join(home, "git-hook-entered")
	release := filepath.Join(home, "git-hook-release")
	hook := "#!/bin/sh\nset -eu\nif [ ! -f " + preflightShellQuote(release) + " ]; then\n  : > " + preflightShellQuote(entered) + "\n  while [ ! -f " + preflightShellQuote(release) + " ]; do /bin/sleep 0.05; done\nfi\n"
	if err := os.WriteFile(filepath.Join(repo, ".git", "hooks", "pre-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Release a held hook even when an earlier assertion fails.
		_ = os.WriteFile(release, nil, 0o600)
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := service.stop(ctx); err != nil {
			t.Errorf("remove temporary update test service: %v", err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := service.start(ctx, service.plistPath(home)); err != nil {
		t.Fatal(err)
	}
	if err := service.waitReady(ctx, configPath, 15*time.Second); err != nil {
		t.Fatalf("old release not ready: %v\n%s", err, mustRead(t, filepath.Join(logDir, "stderr.log")))
	}
	before, err := service.readStatus(ctx, configPath)
	if err != nil || before.Version != "v1.0.0" {
		t.Fatalf("old release status = %+v, %v", before, err)
	}
	write(t, repo, "held-edit.md", "finish this commit before upgrading\n")
	waitUpdateE2E(t, "real commit hook to start", func() bool {
		_, err := os.Stat(entered)
		return err == nil
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"tag_name":"v1.1.0","draft":false,"prerelease":false}`)
	}))
	defer server.Close()
	brew := &updateE2EBrewRunner{
		brew: filepath.Join(home, "never-executed-brew"), binary: binary, replacement: newBinary,
		service: service, plist: service.plistPath(home), hookRelease: release,
	}
	var output strings.Builder
	var notices []string
	u := &updater{
		runner: brew, client: server.Client(), releaseURL: server.URL, service: service,
		binary: binary, brew: brew.brew, version: "v1.0.0", out: &output, now: time.Now,
		gateWait: 300 * time.Millisecond,
		notify: func(_ context.Context, _ commandRunner, message string) error {
			notices = append(notices, message)
			return nil
		},
	}
	if err := u.run(ctx, configPath, false); err == nil || !strings.Contains(err.Error(), "Git work is still active") {
		t.Fatalf("update did not defer while Git was committing: %v\n%s", err, output.String())
	}
	if brew.upgrades != 0 {
		t.Fatalf("Homebrew upgrade started %d times while a commit was active", brew.upgrades)
	}
	if current, err := service.inspect(ctx); err != nil || current.pid != before.PID {
		t.Fatalf("deferred update stopped the active daemon: %+v, %v", current, err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitUpdateE2ERemote(t, remote, "held-edit.md", "finish this commit before upgrading\n")

	// A package-manager failure after shutdown must bring the previous real
	// service back. This tests the recovery boundary that a brew exit code alone
	// cannot establish.
	brew.failAfterStop = true
	u.gateWait = 3 * time.Second
	if err := u.run(ctx, configPath, false); err == nil || !strings.Contains(err.Error(), "simulated interrupted Homebrew upgrade") {
		t.Fatalf("interrupted upgrade result: %v\n%s", err, output.String())
	}
	recovered, err := service.readStatus(ctx, configPath)
	if err != nil || recovered.Version != "v1.0.0" || recovered.PID == before.PID {
		t.Fatalf("previous release did not recover: %+v, %v", recovered, err)
	}
	state, err := loadUpdateState(configPath)
	if err != nil || state.Result != "failed" || len(notices) != 1 {
		t.Fatalf("recovery failure was not visible: state=%+v notices=%v err=%v", state, notices, err)
	}
	write(t, repo, "after-recovery.md", "old release still syncs\n")
	waitUpdateE2ERemote(t, remote, "after-recovery.md", "old release still syncs\n")

	brew.failAfterStop = false
	if err := u.run(ctx, configPath, false); err != nil {
		t.Fatalf("retry upgrade: %v\n%s", err, output.String())
	}
	after, err := service.readStatus(ctx, configPath)
	if err != nil || after.Version != "v1.1.0" || after.PID == recovered.PID {
		t.Fatalf("new release did not run: %+v, %v", after, err)
	}
	state, err = loadUpdateState(configPath)
	if err != nil || state.Result != "updated" || state.SucceededAt.IsZero() || !state.FailureSince.IsZero() {
		t.Fatalf("successful retry was not recorded: %+v, %v", state, err)
	}
	if brew.upgrades != 2 {
		t.Fatalf("upgrade attempts = %d, want one failed and one successful", brew.upgrades)
	}
	if current := string(mustRead(t, configPath)); current != originalConfig {
		t.Fatal("upgrade changed repository settings")
	}
	write(t, repo, "after-upgrade.md", "new release syncs too\n")
	waitUpdateE2ERemote(t, remote, "after-upgrade.md", "new release syncs too\n")
}

func isolateUpdateE2EHome(t *testing.T) string {
	t.Helper()
	cache, err := exec.Command("go", "env", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOMODCACHE", strings.TrimSpace(string(cache)))
	t.Setenv("GOCACHE", "/private/tmp/repo-sync-auto-updates-go-cache")
	home := filepath.Join(t.TempDir(), "home with spaces")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	return home
}

func buildUpdateE2ERelease(t *testing.T, version string) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "repo-sync")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", "-X github.com/vectal-labs/repo-sync/internal/app.buildVersion="+version, "-o", binary, ".")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", version, err, output)
	}
	return binary
}

func waitUpdateE2E(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitUpdateE2ERemote(t *testing.T, remote, file, expected string) {
	t.Helper()
	waitUpdateE2E(t, "remote file "+file, func() bool {
		output, err := exec.Command("git", "--git-dir", remote, "show", "main:"+file).Output()
		return err == nil && string(output) == expected
	})
}

type updateE2EBrewRunner struct {
	brew, binary, replacement, plist, hookRelease string
	service                                       *launchService
	upgrades                                      int
	failAfterStop                                 bool
}

func (r *updateE2EBrewRunner) run(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	if name != r.brew {
		return (execCommandRunner{}).run(ctx, dir, stdin, name, args...)
	}
	switch strings.Join(args, " ") {
	case "update --quiet", "fetch --cask " + homebrewCask:
		return "", nil
	case "info --json=v2 --cask " + homebrewCask:
		data, _ := json.Marshal(map[string]any{"casks": []brewUpdateInfo{{Version: "1.1.0", Tap: "vectal-labs/tap", Installed: "1.0.0"}}})
		return string(data), nil
	case "upgrade --cask " + homebrewCask:
		r.upgrades++
		if _, err := os.Stat(r.hookRelease); err != nil {
			return "", fmt.Errorf("upgrade attempted while real Git hook was blocked: %w", err)
		}
		if err := r.service.stop(ctx); err != nil {
			return "", err
		}
		if r.failAfterStop {
			return "", errors.New("simulated interrupted Homebrew upgrade after daemon shutdown")
		}
		data, err := os.ReadFile(r.replacement)
		if err != nil {
			return "", err
		}
		if err := writeFileAtomic(r.binary, data, 0o755); err != nil {
			return "", err
		}
		return "", r.service.start(ctx, r.plist)
	default:
		return "", fmt.Errorf("unexpected Homebrew command in isolated E2E: %q", args)
	}
}
