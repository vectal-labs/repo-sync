# how repo-sync behaves

## setup

- before writing the config or installing the service, setup verifies that background Git can fetch every configured repository without a terminal prompt.
- for GitHub HTTPS repositories with missing credentials, setup uses GitHub CLI to configure Git. if needed, it starts the browser login and retries the checks.
- stale global Git TLS-version pins are ignored. if authentication, TLS, or repository access still fails, setup stops before changing the config or service and reports the failing repositories.

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
- `repo-sync allow <file>` inside the repo overrides the guard for that file. the list is in `secrets.go`.

## conflicts and failures

- on a rebase conflict it runs `git rebase --abort`, keeps your local commits, and retries later. it never force-pushes and never resolves conflicts.
- failures retry with backoff: 1 minute, doubling, up to 30 minutes. success resets it.
- one failing repo never blocks another.
- being offline (dns, connection, reset, timeout) is not an incident. it never notifies; it just retries.
- any other failure notifies once it has lasted 10 minutes, then silence until it recovers. repos that cross that line together share one notification. short blips stay silent.
- if a file vanishes while staging, or the worktree changes right before the rebase, the cycle is skipped and retried. nothing is committed and no notification is sent.
- there is no paused state and no `resume` command. if something cannot be done safely it logs, waits, and tries again.

## files

- config: `~/Library/Application Support/repo-sync/config.json`. one json file. edit it by hand if you like, then run `repo-sync setup` again or restart the service.
- logs: `~/Library/Logs/repo-sync/`.
- service: `~/Library/LaunchAgents/com.vectal-labs.repo-sync.plist`. it starts at login and restarts on crash.

## development

```sh
go build ./...
go vet ./...
go test -race ./...
```

the tests spin up real git remotes and clones in temp folders. the end to end test runs the actual daemon. decisions are in `docs/adr/`.
