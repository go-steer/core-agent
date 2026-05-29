# `core-agent` skills bundle

Three [Claude Skills](https://docs.claude.com/en/docs/agent-skills/overview)–shaped bundles that teach an agent how to configure and embed `core-agent`. Self-applying: drop them into any `core-agent` instance's skills directory and the agent itself can walk a user (or itself) through the corresponding setup.

| Skill | What it teaches |
|---|---|
| [`cli-setup`](./cli-setup/) | Configuring `core-agent` for interactive use: provider, `AGENTS.md`, skills, permissions, MCP wiring |
| [`autonomous-setup`](./autonomous-setup/) | Configuring unattended runs (single agent + multi-agent teams): goal framing, budgets, headless permissions, parent/child decomposition |
| [`library-embedding`](./library-embedding/) | Embedding `core-agent` in your own Go binary: minimal embed, extension points, worked HTTP-served example |

Each bundle is a directory with `SKILL.md` (the runbook the agent reads) and `references/` (deeper content the agent fetches on demand).

## Installing

The bundles are project-portable. To make them available to your `core-agent` instance, copy the bundle into a skills directory the agent loads.

**Project-scoped install** (just this repo):

```bash
mkdir -p .agents/skills
cp -r /path/to/core-agent/SKILLS/cli-setup .agents/skills/
```

**User-global install** (every project for this operator):

```bash
mkdir -p ~/.core-agent/skills
cp -r /path/to/core-agent/SKILLS/cli-setup ~/.core-agent/skills/
```

`core-agent` (v2.1+) auto-discovers both `~/.core-agent/skills/` (user-global) and `.agents/skills/` (project-scoped) and merges them. On name collision, the project-scoped skill wins — handy for forking the generic bundle and shipping a project-specific variant under the same name.

## Why ship these as skills, not just docs

The site docs ([core-agent docs](https://github.com/go-steer/core-agent/tree/main/docs/site/content/docs)) cover the same material in reference form — schema, flags, patterns. The skills cover the *workflow* form: when the agent is invoked with "help me configure core-agent for my project" or "set up an autonomous monitor for me," the skill body is the runbook the agent walks through. Same content, different shape; pick the surface that matches your read intent.

The recursive use case ("agent uses the skill to help a user configure another agent") is the canonical motivation. It's how you'd onboard a team to `core-agent` without writing custom onboarding tooling — the existing agent IS the onboarding tooling.

## Adapting

These bundles are deliberately conservative — they teach `core-agent`'s defaults and the patterns from the [agent-design]({{< relref "agent-design/_index.md" >}}) docs section. If your organization has stricter standards (e.g., "every project must use anthropic-vertex with mode=ask"), fork the bundle and modify the body or references accordingly. The `description` field's triggers, the numbered runbook, and the `references/` router pattern all carry over to any project-specific shape.

## More

- [Skills format reference](https://github.com/go-steer/core-agent/blob/main/docs/site/content/docs/reference/skills.md) — the `SKILL.md` schema, discovery rules, permission gating
- [Skill design patterns](https://github.com/go-steer/core-agent/blob/main/docs/site/content/docs/agent-design/skills.md) — when to write a skill, how to write a description that triggers, body structure
- [Claude Skills spec](https://docs.claude.com/en/docs/agent-skills/overview) — the upstream format spec we mirror
