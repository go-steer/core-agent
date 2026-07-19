---
title: Skills
---


`core-agent` loads `SKILL.md` bundles from `.agents/skills/<name>/`. The schema mirrors [Anthropic's published skills format](https://docs.claude.com/en/docs/agent-skills/overview) so existing skill bundles drop in directly.

---

## Directory layout

```
.agents/
└── skills/
    ├── echo/
    │   └── SKILL.md
    ├── jira-triage/
    │   ├── SKILL.md
    │   └── examples/
    │       └── ticket.md
    └── data-export/
        ├── SKILL.md
        └── helpers/
            └── export.py
```

Each subdirectory of `.agents/skills/` is one skill. A skill must contain a `SKILL.md` at its root; the directory name is the skill's identifier (and what the agent invokes by). Other files in the directory are referenced from `SKILL.md` and loaded on demand.

---

## `SKILL.md` format

Standard YAML frontmatter + a markdown body. The frontmatter declares the skill's identity and surface; the body is the prompt content the agent reads when it invokes the skill.

```markdown
---
name: jira-triage
description: Triage a Jira ticket — read it, classify, recommend next action.
---

When asked to triage a Jira ticket:

1. Read the ticket via the `jira_get_issue` tool.
2. Classify by severity using the rubric in `examples/severity.md`.
3. Suggest the next action: assign, request more info, or close.

Always include the ticket key (e.g. `PROJ-123`) in your response.
```

### Required frontmatter

| Field | Notes |
|---|---|
| `name` | Skill identifier. Should match the directory name. |
| `description` | One-line summary the agent sees in its tool list. Used by the model to decide when to invoke. |

### Optional frontmatter

The Anthropic SKILL.md spec allows additional fields (`allowed_tools`, `version`, etc.). `core-agent` parses the `name` and `description` for its own metadata; the rest is preserved verbatim and passed through to the underlying ADK `skilltoolset`.

### Body conventions

- Write in second person, addressed to the agent ("When asked X, do Y").
- Reference sibling files with relative paths — they're loaded lazily when the skill is invoked, so a large bundle doesn't blow up cold-start.
- Keep frontmatter terse; details belong in the body.

---

## Discovery

At startup, `core-agent`:

1. Stats `<agentsDir>/skills/`. Missing directory → no-op (most projects don't use skills).
2. Lists frontmatters via ADK's `skill.NewFileSystemSource`.
3. If at least one valid frontmatter is found, builds a `skilltoolset` and registers it as a single ADK Toolset.
4. If a [permission gate](/concepts/permissions/) is configured, wraps the toolset with the gate under the `skill` namespace.

The `Skills` returned by `skills.Load` carries:

- `Toolset` — pass to `agent.WithToolsets(...)`.
- `Infos []Info` — name + description for each discovered skill, suitable for rendering a `/skills` view in your host.

```go
loaded, err := skills.Load(ctx, agentsDir, gate)
if err != nil { ... }
if !loaded.Empty() {
    opts = append(opts, agent.WithToolsets([]adktool.Toolset{loaded.Toolset}))
}
```

---

## Permission gating

When a gate is supplied to `skills.Load`, every skill invocation goes through it under the `skill` namespace. Allowlist patterns look like:

```json
{
  "permissions": {
    "allow": ["skill:jira-triage", "skill:data-export"]
  }
}
```

The detail string surfaced in prompts is `<skill_name> <json-args>` (truncated). Skip gating entirely with `permissions.mode: yolo`.

---

## Empty bundles

A `.agents/skills/` directory that exists but contains no valid `SKILL.md` bundles is treated as a no-op (returns an empty `Skills`). No error, no toolset registered. This means you can scaffold the directory in advance and add bundles incrementally.

---

## What's not covered today

- **Hot reload** — adding a new skill requires restarting the process.
- **Per-skill permission scopes** — gating is `skill:<name>` granular; sub-tool gating within a skill bundle isn't exposed.
- **Versioning** — `SKILL.md` may carry a `version` field, but `core-agent` doesn't currently surface or enforce it. Use the bundle's directory layout (e.g. one directory per major version) if you need version pinning.

These could land in a later milestone if downstream consumers ask. See the [Roadmap](https://github.com/go-steer/core-agent#roadmap).
