# how repo-sync behaves

repo-sync is for shared team documents and context where edits should be committed and shared automatically on the default branch. It does not manage pull requests or code review workflows.

## setup

- setup explains automatic commits, finds repositories (including folders with just 1 repo), and lets you select repos or enter a path. nothing is preselected. rerunning setup keeps existing repositories.
- before changing config or service files, setup checks Git identity, noninteractive fetch access, and push access with a dry run for every configured repo. these checks use the background service's `PATH`.
- a push dry run does not upload commits. server hooks and branch protection can still reject a real push later.
- GitHub HTTPS credential repair uses GitHub CLI only when needed. Homebrew installs Git and `gh`; other installations get clear instructions if a required tool is missing.
- stale global Git TLS-version pins are ignored. if a check fails, setup reports the repo and stops before installing.
- setup starts the service and checks that it stays running. `--no-launch` writes the files without starting or verifying the service.
- setup then offers the optional bundled agent skill and remembers yes or no. end-of-input skips the offer without recording an answer. repeating setup refreshes previously managed, unchanged copies. skill installation does not select repos or change syncing.

## status and upgrades

- `repo-sync version` prints the installed release. development builds report `dev`.
- `repo-sync status` shows installed and running versions, latest known release, update settings/result, and repository health. service failures, mismatched running releases, failed updates, conflicts, and repositories retrying errors return a nonzero exit code. update details remain visible if the daemon is stopped. status uses cached update information and does not commit, push, or contact release servers. pass `--config /path/to/config.json` for custom settings.
- setup installs a separate updater. Homebrew cask installations update automatically by default. other installations receive a notification naming the installation to update manually. development builds are never automatically replaced.
- `repo-sync updates off` disables automatic installation and keeps notifications. `repo-sync updates on` re-enables it. settings take effect without restarting the sync daemon and survive upgrades and repeated setup. `repo-sync update` checks immediately and upgrades a Homebrew cask even when automatic installation is disabled; it does not re-enable the setting.
- checks run daily. a separate LaunchAgent wakes every 15 minutes and after sleep to check whether work is due. failed checks retry every 15 minutes. short network or Homebrew failures stay quiet; failures lasting 24 hours notify once. installation/restart failures notify immediately. manual-action reminders notify once per release. notification settings may hide banners, so status always retains the result.
- the updater checks stable GitHub releases, refreshes Homebrew metadata, and requires the cask to match the release. it targets `vectal-labs/tap/repo-sync`; Homebrew may also update required dependencies. pinned or disabled casks require manual action. a stale tap retries and eventually notifies. prereleases and downgrades are refused.
- downloads finish before the updater waits for active Git work. the daemon and updater share an operating-system lock: existing sync cycles finish, new cycles wait, and a busy cycle postpones the upgrade. concurrent update/setup/uninstall operations are refused. the setting is checked again before installation, so turning automatic updates off during a download prevents installation.
- Homebrew runs under a separate supervisor which retains the locks until its subprocesses stop. a timeout or an updater crash cancels the package operation and cleans up its descendants, including Homebrew's separate process groups. if macOS cannot establish that cleanup is complete, exclusion stays held and the reason is logged.
- success requires the installed binary and configured daemon to report the expected version with fresh readiness. Homebrew owns package rollback. after an installation failure, repo-sync attempts to restart the verified binary Homebrew preserved; recovery is best effort, and unresolved failures remain visible. update failures never trigger automatic edits to repositories or Git history.
- `brew upgrade --cask repo-sync` preserves settings and logs. if a service plist already exists, the install hook updates its binary path and reloads it. it never opens interactive setup.
- existing users must manually upgrade once to receive the updater. that upgrade enrolls an already configured service. an unconfigured Homebrew install creates no background jobs until setup.
- after an upgrade, run `repo-sync status` to check readiness. for a Go installation, install the new binary and rerun `repo-sync setup`.
- the binary embeds the official agent skill. `repo-sync skill install`, `status`, `refresh`, and `uninstall` manage its local copies without touching repositories or services. Homebrew post-install refreshes managed copies, including installations with no sync service. Go users rerun setup or `repo-sync skill refresh` after updating the binary. explicit installation may adopt an identical unowned copy; other unowned or customized skill folders are preserved. see [agent skill installation](agent-skill.md).

