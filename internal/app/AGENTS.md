# Application code

- This package owns CLI commands, configuration, discovery, syncing, and macOS integration.
- `Run(args)` is the entry point used by the root `main.go`.
- Keep tests beside their implementation. The end-to-end test runs the real daemon against temporary Git repositories.
- From the repository root: `go build ./...`, `go vet ./...`, and `go test -race -count=1 ./...`.
