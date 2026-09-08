package app

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
)

func updaterService(service *launchService) *launchService {
	if service == nil {
		service = defaultService()
	}
	return &launchService{runner: service.runner, domain: service.domain, label: service.label + ".updates"}
}

// Homebrew removes versioned directories on upgrade. launchd must retain a path
// that resolves to the next installation after the current updater exits.
func stableUpdateBinary(binary string) (string, error) {
	absolute, err := absoluteInstallPath(binary)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err != nil {
		resolved = absolute
	}
	for _, directory := range []string{"Caskroom", "Cellar"} {
		if prefix, _, found := strings.Cut(resolved, "/"+directory+"/repo-sync/"); found {
			return filepath.Join(prefix, "bin", "repo-sync"), nil
		}
	}
	return absolute, nil
}

// installUpdater is also called from Homebrew's postflight while the updater
// itself may own the upgrade. An existing registration must never be booted out.
func installUpdater(ctx context.Context, configPath, binary string, noLaunch bool, service *launchService) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath, err = absoluteInstallPath(configPath)
	if err != nil {
		return err
	}
	binary, err = stableUpdateBinary(binary)
	if err != nil {
		return err
	}
	updater := updaterService(service)
	path := updater.plistPath(home)
	previous, err := snapshotFile(path)
	if err != nil {
		return err
	}
	logDir := filepath.Join(home, "Library", "Logs", "repo-sync")
	plist := updaterPlist(updater.label, binary, configPath, logDir)
	if !noLaunch {
		state, err := updater.inspect(ctx)
		if err != nil {
			return err
		}
		if state.loaded {
			if previous.exists && string(previous.data) == plist {
				return nil
			}
			return fmt.Errorf("updater is already registered with different settings; run `repo-sync setup` to replace them")
		}
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(path, []byte(plist), 0o644); err != nil {
		return err
	}
	if noLaunch {
		return nil
	}
	if err := updater.start(ctx, path); err != nil {
		return errors.Join(err, previous.restore(path))
	}
	state, err := updater.inspect(ctx)
	if err == nil && state.loaded {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("updater did not remain registered")
	}
	if stopErr := updater.stop(ctx); stopErr != nil {
		return errors.Join(err, stopErr)
	}
	return errors.Join(err, previous.restore(path))
}

func updaterPlist(label, binary, configPath, logDir string) string {
	escape := html.EscapeString
	home, _ := os.UserHomeDir()
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>` + escape(label) + `</string>
  <key>ProgramArguments</key>
  <array>
    <string>` + escape(binary) + `</string>
    <string>update</string>
    <string>--scheduled</string>
    <string>--config</string>
    <string>` + escape(configPath) + `</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>StartCalendarInterval</key>
  <array>
    <dict><key>Minute</key><integer>0</integer></dict>
    <dict><key>Minute</key><integer>15</integer></dict>
    <dict><key>Minute</key><integer>30</integer></dict>
    <dict><key>Minute</key><integer>45</integer></dict>
  </array>
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
  <string>` + escape(filepath.Join(logDir, "updates-stdout.log")) + `</string>
  <key>StandardErrorPath</key>
  <string>` + escape(filepath.Join(logDir, "updates-stderr.log")) + `</string>
</dict>
</plist>
`
}
