# Installation checks

Run the automated checks before release:

```sh
go build ./...
go vet ./...
go test -race -count=1 ./...
ruby scripts/test-cask.rb
goreleaser check --config .github/.goreleaser.yaml
```

The real launchd lifecycle test uses a unique temporary service label and throwaway repositories. On a logged-in Mac, run:

```sh
REPO_SYNC_LAUNCHD_TEST=1 go test -race ./internal/app -run '^TestLaunchdInstallSyncRestartAndUninstall$' -count=1
```

It verifies service readiness, real syncing, repeated setup, failed-start rollback, and uninstall. It does not touch the normal repo-sync service.

Automated cleanup tests use disposable compiled binaries and injected process/service controls. They cover old configs, duplicate binaries, generated leftovers, preserved personal files, failed deletion, and retries. The foreground end-to-end test stops 2 real repo-sync binaries while preserving an unrelated process and staged Git work. Native process tests verify identity checks and graceful shutdown; processes whose executable macOS hides cannot be identified.

Use a disposable macOS account or VM for Homebrew and launchd checks. Changing `HOME` alone does not isolate the logged-in launchd domain. Use throwaway repositories and remotes.

## Fresh install

1. Install with the README one-liner. Confirm Homebrew installs `git` and `gh`.
2. Confirm setup explains automatic commits and selects nothing by default.
3. Select a repo in a folder containing only 1 repo. Add another by path. Confirm both appear in the saved config.
4. Confirm setup reports the running service. Run `repo-sync status`.
5. Edit a tracked file. Confirm the background service commits and pushes it after the idle interval. Restart the Mac and repeat.

## Setup failures

- Remove the test repo's Git identity. Setup must explain how to fix it before changing config or service files.
- Test an unreachable remote, read-only credentials, and missing `gh` when GitHub login is required. Setup must identify the failed check and preserve any existing installation.
- Test working SSH credentials without `gh`. Setup must not require GitHub login.
- Fail service startup. Setup must report the failure instead of claiming success.
- Run setup twice. Existing selected repos must remain. Exit or select no repos on a fresh install; no empty service should be installed.
- Run `setup --no-launch`. Files should be written, with a clear message that the service was not started.

## Upgrade and uninstall

1. Install an older release and configure a repo. Upgrade with Homebrew. Confirm config and logs survive and the service uses the new binary. Repeat with a custom config path.
2. Upgrade an installation with no service plist. Confirm no service or interactive setup starts.
3. Cancel `repo-sync uninstall`. Confirm nothing changes.
4. Run setup with different config and binary paths. Confirm `~/Library/Application Support/repo-sync/install.json` retains their paths. Start a matching foreground process, then run `repo-sync uninstall --yes`. Confirm it stops that process and the service, removes recorded configs and verified copies in recorded or standard locations, and removes the Homebrew receipt when applicable. Repositories, Git history, shared credentials, and unrelated processes must survive.
5. Repeat with stale `status-<64 lowercase hex digits>.json` cache files, numbered `stdout.log` / `stderr.log` rotations, and `.repo-sync-<digits>` temporary files in config, cache, or LaunchAgents folders. Confirm they are removed. Put `personal.log` in the app's log folder; it must appear under `Preserved`. Check the final `Removed`, `Preserved`, and `Failed` sections.
6. Make a recorded custom config's parent folder unwritable. Confirm uninstall returns an error, reports the failed path, and retains `install.json` for retry. Restore permissions and rerun. Confirm the old config is removed even after its plist is gone.
7. Repeat with a Go installation and `--config` pointing to custom settings. Test `--keep-binary` twice: binaries remain, and `install.json` retains only binary paths for later removal.
8. Test `brew uninstall --cask repo-sync`: the service stops and user files remain. With `--zap`, standard config, logs, cache, and plist move to the trash.

The cask keeps cleanup under `zap` because Homebrew also runs uninstall hooks during upgrades. Its pre-uninstall hook stops launchd without deleting the plist and waits up to 45 seconds for the old process to exit. A timeout aborts removal or restart. Post-install reloads only an existing plist and preserves its config path. See the [GoReleaser cask schema](https://goreleaser.com/customization/homebrew_casks/) and [Homebrew Cask Cookbook](https://docs.brew.sh/Cask-Cookbook#stanza-zap).
