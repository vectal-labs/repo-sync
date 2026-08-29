# ADR 0002: Secret guard by filename only

## Context

repo-sync commits every non-ignored change automatically. A secret that lands in a synced folder would be pushed within a minute. The guard has to be reliable, fast, and predictable, and it must never stop the service.

## Decision

Secret files are blocked by filename and path patterns only. There is no content scanning and no bundled scanner such as Gitleaks.

- The default list covers env files, private keys and key stores, tool credential files, cloud tool configs, infrastructure state, and generic `secrets.*` / `credentials.*` names. See `secrets.go`.
- Templates such as `.env.example` are allowed.
- Broad patterns like `*secret*` or `*token*` are deliberately excluded to avoid false alarms.
- Matching is case-insensitive.
- A blocked file is left out of the commit. Everything else still syncs. The user is notified once per file.
- `repo-sync allow <file>` overrides the guard per file and always wins.

## Consequences

The guard is predictable and costs nothing per sync. It catches the common cases, not every case. A secret stored under an ordinary filename is not caught; keeping such files out of a synced repo remains the job of `.gitignore`. Adding content scanning later would be a new ADR.
