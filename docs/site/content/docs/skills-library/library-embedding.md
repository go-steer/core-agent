---
title: library-embedding skill
weight: 3
---

The `library-embedding` skill walks a Go developer through embedding `core-agent` in their own binary. Bundled in [`SKILLS/library-embedding/`](https://github.com/go-steer/core-agent/tree/main/SKILLS/library-embedding).

## What it covers

A 5-step runbook:

1. Confirm the use case (CLI use vs library use)
2. Show the minimal embed (20-line "hello world")
3. Identify which extension point the user needs
4. Show a full worked example for non-trivial embeddings
5. Discuss long-term maintenance (`go.mod` pinning, breaking-change policy)

The extension points covered include `Prompter`, custom tools, custom `Provider`, custom `session.Service`, `Compactor` / `Checkpointer`, `BackgroundAgentManager`, and `RemoteAgentSpawner`.

## Triggers

The skill matches:

- "how do I embed core-agent"
- "use core-agent as a library"
- "build my own coding assistant on core-agent"
- "agent.New"
- "custom prompter" / "custom tool" / "custom provider"
- "HTTP-served agent"
- "integrate core-agent into [Go project]"

Plus any phrasing implying use of the Go API rather than the bundled CLI.

## References

Three reference files; the agent fetches based on the user's needs:

- **[`minimal-embed.md`](https://github.com/go-steer/core-agent/blob/main/SKILLS/library-embedding/references/minimal-embed.md)** — the 20-line "hello world" + variations: multi-turn, durable sessions, context management, model selection, event-logging patterns, concurrency notes, `go.mod` pinning. Read first for any embedding question; it's the foundation.
- **[`extension-points.md`](https://github.com/go-steer/core-agent/blob/main/SKILLS/library-embedding/references/extension-points.md)** — the seven customization surfaces (`Prompter`, tools, `Provider`, `session.Service`, `Compactor`/`Checkpointer`, `BackgroundAgentManager`, `RemoteAgentSpawner`) with contract + minimal example for each. Read when narrowing in on a specific extension.
- **[`http-served-agent.md`](https://github.com/go-steer/core-agent/blob/main/SKILLS/library-embedding/references/http-served-agent.md)** — full worked HTTP-served agent: agent pool, web prompter, SSE streaming, durable sessions. ~150 lines of working Go code. Read when building a web service.

## When to invoke vs read the docs

| You want | Use |
|---|---|
| Agent to walk you through embedding | Install + invoke `library-embedding` |
| Reference for every option function + public type | [Library API]({{< relref "library/api.md" >}}) |
| Narrative tour of extension points | [Library guide]({{< relref "library/guide.md" >}}) |
| Quickstart for the first 15 minutes | [Library quickstart]({{< relref "library/_index.md" >}}) (in progress) |

The skill IS the docs in workflow form. Same content, different surface.

## Installing

```bash
cp -r /path/to/core-agent/SKILLS/library-embedding .agents/skills/
```

See [Skills library → Installing]({{< relref "skills-library/_index.md" >}}) for global install options.

## Adapting

Common adaptations:

**Org-specific extensions.** If your org has a standard custom `Prompter` (e.g., a corporate-Slack approval flow), document it in a `references/` addition:

```bash
cat > .agents/skills/library-embedding/references/acme-slack-prompter.md <<'EOF'
# Acme Slack-button prompter

For Acme web services, use github.com/acme/internal-mcp/prompters.SlackButton
as the Prompter implementation. It posts approval requests to #ops-approvals
and routes the user's button click back to the pending request.
[... full implementation example ...]
EOF
```

Then update `SKILL.md`'s extension-points step to mention "for Acme web services, see references/acme-slack-prompter.md."

**Project-specific scaffolding.** Replace the generic project layout with your team's `cookiecutter`-style template:

```markdown
For Acme web services, scaffold the project from our template:
gh repo create my-agent --template acme/agent-service-template
Then walk steps 2-5 against that scaffold.
```

## Where to go next

- **[Library quickstart]({{< relref "library/_index.md" >}})** — 15-minute hello-world (in progress)
- **[Library guide]({{< relref "library/guide.md" >}})** — narrative tour of the extension points
- **[Library API]({{< relref "library/api.md" >}})** — exhaustive type + option reference
- **[Agent design]({{< relref "agent-design/_index.md" >}})** — prescriptive patterns that apply equally to embedded use
