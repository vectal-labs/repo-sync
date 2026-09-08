# Agent skill

The official skill is in [`.agents/skills/repo-sync`](../.agents/skills/repo-sync/SKILL.md). It explains setup, adding and removing repos, configuration, verification, and troubleshooting. It includes a fallback for older binaries without `remove`.

Agents working in this checkout can discover it through `.agents/skills/` or the existing `.claude/skills` symlink.

To install it globally from this checkout:

```sh
mkdir -p "$HOME/.agents/skills"
cp -R .agents/skills/repo-sync "$HOME/.agents/skills/"
```

If that destination already contains a customized skill, compare it before replacing files. Start a new agent session after installation. Agents with a different skill directory need the same `repo-sync` folder placed there.

The skill includes its own references and works outside the source checkout. Installing it does not install or update the CLI. Keep the repo and global copies aligned when updating it.
