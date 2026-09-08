# Operations and troubleshooting

## Contents

- [Inspect service and configuration](#inspect-service-and-configuration)
- [Repair a conflict](#repair-a-conflict)
- [Legacy removal](#legacy-removal)
- [Common failures](#common-failures)
- [Updates and unsupported commands](#updates-and-unsupported-commands)
- [Agent skill installation](#agent-skill-installation)

## Inspect service and configuration

```sh
repo-sync status
/usr/bin/plutil -extract ProgramArguments json -o - "$HOME/Library/LaunchAgents/com.vectal-labs.repo-sync.plist"
```

Use `--config "/absolute/path/to/config.json"` for the path selected in the plist. A missing plist means no standard installation was found. Do not assume another config controls the running service.

Status checks readiness and repository health without committing or pushing. Newer binaries also report installed/running versions and cached update information. A nonzero exit may describe a repo failure, stale settings, version mismatch, or update failure. Read the reason.

Logs are in `~/Library/Logs/repo-sync/`. Read only what the task needs. Redact credentials and private content before reporting output. Runtime status lives in `~/Library/Caches/repo-sync/`; do not edit it to make checks pass.

The config contains:

- `idle_debounce`: positive duration string; default `"1m"`.
- `fetch_interval`: positive duration string; default `"1m"`.
- `repositories`: entries with unique `name`, absolute `path`, `remote`, and optional `allow` patterns.

For authorized setting changes, resolve config symlinks, preserve unrelated fields, and write atomically. Recheck the original contents immediately before writing; do not overwrite concurrent edits. The daemon reads settings at startup. Apply changes with `repo-sync setup --config "/absolute/path/to/config.json"`, then verify status with that config. For removing repos, prefer `remove` and use the legacy procedure below only when unavailable.

## Repair a conflict

Check installed help for `conflicts` first. An older binary may only log conflicts. Do not invent an unavailable command; report the limitation and use the installed version's supported workflow.

```sh
repo-sync conflicts --config "/absolute/path/to/config.json" "/absolute/path/to/notes"
```

Omit `--config` for the default config. Omit the repo path to list all saved conflicts in that config. For a missing folder, use its configured path to inspect the saved incident; restore or locate the clone before repairing.

The command reads saved conflict state without changing Git. It writes a private report in repo-sync's cache, outside synced folders. The report includes affected paths, snapshot time, commit IDs, base version, upstream side, and local commit being replayed. During a rebase, the upstream side can include earlier replayed local commits. These are conflict-time snapshots, not necessarily the latest branch tips.

Binary, oversized, missing, or submodule versions have explicit notes. Follow those notes and inspect Git objects or the relevant submodule when needed. Never treat an unavailable snapshot as an empty file, choose a winner automatically, or upload reports or private file contents to a remote service.

Use existing task authorization for the repair. Inspection alone does not authorize edits. Once authorized:

1. Read fresh `git status`, the current branch, configured remote, and default branch. If another merge, rebase, cherry-pick, revert, or bisect is active, leave it alone unless the task authorizes completing that operation. repo-sync also steps back while it is active.
2. Start from a clean worktree on the configured default branch. Preserve uncommitted work and inspect any branch mismatch before proceeding. Do not reset history, switch branches, or discard changes just to satisfy this step.
3. Fetch the configured remote, then start a fresh rebase onto its default branch. repo-sync already aborted its failed automatic rebase. For a verified `origin` remote and `main` default branch:

   ```sh
   git fetch origin
   git rebase origin/main
   ```

4. Resolve the fresh conflict in the editor using both sides and the base. Use the saved report as context only. Stage each intended resolution with `git --literal-pathspecs add -- "relative/path"` or `git --literal-pathspecs rm -- "relative/path"` for an intended deletion. Run `git rebase --continue`; repeat if later commits conflict. Do not use `--skip` to discard a commit. To cancel only the manual rebase you started, use `git rebase --abort`.
5. Check the resulting files and `git status`. Let repo-sync finish its normal sync, then verify fresh `repo-sync status` with the same config. A successful rebase alone does not prove the push succeeded. Report any remaining conflict or sync failure.

There is no pause or resume command. Automatic retries continue, and active manual Git operations are left alone. macOS sends one conflict notification per incident across ordinary retries and restarts. Notification settings can hide the banner; status keeps the incident visible until a verified full sync succeeds.

## Legacy removal

Use this only if installed help lacks `remove`. An available upgrade may add the command, but never assume it does. Installing the skill alone does not change CLI capabilities.

1. Find the exact registration and installed config using the plist. Match the configured absolute repo path, resolving symlinks when possible. For a missing folder, use its recorded path. If no registration matches, make no changes.
2. Inspect the service and record its PID before stopping it:

   ```sh
   /bin/launchctl print "gui/$(id -u)/com.vectal-labs.repo-sync"
   ```

   Treat only an explicit missing-service response as absent. For other inspection errors, stop and report the error. Verify the installed service uses the config being edited. If it uses another config, resolve that mismatch before proceeding.
3. If loaded, stop it gracefully:

   ```sh
   /bin/launchctl bootout "gui/$(id -u)/com.vectal-labs.repo-sync"
   ```

   Verify that the service is absent and its recorded PID has exited. Wait up to 45 seconds; a Git operation may finish during that time. If shutdown cannot be verified, leave the config unchanged and report the failure. Do not force-kill processes.
4. Remove only the requested entry from `repositories`. Preserve other settings, file permissions, and any config symlink. Use an atomic write to the resolved target, checking for concurrent changes first. Never delete the clone, `.git`, its remote, or unrelated registrations. If saving fails after shutdown, report that the service is stopped and the removal was not saved.
5. If the service was loaded and other registrations remain, restart the same plist:

   ```sh
   /bin/launchctl bootstrap "gui/$(id -u)" "$HOME/Library/LaunchAgents/com.vectal-labs.repo-sync.plist"
   ```

   Verify `repo-sync status` with the same config. If the binary lacks `status`, verify a fresh service PID and fresh startup logs showing the expected repositories; clearly state that this older binary lacks the normal health check.
6. If no registrations remain, keep the service unloaded. Re-read the saved config and verify service absence again after saving. If an updater restarted it, recheck its config and stop it gracefully again before claiming completion. Older daemons cannot report readiness when empty, and older `setup` exits without restarting an empty configuration. Keep settings and the plist so the user can add repos later.
7. If the service was absent, report only removal from the config. Any foreground `repo-sync run` process must be stopped by its owner or restarted within explicit authorization. Do not claim all syncing stopped from a config edit alone.

If restart or verification fails, report that the removal was saved and remaining repos may not be syncing. Do not silently restore the removed registration. These steps affect only this Mac.

## Common failures

- **Identity or authentication:** follow the named CLI failure. Use existing identity and credentials. Setup checks fetch and dry-run push access; real server hooks or branch protection can still reject a later push.
- **Feature branch or detached HEAD:** report the skipped branch. Do not switch branches automatically.
- **Conflict:** inspect `repo-sync conflicts` when available, then follow **Repair a conflict** above. The notification is immediate; status remains `conflict` until a full sync succeeds.
- **Missing folder:** the registration stays; other repos continue. Returning the folder triggers recovery. Remove its registration when the user wants permanent un-syncing.
- **Offline:** automatic retries continue without offline notifications.
- **Other failures:** retries start at 1 minute and double up to 30 minutes. Persistent failures notify after 10 minutes.
- **Blocked files or held pushes:** inspect named paths and unpublished commits without revealing secret contents. repo-sync does not rewrite those commits for you.

Inside the relevant repo, `repo-sync allow "relative/path"` overrides filename protection for an explicitly approved file. Recheck readiness afterward because older `allow` commands can save settings despite a failed restart.

## Updates and unsupported commands

Check installed help first. Newer releases support:

```sh
repo-sync version
repo-sync update
repo-sync updates off
repo-sync updates on
```

`update` checks and upgrades a Homebrew installation. `updates off` disables automatic installation while keeping update notifications. Change this preference only when requested.

For older Homebrew installs, an authorized upgrade uses `brew upgrade --cask repo-sync`. For Go installs, update the binary through the original installation method, then rerun setup. After an upgrade, check help, version when available, and service status. If the needed command is still absent, explain the installed release's limitation.

## Agent skill installation

Use the installed `repo-sync help` and `repo-sync skill --help` as the contract. Releases that include `skill` support:

```sh
repo-sync skill install
repo-sync skill status
repo-sync skill refresh
repo-sync skill uninstall
```

Installation uses `~/.agents/skills/repo-sync` for Codex, Pi, and Cursor. Detected Claude Code and Hermes installations also receive copies under `${CLAUDE_CONFIG_DIR:-~/.claude}/skills` and `${HERMES_HOME:-~/.hermes}/skills`. Existing native Pi or Cursor copies are checked so they do not silently hide the shared copy; Pi honors `PI_CODING_AGENT_DIR`. Check the actual paths reported by the command. Local installation does not configure cloud agents.

Explicit installation can adopt a folder that exactly matches the bundled files. Other unowned folders, customized managed copies, extra files, and symlinked skill folders are preserved. Parent skill-directory aliases are deduplicated. Do not delete personal instructions to resolve a conflict; inspect the reported folder and use the user's authorization for any replacement.

Refresh updates only previously managed copies. Missing copies require explicit `skill install`, except when resuming an interrupted installation. Failures retain ownership information for retry. `skill status` reports managed copies, customizations, and incomplete operations. A successful installation verifies files on disk, not whether a running agent has loaded them; start a new session.

The installation receipt is `~/Library/Application Support/repo-sync/skill-install.json`. It is separate from sync configuration and must not be edited to claim ownership of personal files. Skill commands do not start, stop, or configure syncing. Full CLI uninstall and true Homebrew uninstall remove unchanged managed copies; upgrades and reinstalls preserve them for refresh.

## Sources

- [Repository and installation](https://github.com/vectal-labs/repo-sync)
- [Detailed behavior](https://github.com/vectal-labs/repo-sync/blob/main/docs/behavior.md)

These pages may describe a newer release. Installed help takes precedence for available commands.
