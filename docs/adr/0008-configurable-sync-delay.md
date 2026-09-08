# ADR 0008: Configurable sync delay

Status: accepted (David, 2026-09-08)

60 seconds after the last edit feels like a good default to me. It gives changes time to settle without making the team wait too long.

The delay is easy to change: edit `idle_debounce` in `~/Library/Application Support/repo-sync/config.json`, for example to `"30s"`. Then rerun `repo-sync setup` or restart the service. The setting applies to all synced repos on that Mac.
