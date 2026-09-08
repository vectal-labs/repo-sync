# ADR 0007: Why Go

Status: accepted (David, 2026-09-08)

We're using Go because it's fast and easy to distribute. Easy distribution is a big reason: we ship one executable, so nobody on the team needs Go installed.

Go handles concurrent work well, which fits watching files and syncing multiple repos at once. Its standard library covers most of what we need, so we can keep dependencies minimal.

The built-in testing tools and race detector also help us catch bugs when multiple operations run at the same time.
