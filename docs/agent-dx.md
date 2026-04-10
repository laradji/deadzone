# Agent DX — working on Deadzone with Claude Code

This doc captures the conventions for working on Deadzone with Claude Code (or
any tool that respects the `.claude/` settings layout). It exists because two
recurring sources of session friction kept eating time:

- **Permission prompts for routine commands** — see [#22][issue-22]
- **Toolchain confusion** with the mise-pinned Go — see [#21][issue-21]

Issue #21 was addressed by the [`justfile`](../justfile) and the README
quick-start. This doc is the answer to issue #22.

## The three permission levers in Claude Code

When Claude Code runs commands on your behalf, three independent settings
control whether it prompts you first. They are easy to confuse:

| # | Lever                                      | Lives in                                           | Effect                                                                                              |
|---|--------------------------------------------|----------------------------------------------------|-----------------------------------------------------------------------------------------------------|
| 1 | `permissions.allow` / `permissions.deny`   | `.claude/settings.json` or `settings.local.json`   | Allowlist of pre-approved command patterns. Anything not on the list still triggers a prompt.       |
| 2 | `permissions.defaultMode: "bypassPermissions"` | Same files                                     | Sets the global session mode. `bypassPermissions` auto-approves *everything*. The "full permission" mode. |
| 3 | `--permission-mode bypassPermissions`      | CLI flag at launch (alias: `--dangerously-skip-permissions`) | Same as lever 2, but session-scoped, no config change.                                              |

If you think you're running in "full permission" mode but every new command
pattern still asks for approval, you almost certainly have lever 1 configured
and not lever 2.

Authoritative reference: [Claude Code settings docs][claude-settings].

## Deadzone's choice — committed allowlist baseline (Fix A)

Deadzone ships a committed [`.claude/settings.json`](../.claude/settings.json)
with a broad `permissions.allow` list covering the tools we use every day:

- `git`, `gh` — version control + GitHub operations
- `mise`, `just`, `go` — pinned toolchain and task runner (see [#21][issue-21])
- `podman` — local container ops (project policy: podman, never docker)
- `ls`, `pwd`, `which` — quick filesystem orientation
- `WebFetch` against the docs domains we read against most often
  (`github.com`, `pkg.go.dev`, `go.dev`, `modelcontextprotocol.io`,
  `turso.tech`, `docs.turso.tech`)

This is the "Fix A" approach from [#22][issue-22]: trade some occasional
prompts for unfamiliar command families against a much smaller daily-friction
footprint, and rely on the agent's own judgment for destructive operations
(`git push --force`, `rm -rf`, `git reset --hard`, ...) — those still want a
human-in-the-loop confirmation regardless of what the allowlist says.

We deliberately do **not** ship `defaultMode: "bypassPermissions"` in the
committed file. That's a per-developer choice — opt in via your local override
if you want lever 2.

## Per-developer overrides — `settings.local.json`

`.claude/settings.local.json` is gitignored. Use it for anything you don't
want to share:

- Personal hooks (e.g. emdash notification webhooks)
- Stricter or looser permission overrides
- `defaultMode: "bypassPermissions"` if you want lever 2

When both files define `permissions.allow`, Claude Code **concatenates and
de-duplicates** the arrays — your local entries layer on top of the committed
baseline rather than replacing it. Scalar fields like `defaultMode` follow
normal precedence: local overrides project. Full precedence order is
*managed → CLI → local → project → user* — see the [settings docs][claude-settings].

Example local-override snippet for full bypass on your machine only:

```json
{
  "permissions": {
    "defaultMode": "bypassPermissions"
  }
}
```

## Why CLAUDE.md isn't the place for this

The repo's `CLAUDE.md` was deliberately removed from version control (commit
[`f71a5e1`][commit-f71a5e1]) so each contributor can keep their own agent-only
context. Anything that should be shared across contributors lives in `docs/`
or the README instead, and that's where this lives too.

## Acceptance check

After pulling this change and restarting your Claude Code session in this
repo, you should see:

- No prompt for any `git`, `gh`, `mise`, `just`, `go`, or `podman` invocation
- Prompts only for genuinely new command families (`curl`, `aws`, `npm`, ...) —
  those still need an explicit one-off approval, by design

[issue-21]: https://github.com/laradji/deadzone/issues/21
[issue-22]: https://github.com/laradji/deadzone/issues/22
[claude-settings]: https://docs.claude.com/en/docs/claude-code/settings
[commit-f71a5e1]: https://github.com/laradji/deadzone/commit/f71a5e1
