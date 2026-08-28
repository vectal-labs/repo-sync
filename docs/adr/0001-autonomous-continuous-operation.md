# ADR 0001: Autonomous continuous operation

## Context

repo-sync exists to keep repositories synchronized without ongoing user attention. Pausing for staged files, suspected secrets, transient failures, or recoverable Git states defeats that purpose.

## Decision

After setup, repo-sync runs without interactive prompts or manual recovery commands.

- Staged changes are included in normal syncs.
- Common secret files are excluded from automatic commits. Other safe changes continue syncing.
- Failures use automatic retries with backoff.
- A failure in one repository never blocks another repository.
- The service never enters a paused state and has no `resume` workflow.
- Unsafe Git operations are aborted. repo-sync never force-pushes or auto-resolves conflicts. It keeps running and retries later.

## Consequences

Normal operation requires no user attention. Individual repositories may temporarily remain out of sync when Git cannot resolve a state safely, but the service and all other repositories continue running. Status and errors must be observable without becoming blocking prompts.
