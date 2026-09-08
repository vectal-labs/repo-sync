package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const updateRetryInterval = 15 * time.Minute

type updater struct {
	runner                commandRunner
	client                *http.Client
	releaseURL            string
	service               *launchService
	binary, brew, version string
	out                   io.Writer
	now                   func() time.Time
	notify                func(context.Context, commandRunner, string) error
	gateWait              time.Duration
}

func runUpdate(ctx context.Context, configPath string, scheduled bool, out io.Writer) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	runner := backgroundRunner()
	runner.timeout = 15 * time.Minute
	runner.env = append(runner.env, "NONINTERACTIVE=1", "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_INSTALL_CLEANUP=1", "HOMEBREW_NO_AUTOREMOVE=1", "HOMEBREW_NO_INSTALLED_DEPENDENTS_CHECK=1")
	brewRunner := &updateBrewRunner{base: runner}
	u := &updater{runner: brewRunner, client: &http.Client{Timeout: 20 * time.Second}, releaseURL: latestReleaseURL,
		service: defaultService(), binary: binary, version: appVersion(), out: out, now: time.Now, notify: macOSNotify, gateWait: 30 * time.Second}
	installation, err := planBinaryRemoval(binary)
	if err != nil {
		unlock, lockErr := acquireUpdateLock()
		if lockErr != nil {
			return errors.Join(err, lockErr)
		}
		defer unlock()
		state, stateErr := loadUpdateState(configPath)
		if stateErr != nil {
			return errors.Join(err, stateErr)
		}
		return u.failure(ctx, configPath, &state, err, true)
	}
	if installation.kind == "--cask" {
		u.brew = installation.brew
		brewRunner.brew = installation.brew
		u.binary = filepath.Join(filepath.Dir(u.brew), "repo-sync")
	}
	return u.run(ctx, configPath, scheduled)
}

