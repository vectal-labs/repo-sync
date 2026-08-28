package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type commandRunner interface {
	run(ctx context.Context, dir, stdin, name string, args ...string) (string, error)
}

type execCommandRunner struct{}

var (
	urlCredentials = regexp.MustCompile(`(https?://)[^\s/@]+(?::[^\s/@]*)?@`)
	querySecret    = regexp.MustCompile(`(?i)(token|access_token|password)=[^&\s]+`)
)

func (execCommandRunner) run(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, name, args...)
	cmd.Dir = dir
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

func redactCredentials(value string) string {
	value = urlCredentials.ReplaceAllString(value, `${1}***@`)
	return querySecret.ReplaceAllString(value, `${1}=***`)
}

func runGit(ctx context.Context, runner commandRunner, dir string, args ...string) (string, error) {
	return runner.run(ctx, dir, "", "git", args...)
}
