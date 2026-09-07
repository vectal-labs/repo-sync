package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
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

func TestCommandRunnerReturnsOnlyStdoutAndForwardsWarnings(t *testing.T) {
	var warnings []string
	runner := execCommandRunner{warn: func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}}
	// The warning arrives via stdin so the secret is not part of the command line.
	output, err := runner.run(context.Background(), "", "warning: https://user:secret@example.com noisy\n", "/bin/sh", "-c",
		`echo data; cat >&2`)
	if err != nil {
		t.Fatal(err)
	}
	if output != "data\n" {
		t.Fatalf("stdout must not contain stderr, got %q", output)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "warning: https://***@example.com noisy") {
		t.Fatalf("stderr of a successful command must reach warn, redacted: %q", warnings)
	}
	if strings.Contains(warnings[0], "secret") {
		t.Fatalf("warning leaked credentials: %q", warnings[0])
	}
}

func TestCommandRunnerDropsWarningsWithoutSink(t *testing.T) {
	output, err := execCommandRunner{}.run(context.Background(), "", "", "/bin/sh", "-c", `echo data; echo warn >&2`)
	if err != nil || output != "data\n" {
		t.Fatalf("output = %q, err = %v", output, err)
	}
}

func TestCommandRunnerFailureKeepsRedactedDiagnostics(t *testing.T) {
	// Secrets are piped through stdin so they never appear on the command line.
	tests := []struct {
		name   string
		stdin  string
		script string
		want   []string
	}{
		{"stderr only", "fatal: https://user:token123@github.com/x: token=abc\n",
			`cat >&2; exit 1`,
			[]string{"fatal: https://***@github.com/x", "token=***"}},
		{"mixed output", "error: password=hunter2 rejected\n",
			`echo 'partial stdout'; cat >&2; exit 3`,
			[]string{"partial stdout", "error: password=*** rejected"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := execCommandRunner{}.run(context.Background(), "", tt.stdin, "/bin/sh", "-c", tt.script)
			if err == nil {
				t.Fatal("expected failure")
			}
			if strings.Contains(output, "fatal:") || strings.Contains(output, "error:") {
				t.Fatalf("stderr leaked into stdout result: %q", output)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q lacks %q", err, want)
				}
			}
			for _, secret := range []string{"token123", "token=abc", "hunter2"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaked %q: %v", secret, err)
				}
			}
		})
	}
}

// A Git warning on stderr (here from a broken core.fsmonitor hook) must never
// be parsed as a changed file. This is the real failure that inspired the
// stdout-only contract: the daemon invented paths from warning text.
func TestGitStatusWarningDoesNotInventFiles(t *testing.T) {
	_, local := makeGitFixture(t)
	hook := filepath.Join(local, ".git", "fsmonitor-warns")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf 'warning: custom monitor startup failed\\n' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, local, "config", "core.fsmonitor", hook)
	if err := os.WriteFile(filepath.Join(local, "real change.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "README.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var warnings []string
	runner := execCommandRunner{warn: func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}}
	syncer := gitSyncer{runner: runner}
	repo := repoConfig{Name: "notes", Path: local, Remote: "origin"}
	changed, blocked, err := syncer.changes(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range changed {
		paths = append(paths, entry.path)
	}
	sort.Strings(paths)
	if want := []string{"README.md", "real change.txt"}; !reflect.DeepEqual(paths, want) || len(blocked) != 0 {
		t.Fatalf("changes = %v (blocked %v), want %v", paths, blocked, want)
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "custom monitor startup failed") {
		t.Fatalf("warning was lost instead of forwarded: %q", warnings)
	}

	// The whole cycle still commits exactly the real files.
	report, err := syncer.sync(context.Background(), repo, true)
	if err != nil || report.Committed != 2 || !report.Pushed {
		t.Fatalf("report = %+v, err = %v", report, err)
	}
	message := gitOutput(t, local, "log", "-1", "--pretty=%B")
	if !strings.Contains(message, "- README.md\n- real change.txt\n") || strings.Contains(message, "warning") {
		t.Fatalf("commit message lists wrong files:\n%s", message)
	}
}
