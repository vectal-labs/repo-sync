# how repo-sync behaves

## setup

- setup explains automatic commits, finds repositories (including folders with just 1 repo), and lets you select repos or enter a path. nothing is preselected. rerunning setup keeps existing repositories.
- before changing config or service files, setup checks Git identity, noninteractive fetch access, and push access with a dry run for every configured repo. these checks use the background service's `PATH`.
- a push dry run does not upload commits. server hooks and branch protection can still reject a real push later.
- GitHub HTTPS credential repair uses GitHub CLI only when needed. Homebrew installs Git and `gh`; other installations get clear instructions if a required tool is missing.
- stale global Git TLS-version pins are ignored. if a check fails, setup reports the repo and stops before installing.
- setup starts the service and checks that it stays running. `--no-launch` writes the files without starting or verifying the service.

## status and upgrades

- `repo-sync status` checks the service and reports repository health. a service failure or a repo retrying after an error returns a nonzero exit code. it does not commit or push changes. pass `--config /path/to/config.json` for custom settings.
- `brew upgrade --cask repo-sync` preserves settings and logs. if a service plist already exists, the install hook updates its binary path and reloads it. it never opens interactive setup.
- after an upgrade, run `repo-sync status` to check readiness. for a Go installation, install the new binary and rerun `repo-sync setup`.

## uninstall

- `repo-sync uninstall` asks for confirmation, stops the service, and removes its plist, settings, logs, cache, and installed binary. Homebrew installations are removed through Homebrew.
- use `--yes` for noninteractive removal or `--keep-binary` to retain the executable. use `--config /path/to/config.json` for a custom config.
- repositories, Git history, shared Git credentials, Git, and GitHub CLI are preserved.
- `brew uninstall --cask repo-sync` stops the service but retains user files. add `--zap` to remove the standard settings, logs, cache, and plist too. Homebrew moves these files to the trash.

## syncing

- after 60 seconds without local edits, repo-sync commits every non-ignored change, including files you already staged. the commit message lists the files.
- it rebases on top of the remote, then pushes. every 60 seconds it also fetches and pulls what teammates pushed.
- it syncs again right after wake and when the network comes back.
- both intervals are in the config file.

## branches

- only the remote's default branch is synced. it is read from `origin/HEAD`, with `main` as fallback.
- on a feature branch or detached head the checkout is left untouched and the cycle is skipped. it never switches branches.
- if a repo stays off its default branch for 24 hours you get one notification. no nagging.
- during a merge, rebase, cherry-pick, revert, or bisect it steps back until you are done.

## secrets

- files are blocked by name only. no content scanning.
- blocked: `.env`, `.env.*`, `*.env`, private keys (`id_rsa*`, `id_ed25519*`, `*.key`, `*.pem`, `*.p8`, `*.p12`, `*.pfx`, `*.ppk`, `*.jks`, `*.keystore`, `*.kdbx`), tool credentials (`.npmrc`, `.pypirc`, `.netrc`, `.git-credentials`, `.htpasswd`, `.pgpass`, `.my.cnf`, `.vault-token`, `auth.json`), cloud files (`.aws/credentials`, `application_default_credentials.json`, `.config/gh/hosts.yml`, `.docker/config.json`, `.kube/config`, `*.kubeconfig`), infra (`*.tfstate*`, `*.tfvars`, `.terraform.d/credentials.tfrc.json`), and `secrets.*`, `credentials.*`, `client_secret*.json`, `service-account*.json`.
- allowed: `.env.example`, `.env.sample`, `.env.template`, `.env.dist`.
- a blocked file is left out. everything else still syncs. you get one notification per file.
- a blocked file you staged by hand is unstaged so it never reaches the remote.
- `repo-sync allow <file>` inside the repo overrides the guard for that file. the list is in [`internal/app/secrets.go`](../internal/app/secrets.go).

## conflicts and failures

- on a rebase conflict it runs `git rebase --abort`, keeps your local commits, and retries later. it never force-pushes and never resolves conflicts.
- if a teammate pushes between the fetch and the push, it fetches, rebases, and pushes once more right away. only Git's own race reasons count: `fetch first`, `non-fast-forward`, or the remote reporting the ref is at one commit but expected another. a stuck remote lock file, a permission error, or a hook refusal is a normal failure: it backs off and notifies like any other error.
- failures retry with backoff: 1 minute, doubling, up to 30 minutes. success resets it.
- one failing repo never blocks another.
- each repo runs one sync cycle at a time. a cycle owns its repo until the Git work, the result bookkeeping, the notifications, and the retry decision are all applied. only then can the next cycle for that repo start. different repos sync in parallel.
- being offline (dns, connection, reset, timeout) is not an incident. it never notifies; it just retries.
- any other failure notifies once it has lasted 10 minutes, then silence until it recovers. repos that cross that line together share one notification. short blips stay silent.
- if a file vanishes while staging, or the worktree changes right before the rebase, the cycle is skipped and retried. nothing is committed and no notification is sent.
- there is no paused state and no `resume` command. if something cannot be done safely it logs, waits, and tries again.

## files

- config: `~/Library/Application Support/repo-sync/config.json`. one json file. edit it by hand if you like, then run `repo-sync setup` again or restart the service.
- logs: `~/Library/Logs/repo-sync/`.
- runtime status: `~/Library/Caches/repo-sync/`. kept separate from custom config files so health updates do not change your repos.
- service: `~/Library/LaunchAgents/com.vectal-labs.repo-sync.plist`. it starts at login and restarts on crash.

## development

```sh
go build ./...
go vet ./...
go test -race ./...
```

the tests spin up real git remotes and clones in temp folders. the end to end test runs the actual daemon. see [installation checks](install-testing.md) for release validation. decisions are in `docs/adr/`.
