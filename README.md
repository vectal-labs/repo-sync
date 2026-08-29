# repo-sync

keeps your git repos in sync without you thinking about it. macos only.

you edit files. after 60 seconds of quiet, repo-sync commits them, pulls what your teammates pushed, and pushes. it runs in the background from login and never asks you anything.

built for notes, docs, config, and small team repos that should always be current everywhere.

## install

```sh
brew install --cask vectal-labs/tap/repo-sync
repo-sync setup
```

or `go install github.com/vectal-labs/repo-sync@latest`, then `repo-sync setup`.

`setup` finds your repos, asks which ones to sync, and installs the background service. nothing is preselected.

## commands

```sh
repo-sync add                 # sync the repo you're in
repo-sync add ~/code/notes    # sync a repo by path
repo-sync allow .env          # let a secret-guarded file sync
```

## safety

- secret files like `.env`, `*.pem`, `.npmrc` are never pushed. by filename, no content scanning.
- it never force-pushes and never resolves conflicts for you. on a conflict it aborts and retries later.
- it only touches the remote's default branch. on a feature branch it leaves you alone.

full behavior, paths, and retry rules: [docs/behavior.md](docs/behavior.md).

## uninstall

```sh
launchctl bootout gui/$(id -u)/com.vectal-labs.repo-sync
rm ~/Library/LaunchAgents/com.vectal-labs.repo-sync.plist
brew uninstall repo-sync
```

## license

mit
