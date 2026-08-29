# ADR 0004: Distribute through a Homebrew cask

## Context

repo-sync ships as a prebuilt Go binary from GoReleaser. The first release used a Homebrew formula in the `vectal-labs/tap` tap. Installing it failed: a formula without a prebuilt bottle makes Homebrew assume it must compile, so it demands a current Xcode. GoReleaser has also deprecated its `brews` publisher in favor of casks for exactly this reason.

Options considered:

1. Cask in our tap, with a post-install hook that clears the quarantine flag.
2. Sign and notarize with an Apple Developer ID and keep the cask.
3. Formula in our tap with bottles built by CI.
4. Submit to homebrew-core, where Homebrew's CI builds bottles.
5. Skip Homebrew: `go install` or a curl script.

## Decision

Ship as a cask in `vectal-labs/homebrew-tap` (option 1). GoReleaser publishes `Casks/repo-sync.rb` on every tag. The cask strips the quarantine flag after install because the binary is not notarized. `go install` stays as the fallback.

## Consequences

`brew install --cask vectal-labs/tap/repo-sync` works with no Xcode requirement. The quarantine workaround is a known hack: Homebrew removed its own `--no-quarantine` bypass in late 2025 and GoReleaser warns Apple may block the post-install trick in a future macOS. Planned path: sign and notarize with an Apple Developer account (option 2) when justified, and submit to homebrew-core (option 4) once the project has traction. Either change gets a new ADR.
