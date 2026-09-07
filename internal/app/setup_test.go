package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestLaunchAgentPlist(t *testing.T) {
	plist := launchAgentPlist("/tmp/a&b/repo-sync", "/tmp/config.json", "/tmp/log")
	for _, expected := range []string{
		"<string>" + launchAgentLabel + "</string>",
		"<key>RunAtLoad</key>\n  <true/>",
		"<key>KeepAlive</key>\n  <true/>",
		"/opt/homebrew/bin",
		"/tmp/a&amp;b/repo-sync",
		"<string>run</string>",
	} {
		if !strings.Contains(plist, expected) {
			t.Errorf("plist missing %q", expected)
		}
	}
}

func TestExecutablePathRejectsTemporaryBuilds(t *testing.T) {
	// The test binary itself lives in a go-build temp directory.
	if _, err := executablePath(); err == nil {
		t.Fatal("setup must refuse to install a temporary build")
	}
}

func TestRunSetupIsRepeatableAndKeepsExistingConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	remoteRoot := t.TempDir()
	for _, name := range []string{"notes", "blog"} {
		remote := filepath.Join(remoteRoot, name+".git")
		path := filepath.Join(home, "code", name)
		gitRun(t, "", "init", "--bare", "--initial-branch=main", remote)
		gitRun(t, "", "clone", "-q", remote, path)
		configureGitUser(t, path)
		writeAndCommit(t, path, "README.md", name+"\n", "initial")
		gitRun(t, path, "push", "-q", "-u", "origin", "main")
	}
	configPath := filepath.Join(home, "Library", "Application Support", "repo-sync", "config.json")
	opts := setupOptions{configPath: configPath, binary: "/usr/local/bin/repo-sync", noLaunch: true, runner: execCommandRunner{}}

	var out strings.Builder
	opts.in, opts.out = strings.NewReader("2\n"), &out
	if err := runSetup(context.Background(), opts); err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 1 || cfg.Repositories[0].Name != "notes" {
		t.Fatalf("repositories = %+v, want only notes", cfg.Repositories)
	}
	plist, err := os.ReadFile(filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist"))
	if err != nil || !strings.Contains(string(plist), "/usr/local/bin/repo-sync") {
		t.Fatalf("LaunchAgent not written correctly: %v", err)
	}

	out.Reset()
	opts.in, opts.out = strings.NewReader("\n"), &out
	if err := runSetup(context.Background(), opts); err != nil {
		t.Fatalf("second setup: %v", err)
	}
	cfg, err = loadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repositories) != 1 {
		t.Fatalf("re-running setup changed repositories: %+v", cfg.Repositories)
	}
	if !strings.Contains(out.String(), "notes (already synced)") || !strings.Contains(out.String(), "1. blog") {
		t.Fatalf("second run must list the remaining repo and mark the synced one:\n%s", out.String())
	}
}

