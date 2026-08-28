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
)

const launchAgentLabel = "com.vectal-labs.repo-sync"

type setupOptions struct {
	configPath string
	binary     string // installed repo-sync binary the LaunchAgent should run
	noLaunch   bool
	in         io.Reader
	out        io.Writer
}

func runSetup(ctx context.Context, opts setupOptions) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	runner := execCommandRunner{}
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
