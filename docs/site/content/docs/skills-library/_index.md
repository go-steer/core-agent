---
title: Skills library
weight: 7
---

`core-agent` ships three Claude-Skills–shaped bundles in [`SKILLS/`](https://github.com/go-steer/core-agent/tree/main/SKILLS) at the repo root. They teach an agent how to configure and embed `core-agent` itself — the meta-use case is that `core-agent` can use them to help a user set up another `core-agent`.

## Why ship these as skills

The reference docs (the [Reference]({{< relref "reference/_index.md" >}}) section) and the prescriptive guides (the [Agent design]({{< relref "agent-design/_index.md" >}}) section) cover the same material in static form. The skills cover the *workflow* form: when an agent is invoked with "help me set up `core-agent` for my project" or "configure an autonomous monitor for me," the skill body is the runbook the agent walks through.

Same material, different surface. Pick what matches your read intent:

- **Reading first time, want depth?** → [Agent design]({{< relref "agent-design/_index.md" >}}) (prescriptive) + [Reference]({{< relref "reference/_index.md" >}}) (schemas)
- **Already running an agent, want it to walk you through configuration?** → install the skill below, ask the agent for help

## The three bundles

### [`cli-setup`]({{< relref "skills-library/cli-setup.md" >}})

Walks a user through configuring `core-agent` for interactive (TUI / REPL) use. Triggers on phrases like "set up core-agent", "help me configure core-agent for my repo", "what should my AGENTS.md look like". Walks the four customization layers (provider → AGENTS.md → skills → tools) plus permission posture.

### [`autonomous-setup`]({{< relref "skills-library/autonomous-setup.md" >}})

Walks a user through configuring an unattended `core-agent` — single-agent monitor or multi-agent team. Triggers on "set up an autonomous agent", "run core-agent unattended", "build a multi-agent team". Includes patterns for single-agent monitors, multi-agent decomposition (parent + specialists), budget tuning, crash-resume.

### [`library-embedding`]({{< relref "skills-library/library-embedding.md" >}})

Walks a Go developer through embedding `core-agent` in their own binary. Triggers on "how do I embed core-agent", "use core-agent as a library", "custom prompter", "HTTP-served agent". Covers the minimal embed, the seven extension points, and a full HTTP-served agent worked example.

## Installing

The bundles are portable. To make them available to your `core-agent` instance, copy the bundle into a skills directory the agent loads.

**Project-scoped install** (just this repo):

```bash
mkdir -p .agents/skills
cp -r /path/to/core-agent/SKILLS/cli-setup .agents/skills/
```

After the copy, `core-agent` auto-discovers the skill on next launch. Verify with `/skills` in the TUI.

**User-global install** (every project for this operator):

```bash
mkdir -p ~/.core-agent/skills
cp -r /path/to/core-agent/SKILLS/cli-setup ~/.core-agent/skills/
```

> **v2.0 caveat:** `core-agent` v2.0 only auto-discovers project-scoped skills from `.agents/skills/`. User-global discovery (`~/.core-agent/skills/`) lands in v2.1 — until then the user-global install works only if your `--agents-dir` points at `~/.core-agent`.

## Using a skill once installed

Trigger the skill by phrasing your request to match its description:

```text
$ core-agent
> help me set up core-agent for my repo
[agent invokes cli-setup skill, walks the four layers, writes config files as you go]
```

The agent reads the skill's `SKILL.md`, follows the numbered procedure, and fetches `references/*.md` files on demand for the specific topic the user asks about.

To inspect what's loaded:

```text
> /skills
[lists all loaded skills with their descriptions]
```

To verify a skill triggered (or diagnose why it didn't):

```text
> /btw why didn't you invoke the cli-setup skill for the prior turn?
[the model often explains: "the request didn't match the description's phrasing" — and that's your signal to refine the description if it's your own skill]
```

## Adapting

These bundles teach `core-agent`'s defaults and the patterns from the [Agent design]({{< relref "agent-design/_index.md" >}}) docs. If your organization has stricter standards (e.g., "every project must use anthropic-vertex with mode=ask"), fork the bundle and modify the body or references accordingly. The skill format is just markdown + YAML frontmatter; everything is editable.

For writing your own skills from scratch (not derived from these bundles), see:

- [Agent design → Skills]({{< relref "agent-design/skills.md" >}}) — design patterns
- [Reference → Skills]({{< relref "reference/skills.md" >}}) — `SKILL.md` schema, discovery, permission gating
- [Claude Skills spec](https://docs.claude.com/en/docs/agent-skills/overview) — upstream format spec we mirror

## Roadmap

- **v2.1: `~/.core-agent/skills/` auto-discovery.** Today the user-global install requires pointing `--agents-dir` at `~/.core-agent`. v2.1 adds automatic discovery so a bare `cp -r SKILLS/* ~/.core-agent/skills/` makes the bundles available across every project.
- **v2.1+: First-run bundle install.** A `core-agent --install-skills` (or auto-install on first run if `~/.core-agent/skills/` is empty) so operators don't have to know about the `cp` step at all.
- **Future: more bundles.** Specific role-shaped skills (code-reviewer-setup, devops-monitor-setup, etc.) once the three above prove out.
