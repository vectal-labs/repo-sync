# Installation checks

Run the automated checks before release:

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
ruby scripts/test-cask.rb
REPO_SYNC_HOMEBREW_TEST=1 ruby scripts/test-cask.rb
goreleaser check --config .github/.goreleaser.yaml
```

The real launchd lifecycle test uses a unique temporary service label and throwaway repositories. On a logged-in Mac, run:

```sh
REPO_SYNC_LAUNCHD_TEST=1 go test -race ./internal/app -run '^TestLaunchdInstallSyncRestartAndUninstall$' -count=1
REPO_SYNC_LAUNCHD_TEST=1 go test -race ./internal/app -run '^TestLaunchdUpdateWaitsForGitRecoversAndRunsNewRelease$' -count=1
```

These verify both LaunchAgents, service readiness, real syncing, repeated setup, failed-start rollback, and uninstall. The updater test holds a real Git hook, verifies deferral, recovers from a controlled upgrade failure, then starts a newer compiled release and verifies real pushes. Homebrew operations are intercepted; the normal repo-sync service and installed packages remain untouched.

Automated cleanup tests use disposable compiled binaries and injected process/service controls. They cover old configs, duplicate binaries, generated leftovers, preserved personal files, failed deletion, and retries. The foreground end-to-end test stops 2 real repo-sync binaries while preserving an unrelated process and staged Git work. Native process tests verify identity checks and graceful shutdown; processes whose executable macOS hides cannot be identified.

Use a disposable macOS account or VM for Homebrew and launchd checks. Changing `HOME` alone does not isolate the logged-in launchd domain. Use throwaway repositories and remotes.

## Fresh install

1. Install with the README one-liner. Confirm Homebrew installs `git` and `gh`.
2. Confirm setup explains automatic commits and selects nothing by default.
3. Select a repo in a folder containing only 1 repo. Add another by path. Confirm both appear in the saved config.
4. Confirm setup reports the running service and daily update policy. Run `repo-sync status`. Both the sync job and the separate updater job should be registered.
5. Edit a tracked file. Confirm the background service commits and pushes it after the idle interval. Restart the Mac and repeat.

## Setup failures

- Remove the test repo's Git identity. Setup must explain how to fix it before changing config or service files.
- Test an unreachable remote, read-only credentials, and missing `gh` when GitHub login is required. Setup must identify the failed check and preserve any existing installation.
- Test working SSH credentials without `gh`. Setup must not require GitHub login.
- Fail service startup. Setup must report the failure instead of claiming success.
- Run setup twice. Existing selected repos must remain. Exit or select no repos on a fresh install; no empty service should be installed.
- Run `setup --no-launch`. Both plists should be written, with a clear message that the service was not started.

## Upgrade and uninstall

1. Install an older release and configure a repo. Upgrade with Homebrew. Confirm config and logs survive and the service uses the new binary. Repeat with a custom config path.
2. Upgrade an installation with no service plist. Confirm no service or interactive setup starts.
3. Cancel `repo-sync uninstall`. Confirm nothing changes.
4. Run setup with different config and binary paths. Confirm `~/Library/Application Support/repo-sync/install.json` retains their paths. Start a matching foreground process, then run `repo-sync uninstall --yes`. Confirm it stops that process and the service, removes recorded configs and verified copies in recorded or standard locations, and removes the Homebrew receipt when applicable. Repositories, Git history, shared credentials, and unrelated processes must survive.
5. Repeat with stale `status-<64 lowercase hex digits>.json` cache files, numbered `stdout.log` / `stderr.log` rotations, and `.repo-sync-<digits>` temporary files in config, cache, or LaunchAgents folders. Confirm they are removed. Put `personal.log` in the app's log folder; it must appear under `Preserved`. Check the final `Removed`, `Preserved`, and `Failed` sections.
6. Make a recorded custom config's parent folder unwritable. Confirm uninstall returns an error, reports the failed path, and retains `install.json` for retry. Restore permissions and rerun. Confirm the old config is removed even after its plist is gone.
7. Repeat with a Go installation and `--config` pointing to custom settings. Test `--keep-binary` twice: binaries remain, and `install.json` retains only binary paths for later removal.
8. Test `brew uninstall --cask repo-sync`: the service stops and settings remain. The updater job and its schedule must be removed immediately. Unchanged managed skills are removed; customized skill folders remain. With `--zap`, standard config, logs, cache, and the remaining plist move to the trash.

The cask keeps settings cleanup under `zap` because Homebrew also runs uninstall hooks during upgrades. Its pre-uninstall hook preserves the main plist and waits up to 45 seconds for the old daemon to exit. A scoped artifact extension receives Homebrew’s actual upgrade/reinstall flags, preserving the updater and skills during those operations. True uninstall removes the updater schedule and calls the binary's skill-only cleanup. Direct Homebrew uninstall retains the update lock after preflight until its process exits, preventing a competing updater from restarting the daemon during package removal. Skill commands use their own lock. A timeout or cleanup error aborts removal or restart. Post-install reloads only an existing plist, preserves its config path, switches to the stable Homebrew binary link, and enrolls existing setups into updates without replacing an already registered updater. It also refreshes managed skills through the new binary, even without a service plist. See the [GoReleaser cask schema](https://goreleaser.com/customization/homebrew_casks/) and [Homebrew Cask Cookbook](https://docs.brew.sh/Cask-Cookbook#stanza-zap).

## Agent skill

Use temporary homes and agent directories. Keep service commands stubbed for these checks.

1. Run the built binary outside the source checkout. Install the skill, then check status and all referenced files. Confirm no download or checkout is needed.
2. Run setup with yes, no, and EOF at the skill offer. Confirm yes/no is remembered, EOF records nothing, and repeating setup keeps existing repo selections.
3. Test shared, Claude Code, and Hermes destinations, including `CLAUDE_CONFIG_DIR` and `HERMES_HOME`. Explicit overrides must be respected. Unrelated profiles stay untouched.
4. Install a different skill at a destination. Confirm it is preserved. Modify a managed skill or add a personal file, then refresh and uninstall. Confirm the whole customized folder survives.
5. Upgrade a managed, unchanged copy with a newer binary. Confirm all embedded references refresh. Repeat with no receipt; refresh must create no installation.
6. Run Homebrew post-install with and without a service plist while the update lock is held. It must invoke the new staged binary's skill refresh without prompting or taking that lock.
7. Test full CLI uninstall, direct Homebrew uninstall, and uninstall with `--zap`. Unchanged managed copies must disappear and customized folders must remain. Upgrade and reinstall hooks must preserve skills for refresh.
8. Make a managed destination unwritable. Confirm failure is reported and ownership remains available for retry. Restore access and retry.

The Ruby harness exercises the actual cask hooks with service and helper commands intercepted. Its optional Homebrew loader check verifies the real artifact forwarding. Neither test installs packages or changes a logged-in service.

## Automatic updates

1. Configure an older Homebrew release, then perform the one-time manual upgrade that includes the updater. Confirm `com.vectal-labs.repo-sync.updates` appears in launchd, with the stable Homebrew `bin/repo-sync` path. An unconfigured install must remain unconfigured.
2. Publish a newer stable release to the test tap. Run `repo-sync update`. Confirm the updater remains registered while Homebrew replaces the binary, the daemon restarts with the expected version, and settings/repositories survive.
3. Set `repo-sync updates off`, upgrade manually, and rerun setup. Confirm automatic installation remains off and update checks remain registered. Turn it on and confirm the next due check can install.
4. Simulate a failed download, unavailable tap release, concurrent Homebrew operation, and failed daemon restart. Confirm status shows the actionable failure, repeated errors produce notifications without repeated alerts, and a retry recovers.
5. Put the Mac to sleep across a scheduled check. The calendar schedule should coalesce missed checks at wake; the engine should honor its stored daily check or retry time.
6. Hold a sync in progress during upgrade. Confirm Git work finishes before daemon replacement. Run two update commands and attempt `repo-sync uninstall --yes` during updating; competing commands must refuse without stopping the active update.
7. After successful uninstall, confirm both jobs, plists, update settings/state, lock files, and updater logs are gone. Keep personal files in app directories and confirm they survive.

The optional Homebrew test loads the cask with Homebrew’s own Ruby loader and exercises real artifact forwarding with command execution stubbed. It never registers jobs or installs packages. See [Homebrew’s installer flags](https://github.com/Homebrew/brew/blob/master/Library/Homebrew/cask/installer.rb) and [flight block implementation](https://github.com/Homebrew/brew/blob/master/Library/Homebrew/cask/artifact/abstract_flight_block.rb).
