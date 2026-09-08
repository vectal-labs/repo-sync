# Automatic update evidence — 2026-09-08

## Scope and approved policy

David approved daily automatic Homebrew updates, an off switch that keeps update reminders, and notifications for failures or manual action. This eval checks the assumptions behind that policy. It does not prove that every user prefers automatic installation.

Research: 20 successful DeepAPI web searches, 6 successful website requests covering 12 source pages, and 3 separate successful deep researches. Live measurements below were read-only. No installed package, user configuration, or launchd job was changed. Release archives were downloaded only to temporary storage and inspected without executing them.

## Findings that affect implementation

- **Refreshing metadata is essential.** The real installed cask and local tap both reported 0.1.1. `brew outdated` returned no outdated casks. GitHub's live release and remote tap both reported 0.1.2. A cached Homebrew check alone would reproduce the original failure. Refresh metadata before deciding that the installation is current; retain a useful error if refresh fails. [Homebrew update behavior](https://docs.brew.sh/Manpage)
- **Use Homebrew for Homebrew installations.** Tailscale preserves the installation mechanism when updating and exposes per-device controls. This supports the chosen ownership model. Target `vectal-labs/tap/repo-sync`; unrelated installed packages must not become explicit upgrade targets. Homebrew may still install or update required dependencies. [Tailscale update policy](https://tailscale.com/docs/features/client/update), [Homebrew FAQ](https://docs.brew.sh/FAQ)
- **Keep the updater alive independently of the sync daemon.** In Homebrew 6.0.21, an upgrade fetches the archive, runs the old uninstall artifacts, backs up the old version, installs the new artifacts and postflight, purges the old version, and finally writes the new receipt. A postflight check cannot assume the installed receipt already describes the new version. The updater must survive old-file removal and daemon shutdown. [Homebrew upgrade source](https://github.com/Homebrew/brew/blob/6.0.21/Library/Homebrew/cask/upgrade.rb), [installer source](https://github.com/Homebrew/brew/blob/6.0.21/Library/Homebrew/cask/installer.rb)
- **Recovery is best effort, not guaranteed rollback.** Homebrew attempts to restore the previous cask after installation errors, but its rollback can also fail. A later failed daemon readiness check is outside a completed Homebrew transaction. Inspect the binary that actually remains, restore/start its valid service configuration when possible, and persist unresolved failure details. Do not silently claim rollback or edit Homebrew receipts. [Homebrew upgrade source](https://github.com/Homebrew/brew/blob/6.0.21/Library/Homebrew/cask/upgrade.rb)
- **Wait for application readiness and version.** A loaded launchd job or successful `brew` exit does not establish that the new daemon is healthy. Check its fresh status, PID, and expected version. The existing daemon uses a stable symlink, while its cask hooks also support older version-specific program paths. [Current release cask](https://github.com/vectal-labs/homebrew-tap/blob/main/Casks/repo-sync.rb)
- **Handle sleep and process groups deliberately.** The installed macOS `launchd.plist(5)` explicitly says `StartInterval` misses firings during sleep. `StartCalendarInterval` coalesces missed firings on wake. It also says launchd kills remaining children in a job's process group when that job exits. Use calendar wakeups plus persisted due times/retries; do not unload the updater while its Homebrew subprocess is active. These statements were checked against `/usr/share/man/man5/launchd.plist.5` on the measured Mac. Apple's lifecycle documentation also requires clean termination and per-user agent lifecycle handling. [Apple launchd jobs](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html)
- **Allow enough time for Git work to stop.** The real existing service showed a system-defined exit timeout of only 5 seconds. Its plist did not set `ExitTimeOut`. Waiting 45 seconds in a hook does not override launchd's earlier SIGKILL deadline. Set an explicit finite shutdown allowance and verify the old process has exited before starting another one.
- **Do not infer contention from lock-file existence.** A repo-sync Homebrew lock file existed, but `lsof` found no holder. Homebrew's code uses a nonblocking `flock` and can retain unlocked files. Keep an independent updater lock, respect Homebrew's errors, and retry; do not delete its lock files. [Homebrew lock implementation](https://github.com/Homebrew/brew/blob/6.0.21/Library/Homebrew/lock_file.rb)
- **Notifications cannot be the only record.** AppleScript notifications depend on user notification settings; modern notification APIs require authorization that can change later. Persist last result and next action in `status`, deduplicate reminders, and avoid treating notification submission as confirmed delivery. GitHub CLI supplies precedent for cached 24-hour checks and an explicit notification opt-out. [AppleScript notifications](https://developer.apple.com/library/archive/documentation/LanguagesUtilities/Conceptual/MacAutomationScriptingGuide/DisplayNotifications.html), [Apple authorization](https://developer.apple.com/documentation/usernotifications/asking-permission-to-use-notifications), [GitHub CLI environment](https://cli.github.com/manual/gh_help_environment)

## Live measurement suite

Measured around 08:42 UTC on 2026-09-08. All Homebrew information commands used `HOMEBREW_NO_AUTO_UPDATE=1` to avoid changing the real installation during investigation. A command returning “missing” or “not outdated” is an observed result, not a skipped case.

1. **Host compatibility:** `uname -m` and `sw_vers -productVersion` reported arm64 and macOS 26.6.2. This is one Apple Silicon machine, not a fleet sample.
2. **Package-manager version:** `brew --version` reported Homebrew 6.0.21.
3. **Package-manager prefix:** `brew --prefix` reported `/opt/homebrew`. Intel's usual prefix was not tested on real Intel hardware.
4. **Installed package:** `brew list --cask --versions repo-sync` reported 0.1.1.
5. **Cached cask metadata:** `brew info --cask --json=v2 vectal-labs/tap/repo-sync` reported available 0.1.1, installed 0.1.1, `outdated: false`, `pinned: false`, and the expected tap identity.
6. **False-current reproduction:** `brew outdated --cask --json=v2 repo-sync` exited 0 with empty formula and cask arrays, despite the live 0.1.2 release.
7. **Running daemon:** `launchctl print gui/<uid>/com.vectal-labs.repo-sync` reported one active running process, launched from `/opt/homebrew/bin/repo-sync`, and no previous exit. No stop/restart was performed.
8. **Installed binary provenance:** `go version -m /opt/homebrew/bin/repo-sync` reported module v0.1.1, darwin/arm64, and a clean VCS revision `0157499b3ac1ddd58035b2a3d1bfcc3523d2e6c8`.
9. **Old-user command surface:** installed `repo-sync --help` offered setup/add/allow/run, with neither version nor status/update controls. Existing users therefore need an initial manual upgrade to gain the new functionality.
10. **Updater availability:** the initial probe of `com.vectal-labs.repo-sync.updater` and final probe of the implemented label `com.vectal-labs.repo-sync.updates` both returned missing-service diagnostics. The existing installation has neither job.
11. **Real Homebrew lock state:** a zero-byte `repo-sync.formula.lock` existed; `lsof` found no process holding it. File existence is not a contention signal.
12. **Local tap freshness:** `git log -1` in the installed tap reported commit `469b749`, dated September 1, publishing 0.1.1. The real metadata was a release behind.
13. **Live latest release:** GitHub's latest-release API returned v0.1.2, published `2026-09-07T20:10:50Z`, with both `draft` and `prerelease` false. It listed 3 assets: checksums and 2 Darwin archives.
14. **Live tap availability:** GitHub's cask-content API returned version 0.1.2, arm64 and amd64 artifact URLs/hashes, `git` and `gh` dependencies, and daemon uninstall/post-install lifecycle hooks. The package was available remotely; local metadata was the limiting factor.
15. **GUI domain:** the current user's GUI launchd domain existed as a login session. Logged-out and SSH-only behavior remains a separate lifecycle test requirement.
16. **Executable ownership:** the real public command was a symlink into `Caskroom/repo-sync/0.1.1/repo-sync`. Its versioned target is vulnerable to normal upgrade cleanup, establishing the need for updater continuity outside that old path.
17. **Installed service contract:** `plutil -p` showed `KeepAlive`, `RunAtLoad`, a Homebrew-inclusive PATH, a stable binary path, and no explicit `ExitTimeOut`. Live launchd output showed a 5-second exit timeout.
18. **Release ordering:** GitHub's releases API returned 3 stable releases: v0.1.2, v0.1.1, and v0.1.0. No prerelease or draft was present, so prerelease filtering still requires synthetic coverage.
19. **Real arm64 artifact:** downloaded 1,690,776 bytes. SHA-256 `eb7875cdd3036402dcd7ed06eeb970d89bac6aaa105bde07d703883717173559` matched both live cask and release digest. The 3-entry archive contained a darwin/arm64 binary with Go module v0.1.2 and clean revision `051a5c85524c9489c0c47bd308623fa892071239`.
20. **Real amd64 artifact:** downloaded 1,793,616 bytes. SHA-256 `88577f762a5492e52ee1c35677e5e00fb1b80e90247854dfac1c3bdf5f91f1a9` matched both live cask and release digest. The 3-entry archive contained a darwin/amd64 binary with the same v0.1.2 module and clean revision.

Release metadata: [v0.1.2](https://github.com/vectal-labs/repo-sync/releases/tag/v0.1.2). Both archive checks passed; neither archive was installed or executed. Checksums establish integrity against the cask/release metadata, not an independent developer signature.

## DeepAPI request provenance

All three requests used `POST /v1/research/deep`, returned `status: succeeded`, reported `completeness: complete`, and had no pending polling action.

- Best-in-class unattended package-manager updates: `9effd18d-cea9-4c92-8fd9-c5f8cb4ced96`.
- Homebrew/launchd lifecycle, locks, sleep, and recovery: `c6bc3385-6e2a-4aa8-976a-ad9196c0c665`.
- User needs, opt-out, notification fatigue, and manual installations: `0c97dc4b-6882-4983-87fd-1a1bb035e7d8`.

Raw responses and read-only command results are retained locally in `/private/tmp/repo-sync-update-research`; request IDs can recover DeepAPI results. Search snippets and forum anecdotes were not treated as verified platform contracts. The source-backed constraints above use official documentation, current local Homebrew source, and live measurements.

## Implementation validation

- `go test -race -count=1 ./...`, `go vet ./...`, and release binary compilation passed. Behavioral tests cover disabled updates, stale metadata, release validation, failed package operations, recovery, concurrency, notifications, and cached status.
- Both isolated real macOS launchd tests passed: setup/sync/restart/uninstall, and an update blocked by a real Git hook followed by failed-upgrade recovery and a verified newer daemon pushing real commits. They used unique service labels and disposable repositories. Homebrew operations were intercepted; installed packages and the normal user service were untouched.
- Five real-process supervisor tests passed with the race detector. They cover cancellation, timeout, nested process groups, updater death while inherited locks remain held, and command output. The compiled release also passed its private supervisor dispatch and persistent on/off/version CLI checks.
- All 16 cask tests passed (56 assertions), including Homebrew 6.0.21's native loader and artifact forwarding. Tests cover true uninstall versus upgrade/reinstall, preserving the running updater, retaining uninstall locks through package removal, and custom config arguments.
- All 27 CI policy tests passed. GoReleaser 2.18.0 validated the release configuration and built a local snapshot with both Darwin architectures, archives, checksums, and a cask. Homebrew successfully loaded that generated cask. Nothing was published.

## Remaining verification boundaries

The implementation has not yet performed a real unattended Homebrew package replacement. Native cask tests intercept commands, and the launchd update test replaces the package-manager boundary. After publication, perform the one-time bootstrap upgrade on an opted-in machine and verify its next automatic installation and daemon version. Intel binaries were built, but not executed on Intel hardware. Sleep/wake behavior follows launchd's calendar contract and still needs a real sleep-cycle check. Notification submission is tested; visible banner delivery depends on macOS settings.

The default is an approved product choice, not an empirical majority preference. The research found no representative repo-sync user survey or fleet failure-rate data. Desktop notifications remain best effort. Signing/notarization remains the separate issue recorded in ADR 0004.