func TestSetupRecordsCurrentAndPreviousCustomConfigs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, repo := makeGitFixture(t)
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}}
	oldConfig := filepath.Join(home, "old", "sync.json")
	newConfig := filepath.Join(home, "new", "sync.json")
	for _, path := range []string{oldConfig, newConfig} {
		if err := writeConfig(path, cfg); err != nil {
			t.Fatal(err)
		}
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	if err := writeFileAtomic(plistPath, []byte(launchAgentPlist("/old/repo-sync", oldConfig, filepath.Join(home, "logs"))), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := runSetup(context.Background(), setupOptions{configPath: newConfig, binary: "/new/repo-sync", noLaunch: true, in: strings.NewReader("\n"), out: &strings.Builder{}, runner: execCommandRunner{}}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(defaultConfigPath()), "install.json"))
	if err != nil {
		t.Fatalf("setup must save ownership for complete uninstall: %v", err)
	}
	var record struct {
		Version     int      `json:"version"`
		ConfigPaths []string `json:"config_paths"`
		BinaryPaths []string `json:"binary_paths"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Version != 1 || !slices.Equal(record.ConfigPaths, uniquePaths([]string{oldConfig, newConfig})) || !slices.Equal(record.BinaryPaths, []string{"/new/repo-sync", "/old/repo-sync"}) {
		t.Fatalf("setup must retain old config and binary paths without duplicates: %+v", record)
	}
}

func TestSetupRejectsMalformedRecordBeforeChangingFilesOrService(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := defaultConfigPath()
	if err := writeConfig(configPath, newDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	plistPath := defaultService().plistPath(home)
	oldPlist := launchAgentPlist("/old/repo-sync", configPath, filepath.Join(home, "logs"))
	if err := writeFileAtomic(plistPath, []byte(oldPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(installRecordPath(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	beforeConfig := readPreflightFile(t, configPath)
	runner := &lifecycleRunner{loaded: true}
	toolCalls := 0
	gitRunner := preflightRunnerFunc(func(_ context.Context, _, _, _ string, _ ...string) (string, error) {
		toolCalls++
		return "", errors.New("unexpected tool call")
	})
	err := runSetup(context.Background(), setupOptions{configPath: configPath, binary: "/new/repo-sync", in: strings.NewReader("\n"), out: &strings.Builder{}, runner: gitRunner, service: fakeService(runner)})
	if err == nil || !strings.Contains(err.Error(), "installation record") || toolCalls != 0 || !runner.loaded || runner.starts != 0 {
		t.Fatalf("invalid record must fail before touching tools or service: %v; calls=%d; runner=%+v", err, toolCalls, runner)
	}
	if string(readPreflightFile(t, configPath)) != string(beforeConfig) || string(readPreflightFile(t, plistPath)) != oldPlist || string(readPreflightFile(t, installRecordPath())) != "{broken" {
		t.Fatal("setup changed files despite malformed ownership record")
	}
}

func TestSetupCommitsRecordAfterServiceReadiness(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	_, repo := makeGitFixture(t)
	cfg := newDefaultConfig()
	cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}}
	configPath := defaultConfigPath()
	if err := writeConfig(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	runner := &lifecycleRunner{}
	service := setupOwnershipReadyService(t, runner, configPath, func() {
		if _, err := os.Stat(installRecordPath()); !os.IsNotExist(err) {
			t.Fatalf("record committed before service readiness: %v", err)
		}
	})
	if err := runSetup(context.Background(), setupOptions{configPath: configPath, binary: "/new/repo-sync", in: strings.NewReader("\n"), out: &strings.Builder{}, runner: execCommandRunner{}, service: service}); err != nil {
		t.Fatal(err)
	}
	record, err := loadInstallRecord()
	if err != nil || !slices.Equal(record.ConfigPaths, []string{configPath}) || !slices.Equal(record.BinaryPaths, []string{"/new/repo-sync"}) {
		t.Fatalf("ready setup did not commit installation record: %+v, %v", record, err)
	}
}

func TestSetupRestoresRecordFilesAndServiceWhenRecordingFails(t *testing.T) {
	for _, existingRecord := range []bool{false, true} {
		t.Run(map[bool]string{false: "first record", true: "existing record"}[existingRecord], func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			_, repo := makeGitFixture(t)
			cfg := newDefaultConfig()
			cfg.Repositories = []repoConfig{{Name: "notes", Path: repo, Remote: "origin"}}
			configPath := defaultConfigPath()
			oldConfig, _ := json.Marshal(cfg)
			if err := writeFileAtomic(configPath, oldConfig, 0o640); err != nil {
				t.Fatal(err)
			}
			plistPath := defaultService().plistPath(home)
			oldPlist := launchAgentPlist("/old/repo-sync", configPath, filepath.Join(home, "logs"))
			if err := writeFileAtomic(plistPath, []byte(oldPlist), 0o644); err != nil {
				t.Fatal(err)
			}
			var oldRecord []byte
			if existingRecord {
				if err := recordInstallation(configPath, "/old/repo-sync"); err != nil {
					t.Fatal(err)
				}
				oldRecord = readPreflightFile(t, installRecordPath())
			}
			sentinel := filepath.Join(home, "unrelated.txt")
			if err := os.WriteFile(sentinel, []byte("keep me"), 0o600); err != nil {
				t.Fatal(err)
			}
			runner := &lifecycleRunner{loaded: true}
			service := setupOwnershipReadyService(t, runner, configPath, func() {
				if existingRecord {
					if err := os.Remove(installRecordPath()); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(sentinel, installRecordPath()); err != nil {
					t.Fatal(err)
				}
			})
			err := runSetup(context.Background(), setupOptions{configPath: configPath, binary: "/new/repo-sync", in: strings.NewReader("\n"), out: &strings.Builder{}, runner: execCommandRunner{}, service: service})
			if err == nil || !strings.Contains(err.Error(), "write installation record") || !strings.Contains(err.Error(), "previous setup restored") {
				t.Fatalf("recording failure must roll setup back: %v", err)
			}
			if !runner.loaded || runner.starts != 2 || string(readPreflightFile(t, configPath)) != string(oldConfig) || string(readPreflightFile(t, plistPath)) != oldPlist || string(readPreflightFile(t, sentinel)) != "keep me" {
				t.Fatalf("previous files/service were not restored: %+v", runner)
			}
			if existingRecord {
				if string(readPreflightFile(t, installRecordPath())) != string(oldRecord) {
					t.Fatal("previous record was not restored")
				}
			} else if _, err := os.Lstat(installRecordPath()); !os.IsNotExist(err) {
				t.Fatalf("failed first setup left an installation record: %v", err)
			}
			if info, err := os.Stat(configPath); err != nil || info.Mode().Perm() != 0o640 {
				t.Fatalf("config permissions were not restored: %v, %v", info, err)
			}
		})
	}
}

func setupOwnershipReadyService(t *testing.T, runner *lifecycleRunner, configPath string, onFirstStart func()) *launchService {
	t.Helper()
	return &launchService{domain: "gui/test", label: launchAgentLabel, runner: preflightRunnerFunc(func(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
		output, err := runner.run(ctx, dir, stdin, name, args...)
		if err == nil && args[0] == "bootstrap" && runner.starts == 1 {
			runner.pid = 99999999
			onFirstStart()
			cfg, err := loadConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(serviceStatus{PID: runner.pid, ConfigHash: configHash(cfg), UpdatedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			if err := writeFileAtomic(statusPath(configPath), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return output, err
	})}
}

func TestRunSetupRejectsRepoThatDaemonCannotFetch(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			_, _ = connection.Read(make([]byte, 4096))
			_, _ = connection.Write([]byte("HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Basic realm=\"test\"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			_ = connection.Close()
		}
	}()

	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"broken", "other"} {
		path := filepath.Join(home, "code", name)
		initRepo(t, path, false)
		gitRun(t, path, "remote", "add", "origin", "http://"+listener.Addr().String()+"/"+name+".git")
	}

	configPath := filepath.Join(home, "Library", "Application Support", "repo-sync", "config.json")
	var out strings.Builder
	err = runSetup(context.Background(), setupOptions{
		configPath: configPath,
		binary:     "/usr/local/bin/repo-sync",
		noLaunch:   true,
		runner:     execCommandRunner{},
		in:         strings.NewReader("1\n"),
		out:        &out,
	})
	if err == nil {
		t.Fatalf("setup accepted a repository that the daemon cannot fetch:\n%s", out.String())
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("config was written despite failed access check: %v", statErr)
	}
	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	if _, statErr := os.Stat(plistPath); !os.IsNotExist(statErr) {
		t.Fatalf("LaunchAgent was written despite failed access check: %v", statErr)
	}
}

type authRepairRunner struct {
	fetches          int
	calls            []string
	statusFails      bool
	loggedIn         bool
	interactiveLogin bool
}

func (r *authRepairRunner) run(_ context.Context, _, _, name string, args ...string) (string, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if name == "git" {
		switch {
		case slices.Equal(args, []string{"config", "--get", "user.name"}):
			return "repo-sync test\n", nil
		case slices.Equal(args, []string{"config", "--get", "user.email"}):
			return "repo-sync@example.invalid\n", nil
		case slices.Equal(args, []string{"var", "GIT_AUTHOR_IDENT"}), slices.Equal(args, []string{"var", "GIT_COMMITTER_IDENT"}):
			return "repo-sync test <repo-sync@example.invalid> 0 +0000\n", nil
		case slices.Equal(args, []string{"symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"}):
			return "origin/main\n", nil
		case slices.Equal(args, []string{"push", "--dry-run", "--no-verify", "--porcelain", "origin", "refs/heads/main:refs/heads/main"}):
			return "Everything up-to-date\n", nil
		}
	}
	if name == "git" && len(args) >= 3 && args[0] == "remote" && args[1] == "get-url" {
		return "https://github.com/acme/notes.git\n", nil
	}
	if name == "git" && slices.Contains(args, "fetch") {
		r.fetches++
		if r.fetches == 1 {
			return "", errors.New("fatal: could not read Username for 'https://github.com': terminal prompts disabled")
		}
		return "", nil
	}
	if name == "gh" && len(args) >= 2 && args[0] == "auth" && args[1] == "status" && r.statusFails && !r.loggedIn {
		return "", errors.New("not logged in")
	}
	if name == "gh" && len(args) >= 2 && args[0] == "auth" && args[1] == "login" {
		r.loggedIn = true
		return "", nil
	}
	if name == "gh" {
		return "", nil
	}
	return "", errors.New("unexpected command")
}

func (r *authRepairRunner) runInteractive(_ context.Context, _ string, _ io.Reader, _ io.Writer, name string, args ...string) error {
	r.calls = append(r.calls, "interactive "+name+" "+strings.Join(args, " "))
	r.interactiveLogin = true
	r.loggedIn = true
	return nil
}

func TestVerifyRepositoriesLogsIntoGitHubWhenNeeded(t *testing.T) {
	runner := &authRepairRunner{statusFails: true}
	if err := verifyRepositories(context.Background(), runner, []repoConfig{{
		Name: "notes", Path: t.TempDir(), Remote: "origin",
	}}, strings.NewReader(""), &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(runner.calls, "\n")
	if !runner.interactiveLogin || !strings.Contains(calls, "interactive gh auth login --hostname github.com --git-protocol https --web") {
		t.Fatalf("GitHub login was not started:\n%s", calls)
	}
}

func TestVerifyRepositoriesConfiguresGitHubCredentialsAndRetries(t *testing.T) {
	runner := &authRepairRunner{}
	var out strings.Builder
	err := verifyRepositories(context.Background(), runner, []repoConfig{{
		Name: "notes", Path: t.TempDir(), Remote: "origin",
	}}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatal(err)
	}
	if runner.fetches != 2 {
		t.Fatalf("fetches = %d, want an initial check and one retry", runner.fetches)
	}
	calls := strings.Join(runner.calls, "\n")
	if !strings.Contains(calls, "gh auth status --hostname github.com") ||
		!strings.Contains(calls, "gh auth setup-git --hostname github.com") {
		t.Fatalf("GitHub credentials were not configured:\n%s", calls)
	}
	if !strings.Contains(out.String(), "Background Git access verified") {
		t.Fatalf("success was not reported:\n%s", out.String())
	}
}

func TestIsGitHubHTTPS(t *testing.T) {
	for _, remote := range []string{
		"https://github.com/acme/notes.git",
		"https://user@github.com/acme/notes.git",
		"HTTPS://GITHUB.COM/acme/notes.git",
	} {
		if !isGitHubHTTPS(remote) {
			t.Errorf("%q should be recognized as GitHub HTTPS", remote)
		}
	}
	for _, remote := range []string{
		"http://github.com/acme/notes.git",
		"https://github.com.evil/acme/notes.git",
		"git@github.com:acme/notes.git",
	} {
		if isGitHubHTTPS(remote) {
			t.Errorf("%q must not be recognized as GitHub HTTPS", remote)
		}
	}
}
