---
title: Slash reference
weight: 3
---

Reference for every slash command and keybinding available in the interactive TUI. Type `/help` in any session for the operator-side version of this catalog.

For attach-mode (`core-agent-tui` remote client) commands, see [Attach mode TUI]({{< relref "/docs/reference/attach-tui.md" >}}).

---

## Quick reference

### Session control

| Command | Aliases | Effect |
|---|---|---|
| `/help` | | Print the command list + keybindings into the scrollback |
| `/clear` | | Clear the local scrollback (session log is untouched) |
| `/quit` | | Leave the TUI cleanly |
| `/interrupt` | | Cancel the in-flight model turn (same as pressing Esc during a turn) |
| `/resume` | | Resume a saved session from `<AgentsDir>/sessions/` |
| `/reload` | | Re-walk `AGENTS.md`, skills, and MCP config on disk. Reports per-surface results inline (`Memory: ✓`, `Skills: ✓`, `MCP: ✗` with errors listed) so you can confirm an edit parsed cleanly. Live MCP server restart and system-prompt rebuild still require a daemon restart. |

### Status + observability

| Command | Aliases | Effect |
|---|---|---|
| `/stats` | | Session token totals, cost, duration, per-model breakdown |
| `/context` | `/boundaries` | Context-management activity: compactions, checkpoints, summarized chars, subtask cost |
| `/tools` | | List the tools the agent has access to (built-ins + MCP + skills) |
| `/skills` | | List loaded skills with their trigger descriptions |
| `/mcp` | | List configured MCP servers and their status |
| `/subagents` | `/sub` | List background subagents spawned this session |
| `/memory` | | Show the resolved `AGENTS.md` chain (user-global + project) |

### Context management

| Command | Aliases | Effect |
|---|---|---|
| `/compact [focus]` | `/summarize` | Manually compact the session; optional `focus` biases what the summary preserves |
| `/done [note]` | `/checkpoint` | Write a task-boundary checkpoint; optional `note` becomes part of the handover record |

Both run a summarizer LLM call (5-15s); the next turn picks up from the summary with prior history sliced. See [Context management]({{< relref "/docs/reference/context-management.md" >}}) for the design.

### Permissions

| Command | Aliases | Effect |
|---|---|---|
| `/permissions` | `/perms` | Show the current gate mode + active allow/deny patterns |
| `/allow <pattern>` | | Add an allow pattern to the live gate (and to `.agents/config.json` if writable) |
| `/deny <pattern>` | | Add a deny pattern (deny wins over allow) |
| `/allow bundle:<name>` | | Apply a pre-defined allow bundle (e.g., `dev_tools`) |

Pattern grammar: `<tool>:<glob>` (e.g., `bash:git diff*`, `read_file:internal/**`). See [Permissions]({{< relref "/docs/reference/permissions.md" >}}).

### Model + pricing

| Command | Aliases | Effect |
|---|---|---|
| `/model [id]` | `/models` | With no argument: list candidate models. With an ID: switch to that model for subsequent turns |
| `/pricing` | | Show the pricing layer in effect for the current model |
| `/pricing refresh` | | Pull the latest LiteLLM pricing JSON into `~/.core-agent/pricing.json` |
| `/pricing set <id> <in> <out>` | | Override pricing for a specific model ID (per-million tokens) |

### Side queries + delegation

| Command | Aliases | Effect |
|---|---|---|
| `/btw <question>` | `/by-the-way` | Ask a one-shot context-grounded question. Answer appears in a dismissible modal; never lands in conversation history |
| `/subagent <goal> [flags]` | `/sub` | Spawn a background subagent against a goal. Flags: `--name`, `--prompt`, `--tools`, `--extras`, `--max-turns`, `--max-cost`, `--max-wallclock`, `--scheduler` |

### Theming + display

| Command | Aliases | Effect |
|---|---|---|
| `/theme` | | Open the theme picker — arrows preview each theme live, Enter accepts and writes the choice to `.agents/config.json` (`ui.theme`), Esc restores the theme that was active when the picker opened |
| `/theme <name>` | | Switch directly to a named theme without opening the picker; persists the same way. `/theme` with no argument lists choices |
| `/mouse` | | Toggle terminal mouse capture (off = native shell selection + scroll wheel) |
| `/keys` | | Print the keybinding cheat sheet |

---

## Keybindings

| Key | Effect |
|---|---|
| **Enter** | Submit input (or run slash command). Mid-turn: queue the input for after the current turn finishes |
| **Shift+Enter** | Insert a newline in the input (multi-line prompts) |
| **Esc** | Contextual: dismiss a modal if one's open; otherwise interrupt the in-flight turn |
| **Ctrl+C** (once) | Cancel the in-flight turn (same as `/interrupt`) |
| **Ctrl+C** (twice within 1s) | Quit the TUI |
| **Ctrl+D** | EOF — quit the TUI |
| **PgUp / PgDn** | Scroll the scrollback up / down |
| **Ctrl+E** | Open `$EDITOR` with the current input buffer (fallback: `$VISUAL` → `vi`) |

---

## Behavior notes

### Cancellation semantics

Esc and Ctrl+C-once both cancel the current model turn. The turn unwinds cleanly — any tool call in flight runs to completion (you can't kill it from the operator side), but no new model call fires. The session continues; you can type a follow-up immediately.

### Typing while the agent is working

You can keep typing during a turn. Each Enter queues your input to the agent's inbox. When the current turn finishes, the agent auto-continues with the queued entries prefixed by a `↻` user message; the model decides whether to adapt the current task or capture each note with the `todo` tool. A soft cap of 10 consecutive auto-continues prevents runaway chains. The queue panel between chat and input mirrors what's pending; press Esc to dismiss queued entries.

### Long-running slashes

`/compact`, `/done`, and `/btw` all fire LLM calls and take 5-15 seconds. The bottom toast (`▸ /<name> running…`) shows for the duration; an in-chat preamble row (`ℹ Capturing checkpoint summary…`, etc.) lands immediately so the dead time is visible. The final result message (success or error) appears below the preamble when the work completes.

### Slash visibility gating

`/done` and `/checkpoint` only appear in `/help` when `WithCheckpointer` was wired (default-on; disable with `--no-checkpoint`). Same for `/compact` + `--no-compact`. Operators who disable a mechanism don't see commands that would only error out.

---

## Where to go next

- **[Workflows]({{< relref "/docs/cli/interactive/workflows.md" >}})** — worked examples (code-reviewer, doc writer)
- **[Context management]({{< relref "/docs/reference/context-management.md" >}})** — `/compact`, `/done`, `/context` in depth
- **[Permissions]({{< relref "/docs/reference/permissions.md" >}})** — `/allow` + `/deny` pattern grammar
- **[Configuration]({{< relref "/docs/reference/configuration.md" >}})** — pin the pieces above into `.agents/config.json`
- **[Attach mode TUI]({{< relref "/docs/reference/attach-tui.md" >}})** — operator client for remote (attach-mode) agents has its own slash catalog
