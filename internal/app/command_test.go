package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCommandRunnerUsesUnattendedGitEnvironment(t *testing.T) {
	t.Setenv("GIT_SSL_VERSION", "tlsv1")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	_, err := (execCommandRunner{}).run(context.Background(), "", "", "/bin/sh", "-c",
		`test -z "$GIT_SSL_VERSION" && test "$GIT_TERMINAL_PROMPT" = 0`)
	if err != nil {
		t.Fatalf("runner did not override unsafe Git environment: %v", err)
	}
}

func TestBackgroundRunnerDoesNotBorrowShellCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "shell-only")
	t.Setenv("GITHUB_TOKEN", "shell-only")
	t.Setenv("XDG_CONFIG_HOME", "/shell-only")
	t.Setenv("GIT_AUTHOR_NAME", "shell-only")
	runner := backgroundRunner()
	_, err := runner.run(context.Background(), "", "", "/bin/sh", "-c", `test -z "$GH_TOKEN" && test -z "$GITHUB_TOKEN" && test -z "$XDG_CONFIG_HOME" && test -z "$GIT_AUTHOR_NAME" && test "$PATH" = '/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin'`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestHomebrewUninstallPreservesSharedDependencies(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "brew")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ntest \"$HOMEBREW_NO_AUTOREMOVE\" = 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "0")
	if _, err := (execCommandRunner{}).run(context.Background(), "", "", binary, "uninstall", "--cask", "repo-sync"); err != nil {
		t.Fatal(err)
	}
}
