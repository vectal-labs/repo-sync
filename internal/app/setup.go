package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const launchAgentLabel = "com.vectal-labs.repo-sync"

type setupOptions struct {
	configPath string
	binary     string // installed repo-sync binary the LaunchAgent should run
	noLaunch   bool
	in         io.Reader
	out        io.Writer
	runner     commandRunner
	service    *launchService
}

func runSetup(ctx context.Context, opts setupOptions) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(opts.configPath) {
		opts.configPath, err = filepath.Abs(opts.configPath)
		if err != nil {
			return err
		}
	}
	runner := opts.runner
	if runner == nil {
		runner = backgroundRunner()
	}
	service := opts.service
	if service == nil {
		service = defaultService()
	}
	fmt.Fprintln(opts.out, "1/4 Checking tools")
	if err := checkGitTools(ctx, runner); err != nil {
		return err
	}
	cfg, err := loadOrDefaultConfig(opts.configPath)
	if err != nil {
		return fmt.Errorf("read existing config: %w", err)
	}
	fmt.Fprintln(opts.out, "\n2/4 Choose repositories")
	fmt.Fprintf(opts.out, "After %s without edits, repo-sync commits, pulls, and pushes on each repo's default branch.\n", cfg.IdleDebounce)
	fmt.Fprintln(opts.out, "This includes staged changes. Secret filenames are blocked; file contents are not scanned.")
	fmt.Fprintf(opts.out, "Scanning %s...\n", home)
	groups, _, err := discoverRepos(ctx, runner, home)
	if err != nil {
		return fmt.Errorf("discover repositories: %w", err)
	}
	input := bufio.NewReader(opts.in)
	selected, err := selectRepos(input, opts.out, groups, cfg)
	if err != nil {
		return err
	}
	for _, repo := range selected {
		if _, err := addRepository(&cfg, repo.Path); err != nil {
			return err
		}
	}
	if len(cfg.Repositories) == 0 {
		fmt.Fprintln(opts.out, "\nNo repositories selected. Setup made no changes. Run `repo-sync setup` when you are ready.")
		return nil
	}
	fmt.Fprintln(opts.out, "\n3/4 Verify Git access")
	if err := verifyRepositories(ctx, runner, cfg.Repositories, input, opts.out); err != nil {
		return err
	}
	plistPath := service.plistPath(home)
	logDir := filepath.Join(home, "Library", "Logs", "repo-sync")
	if opts.noLaunch {
		fmt.Fprintln(opts.out, "\n4/4 Save service files")
	} else {
		fmt.Fprintln(opts.out, "\n4/4 Start the background service")
	}
	// Preserve the previous installation if writing or loading the new one fails.
	previousConfig, err := snapshotFile(opts.configPath)
	if err != nil {
		return err
	}
	previousPlist, err := snapshotFile(plistPath)
	if err != nil {
		return err
	}
	wasLoaded := false
	if !opts.noLaunch {
		prior, err := service.inspect(ctx)
		if err != nil {
			return err
		}
		wasLoaded = prior.loaded
	}
	rollback := func(cause error) error {
		var restoreErrors []error
		if !opts.noLaunch {
			if err := service.stop(ctx); err != nil {
				return fmt.Errorf("%w; cannot restore files while the new service may still be running: %v", cause, err)
			}
		}
		restoreErrors = append(restoreErrors, previousConfig.restore(opts.configPath), previousPlist.restore(plistPath))
		if wasLoaded {
			restoreErrors = append(restoreErrors, service.start(ctx, plistPath))
		}
		if restoreErr := errors.Join(restoreErrors...); restoreErr != nil {
			return fmt.Errorf("%w; restoring previous setup also failed: %v", cause, restoreErr)
		}
		return fmt.Errorf("%w; previous setup restored", cause)
	}
	if !opts.noLaunch {
		if err := service.stop(ctx); err != nil {
			return err
		}
	}
	if err := writeConfig(opts.configPath, cfg); err != nil {
		return rollback(err)
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return rollback(err)
	}
	plist := strings.ReplaceAll(launchAgentPlist(opts.binary, opts.configPath, logDir), launchAgentLabel, service.label)
	if err := writeFileAtomic(plistPath, []byte(plist), 0o644); err != nil {
		return rollback(err)
	}
	if opts.noLaunch {
		fmt.Fprintln(opts.out, "Files saved. The service was not started (--no-launch).")
	} else {
		if err := service.start(ctx, plistPath); err != nil {
			return rollback(err)
		}
		if err := service.waitReady(ctx, opts.configPath, 15*time.Second); err != nil {
			return rollback(err)
		}
		fmt.Fprintf(opts.out, "Service is running with %d repositories. Starts automatically at login.\n", len(cfg.Repositories))
	}
	fmt.Fprintf(opts.out, "Config: %s\nLogs: %s\nCheck progress: %s\nRemove: %s\n", opts.configPath, logDir, configCommand("status", opts.configPath), configCommand("uninstall", opts.configPath))
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

func launchAgentPlist(binary, configPath, logDir string) string {
	escape := html.EscapeString
	home, _ := os.UserHomeDir()
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
  <key>ExitTimeOut</key>
  <integer>40</integer>
  <key>ProcessType</key>
  <string>Background</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>` + escape(home) + `</string>
    <key>PATH</key>
    <string>` + servicePATH + `</string>
  </dict>
  <key>StandardOutPath</key>
  <string>` + escape(filepath.Join(logDir, "stdout.log")) + `</string>
  <key>StandardErrorPath</key>
  <string>` + escape(filepath.Join(logDir, "stderr.log")) + `</string>
</dict>
</plist>
`
}