## stop syncing one repository

- `repo-sync remove [path]` removes a repository from this Mac's config. Without a path, it uses the current repository. A missing folder can be removed using its configured path.
- it stops the matching background service gracefully, saves the removal, restarts the service, and verifies readiness. a Git operation already in progress may finish during shutdown. files and Git history are preserved; previous pushes remain on the remote.
- remaining repositories continue syncing after the restart. removing the last repository leaves a healthy service with no repositories.
- use `repo-sync remove --config /path/to/config.json /path/to/repo` for a custom config. if a loaded service uses another config, removal stops with an error before changing settings.
- if restarting or readiness fails, the command returns an error and keeps the removal saved. if no background service is running, it only removes the registration; any foreground `repo-sync run` processes need restarting separately.
- the [agent skill](agent-skill.md) includes an operational guide and legacy removal instructions.

## uninstall

- `repo-sync uninstall` previews this user's recorded and standard install locations, including `PATH`, Go, and Homebrew locations. it finds verified repo-sync copies there; it does not search the whole disk. Homebrew installations are removed through Homebrew.
- after confirmation, it stops the service and gracefully stops matching processes owned by this user. macOS process identity and executable checks prevent stopping unrelated processes. shutdown waits up to 45 seconds; a failure stops cleanup. processes whose executable macOS hides cannot be identified, including binaries deleted while running.
- cleanup removes known settings, the plist, stale status files, rotated logs, and generated temporary files in recognized locations. unrecognized files are preserved. custom config folders are never deleted recursively.
- use `--yes` for noninteractive removal or `--config /path/to/config.json` for a custom config. `--keep-binary` retains the binaries and their paths in `install.json` for later removal.
- every run reports `Removed`, `Preserved`, and `Failed`. incomplete cleanup returns a nonzero exit code and keeps the ownership record and remaining program when possible. fix the reported failure and rerun uninstall.
- repositories, Git history, shared Git credentials, Git, and GitHub CLI are preserved.
- full CLI uninstall and true Homebrew uninstall remove unchanged managed skill copies. customized folders remain. Homebrew upgrades and reinstalls preserve skill registration so the new binary can refresh it. skill cleanup uses its own lock and never stops processes or invokes full uninstall.
- `brew uninstall --cask repo-sync` stops the service but retains user files. add `--zap` to remove the standard settings, logs, cache, and plist too. Homebrew moves these files to the trash.

## syncing

- after 60 seconds without local edits, repo-sync commits every non-ignored change, including files you already staged. the commit message lists the files.
- staged renames and deletions (`git mv`, `git rm`) sync like any other change, with no manual commit needed. the old path disappears from the remote and the renamed contents are kept, including edits made after the rename.
- it rebases on top of the remote, then pushes. every 60 seconds it also fetches and pulls what teammates pushed.
- it syncs again right after wake and when the network comes back.
- both intervals are in the config file.

## branches

- only the remote's default branch is synced. it is read from `origin/HEAD`, with `main` as fallback.
- on a feature branch or detached head the checkout is left untouched and the cycle is skipped. it never switches branches.
- while it stages, commits, and rebases, repo-sync holds git's own `index.lock`. a normal `git checkout`, `commit`, `rebase`, or `merge` you run at that moment is refused by git with its usual "index.lock exists" message; run it again a second later. `git status` and other reads keep working.
- between those steps a switch succeeds, and repo-sync checks the branch again before changing the checkout. pushes run without the lock and send the exact verified main commit, so switching branches during a push cannot publish feature work.
- `git checkout -b` at the current commit is the one switch git allows without that lock. if it slips in, repo-sync undoes what it just did on the new branch and skips the cycle. main and the new branch end up exactly where they were.
- hooks run as usual and inherit the daemon's index, so a hook could switch branches mid-rebase. repo-sync uses HEAD's reflog to distinguish switches during the rebase from switches after it finishes. a switch during the rebase restores main and stops the push; a switch after it finishes leaves the other branch's work alone and skips the rest of the cycle. repositories without reflogs fall back to comparing commit metadata.
- the daemon never aborts a rebase it did not start. someone else's rebase, merge, or cherry-pick in progress makes it step back.
- if the daemon is killed hard while holding the lock, `index.lock` stays behind, exactly as after any crashed git command. remove it by hand.
- if a repo stays off its default branch for 24 hours you get one notification. no nagging.
- during a merge, rebase, cherry-pick, revert, or bisect it steps back until you are done.

