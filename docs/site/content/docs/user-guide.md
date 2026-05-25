---
title: User guide
weight: 4
---

This guide walks you through giving `core-agent` a personality — system prompt, skills, tools, permissions — using the CLI. By the end, your `.agents/` directory will describe a specific assistant tailored to your project, and the same configuration will work for any teammate who clones the repo.

If you haven't yet run the binary against a provider, do [Getting started](../getting-started/) first. This guide picks up where that page leaves off.

## What "personality" means here

A `core-agent` agent has four layers of customization, each adding more specificity:

1. **The model and provider.** Which LLM is behind it. Configured in `.agents/config.json`.
2. **The system prompt.** What the agent *is* — its role, voice, constraints. Configured in `AGENTS.md`.
3. **Skills.** Reusable named procedures the agent invokes by name. Bundles under `.agents/skills/`.
4. **Tools.** What the agent *can do*. Built-in tools (file I/O, shell, search), plus any MCP servers you wire up in `.agents/mcp.json`.

The CLI reads all of this automatically — no flags needed. Drop the right files in the right places and you have a tailored agent.

The running example throughout this guide will be a "Go code-reviewer" agent for a hypothetical project. By the end, that agent will: review staged diffs, lean on a `code-review` skill with house style rules, search the web via an MCP server when it needs context, and ask before running anything beyond its allowlist.

## Layer 1: pick a provider and model

Your `.agents/config.json` selects the backend. Minimum viable file:

```json
{
  "version": 1,
  "model": {
    "provider": "anthropic-vertex",
    "name": "claude-opus-4-7"
  }
}
```

Provider choices: `gemini`, `vertex`, `anthropic`, `anthropic-vertex`. When the field is empty, `core-agent` auto-detects from environment variables — handy for personal use but worth pinning explicitly in a shared repo so teammates don't depend on their local env. See [Providers](../providers/) for env vars, model IDs, and the gotchas around each backend.

A few model-selection tips that matter in practice:

- **Default Gemini model is `gemini-3.1-pro-preview-customtools`** — a Vertex behavioral variant that prefers developer-defined tools over raw shell. Strongly recommended for any agent doing code work. Override to `gemini-3.1-pro-preview` (no suffix) only when you specifically need behavior-baseline comparisons.
- **Claude Opus 4.7** is the most capable Anthropic model; use `claude-sonnet-4-6` if you're cost-sensitive and the workflow doesn't need top-tier reasoning.
- **For test fixtures or CI**, use the `echo` mock provider (replays the prompt verbatim, no credentials) or `scripted` (replays a recorded transcript). See [Providers → Mock providers](../providers/#mock-providers).

## Layer 2: the personality — `AGENTS.md`

The single most impactful customization. Drop a file named `AGENTS.md` at your repo root. `core-agent` prepends its contents to every system instruction, so the model sees your guidance on every turn.

For the code-reviewer example, an effective `AGENTS.md` might look like:

```markdown
You are a Go code-reviewer for the Acme platform monorepo.

## What you review

- Staged diffs (`git diff --staged`) the user pastes in or asks you to fetch.
- Test failures: surface what broke, propose the minimum fix.
- Build failures from `go vet` / `golangci-lint`: same.

## House style

- Prefer table-driven tests with `t.Parallel()`.
- Error wrapping with `fmt.Errorf("op: %w", err)`, not `errors.New`.
- New exported types and functions get a godoc starting with the name.
- Prefer `slog` over `log`; structured fields, no string interpolation in messages.

## What to do, what not to do

- When suggesting a fix, show the smallest diff possible — no opportunistic refactors.
- Never use `panic` in library code; never call `os.Exit` outside of `main`.
- If a change touches `internal/billing/`, flag it explicitly and ask the user
  to add the `billing-team` reviewer.
- Before running tests, always ask the user — `go test ./...` can be slow in
  this repo.
```

This file is checked into the repo, so every teammate gets the same agent automatically.

### Fallback chain and layering

`core-agent` reads (in order, first match wins per directory):

1. **User-global**: `~/.core-agent/AGENTS.md` → `CLAUDE.md` → `GEMINI.md`
2. **Project-scoped**: `<repo>/AGENTS.md` → `CLAUDE.md` → `GEMINI.md`

Both are prepended; user-global comes first, then project. The convention matches what [Claude Code](https://docs.claude.com/en/docs/claude-code) and other coding agents have settled on, so a single `AGENTS.md` works across tools.

### Tips for writing a good `AGENTS.md`

- **Be specific.** "You are helpful" doesn't change behavior. "Always ask before running tests" does.
- **Lead with the role, then the do/don't list.** Models follow imperative direction better than aspirational descriptions.
- **Include examples for the patterns you care about.** Two lines of code showing your error-wrapping convention is worth a paragraph describing it.
- **Don't fight the default instruction.** `core-agent` ships `agent.DefaultInstruction` with a parallelism mandate that tells the model to batch independent tool calls. `AGENTS.md` is prepended *to* that, not in place of it, so you don't need to repeat the mandate.

## Layer 3: skills — reusable procedures

Skills are markdown files the agent can invoke by name. Use them for procedures you'd otherwise paste into the prompt repeatedly: "run our deploy checklist", "triage a Jira ticket", "do our code review."

Each skill lives in its own directory under `.agents/skills/`:

```
.agents/skills/
├── code-review/
│   ├── SKILL.md
│   └── examples/
│       └── good-vs-bad.md
└── deploy-checklist/
    └── SKILL.md
```

A skill is a `SKILL.md` with YAML frontmatter and a markdown body:

```markdown
---
name: code-review
description: Review a Go diff against Acme house style. Use when the user asks for a review or pastes a diff.
---

When invoked:

1. Read the diff. If the user gave you a path, fetch it with `git diff`. If
   they pasted text, work from that.
2. For each hunk, evaluate against the house-style rubric below.
3. Output a structured review:

   - ✅ Things done well (brief)
   - ⚠️ Issues to fix before merging (file:line + suggested change)
   - 💡 Optional improvements (separate section, do not block on these)

## House-style rubric

- **Errors:** wrapped with `fmt.Errorf("op: %w", err)`. Bare `errors.New` only
  for sentinel errors at package scope.
- **Tests:** table-driven, `t.Parallel()` at top, `t.Run(name, ...)` per case.
- **Concurrency:** any new goroutine has an explicit shutdown story.
- **Logging:** structured with `slog`; no `fmt.Println` in non-test code.

See `examples/good-vs-bad.md` for worked examples of each.

## When NOT to use this skill

- For non-Go code; this rubric is Go-specific.
- For purely cosmetic changes (whitespace, renames); just approve those.
```

The agent will invoke the skill when its description matches the user's request. The body is the *prompt content* the agent sees when it does — write it the way you'd write a runbook for a new team member.

### Skills compose with `AGENTS.md`

`AGENTS.md` sets the persistent personality; skills are pulled in on demand. The same agent can have a dozen skills loaded and only invoke the one relevant to the current task. Skills format mirrors [Anthropic's published spec](https://docs.claude.com/en/docs/agent-skills/overview) so anything you write here also works in Claude Code.

See the [Skills reference](../skills/) for the full SKILL.md schema and allow/deny lists.

## Layer 4: tools — built-ins plus MCP

Out of the box, `core-agent` ships nine built-in tools: `read_file`, `read_many_files`, `write_file`, `edit_file`, `list_dir`, `glob`, `grep`, `bash`, and `todo`. They cover most code-investigation and code-editing workflows; their descriptions explicitly tell the model to prefer them over raw shell for file operations.

For everything else — search the web, query a database, hit an internal API, browse with a real browser — wire up an MCP server.

### Wiring an MCP server

Drop a `.agents/mcp.json` next to your config:

```json
{
  "version": 1,
  "servers": {
    "filesystem": {
      "transport": "stdio",
      "command":   "mcp-server-filesystem",
      "args":      ["--root", "/Users/alex/code"]
    },
    "search": {
      "transport": "stdio",
      "command":   "uvx",
      "args":      ["mcp-server-tavily"],
      "env":       { "TAVILY_API_KEY": "${env:TAVILY_API_KEY}" }
    },
    "github": {
      "transport": "http",
      "url":       "https://api.githubcopilot.com/mcp/",
      "headers":   { "Authorization": "Bearer ${env:GITHUB_TOKEN}" }
    }
  }
}
```

Each server's tools are namespaced — the `tavily_search` tool above is exposed to the agent as `search_tavily_search`. That keeps two servers from colliding when they both expose `search` or `list`.

`${env:NAME}` interpolation resolves at startup. If a referenced variable isn't set, the server fails to start with a clear error — better than booting silently and returning surprising failures later.

See the [MCP servers reference](../mcp/) for the full schema.

### Disabling built-in tools

If you want a read-only agent (e.g. for an "explain this code" assistant), turn off the write tools:

```json
{
  "tools": {
    "disable": ["bash", "write_file", "edit_file"]
  }
}
```

Or pass `--disable-tools=bash,write_file` at the command line. Unknown tool names fail loudly at startup, so typos surface immediately.

## Permission posture — when does the agent ask?

`core-agent` ships three permission modes, configurable in `.agents/config.json` under `permissions.mode`:

| Mode | When the agent asks | When you'd use it |
|---|---|---|
| `ask` (default) | Before every gated tool call | Interactive use, your terminal |
| `allow` | Only when a call falls outside your `allow` patterns | Daily driving with trust built up |
| `yolo` | Never | CI, fully automated runs (use `--yolo` flag) |

The `allow` list uses `<tool>:<pattern>` syntax (or just `<pattern>` for path-scoped tools):

```json
{
  "permissions": {
    "mode": "allow",
    "allow": [
      "bash:git status",
      "bash:git diff*",
      "bash:go vet ./...",
      "read_file:internal/**",
      "grep:**"
    ]
  }
}
```

With this, common workflows run without prompting; anything else prompts you. The pattern grammar is described in the [Permissions reference](../permissions/) — patterns support `*` (one segment) and `**` (recursive) globs, with `<tool>:` scoping limits the rule to that tool.

A handful of bash commands are on a non-overridable denylist (the `rm -rf /` class). You can't allowlist past those; they require an explicit hard override no agent can choose for itself.

### Path scope

By default, file tools can only read/write inside the current repo (`.agents/`'s parent directory) and your home directory. To expand:

```json
{
  "path_scope": {
    "allow": [
      "/var/log/myapp/...",     // tree
      "/etc/myapp/config.yaml"  // exact path
    ]
  }
}
```

The path scope is enforced even in `yolo` mode — it's a safety floor below the permission gate. See [Permissions → Path scope](../permissions/#path-scope) for details.

## Putting it together — the code-reviewer in practice

With everything from the previous sections, your project's `.agents/` looks like:

```
.agents/
├── config.json
├── mcp.json
└── skills/
    └── code-review/
        └── SKILL.md
AGENTS.md
```

A user clones the repo, runs `core-agent`, and gets exactly your code-reviewer — same personality, same skills, same MCP servers, same permission posture. They don't need any setup beyond credentials for the provider you chose.

## Interactive, one-shot, and autonomous

Three ways to run the same agent:

**Interactive (REPL).** Bare `core-agent` drops you into a stdin REPL. Conversation history is preserved across turns. ESC mid-turn cancels just the current turn; double-Ctrl-C exits cleanly.

```text
$ core-agent
core-agent REPL — /exit or Ctrl-D to quit
> review the diff in pending.diff
[agent reads the file, runs the skill, returns the structured review]
> address the first issue
[agent proposes an edit, asks before writing]
> /exit
```

When stdin is a real terminal, `core-agent` launches an in-process bubble-tea TUI by default; the line-mode REPL is the fallback used for non-TTY environments or when you pass `--no-tui`. In the TUI you can keep typing while the agent is working: hit Enter to add a follow-up note to the agent's inbox without interrupting the current turn. A small queue panel between the chat and your input box mirrors what's pending. When the turn finishes, the agent auto-continues with the queued notes prefixed by a `↻` user message; the model decides whether to adapt the current task or capture each note with the `todo` tool. A soft cap (10) on consecutive auto-continues keeps things from chaining indefinitely when you type faster than the model can answer. Press Esc to dismiss any queued entries that failed to inject.

For one-off context-grounded questions that you don't want in the conversation, type `/btw <question>` (alias `/by-the-way`). The TUI spawns a parallel one-shot model call that sees the full session history but has no tools, and the answer appears in a dismissible overlay that never enters history. Press Space, Enter, or Esc to dismiss. The main turn keeps running.

To spawn a background subagent directly without asking the main agent, use `/subagent <goal>` (alias `/sub`). Optional flags: `--name=<id>` (auto-generated otherwise), `--prompt=<system_prompt>` (override the default), `--tools=<csv>` (restrict to specific built-ins), `--extras=<csv>` (MCP/skill tools; alias `--skill`), `--max-turns=<n>`, `--max-cost=<usd>`, `--max-wallclock=<duration>` (e.g. `10m`), `--scheduler=<default|sleep|exit_on_defer|none>`. The subagent's `report_alert` and completion land as alerts in the chat just like manager-spawned subagents.

**One-shot.** Pass `-p "..."` for a single turn that exits when complete. Useful for shell pipelines and shell-completion-style queries.

```bash
core-agent -p "what does the function ParseToken in internal/auth do?"
```

**Autonomous.** For batch processing or unattended work, run the autonomous driver from a small Go program (`agent.RunAutonomous`) with budgets on turns, tokens, cost, and wallclock. The model gets a `lifecycle` tool to declare it's done. See [Autonomous runs](../autonomous/) for the full pattern.

When running unattended, pass `--ask=auto` so prompts in your `AGENTS.md` like "always ask before running tests" get a clean refusal instead of blocking forever waiting on stdin. The agent sees the refusal and adapts.

## Sessions and crash-resume

By default, conversation history lives in memory only. To persist across restarts and unlock crash-resume:

```bash
core-agent --session-db
```

This creates `~/.core-agent/sessions.db` (SQLite). Every turn — every prompt, every tool call, every model response — is appended to a durable event log. If the process crashes mid-turn, you can resume the same session and the agent picks up where it left off.

For team-shared sessions or higher-throughput workloads, point at Postgres or MySQL via `--session-db-path=postgres://...`. See [Sessions and event log](../sessions/) for the full Stream API (`Since(seq)`, `Watch(seq)`) and the audit-log shape.

## Cost and observability

`core-agent` tracks token usage and cost per turn against a built-in price table. The chat-style output surfaces a usage summary at the end of every one-shot run:

```text
core-agent: 4 turn(s), 12.3k input + 1.2k output tokens, $0.024
```

For production use, opt into OpenTelemetry export:

```json
{
  "otel": {
    "exporter": "otlp",
    "endpoint": "otel-collector.internal:4317"
  }
}
```

Spans cover the agent turn, every tool call, and provider requests. See the OTEL section of [Configuration](../configuration/) for the full schema.

## Where to go next

- **[Library guide](../library-guide/)** — embed `core-agent` in your own Go binary, with extension points for custom prompters, remote subagents, custom tools and providers.
- **[Configuration reference](../configuration/)** — every field of `.agents/config.json` with types and defaults.
- **[Permissions reference](../permissions/)** — pattern grammar, the bash denylist, path scope details.
- **[MCP servers](../mcp/)** — full schema, transport details, lifecycle behavior.
- **[Skills](../skills/)** — full `SKILL.md` format, allow/deny lists, composition with MCP.
- **[Autonomous runs](../autonomous/)** — the unattended worker pattern, lifecycle tool, budgets.
- **[Sessions and event log](../sessions/)** — durable persistence, replay, live tail, crash-resume.