func (u *updater) run(ctx context.Context, configPath string, scheduled bool) error {
	lock, err := lockUpdateFileHandle(filepath.Join(updateCacheDir(), "update.lock"), syscall.LOCK_EX)
	if err != nil {
		if scheduled && errors.Is(err, errUpdateBusy) {
			return nil
		}
		return err
	}
	defer u.holdLock(lock)()
	home, _ := os.UserHomeDir()
	plistPath := u.service.plistPath(home)
	plist, err := snapshotFile(plistPath)
	if err != nil {
		return err
	}
	if scheduled && !plist.exists {
		return nil
	}
	if plist.exists {
		// A queued launch must never recreate an uninstalled or replaced setup.
		activeConfig, err := installedConfigPath(plistPath)
		if err != nil {
			return err
		}
		if activeConfig == "" {
			activeConfig = defaultConfigPath()
		}
		active, _ := filepath.Abs(activeConfig)
		requested, _ := filepath.Abs(configPath)
		if active != requested {
			return fmt.Errorf("the service uses another configuration; run `%s`", configCommand("update", activeConfig))
		}
		if _, err := loadConfig(configPath); err != nil {
			return err
		}
		activeBinary, err := installedBinaryPath(plistPath)
		if err != nil {
			return err
		}
		activeStable, err := stableUpdateBinary(activeBinary)
		if err != nil {
			return err
		}
		selectedStable, err := stableUpdateBinary(u.binary)
		if err != nil {
			return err
		}
		if canonicalInstallPath(activeStable) != canonicalInstallPath(selectedStable) {
			return fmt.Errorf("the service uses another installation; update %s and rerun setup", activeStable)
		}
	}
	state, err := loadUpdateState(configPath)
	if err != nil {
		return err
	}
	if scheduled && u.now().Before(state.NextCheck) {
		return nil
	}
	settings, err := loadUpdateSettings()
	if err != nil {
		return u.failure(ctx, configPath, &state, err, true)
	}
	state.NextCheck = u.now().Add(updateRetryInterval)
	if state.Result == "installing" && (!scheduled || (settings.Automatic && u.brew != "")) {
		// A previous updater died after Homebrew began changing the service.
		gate, err := u.waitForSync(ctx)
		if err != nil {
			return u.failure(ctx, configPath, &state, err, false)
		}
		recoveryErr := u.recoverService(ctx, configPath, plist)
		gate()
		if recoveryErr != nil {
			return u.failure(ctx, configPath, &state, fmt.Errorf("interrupted update: %w", recoveryErr), true)
		}
	}
	latest, err := fetchLatestRelease(ctx, u.client, u.releaseURL)
	if err != nil {
		return u.failure(ctx, configPath, &state, err, false)
	}
	state.LatestVersion, state.CheckedAt = latest, u.now()
	state.NextCheck = u.now().Add(24 * time.Hour)
	if !stableReleaseVersion(u.version) {
		return u.available(ctx, configPath, &state, "This is a development build. Install a released version to receive automatic upgrades.")
	}
	if compareReleaseVersions(latest, u.version) <= 0 {
		if plist.exists {
			status, statusErr := u.service.readStatus(ctx, configPath)
			if statusErr != nil || status.Version != u.version {
				if scheduled && (!settings.Automatic || u.brew == "") {
					return u.available(ctx, configPath, &state, "The installed version is current but the service needs restarting. Run `"+configCommand("update", configPath)+"`.")
				}
				return u.repair(ctx, configPath, &state, plist)
			}
		}
		return u.success(configPath, &state, "up-to-date", "The latest installed release is running.")
	}
	if u.brew == "" {
		return u.available(ctx, configPath, &state, "New release "+latest+" available. Update the installation at "+u.binary+", then run that binary's `setup` command. See https://github.com/vectal-labs/repo-sync#install.")
	}
	if scheduled && !settings.Automatic {
		return u.available(ctx, configPath, &state, "New release "+latest+" available. Run `"+configCommand("update", configPath)+"`.")
	}
	fmt.Fprintln(u.out, "Checking Homebrew for "+latest+"...")
	if _, err := u.runner.run(ctx, "", "", u.brew, "update", "--quiet"); err != nil {
		return u.failure(ctx, configPath, &state, err, false)
	}
	info, err := readBrewUpdateInfo(ctx, u.runner, u.brew)
	if err != nil {
		return u.failure(ctx, configPath, &state, err, false)
	}
	if info.Pinned || info.Disabled {
		return u.available(ctx, configPath, &state, "Homebrew has pinned or disabled repo-sync. Review Homebrew settings before updating.")
	}
	// Publishing a release and its cask is not atomic. Never install a version
	// different from the stable release we checked, including a newer tap race.
	if compareReleaseVersions(info.Version, latest) != 0 {
		return u.failure(ctx, configPath, &state, fmt.Errorf("the Homebrew tap has not caught up with the release; will retry"), false)
	}
	if info.Installed == "" {
		return u.failure(ctx, configPath, &state, fmt.Errorf("Homebrew does not record an installed repo-sync cask; repair the installation"), true)
	}
	if _, err := u.runner.run(ctx, "", "", u.brew, "fetch", "--cask", homebrewCask); err != nil {
		return u.failure(ctx, configPath, &state, err, false)
	}
	gate, err := u.waitForSync(ctx)
	if err != nil {
		return u.failure(ctx, configPath, &state, err, false)
	}
	defer gate()
	installed, err := binaryVersion(ctx, u.runner, u.binary)
	if err != nil {
		return u.failure(ctx, configPath, &state, err, true)
	}
	if compareReleaseVersions(installed, latest) >= 0 {
		if err := u.verify(ctx, configPath, installed, plist.exists); err != nil {
			return u.failure(ctx, configPath, &state, err, true)
		}
		return u.success(configPath, &state, "up-to-date", "An equal or newer release is already installed; no downgrade performed.")
	}
	// Re-read the setting at the last safe boundary: off takes effect even
	// while a scheduled check or download is already underway.
	settings, err = loadUpdateSettings()
	if err != nil {
		return u.failure(ctx, configPath, &state, err, true)
	}
	if scheduled && !settings.Automatic {
		return u.available(ctx, configPath, &state, "Automatic updates were disabled. Run `"+configCommand("update", configPath)+"` to install "+latest+".")
	}
	state.Result, state.Detail, state.AttemptedAt = "installing", "Installing "+latest+" through Homebrew.", u.now()
	state.NextCheck = u.now().Add(updateRetryInterval)
	if err := writeUpdateState(configPath, state); err != nil {
		return err
	}
	_, upgradeErr := u.runner.run(ctx, "", "", u.brew, "upgrade", "--cask", homebrewCask)
	if upgradeErr != nil {
		recoveryErr := u.recoverService(ctx, configPath, plist)
		return u.failure(ctx, configPath, &state, errors.Join(upgradeErr, recoveryErr), true)
	}
	if err := u.verify(ctx, configPath, latest, plist.exists); err != nil {
		recoveryErr := u.recoverService(ctx, configPath, plist)
		return u.failure(ctx, configPath, &state, errors.Join(err, recoveryErr), true)
	}
	state.SucceededAt, state.NextCheck = u.now(), u.now().Add(24*time.Hour)
	return u.success(configPath, &state, "updated", latest+" installed and verified.")
}

