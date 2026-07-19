---
title: Workflows
---

Worked examples of using `core-agent` interactively. Each one shows a full `.agents/` configuration you can adapt and a walkthrough of running it.

If you haven't done the [Interactive quickstart](/run/interactive/quickstart/) yet, do that first — these examples assume you know how `AGENTS.md`, skills, and permission posture fit together.

---

## Workflow 1 — Go code reviewer with MCP-backed web search

The marquee operator example. A code-reviewer agent that knows your house style, follows a documented review procedure, and can reach the web when it needs context on an unfamiliar library.

### Final `.agents/` layout

```
your-repo/
├── .agents/
│   ├── config.json
│   ├── mcp.json
│   └── skills/
│       └── code-review/
│           ├── SKILL.md
│           └── examples/
│               └── good-vs-bad.md
└── AGENTS.md
```

### `AGENTS.md`

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

## When to use the web

Acme uses a handful of external libraries. If you need to verify current
behavior of a third-party package the user mentioned, use the `search` MCP
server's `tavily_search` tool rather than guessing. Cite the URL in your
review.
```

### `.agents/config.json`

```json
{
  "version": 1,
  "model": { "provider": "anthropic-vertex", "name": "claude-opus-4-7" },
  "permissions": {
    "mode": "allow",
    "allow": [
      "bash:git status",
      "bash:git diff*",
      "bash:go vet ./...",
      "bash:go build ./...",
      "read_file:**",
      "grep:**",
      "glob:**",
      "list_dir:**"
    ]
  }
}
```

Read-side operations run without prompting; writes, broader shell, and the MCP web fetch still gate.

### `.agents/mcp.json`

```json
{
  "version": 1,
  "servers": {
    "search": {
      "transport": "stdio",
      "command":   "uvx",
      "args":      ["mcp-server-tavily"],
      "env":       { "TAVILY_API_KEY": "${env:TAVILY_API_KEY}" }
    }
  }
}
```

The `search` server adds `search_tavily_search` to the agent's tool list (MCP tools are namespaced with the server name). `${env:TAVILY_API_KEY}` interpolates at startup; missing env vars fail loudly so you spot misconfiguration immediately.

### `.agents/skills/code-review/SKILL.md`

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

### Running it

```text
$ core-agent
> please review the diff in pending.diff
[agent reads pending.diff, recognizes "review" + "diff" triggers the
 code-review skill, loads the rubric, walks each hunk]

> ✅ ...
> ⚠️ pkg/orders/api.go:142 — error wrapped with errors.New(...) where
> we'd want fmt.Errorf("orders.api: %w", err). Suggested change: ...
> 💡 ...

> the second issue talks about the new dependency — is that library
> still maintained?
[agent calls search_tavily_search, returns with a citation]

> /done first-review-pass-complete
[checkpoint summary written; the next prompt starts with a clean
 working set + the prior task as authoritative context]
```

### What this demonstrates

- **`AGENTS.md` shapes voice + scope** — every turn, the model sees "you're a code-reviewer for Acme."
- **Skills shape procedure** — the rubric is too long to repeat each turn; the skill loads on demand when the description matches.
- **MCP servers add reach** — the model can answer "is this library still maintained" from a real source instead of training-data guessing.
- **Permission posture matches workflow** — reads + safe shell run without prompting; writes + web access still gate.
- **`/done` carves task boundaries** — after a long review session, drop a checkpoint so the next task doesn't drag along the full review context.

---

## Workflow 2 — Documentation writer with no shell access

A read-only agent for "explain this code" / "write me a docstring" tasks. No write tools, no shell — strictly read + reason + propose.

### `.agents/config.json`

```json
{
  "version": 1,
  "model": { "provider": "gemini", "name": "gemini-3.1-pro-preview-customtools" },
  "tools": {
    "disable": ["bash", "write_file", "edit_file", "delete_file"]
  },
  "permissions": {
    "mode": "allow",
    "allow": [
      "read_file:**",
      "read_many_files:**",
      "grep:**",
      "glob:**",
      "list_dir:**"
    ]
  }
}
```

Disabling the write tools removes them from the model's tool list entirely — it can't even propose a write call. The model surfaces suggested edits as plain text in chat for you to apply manually. Unknown tool names in `disable` fail loudly at startup, so typos surface immediately.

### `AGENTS.md`

```markdown
You are a documentation assistant for the Acme platform monorepo.

You explain code, suggest docstrings, and answer "how does X work" questions.
You do NOT modify files — surface suggested edits as ``` code blocks for the
user to apply manually.

## House style

- Package-level doc comments start with the package name.
- Exported identifiers get godoc starting with the identifier name.
- One-paragraph explanations for "how it works" questions; expand on request.
- Quote file:line when citing specific code; never paste more than 10 lines
  verbatim unless asked.
```

### Running it

```text
$ core-agent
> what does internal/auth/middleware.go actually do?
[agent reads middleware.go + grep for callers, returns a paragraph
 with file:line citations]

> suggest a godoc for ParseToken
[agent proposes a doc block as a markdown ``` block]
```

### What this demonstrates

- **`tools.disable`** removes built-in capabilities the model shouldn't have access to. The agent surface narrows to "investigation + explanation."
- **Choosing a different model** — Gemini Pro is cheaper than Opus and excellent at code explanation; pick the model that fits the workload.
- **`AGENTS.md` reinforces tool absence** — "you do NOT modify files" is belt-and-suspenders on top of `tools.disable`. Helps when the model would otherwise propose calling a missing tool and the user has to wait for the failure.

---

## Patterns across both workflows

**Pin the model in `config.json`, not in environment.** Auto-detection from env vars is fine for personal use; for a checked-in `.agents/` setup, pin `model.provider` and `model.name` explicitly so teammates don't depend on their local environment.

**Permission posture comes after personality.** Get the agent answering useful questions first, then tighten or loosen permissions. `ask` mode is the right starting point; `allow` with a curated list is the right steady state for daily driving.

**Skills are for procedures, not facts.** A skill should describe a workflow the agent follows. Facts (your house style, your library list) go in `AGENTS.md` or in a skill's reference file linked from the skill body.

**MCP servers are the integration point.** Anything outside the built-in catalog (files, search, shell, data + network, planning — see [Built-in tools](/concepts/tools/)) goes through MCP. Don't write custom Go tools unless you're embedding the library; for CLI use, MCP is the right surface.

---

## Where to go next

- **[Slash reference](/run/interactive/slash-reference/)** — every slash command + keybinding for the TUI
- **[Agent design](/agent-design/)** — prescriptive section: prompt patterns, when skills vs. `AGENTS.md`, getting the model to use subagents, cost efficiency
- **[Context management](/concepts/context-management/)** — make long sessions survivable
- **[Configuration](/reference/configuration/)** — full `.agents/config.json` schema
- **[Permissions](/concepts/permissions/)** — pattern grammar, path scope, bash denylist
- **[Skills](/concepts/skills/)** — `SKILL.md` schema, discovery, allow/deny
- **[MCP servers](/concepts/mcp/)** — full schema, transports, lifecycle
