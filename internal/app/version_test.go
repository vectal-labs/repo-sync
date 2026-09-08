package app

import (
	"context"
	"errors"
	"runtime/debug"
	"slices"
	"testing"
)

func TestVersionReportsReleaseOrGoInstallVersion(t *testing.T) {
	for _, test := range []struct {
		name, injected, module, want string
	}{
		{"release", "1.2.3", "", "v1.2.3"},
		{"prefixed release", "v1.2.3", "", "v1.2.3"},
		{"release takes precedence", "1.2.3", "v1.2.2", "v1.2.3"},
		{"go install", "", "v1.2.3", "v1.2.3"},
		{"working copy", "", "(devel)", "dev"},
		{"unversioned", "", "", "dev"},
		{"development build", "snapshot", "(devel)", "dev"},
		{"development overrides module", "snapshot", "v1.2.3", "dev"},
		{"pseudo version", "", "v0.0.0-20260908000000-abcdef123456", "dev"},
		{"prerelease", "1.2.3-rc.1", "", "dev"},
	} {
		t.Run(test.name, func(t *testing.T) {
			info := &debug.BuildInfo{Main: debug.Module{Version: test.module}}
			if got := resolveAppVersion(test.injected, info); got != test.want {
				t.Fatalf("version = %q; want %q", got, test.want)
			}
		})
	}
	if got := resolveAppVersion("", nil); got != "dev" {
		t.Fatalf("missing build information = %q", got)
	}
}

func TestBinaryVersionRequiresVerifiedReleaseOutput(t *testing.T) {
	for _, test := range []struct {
		name, output, want string
		commandErr         error
	}{
		{name: "release", output: "repo-sync v1.2.3\n", want: "v1.2.3"},
		{name: "unprefixed release", output: "repo-sync 1.2.3\n", want: "v1.2.3"},
		{name: "unknown", output: "repo-sync unknown\n"},
		{name: "development", output: "repo-sync dev\n"},
		{name: "missing identity", output: "v1.2.3\n"},
		{name: "other program", output: "other v1.2.3\n"},
		{name: "extra output", output: "repo-sync v1.2.3\nwarning"},
		{name: "multiline identity", output: "repo-sync\nv1.2.3\n"},
		{name: "prerelease", output: "repo-sync v1.2.3-rc.1\n"},
		{name: "failed command", output: "repo-sync v1.2.3\n", commandErr: errors.New("could not start")},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := preflightRunnerFunc(func(_ context.Context, dir, stdin, binary string, args ...string) (string, error) {
				if dir != "" || stdin != "" || binary != "/verified/repo-sync" || !slices.Equal(args, []string{"version"}) {
					t.Fatalf("unexpected version command: %q %q %q %v", dir, stdin, binary, args)
				}
				return test.output, test.commandErr
			})
			got, err := binaryVersion(context.Background(), runner, "/verified/repo-sync")
			if got != test.want || (err != nil) != (test.want == "") {
				t.Fatalf("binary version = %q, %v; want %q", got, err, test.want)
			}
			if test.commandErr != nil && !errors.Is(err, test.commandErr) {
				t.Fatalf("command error was lost: %v", err)
			}
		})
	}
}
