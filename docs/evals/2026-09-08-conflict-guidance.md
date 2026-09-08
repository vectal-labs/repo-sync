# Conflict alerts and guided repair

## Scope

Implement the user-approved immediate macOS alert, persistent conflict status, and read-only Git conflict inspection/repair guide. No automatic resolution, force-push, service installation, or release.

## Evidence and assumptions

- Git conflicts require explicit human judgment; retries alone cannot resolve competing edits. Two completed deep researches and two deep scrapes compared GitJournal/git-auto-sync, Nextcloud, Syncthing, Obsidian Git and alternative models. Primary sources: [GitJournal](https://github.com/GitJournal/git-auto-sync#merge-conflicts), [Nextcloud](https://docs.nextcloud.com/server/34/user_manual/en/desktop/conflicts.html), [Syncthing](https://docs.syncthing.net/users/syncing.html#conflicting-changes), [Obsidian Git source](https://github.com/Vinzent03/obsidian-git/blob/master/src/main.ts).
- One additional research covered real Git edge cases. [Git documentation](https://git-scm.com/docs/git-rebase/2.37.2) confirms the need to start a fresh rebase after abort, resolve each stopped commit, and continue deliberately.
- The daemon must read stages from its owned index before aborting. Exact blobs are read afterward with replacement objects disabled. Stage 2 includes earlier replayed commits; stage 3 is the stopped local commit. Real Git fixtures verified these assumptions.

## Before and after

The initial `TestEndToEndConflictIsVisibleImmediately` failed with state `waiting` and an empty notification list. It now passes with state `conflict`, affected paths, and one immediate notification.

18 real-Git scenarios in `TestConflictCases` passed, including race detection:

1. Edit/edit.
2. Add/add.
3. Local deletion and remote modification.
4. Local modification and remote deletion.
5. Binary content with non-UTF-8 bytes.
6. Empty local file and remote deletion.
7. Empty remote file and local deletion.
8. Spaces in names.
9. Tabs in names.
10. Newlines in names.
11. Unicode names.
12. Git pathspec characters in names.
13. Local rename and remote deletion.
14. Remote rename and local deletion.
15. Rename/rename.
16. Executable file mode.
17. Multiple local commits, with the conflict after an earlier replay.
18. Injected abort failure, preserved unresolved index, and manual recovery.

Each successful abort checks original local and remote tips, file versions, a clean index/worktree, and removal of the owned rebase state. Additional tests cover replacement blobs, oversized snapshots, submodule pointers, missing/empty versions, Markdown/control characters, private permissions, cache failure/corruption, and preventing report storage inside any synced repository.

## End-to-end recovery

`TestEndToEndConflictRestartGuideAndManualRepair` starts the real daemon, restarts it with the saved incident, runs the built `conflicts` executable, performs a manual rebase, and verifies both remote recovery and continued syncing of another repo. Five consecutive runs passed. The test waits for Git's actual CONFLICT result before treating a concurrent operation as the user's manual rebase; a generic operation-in-progress error is not sufficient.

Repeat/restart deduplication, notification retry after delivery errors, status exit codes, new conflicts after recovery, and uninstall cleanup are separately covered. The existing two-clone end-to-end test now expects both the secret alert and the new conflict alert, while retaining its preservation checks.

## Agent skill evaluation

Fresh BB threads read the baseline or revised skill plus its operations reference. Transcripts were inspected; they ran only the requested file reads.

- Baseline inspection: preserved authorization boundaries, but had no conflict report command or saved-file snapshot guidance.
- Revised inspection: checks installed help/config, uses `conflicts`, separates inspection from repair, and treats saved versions as historical context.
- Revised repair with uncommitted work and newer remote edits: preserves current work, fetches fresh history, starts a new rebase after the automatic abort, resolves each stop, and verifies a full sync including push.

The revised prompts passed their behavioral criteria. Two initial evaluation threads failed BB environment provisioning; replacements used the existing environment and completed. The later `--literal-pathspecs` documentation refinement matches the generated guide and the real unusual-filename tests.

## Notification verification limit

Expected default body: `Conflict in team-docs. Local changes are saved. Run repo-sync conflicts for help.`

The real sandboxed AppleScript invocation returned exit 0 but reported a Notification Center connection failure on stderr. A regression test now ensures this is treated as delivery failure, allowing a later retry. The host invocation was rejected by automatic approval review because its usage limit was exhausted. Computer Use access to Notification Center was also unavailable. Banner display remains unverified; notification settings may hide it. No OS settings were changed.

## Final validation

- `go test -race -count=1 ./...` passed (95.705 seconds).
- `go build ./...`, `go vet ./...`, and `git diff --check` passed.
- Targeted conflict checks and five consecutive end-to-end recovery runs passed.

No production data or remote user repositories were changed. The changes remain in the isolated worktree; no installation or push was performed. Live macOS banner display is the only outstanding validation, for the reason above.
