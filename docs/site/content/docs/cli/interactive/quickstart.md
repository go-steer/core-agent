---
title: Interactive quickstart
weight: 1
---

15 minutes from `core-agent` installed → a tailored agent your whole team can use.

> **Prefer to have an agent walk you through this?** The [`cli-setup` skill]({{< relref "/docs/skills-library/cli-setup.md" >}}) covers the same material in workflow form. Install once, then say "help me set up core-agent for my project" and the agent walks the four layers with you, writing files as you go.

## What you'll have at the end

A project-scoped agent that:

- Knows what your project is (system prompt via `AGENTS.md`)
- Has a reusable named procedure (a skill)
- Asks before running anything risky (permissions in `ask` mode by default)
- Is checked into the repo so every teammate gets the same agent

The running example is a "Go code-reviewer" for a hypothetical project. Adapt as needed.

## Before you start

You should have completed [Getting started]({{< relref "/docs/getting-started.md" >}}) — `go install`, environment credentials for a provider, and `core-agent -p "hello"` returning a response.

This page works in your project directory (anywhere `core-agent` can find a `.agents/` folder by walking up from your CWD).

---

## Step 1 — Drop an `AGENTS.md` (5 min)

The single most impactful customization. Create `AGENTS.md` at your repo root. `core-agent` prepends its contents to every system instruction the model sees on every turn.

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

**Tips that matter:**

- **Be specific.** "You are helpful" doesn't change behavior. "Always ask before running tests" does.
- **Lead with the role, then the do/don't list.** Models follow imperative direction better than aspirational descriptions.
- **Include examples for the patterns you care about.** Two lines of code showing your error-wrapping convention is worth a paragraph describing it.

`AGENTS.md` is checked into the repo. Every teammate who clones gets the same agent. The fallback chain (`AGENTS.md` → `CLAUDE.md` → `GEMINI.md`, plus user-global at `~/.core-agent/AGENTS.md`) matches what Claude Code and other coding agents settled on, so one file works across tools.

For deeper prompt-engineering patterns see [Agent design → System instructions]({{< relref "/docs/agent-design/system-instructions.md" >}}).

---

## Step 2 — Run a turn (2 min)

```text
$ core-agent
[bubble-tea TUI takes over the terminal]
> what files are in this repo?
[agent runs list_dir + read_many_files, returns a summary]
> /quit
```

The TUI is the default when stdin is a real terminal. Conversation history persists across turns. While the agent is working you can keep typing — your follow-up notes queue up and the model picks them up when the current turn finishes.

For the slash command catalog see [Slash reference]({{< relref "/docs/cli/interactive/slash-reference.md" >}}). For the line-mode REPL fallback (non-TTY environments, scripts, slim builds) pass `--no-tui`.

---

## Step 3 — Add a skill (5 min)

Skills are reusable named procedures the agent invokes by name. Use them for anything you'd otherwise paste into the prompt repeatedly: "run our deploy checklist", "triage a Jira ticket", "do our code review."

Create `.agents/skills/code-review/SKILL.md`:

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

## When NOT to use this skill

- For non-Go code; this rubric is Go-specific.
- For purely cosmetic changes (whitespace, renames); just approve those.
```

The agent invokes the skill when the description matches the user's request. The body is the **prompt content** the agent sees when it does — write it the way you'd write a runbook for a new team member.

`AGENTS.md` sets persistent personality; skills are pulled in on demand. Same agent can have a dozen skills loaded and only fire the one relevant to the current task. Format mirrors [Anthropic's published spec](https://docs.claude.com/en/docs/agent-skills/overview) so anything you write here also works in Claude Code.

See the [Skills reference]({{< relref "/docs/reference/skills.md" >}}) for the full `SKILL.md` schema, and [Agent design → Skills]({{< relref "/docs/agent-design/skills.md" >}}) for patterns on when to write a skill vs. an `AGENTS.md` rule.

---

## Step 4 — Permission posture (3 min)

`core-agent` ships in `ask` mode by default — every gated tool call (writes, shell, network, deletes) prompts you. That's right for first-time use; it's also annoying once you've built up trust.

The two common adjustments:

**Allow-list your common workflows.** In `.agents/config.json`:

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
      "read_file:internal/**",
      "grep:**"
    ]
  }
}
```

With this, common workflows run without prompting; anything outside the allow-list still prompts. The pattern grammar (`*` = one segment, `**` = recursive) is documented in [Permissions]({{< relref "/docs/reference/permissions.md" >}}).

**Approve interactively, persist on the fly.** In `ask` mode, when the agent requests a gated call you can choose "always allow this tool" or "always allow this exact call" and the entry lands in `.agents/config.json` automatically. No need to draft the allow-list up front.

A handful of bash commands are on a non-overridable denylist (the `rm -rf /` class). You can't allowlist past those.

---

## Step 5 — Check it in

Your `.agents/` directory should now look something like:

```
your-repo/
├── .agents/
│   ├── config.json          # provider + permission posture
│   └── skills/
│       └── code-review/
│           └── SKILL.md
└── AGENTS.md                # the personality
```

`git add .agents/ AGENTS.md && git commit`. Every teammate who clones the repo and runs `core-agent` gets exactly your code-reviewer — same personality, same skills, same permission posture. No setup beyond provider credentials.

---

## Where to go next

- **[Workflows]({{< relref "/docs/cli/interactive/workflows.md" >}})** — the full worked example with MCP servers wired in, plus alternative agent shapes
- **[Slash reference]({{< relref "/docs/cli/interactive/slash-reference.md" >}})** — every slash command + keybinding
- **[Agent design]({{< relref "/docs/agent-design/_index.md" >}})** — prescriptive patterns: when to use skills vs. `AGENTS.md` rules, how to get the model to use subagents efficiently, cost-efficiency tips
- **[Context management]({{< relref "/docs/reference/context-management.md" >}})** — compaction, checkpoints, agentic tool wrappers for long sessions
- **[Configuration reference]({{< relref "/docs/reference/configuration.md" >}})** — every field of `.agents/config.json`
- **[MCP servers]({{< relref "/docs/reference/mcp.md" >}})** — wire in third-party tools (web search, GitHub, databases)
- **[Autonomous quickstart]({{< relref "/docs/cli/autonomous/quickstart.md" >}})** — running unattended against a goal
