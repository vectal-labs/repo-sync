# repo-sync agent skill evaluation

## Skill checks

Fresh BB threads ran read-only scenarios. Transcripts were checked for skill loading, command choice, and unintended actions.

- **Start syncing an existing clone:** `thr_zefbu9f3eg` loaded the skill and operations reference. It selected the installed config, included staged changes in the explanation, and distinguished saved registration from verified syncing after a restart failure. Passed.
- **Remove the last, moved repo with an older CLI:** `thr_ivedsqdrnf` loaded both files. It avoided unsupported commands, waited for shutdown, preserved config symlinks and other settings, and left the empty service unloaded. Passed.
- **Pull once without automatic syncing:** `thr_rjq3qdzq5h` loaded no skills and returned `git pull --ff-only`. Passed.
- **No-skill legacy baseline:** `thr_tdnkbiwqu8` found the core stop-and-edit procedure from the code. It additionally proposed persistent launchd disabling and omitted the skill's explicit atomic-write/concurrent-edit checks. Both legacy runs used 7 terminal calls, so this evaluation does not establish a speed improvement.

The 3 routing/plan checks passed. They do not prove a fresh agent can execute an entire installation unattended. No evaluation changed a real registration or repository.

The main file is 95 lines. YAML frontmatter was parsed with Ruby's safe YAML loader. Its name matches the folder. Bundled references exist and remain usable after copying the package outside the checkout. The global copy was verified byte-for-byte.

## CLI validation

- Before implementation, the real CLI rejected `remove` as an unknown command.
- The empty-daemon end-to-end reproduction failed because no readiness heartbeat was published. It passes after the fix.
- `go build ./...` and `go vet ./...` passed.
- `go test -race -count=1 ./...` passed after final review fixes.
- `REPO_SYNC_LAUNCHD_TEST=1 go test -race ./internal/app -run '^TestLaunchdRemoveKeepsOtherReposSyncing$' -count=1` passed.

The launchd test uses a unique temporary service label, 2 local clones, and local bare remotes. It verifies that removing one repo preserves its staged and unstaged work while the other keeps syncing, then removes the last repo and verifies an empty healthy service. It also exercises a config symlink. The temporary jobs are stopped during cleanup.

Focused tests cover missing paths, current/subdirectories, relative paths, config aliases, preserved Git history and settings, service failures, concurrent config changes during shutdown, and duplicate paths referring to one clone.

Review caught and fixed lost concurrent config edits, ambiguous duplicate aliases, and missing final verification in legacy removal instructions. The new `remove` command is source code until released; the installed skill checks the binary's help and includes a legacy fallback.
