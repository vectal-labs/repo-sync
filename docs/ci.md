# CI and releases

Every code change runs formatting, vet, build, the full Go race/E2E suite, cask lifecycle tests, and release-config validation. Independent Git and daemon tests run in parallel; tests that change the process environment stay serial.

Markdown, `LICENSE`, and images/PDFs under `docs/` skip heavy checks. Files under `.agents/skills/repo-sync/` always run full checks because they are embedded in the binary. Pull requests compare against their merge base. Main pushes also compare against the latest fully tested main commit, so a cancelled code push cannot disappear behind a later docs-only push. CI policy tests always run.

A release tag reuses successful `CI` from a push to this repository's `main` only when its commit matches exactly and the Go test step actually passed. A successful docs-only run does not qualify. Missing history, failed API lookups, or no qualifying run among the latest 20 successful runs cause all checks to run. Publishing still waits for the test job to succeed.

New pushes cancel older CI for the same branch or PR. Tag runs have separate concurrency groups and never cancel an active release. This follows [GitHub's concurrency behavior](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency); reuse checks use its [workflow-runs API](https://docs.github.com/en/rest/actions/workflow-runs#list-workflow-runs-for-a-workflow).

Validate changes locally:

```sh
node --test scripts/ci-plan.test.cjs
go test -race -count=1 ./...
ruby scripts/test-cask.rb
goreleaser check --config .github/.goreleaser.yaml
```
