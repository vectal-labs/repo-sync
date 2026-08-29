# ADR 0003: Only the remote default branch is synced

## Context

repo-sync auto-commits and pushes. When the user has a feature branch or a detached HEAD checked out, the daemon has to decide what to do with local edits. Research across auto-sync tools (Obsidian Git, git-sync, git-auto-sync, Auto-Commit-Daemon, beads, Magit WIP, GitButler, Jujutsu) surfaced five approaches: skip and wait, sync the current branch, opt-in per branch, a hidden backup ref, and a hidden second checkout of the default branch. Auto-stash-and-switch was ruled out outright; it is the most common source of lost work.

## Decision

repo-sync syncs only the remote default branch, read from `refs/remotes/origin/HEAD` with `main` as the fallback.

- On a feature branch or detached HEAD the repository is left untouched and the cycle is skipped. It retries on the next cycle.
- repo-sync never switches branches, never syncs feature branches, never stashes, and never creates hidden worktrees.
- If a repository stays off its default branch for 24 hours, the user is notified once.
- In-progress merge, rebase, cherry-pick, revert, or bisect operations also cause a temporary skip.

Reasons: auto-committing on a feature branch pushes half-done work into pull requests and forces force-pushes after rebases. Hidden worktrees share `.git` state and have caused silent data loss in other tools. Skip-and-wait is the behavior the safest tools converge on: protect the checkout, never publish behind the user's back.

## Consequences

Work on feature branches is not backed up by repo-sync. The user sees one notification after a day, not a stream of warnings. If feature-branch coverage is wanted later, the natural next step is explicit opt-in per branch (push to that branch only, fast-forward only), recorded in a new ADR. A hidden backup ref is a possible "safety net" feature but is a different product.