func (u *updater) waitForSync(ctx context.Context) (func(), error) {
	deadline := time.Now().Add(u.gateWait)
	for {
		lock, err := lockUpdateFileHandle(updateGatePath(), syscall.LOCK_EX)
		if err == nil {
			return u.holdLock(lock), nil
		}
		if !errors.Is(err, errUpdateBusy) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("Git work is still active; update postponed")
		}
		if err := waitContext(ctx, 100*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

func (u *updater) holdLock(file *os.File) func() {
	runner, supervised := u.runner.(*updateBrewRunner)
	if supervised {
		runner.locks = append(runner.locks, file)
	}
	return func() {
		if supervised {
			for i, held := range runner.locks {
				if held == file {
					runner.locks = append(runner.locks[:i], runner.locks[i+1:]...)
					break
				}
			}
		}
		_ = file.Close()
	}
}

func (u *updater) verify(ctx context.Context, configPath, expected string, configured bool) error {
	actual, err := binaryVersion(ctx, u.runner, u.binary)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("installed version is %s; expected %s", actual, expected)
	}
	if !configured {
		return nil
	}
	if err := u.service.waitReady(ctx, configPath, 15*time.Second); err != nil {
		return err
	}
	status, err := u.service.readStatus(ctx, configPath)
	if err != nil {
		return err
	}
	if status.Version != expected {
		return fmt.Errorf("running version is %s; expected %s", status.Version, expected)
	}
	return nil
}

func (u *updater) repair(ctx context.Context, configPath string, state *updateState, before fileSnapshot) error {
	gate, err := u.waitForSync(ctx)
	if err != nil {
		return u.failure(ctx, configPath, state, err, false)
	}
	defer gate()
	if err := u.service.stop(ctx); err != nil {
		return u.failure(ctx, configPath, state, err, true)
	}
	if err := u.refreshServiceBinary(ctx); err != nil {
		return u.failure(ctx, configPath, state, errors.Join(err, u.recoverService(ctx, configPath, before)), true)
	}
	if err := u.recoverService(ctx, configPath, before); err != nil {
		return u.failure(ctx, configPath, state, err, true)
	}
	if err := u.verify(ctx, configPath, u.version, true); err != nil {
		return u.failure(ctx, configPath, state, err, true)
	}
	return u.success(configPath, state, "restarted", u.version+" is running.")
}

// Homebrew owns package rollback. Recover the preserved service without
// rewriting its package receipt or copying binaries into the Caskroom.
func (u *updater) recoverService(ctx context.Context, configPath string, before fileSnapshot) error {
	// The original command may have timed out or been interrupted. Recovery
	// still gets a bounded chance to bring the configured service back.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if !before.exists {
		return nil
	}
	if _, err := u.service.readStatus(ctx, configPath); err == nil {
		return nil
	}
	home, _ := os.UserHomeDir()
	plist := u.service.plistPath(home)
	binary, err := installedBinaryPath(plist)
	if err != nil || !isRepoSyncBinary(binary) {
		if err := before.restore(plist); err != nil {
			return fmt.Errorf("restore service configuration: %w", err)
		}
		binary, err = installedBinaryPath(plist)
	}
	if err != nil || !isRepoSyncBinary(binary) {
		if !isRepoSyncBinary(u.binary) {
			return fmt.Errorf("no usable installed binary remains; reinstall with Homebrew, then run `%s`", configCommand("setup", configPath))
		}
		if err := u.refreshServiceBinary(ctx); err != nil {
			return err
		}
	}
	if err := u.service.stop(ctx); err != nil {
		return err
	}
	if err := u.service.start(ctx, plist); err != nil {
		return err
	}
	return u.service.waitReady(ctx, configPath, 15*time.Second)
}

func (u *updater) refreshServiceBinary(ctx context.Context) error {
	if !isRepoSyncBinary(u.binary) {
		return fmt.Errorf("cannot verify installed program %s", u.binary)
	}
	home, _ := os.UserHomeDir()
	plist := u.service.plistPath(home)
	args, err := installedArguments(plist)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("service has no program arguments")
	}
	args[0] = u.binary
	data, err := json.Marshal(args)
	if err != nil {
		return err
	}
	_, err = u.runner.run(ctx, "", "", "/usr/bin/plutil", "-replace", "ProgramArguments", "-json", string(data), plist)
	return err
}

func (u *updater) success(configPath string, state *updateState, result, detail string) error {
	state.Result, state.Detail = result, detail
	state.FailureSince, state.Notified = time.Time{}, ""
	fmt.Fprintln(u.out, detail)
	return writeUpdateState(configPath, *state)
}

func (u *updater) available(ctx context.Context, configPath string, state *updateState, detail string) error {
	state.Result, state.Detail, state.FailureSince = "available", detail, time.Time{}
	fmt.Fprintln(u.out, detail)
	if err := writeUpdateState(configPath, *state); err != nil {
		return err
	}
	u.note(ctx, state, "available:"+state.LatestVersion, detail)
	return writeUpdateState(configPath, *state)
}

func (u *updater) failure(ctx context.Context, configPath string, state *updateState, cause error, urgent bool) error {
	state.Result = "failed"
	state.Detail = redactCredentials(cause.Error())
	if len(state.Detail) > 2000 {
		state.Detail = state.Detail[:2000]
	}
	state.NextCheck = u.now().Add(updateRetryInterval)
	if state.FailureSince.IsZero() {
		state.FailureSince = u.now()
	}
	if err := writeUpdateState(configPath, *state); err != nil {
		return errors.Join(cause, err)
	}
	if urgent || u.now().Sub(state.FailureSince) >= 24*time.Hour {
		u.note(ctx, state, "failed", "repo-sync could not update. Run `"+configCommand("status", configPath)+"` for details, then `"+configCommand("update", configPath)+"` to retry.")
	}
	return errors.Join(cause, writeUpdateState(configPath, *state))
}

func (u *updater) note(ctx context.Context, state *updateState, key, message string) {
	if state.Notified == key {
		return
	}
	notifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := u.notify(notifyCtx, u.runner, strings.TrimSpace(message)); err == nil {
		state.Notified = key
	}
}
