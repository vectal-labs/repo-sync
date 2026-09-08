package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	path    string
	env     []string
	timeout time.Duration                    // zero keeps the normal two-minute Git timeout
	warn    func(format string, args ...any) // receives stderr of successful commands; nil drops it
}

func backgroundRunner() execCommandRunner {
	home, _ := os.UserHomeDir()
	env := []string{"HOME=" + home, "PATH=" + servicePATH, "TMPDIR=" + os.TempDir(), "LC_ALL=C"}
	for _, key := range []string{"USER", "LOGNAME"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	// Use launchd's agent socket in both preflight and the daemon. A shell may
	// point at an agent which will not exist after logout or reboot.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if socket, err := exec.CommandContext(ctx, "/bin/launchctl", "getenv", "SSH_AUTH_SOCK").Output(); err == nil && strings.TrimSpace(string(socket)) != "" {
		env = append(env, "SSH_AUTH_SOCK="+strings.TrimSpace(string(socket)))
	}
	return execCommandRunner{path: servicePATH, env: env}
}

func (r execCommandRunner) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if r.path != "" && !strings.ContainsRune(name, '/') {
		// LookPath otherwise searches the caller's shell PATH, not cmd.Env.
		found := false
		for _, dir := range filepath.SplitList(r.path) {
			candidate := filepath.Join(dir, name)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
				name, found = candidate, true
				break
			}
		}
		if !found {
			name = "/nonexistent-repo-sync-tool/" + name
		}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	if r.env != nil {
		cmd.Env = append([]string(nil), r.env...)
	}
	if r.path != "" {
		cmd.Env = append(cmd.Environ(), "PATH="+r.path)
	}
	if filepath.Base(name) == "brew" && len(args) > 0 && args[0] == "uninstall" {
		cmd.Env = append(cmd.Environ(), "HOMEBREW_NO_AUTOREMOVE=1")
	}
	return cmd
}

type interactiveCommandRunner interface {
	runInteractive(ctx context.Context, dir string, in io.Reader, out io.Writer, name string, args ...string) error
}

var (
	urlCredentials = regexp.MustCompile(`(https?://)[^\s/@]+(?::[^\s/@]*)?@`)
	querySecret    = regexp.MustCompile(`(?i)(token|access_token|password)=[^&\s]+`)
)

func (r execCommandRunner) run(ctx context.Context, dir, stdin, name string, args ...string) (string, error) {
	timeout := r.timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := r.command(commandCtx, name, args...)
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
		return stdout.String(), fmt.Errorf("%s: %w: %s", commandLine(name, args), err, diagnostics)
	}
	if warning := strings.TrimSpace(stderr.String()); warning != "" && r.warn != nil {
		r.warn("%s: %s", commandLine(name, args), redactCredentials(warning))
	}
	return stdout.String(), nil
}

func (r execCommandRunner) runInteractive(ctx context.Context, dir string, in io.Reader, out io.Writer, name string, args ...string) error {
	cmd := r.command(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w", commandLine(name, args), err)
	}
	return nil
}

// commandLine renders a command for diagnostics. Arguments may carry a
// remote URL with a token or password (a push URL, a ls-remote target), so
// they are redacted here; the command itself still runs with the real values.
func commandLine(name string, args []string) string {
	return redactCredentials(name + " " + strings.Join(args, " "))
}

func redactCredentials(value string) string {
	value = urlCredentials.ReplaceAllString(value, `${1}***@`)
	return querySecret.ReplaceAllString(value, `${1}=***`)
}

func runGit(ctx context.Context, runner commandRunner, dir string, args ...string) (string, error) {
	return runner.run(ctx, dir, "", "git", args...)
}
