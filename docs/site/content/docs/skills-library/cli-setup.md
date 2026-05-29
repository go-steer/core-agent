---
title: cli-setup skill
weight: 1
---

The `cli-setup` skill walks a user through configuring `core-agent` for interactive use. It's bundled in [`SKILLS/cli-setup/`](https://github.com/go-steer/core-agent/tree/main/SKILLS/cli-setup) at the repo root.

## What it covers

The four customization layers, in order:

1. **Provider + model** — `.agents/config.json`
2. **Personality** — `AGENTS.md`
3. **Skills** — `.agents/skills/<name>/SKILL.md`
4. **Tools** — built-ins + MCP servers

Plus permission posture (`ask` / `allow` / `yolo` modes, allowlist patterns) and the verify-it-works step.

## Triggers

The skill's description lists the verbatim phrasings the agent will match. The most common:

- "help me set up core-agent"
- "configure core-agent for my repo"
- "what should my AGENTS.md look like"
- "what permissions should I use"
- "how do I add a skill"

Any phrasing implying first-time configuration or extending an existing setup.

## References

The skill body is short (a runbook). It fetches deeper content on demand from three reference files:

- **[`agents-md-patterns.md`](https://github.com/go-steer/core-agent/blob/main/SKILLS/cli-setup/references/agents-md-patterns.md)** — `AGENTS.md` authoring: the three-layer instruction stack, minimum viable shape, patterns that work, anti-patterns, model-specific quirks (Pro vs Flash), iteration approach.
- **[`skills-and-mcp.md`](https://github.com/go-steer/core-agent/blob/main/SKILLS/cli-setup/references/skills-and-mcp.md)** — extending the tool surface: when to write a skill vs an AGENTS.md rule, SKILL.md schema, description-as-trigger pattern, MCP server wiring + namespacing.
- **[`permission-posture.md`](https://github.com/go-steer/core-agent/blob/main/SKILLS/cli-setup/references/permission-posture.md)** — the three modes, pattern grammar, in-session "always allow" persistence, bash denylist, path-scope safety floor, common configurations.

## When to invoke vs read the docs

| You want | Use |
|---|---|
| The agent to walk you through configuration interactively | Install + invoke the `cli-setup` skill |
| To read the patterns yourself first | [Agent design]({{< relref "/docs/agent-design/_index.md" >}}) (prescriptive) + [Reference]({{< relref "/docs/reference/_index.md" >}}) (schemas) |
| The 15-minute first-time walkthrough | [Interactive quickstart]({{< relref "/docs/cli/interactive/quickstart.md" >}}) |

The skill IS the docs in workflow form. Pick the surface that matches your read intent.

## Installing

See [Skills library → Installing]({{< relref "/docs/skills-library/_index.md" >}}).

Quick version:

```bash
cp -r /path/to/core-agent/SKILLS/cli-setup .agents/skills/
```

Verify with `/skills` in the TUI.

## Adapting

The skill teaches `core-agent`'s defaults. If your organization has a stricter standard (e.g., "every project must use anthropic-vertex with mode=ask"), fork the bundle:

```bash
cp -r .agents/skills/cli-setup .agents/skills/cli-setup-acme
# Edit SKILL.md to encode your org's defaults
```

The agent will now invoke `cli-setup-acme` instead of the generic version (assuming the description matches the same triggers — keep the trigger phrasings or extend them).

## Where to go next

- **[Interactive quickstart]({{< relref "/docs/cli/interactive/quickstart.md" >}})** — the static-docs version of the same walkthrough
- **[System instructions]({{< relref "/docs/agent-design/system-instructions.md" >}})** — the prescriptive content the skill's `agents-md-patterns.md` reference is built from
- **[Reference → Skills]({{< relref "/docs/reference/skills.md" >}})** — `SKILL.md` schema, discovery, allow/deny lists
