# repo-sync

your git repos, always in sync.

you edit. after 60 seconds of quiet, repo-sync commits, pulls, and pushes. it starts at login and runs in the background.

for notes, docs, config, and small team repos. macos only.

## install

```sh
brew install --cask vectal-labs/tap/repo-sync && repo-sync setup
```

requires [homebrew](https://brew.sh). git and github cli are installed for you.

`setup` finds your repos, checks git access, and starts the background service. you choose which repos to sync or enter a path. nothing is preselected. it explains automatic commits and verifies the service before finishing.

<details>
<summary>install with go</summary>

requires go, git, and the xcode command line tools. github cli (`gh`) is needed only if setup must configure github https login.

```sh
go install github.com/vectal-labs/repo-sync@latest
"$(go env GOPATH)/bin/repo-sync" setup
```

if you set `GOBIN`, use that directory instead. add the binary directory to your `PATH` to use `repo-sync` directly.

</details>

## usage

```sh
repo-sync add                # sync the repo you're in
repo-sync add ~/code/notes    # sync a repo by path
repo-sync status             # check the service and repositories
```

## safety

- secret filenames like `.env`, `*.pem`, and `.npmrc` are blocked by default. no content scanning.
- no force-pushes or automatic conflict resolution. on conflict, it aborts the rebase and retries later.
- only the remote's default branch syncs. feature branches stay untouched.

[behavior and configuration](docs/behavior.md) · [secret overrides](docs/behavior.md#secrets)

<details>
<summary>uninstall</summary>

```sh
repo-sync uninstall
```

asks before removing the service, settings, logs, cache, and installed binary. repositories and shared git credentials are preserved. use `--yes` to skip confirmation, or `--keep-binary` to keep the program.

for homebrew, `brew uninstall --cask --zap repo-sync` also removes the standard settings, logs, and cache. plain `brew uninstall` stops the service and keeps those files.

</details>

[mit license](LICENSE)
