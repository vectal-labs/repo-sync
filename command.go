package main

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

type commandRunner interface {
	run(ctx context.Context, dir, stdin, name string, args ...string) (string, error)
}

type execCommandRunner struct{}

type interactiveCommandRunner interface {
	runInteractive(ctx context.Context, dir string, in io.Reader, out io.Writer, name string, args ...string) error
}

var (
	urlCredentials = regexp.MustCompile(`(https?://)[^\s/@]+(?::[^\s/@]*)?@`)
	querySecret    = regexp.MustCompile(`(?i)(token|access_token|password)=[^&\s]+`)
)

func (execCommandRunner) run(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
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
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := cmd.Run()
	if commandCtx.Err() != nil {
		return output.String(), fmt.Errorf("%s timed out: %w", name, commandCtx.Err())
	}
	if err != nil {
		cleanOutput := redactCredentials(strings.TrimSpace(output.String()))
		return output.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, cleanOutput)
	}
	return output.String(), nil
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
