package main

import (
	"context"
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
