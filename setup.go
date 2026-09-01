package main

import (
	"context"
	"fmt"
	"html"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const launchAgentLabel = "com.vectal-labs.repo-sync"

type setupOptions struct {
	configPath string
	binary     string // installed repo-sync binary the LaunchAgent should run
	noLaunch   bool
	in         io.Reader
	out        io.Writer
	runner     commandRunner
}

func runSetup(ctx context.Context, opts setupOptions) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	runner := opts.runner
	if runner == nil {
		runner = execCommandRunner{}
	}
	cfg, err := loadOrDefaultConfig(opts.configPath)
	if err != nil {
		return fmt.Errorf("read existing config: %w", err)
	}
	fmt.Fprintf(opts.out, "Scanning %s for repositories...\n", home)
	groups, singles, err := discoverRepos(ctx, runner, home)
	if err != nil {
		return fmt.Errorf("discover repositories: %w", err)
	}
	selected, err := selectRepos(opts.in, opts.out, groups, cfg)
	if err != nil {
		return err
	}
	for _, repo := range selected {
		if _, err := addRepository(&cfg, repo.Path); err != nil {
			return err
		}
	}
	if err := verifyRepositories(ctx, runner, cfg.Repositories, opts.in, opts.out); err != nil {
		return err
	}
	if err := writeConfig(opts.configPath, cfg); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if len(singles) > 0 {
		fmt.Fprintf(opts.out, "\n%d repositories sit alone in their folder and were not listed. Add one any time with `repo-sync add /path/to/repo`.\n", len(singles))
	}

	plistPath := filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
	logDir := filepath.Join(home, "Library", "Logs", "repo-sync")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	plist := launchAgentPlist(opts.binary, opts.configPath, logDir)
	if err := writeFileAtomic(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("write LaunchAgent: %w", err)
	}
	if !opts.noLaunch {
		if err := loadLaunchAgent(plistPath); err != nil {
			return err
		}
	}
	fmt.Fprintf(opts.out, "\nSyncing %d repositories.\nConfig: %s\nLaunchAgent: %s\nLogs: %s\n",
		len(cfg.Repositories), opts.configPath, plistPath, logDir)
	return nil
}

type repoAccessCheck struct {
	remoteURL string
	err       error
}

// verifyRepositories proves that every configured repository can fetch in the
// same non-interactive environment used by the daemon. GitHub HTTPS credentials
// are repaired through GitHub CLI when possible, then checked again.
func verifyRepositories(ctx context.Context, runner commandRunner, repos []repoConfig, in io.Reader, out io.Writer) error {
	if len(repos) == 0 {
		return nil
	}
	fmt.Fprintf(out, "\nChecking background Git access for %d repositories...\n", len(repos))
	checks := checkRepositories(ctx, runner, repos)

	needsGitHubAuth := false
	for _, check := range checks {
		if check.err != nil && isGitHubHTTPS(check.remoteURL) && isAuthenticationFailure(check.err) {
			needsGitHubAuth = true
			break
		}
	}
	if needsGitHubAuth {
		if err := configureGitHubCredentials(ctx, runner, in, out); err != nil {
			return err
		}
		checks = checkRepositories(ctx, runner, repos)
	}

	var failures []string
	for i, check := range checks {
		if check.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", repos[i].Name, check.err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("background Git access failed; config and service were not changed:\n- %s", strings.Join(failures, "\n- "))
	}
	fmt.Fprintln(out, "Background Git access verified.")
	return nil
}

func checkRepositories(ctx context.Context, runner commandRunner, repos []repoConfig) []repoAccessCheck {
	checks := make([]repoAccessCheck, len(repos))
	var wg sync.WaitGroup
	for i := range repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo := repos[i]
			remoteURL, err := runGit(ctx, runner, repo.Path, "remote", "get-url", repo.Remote)
			checks[i].remoteURL = strings.TrimSpace(remoteURL)
			if err != nil {
				checks[i].err = err
				return
			}
			_, checks[i].err = runGit(ctx, runner, repo.Path, "fetch", "--dry-run", repo.Remote)
		}()
	}
	wg.Wait()
	return checks
}

func isGitHubHTTPS(remote string) bool {
	rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(remote)), "https://")
	if !ok {
		return false
	}
	authority, _, _ := strings.Cut(rest, "/")
	if at := strings.LastIndex(authority, "@"); at >= 0 {
		authority = authority[at+1:]
	}
	host, _, _ := strings.Cut(authority, ":")
	return host == "github.com"
}

func isAuthenticationFailure(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"authentication failed", "could not read username", "could not read password",
		"terminal prompts disabled", "invalid username or token", "returned error: 401",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func configureGitHubCredentials(ctx context.Context, runner commandRunner, in io.Reader, out io.Writer) error {
	fmt.Fprintln(out, "GitHub credentials are not available to background Git. Configuring them with GitHub CLI...")
	if _, err := runner.run(ctx, "", "", "gh", "auth", "status", "--hostname", "github.com"); err != nil {
		fmt.Fprintln(out, "GitHub login is required. Follow the browser prompt.")
		var loginErr error
		if interactive, ok := runner.(interactiveCommandRunner); ok {
			loginErr = interactive.runInteractive(ctx, "", in, out, "gh", "auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web")
		} else {
			_, loginErr = runner.run(ctx, "", "", "gh", "auth", "login", "--hostname", "github.com", "--git-protocol", "https", "--web")
		}
		if loginErr != nil {
			return fmt.Errorf("GitHub login failed; install GitHub CLI or run `gh auth login --git-protocol https`, then run setup again: %w", loginErr)
		}
	}
	if _, err := runner.run(ctx, "", "", "gh", "auth", "setup-git", "--hostname", "github.com"); err != nil {
		return fmt.Errorf("configure GitHub credentials for Git: %w", err)
	}
	return nil
}

// executablePath returns the binary launchd should run. Setup refuses to point
// launchd at a temporary build so the service survives reboots.
func executablePath() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(path, os.TempDir()) || strings.Contains(path, "go-build") {
		return "", fmt.Errorf("setup must run from an installed binary (brew install or go install), not `go run`")
	}
	return path, nil
}

func launchdDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func loadLaunchAgent(plistPath string) error {
	_ = exec.Command("launchctl", "bootout", launchdDomain(), plistPath).Run()
	if output, err := exec.Command("launchctl", "bootstrap", launchdDomain(), plistPath).CombinedOutput(); err != nil {
		return fmt.Errorf("load LaunchAgent: %w: %s", err, output)
	}
	return nil
}

// restartDaemon asks launchd to restart the service so config changes apply.
// It is best effort: the caller prints a hint when the service is not loaded.
func restartDaemon() error {
	output, err := exec.Command("launchctl", "kickstart", "-k", launchdDomain()+"/"+launchAgentLabel).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func launchAgentPlist(binary, configPath, logDir string) string {
	escape := html.EscapeString
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + launchAgentLabel + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + escape(binary) + `</string>
    <string>run</string>
    <string>--config</string>
    <string>` + escape(configPath) + `</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>StandardOutPath</key>
  <string>` + escape(filepath.Join(logDir, "stdout.log")) + `</string>
  <key>StandardErrorPath</key>
  <string>` + escape(filepath.Join(logDir, "stderr.log")) + `</string>
</dict>
</plist>
`
}
