package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	opts := setupOptions{configPath: configPath, binary: "/usr/local/bin/repo-sync", noLaunch: true}

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
	if name == "git" && len(args) >= 3 && args[0] == "remote" && args[1] == "get-url" {
		return "https://github.com/acme/notes.git\n", nil
	}
	if name == "git" && len(args) >= 1 && args[0] == "fetch" {
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