## secrets

- files are blocked by name only. no content scanning.
- blocked: `.env`, `.env.*`, `*.env`, private keys (`id_rsa*`, `id_ed25519*`, `*.key`, `*.pem`, `*.p8`, `*.p12`, `*.pfx`, `*.ppk`, `*.jks`, `*.keystore`, `*.kdbx`), tool credentials (`.npmrc`, `.pypirc`, `.netrc`, `.git-credentials`, `.htpasswd`, `.pgpass`, `.my.cnf`, `.vault-token`, `auth.json`), cloud files (`.aws/credentials`, `application_default_credentials.json`, `.config/gh/hosts.yml`, `.docker/config.json`, `.kube/config`, `*.kubeconfig`), infra (`*.tfstate*`, `*.tfvars`, `.terraform.d/credentials.tfrc.json`), and `secrets.*`, `credentials.*`, `client_secret*.json`, `service-account*.json`.
- allowed: `.env.example`, `.env.sample`, `.env.template`, `.env.dist`.
- a blocked file is left out. everything else still syncs. you get one notification per file.
- a blocked file you staged by hand is unstaged so it never reaches the remote.
- deleting a tracked blocked file with `git rm`, or renaming it to a safe name with `git mv`, syncs like any other change. a deletion publishes nothing.
- `git rm --cached` plus a `.gitignore` entry stops tracking a file but keeps it on disk. the deletion syncs when the remote has not moved. if teammates pushed meanwhile, the rebase would overwrite the ignored copy, so repo-sync refuses it, keeps the file untouched, and reports the failure until you move the file aside.
- `repo-sync allow <file>` inside the repo overrides the guard for that file. the list is in [`internal/app/secrets.go`](../internal/app/secrets.go).
- before every push, the exact commits about to be published are checked with the same filename rules, against what the push destination actually holds. a commit that adds or changes a blocked file holds the push back, whether you committed it by hand, a hook staged it, or it was staged at the last moment. a secret committed and deleted again in a later unpublished commit is still caught. replacement refs (`git replace`) are ignored; the real objects are inspected.
- a held push retries automatically. repo-sync does not edit your commits to remove the file and does not delete it. you get one notification per file naming the file and commit. drop the file from your unpublished commits (for example `git reset --soft origin/main`; repo-sync then recommits everything else and leaves the file out) or run `repo-sync allow <file>`. other repositories keep syncing.
- if the remote fetches from one url and pushes to another, a secret commit that is already on the fetch source but not at the push destination is still held. no local reset removes it: fix the push url or allow the file.
- a blocked file the remote already tracks does not freeze unrelated pushes, and a commit that only deletes one still pushes. local changes to such a file never publish: save them outside the repo and restore the published version with `git checkout origin/main -- <file>`.
- the push publishes exactly the validated commit, and only if the destination still holds the tip it was validated against. if the destination moved meanwhile, it fetches and validates again. that is a compare-and-swap, not a force-push: the destination must be part of local history or nothing is pushed.
- submodule commits are never pushed on your behalf. the parent push waits until they are on the submodule's remote; sync the submodule as its own repository or push it yourself.

## conflicts and failures

