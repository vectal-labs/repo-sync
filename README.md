# repo-sync

your git repos, always in sync.

you edit. after 60 seconds of quiet, repo-sync commits, pulls, and pushes. it starts at login and runs in the background.

for notes, docs, config, and small team repos. macos only.

## install

```sh
brew install --cask vectal-labs/tap/repo-sync
repo-sync setup
```

`setup` finds your repos and installs the background service. you choose which repos to sync. nothing is preselected.

<details>
<summary>install with go</summary>

```sh
go install github.com/vectal-labs/repo-sync@latest
repo-sync setup
```

</details>

## usage

```sh
repo-sync add                # sync the repo you're in
repo-sync add ~/code/notes    # sync a repo by path
```

## safety

- secret filenames like `.env`, `*.pem`, and `.npmrc` are blocked by default. no content scanning.
- no force-pushes or automatic conflict resolution. on conflict, it aborts the rebase and retries later.
- only the remote's default branch syncs. feature branches stay untouched.

[behavior and configuration](docs/behavior.md) · [secret overrides](docs/behavior.md#secrets)

<details>
<summary>uninstall</summary>

```sh
launchctl bootout gui/$(id -u)/com.vectal-labs.repo-sync
rm ~/Library/LaunchAgents/com.vectal-labs.repo-sync.plist
brew uninstall repo-sync
```

</details>

[mit license](LICENSE)
