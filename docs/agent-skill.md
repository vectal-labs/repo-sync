# Agent skill

The official skill explains setup, adding and removing repos, configuration, verification, and troubleshooting. Its source is [`.agents/skills/repo-sync`](../.agents/skills/repo-sync/SKILL.md). Each binary includes that skill and its references, so installation needs no source checkout or download.

Agents working in this checkout can discover it through `.agents/skills/` or the existing `.claude/skills` symlink.

Setup offers the optional installation and remembers your answer. You can install it later:

```sh
repo-sync skill install
repo-sync skill status
```

The installer uses the shared `~/.agents/skills/repo-sync` folder, discovered by Codex, Pi, and Cursor. It also installs for detected Claude Code and Hermes installations:

- Claude Code: `${CLAUDE_CONFIG_DIR:-~/.claude}/skills/repo-sync`.
- Hermes: `${HERMES_HOME:-~/.hermes}/skills/repo-sync`.

Explicit environment overrides are respected. The installer does not populate every agent profile. Check `skill status` for the actual paths and any preserved folders. Start a new agent session after installation. Local installation does not configure cloud agents.

Homebrew upgrades refresh previously managed, unchanged copies automatically. Go users should rerun setup after upgrading the binary, or refresh the skill separately:

```sh
repo-sync skill refresh
```

Refresh installs nothing when no managed installation exists. Existing unowned folders and customized managed folders are preserved, including their extra files. The CLI reports conflicts instead of replacing personal instructions.

To remove managed skill copies while keeping repo-sync:

```sh
repo-sync skill uninstall
```

Full `repo-sync uninstall` and direct Homebrew uninstall also remove unchanged managed copies. Customized folders remain. Homebrew upgrades and reinstalls preserve the installation for refresh by the new binary.

Installing this skill does not upgrade the CLI. Check `repo-sync help` before using its commands; older binaries may lack `skill` or `remove`. The skill includes guidance for older releases.
