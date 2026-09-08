package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

func runRemove(ctx context.Context, configPath, path string, service *launchService, out io.Writer) error {
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return err
	}
	// Preserve config symlinks. Atomic writes must replace their target.
	configPath = canonicalInstallPath(absolute)
	unlock, err := acquireUpdateLock()
	if err != nil {
		return err
	}
	defer unlock()
	before, err := snapshotFile(configPath)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("read existing config: %w", err)
	}
	index, err := removalIndex(cfg, path)
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	state, err := service.inspect(ctx)
	if err != nil {
		return err
	}
	plistPath := service.plistPath(home)
	installedConfig := configPath
	if state.loaded {
		installedConfig, err = installedConfigPath(plistPath)
		if err != nil {
			return fmt.Errorf("read installed service: %w", err)
		}
		if installedConfig == "" {
			installedConfig = defaultConfigPath()
		}
		if canonicalInstallPath(installedConfig) != configPath {
			return fmt.Errorf("service uses %s; no changes made. Use --config with that path", installedConfig)
		}
		// Finish any in-flight Git operation before changing the registration.
		if err := service.stop(ctx); err != nil {
			return err
		}
	}
	removed := cfg.Repositories[index]
	cfg.Repositories = append(cfg.Repositories[:index], cfg.Repositories[index+1:]...)
	// A manual editor or older CLI can change the config during shutdown.
	// Keep those edits instead of overwriting them with the earlier snapshot.
	after, err := snapshotFile(configPath)
	if err == nil && (before.exists != after.exists || before.mode != after.mode || !bytes.Equal(before.data, after.data)) {
		err = fmt.Errorf("config changed during removal; no settings were overwritten. Retry removal")
	}
	if err != nil {
		if state.loaded {
			err = errors.Join(err, service.start(ctx, plistPath))
		}
		return err
	}
	if err := writeConfig(configPath, cfg); err != nil {
		if state.loaded {
			err = errors.Join(err, service.start(ctx, plistPath))
		}
		return fmt.Errorf("removal was not saved: %w", err)
	}
	if state.loaded {
		if err := service.start(ctx, plistPath); err != nil {
			return fmt.Errorf("removal saved, but the service could not restart: %w. Restart the saved LaunchAgent at %s", err, plistPath)
		}
		if err := service.waitReady(ctx, installedConfig, 15*time.Second); err != nil {
			return fmt.Errorf("removal saved, but service readiness could not be verified: %w", err)
		}
		fmt.Fprintf(out, "Stopped syncing %s. Files and Git history were kept.\n", removed.Path)
		fmt.Fprintln(out, "Service restarted with the remaining repositories.")
	} else {
		fmt.Fprintf(out, "Removed %s from the config. Files and Git history were kept.\n", removed.Path)
		fmt.Fprintln(out, "The background service is not running. Restart any manually started `repo-sync run` process to apply the change.")
	}
	return nil
}

func removalIndex(cfg config, path string) (int, error) {
	if path == "" {
		path = "."
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return -1, err
	}
	match := func(candidate string) (int, error) {
		index := -1
		for i, repo := range cfg.Repositories {
			if canonicalInstallPath(repo.Path) == candidate {
				if index >= 0 {
					return -1, fmt.Errorf("multiple registrations refer to %s; resolve duplicate paths in the config before removal", candidate)
				}
				index = i
			}
		}
		return index, nil
	}
	// Match the registered path first, even when its folder no longer exists.
	if index, err := match(canonicalInstallPath(absolute)); index >= 0 || err != nil {
		return index, err
	}
	if root, err := repoRoot(absolute); err == nil {
		if index, err := match(root); index >= 0 || err != nil {
			return index, err
		}
	}
	return -1, fmt.Errorf("%s is not a synced repository in this config", absolute)
}
