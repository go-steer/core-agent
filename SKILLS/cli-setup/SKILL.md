---
name: cli-setup
description: Walk a user through configuring core-agent for interactive (TUI / REPL) use against their project. Use when the user asks to "set up core-agent", "configure core-agent for my repo", "help me get started with core-agent", "what should my AGENTS.md look like", "what permissions should I use", "how do I add a skill", or asks any question implying they're customizing core-agent for the first time or expanding an existing configuration.
---

When invoked:

1. **Confirm the context.** Ask the user what they're trying to do — a quick code-reviewer, a docs helper, something else. A 30-second clarification up front prevents a misaligned configuration.

2. **Walk the four customization layers IN ORDER**, stopping at the user's confirmation between each:

   1. Provider + model — `.agents/config.json`
   2. Personality — `AGENTS.md`
   3. (Optional) Skills — `.agents/skills/<name>/SKILL.md`
   4. (Optional) MCP servers — `.agents/mcp.json`

   For each layer, propose a starting configuration based on the user's stated goal, explain why those choices, and ask for confirmation or adjustments before writing files.

3. **Set the permission posture.** After the four layers are in place, walk the user through choosing a starting mode (`ask` for new projects, `allow` with a curated allowlist for established workflows). Use the `references/permission-posture.md` reference for the patterns.

4. **Verify it works.** Suggest the user run `core-agent` and try a representative prompt. If anything misbehaves, diagnose against the failure modes in the references below.

5. **Check it in.** Remind the user to `git add .agents/ AGENTS.md && git commit` so teammates inherit the configuration.

## Triggers in detail

The description above lists the verbatim phrasings. Also match on:

- "Help me write an AGENTS.md"
- "How do I tell the agent to use X library / follow Y convention"
- "What should I put in config.json"
- "Add a skill for [domain]"
- "Wire up [MCP server] so the agent can [capability]"
- "The agent keeps asking me before [common operation] — make it stop"

## References

Use these references when the user's question is more specific than the runbook above. Fetch with `read_file`; only the relevant one for the user's current question, not all of them up front.

- **`references/agents-md-patterns.md`** — `AGENTS.md` authoring patterns. Read when the user is writing their personality file or asking why their existing one isn't producing the behavior they want.
- **`references/skills-and-mcp.md`** — Extending the tool surface: when to write a skill vs an `AGENTS.md` rule, and how to wire MCP servers. Read when the user wants the agent to do something the built-in tools don't cover.
- **`references/permission-posture.md`** — Choosing between `ask` / `allow` / `yolo`, writing allow patterns, the path-scope safety floor. Read when the user is being prompted too often (or not often enough).

## Procedure: provider + model

The minimal `.agents/config.json`:

```json
{
  "version": 1,
  "model": {
    "provider": "anthropic-vertex",
    "name": "claude-opus-4-7"
  }
}
```

Pick the provider based on what credentials the user has. Don't auto-detect from env vars in a checked-in config — that depends on per-operator environment and breaks reproducibility.

| Provider | When | Notes |
|---|---|---|
| `anthropic` | Public Anthropic API key (`ANTHROPIC_API_KEY`) | Simplest if the user just has a key |
| `anthropic-vertex` | User has GCP project + ADC | Cheaper for sustained use; uses GCP billing |
| `gemini` | Public Gemini API key (`GEMINI_API_KEY` or `GOOGLE_API_KEY`) | Free tier covers a lot of dev work |
| `vertex` | User has GCP project + ADC | Gemini via Vertex; uses GCP billing |

Model picks:

- **Coding-heavy work:** Claude Opus 4.7 OR `gemini-3.1-pro-preview-customtools` (the customtools variant prefers function-tools over raw shell; recommended for coding agents).
- **Lighter Q&A, doc explanation:** Claude Sonnet 4.6 OR `gemini-2.5-flash` (both much cheaper).
- **Mixed:** `claude-opus-4-7` parent + `--agentic-tools --agentic-small-model claude-haiku-4-5` for cost-efficient tool work.

## Procedure: `AGENTS.md`

Walk the user through writing the role + scope + house style + do/don't list. Fetch `references/agents-md-patterns.md` if they're stuck on what to put.

The minimum viable `AGENTS.md` is ~15 lines: one paragraph defining the role, one section listing what's in scope, one section with 3-5 house-style rules, one "don't do" list with 2-3 specific anti-patterns. Resist the urge to make it longer — long `AGENTS.md` dilutes attention.

## Procedure: skills

Most projects start with zero skills and add them as recurring procedures surface. If the user is asking for a skill, fetch `references/skills-and-mcp.md` for the design patterns.

Quick test: if the procedure applies to *every turn* of any task, it's `AGENTS.md` content. If it applies to *specific named requests*, it's a skill.

## Procedure: MCP servers

Wire MCP only if the user needs capabilities outside the built-in nine tools (`read_file`, `read_many_files`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`, `todo`). Common MCP additions: web search (`tavily`), GitHub API, internal databases.

Fetch `references/skills-and-mcp.md` for `mcp.json` schema + the namespacing rules.

## Procedure: permission posture

Walk through the modes table:

| Mode | When |
|---|---|
| `ask` (default) | First-time setup, exploratory use. Every gated tool prompts. |
| `allow` | Established workflows. Build the allowlist via in-session "always allow" prompts; the entries land in `config.json` automatically. |
| `yolo` | CI / scripted runs / known-safe workflows. Never prompts. Use only when you can audit the tool surface. |

Fetch `references/permission-posture.md` for the pattern grammar (`bash:git diff*`, `read_file:internal/**`) and the path-scope safety floor.

## When NOT to use this skill

- The user is asking about **autonomous / headless** configuration (long-running unattended workers). Use the `autonomous-setup` skill instead.
- The user is **embedding `core-agent` in their own Go binary** rather than using the CLI. Use the `library-embedding` skill instead.
- The user is asking a quick reference question ("what flag disables compaction?") — answer directly from the docs, don't walk the whole runbook.
- The user has an existing well-configured agent and is asking about a specific failure mode. Investigate the failure first; only walk the configuration runbook if the failure traces back to a missing/misconfigured layer.

## Output style

Conversational. For each layer, propose → confirm → write → next. Don't dump the entire 4-layer configuration up front; that's overwhelming. Walk one layer at a time, showing the user what's about to land in their repo.

When writing files, use `write_file` (or `edit_file` if updating). Show the user what's about to be written before writing it; ask for adjustments. Once written, briefly confirm and move to the next layer.