- On a rebase conflict, repo-sync captures the affected files and available conflict versions before aborting its own rebase. It keeps your local commits and retries later. It never force-pushes or chooses a resolution.
- Conflicts notify immediately, once per incident across ordinary retries and daemon restarts. The title is `repo-sync`. For a repo named `team-docs`, the message is: `Conflict in team-docs. Local changes are saved. Run repo-sync conflicts for help.` A custom config adds the correct `--config` argument. macOS notification settings can hide the banner.
- `repo-sync status` keeps showing `conflict`, affected files, and the help command while retries continue. A conflict clears only after a verified full sync succeeds. Switching branches, going offline, or starting a manual repair does not erase it. Other repositories continue syncing.
- `repo-sync conflicts [--config path] [repo path]` lists saved conflicts for the selected config. A repo path selects one configured repository, including a missing folder. The command does not change Git. It writes a private local report outside synced folders and prints its path, affected files, snapshot time, commit IDs, and repair steps.
- Reports contain the base, upstream side, and local commit being replayed at the conflict. The upstream side may include earlier replayed local commits. These snapshots can differ from today's branch tips. Binary, oversized, missing, and submodule versions have explicit limitations. Reports can contain private document content; keep them local.
- To repair, inspect fresh Git state and start from a clean default branch. Fetch the configured remote and start a new rebase onto its default branch because the automatic rebase was aborted. Resolve the fresh conflicts, stage the intended files, and run `git rebase --continue` for each conflicting commit. Use snapshots as context, not as replacements for current files. `git rebase --abort` cancels a manual rebase you started. repo-sync leaves manual Git operations alone and verifies recovery on its next successful sync.
- if a teammate pushes between the fetch and the push, it fetches, rebases, and pushes once more right away. only Git's own race reasons count: `fetch first`, `non-fast-forward`, or the remote reporting the ref is at one commit but expected another. a stuck remote lock file, a permission error, or a hook refusal is a normal failure: it backs off and notifies like any other error.
- failures retry with backoff: 1 minute, doubling, up to 30 minutes. success resets it.
- one failing repo never blocks another.
- each repo runs one sync cycle at a time. a cycle owns its repo until the Git work, the result bookkeeping, the notifications, and the retry decision are all applied. only then can the next cycle for that repo start. different repos sync in parallel.
- being offline (dns, connection, reset, timeout) is not an incident. it never notifies; it just retries.
- any other failure notifies once it has lasted 10 minutes, then silence until it recovers. repos that cross that line together share one notification. short blips stay silent.
- if a file vanishes while staging, or the worktree changes right before the rebase, the cycle is skipped and retried. nothing is committed and no notification is sent.
- there is no paused state and no `resume` command. if something cannot be done safely it logs, waits, and tries again.

## missing folders

- if a repo folder is gone (deleted, renamed, or on a disk that is not mounted), only that repo is skipped. every other repo keeps syncing. the service starts even when some or all folders are missing.
- the repo stays in the config. nothing is removed and no command is needed.
- a missing folder follows the normal failure rules: retries with backoff, one notification once it has been gone for 10 minutes, then silence.
- every 10 seconds it checks whether the folder is back. when it is, the repo syncs right away, without a restart or a manual command.
- a repo that was missing at startup, or that came back, is covered by that 10-second poll rather than file events. edits still sync after the usual quiet period.

## files

- config: `~/Library/Application Support/repo-sync/config.json`. one json file. edit it by hand if you like, then run `repo-sync setup` again or restart the service.
- installation history: `~/Library/Application Support/repo-sync/install.json`. setup records config and binary paths so uninstall can find older installations.
- logs: `~/Library/Logs/repo-sync/`.
- runtime status: `~/Library/Caches/repo-sync/`. kept separate from custom config files so health updates do not change your repos.
- service: `~/Library/LaunchAgents/com.vectal-labs.repo-sync.plist`. it starts at login and restarts on crash.
- updater: `~/Library/LaunchAgents/com.vectal-labs.repo-sync.updates.plist`. update preferences live in `~/Library/Application Support/repo-sync/update-settings.json`; results and locks live in the standard cache folder. update logs use `updates-stdout.log` and `updates-stderr.log`. uninstall removes both jobs and their owned files.

## development

```sh
go build ./...
go vet ./...
go test -race ./...
```

the tests spin up real git remotes and clones in temp folders. the end to end test runs the actual daemon. see [installation checks](install-testing.md) for release validation. decisions are in `docs/adr/`.
