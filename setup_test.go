package main

import (
	"context"
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
	for _, name := range []string{"notes", "blog"} {
		initRepo(t, filepath.Join(home, "code", name), true)
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
