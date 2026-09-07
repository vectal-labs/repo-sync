package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// commandRunner runs a program and returns what it wrote to stdout.
//
// Output contract: the returned string is stdout only, so callers can parse it
// as data (`git status -z`, `rev-parse`, ...). Stderr never mixes into it, not
// even when the command exits 0 with a warning. On failure the error carries
// the redacted stderr and stdout so classifiers (offline, push rejected,
// authentication) keep working. Stderr from a successful command goes to
// warn, when set, and is otherwise dropped.
type commandRunner interface {
	run(ctx context.Context, dir, stdin, name string, args ...string) (string, error)
}

type execCommandRunner struct {
	warn func(format string, args ...any) // receives stderr of successful commands; nil drops it
}

type interactiveCommandRunner interface {
	runInteractive(ctx context.Context, dir string, in io.Reader, out io.Writer, name string, args ...string) error
}

var (
	urlCredentials = regexp.MustCompile(`(https?://)[^\s/@]+(?::[^\s/@]*)?@`)
	querySecret    = regexp.MustCompile(`(?i)(token|access_token|password)=[^&\s]+`)
)

func (r execCommandRunner) run(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, name, args...)
	cmd.Dir = dir
	// repo-sync is unattended. Never let Git wait for a terminal prompt, and
	// ignore stale global TLS pins so Git can negotiate its secure default.
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSL_VERSION=")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if commandCtx.Err() != nil {
		return stdout.String(), fmt.Errorf("%s timed out: %w", name, commandCtx.Err())
	}
	if err != nil {
		diagnostics := redactCredentials(strings.TrimSpace(stderr.String() + "\n" + stdout.String()))
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, diagnostics)
	}
	if warning := strings.TrimSpace(stderr.String()); warning != "" && r.warn != nil {
		r.warn("%s %s: %s", name, strings.Join(args, " "), redactCredentials(warning))
	}
	return stdout.String(), nil
}

func (execCommandRunner) runInteractive(ctx context.Context, dir string, in io.Reader, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func redactCredentials(value string) string {
	value = urlCredentials.ReplaceAllString(value, `${1}***@`)
	return querySecret.ReplaceAllString(value, `${1}=***`)
}

func runGit(ctx context.Context, runner commandRunner, dir string, args ...string) (string, error) {
	return runner.run(ctx, dir, "", "git", args...)
}
