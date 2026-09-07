# Release checks

- `ruby scripts/test-cask.rb` tests the lifecycle hooks in `.github/.goreleaser.yaml` on macOS.
- Run real plist edits only in temporary folders. Stub Homebrew, launchctl, and xattr so tests never change the logged-in service or installed programs.
- CI also runs `goreleaser check` to validate the release config schema.
