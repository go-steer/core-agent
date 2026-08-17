---
title: Slash reference
---

Reference for every slash command and keybinding available in the interactive TUI. Type `/help` in any session for the operator-side version of this catalog.

For attach-mode (`core-agent-tui` remote client) commands, see [Attach mode TUI](/reference/attach-tui/).

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
| `/usage` | | Extended /stats: cached-vs-uncached input tokens, per-turn history, cache-savings vs uncached-reference cost |
| `/context` | `/boundaries` | Context-management activity: compactions, checkpoints, summarized chars, subtask cost |
| `/tools` | | List the tools the agent has access to (built-ins + MCP + skills) |
| `/skills` | | List loaded skills with their trigger descriptions |
| `/mcp` | | List configured MCP servers and their status |
| `/subagents` | | List background subagents spawned this session, with live status |
| `/memory` | | Show the resolved `AGENTS.md` chain (user-global + project) |

### Context management

| Command | Aliases | Effect |
|---|---|---|
| `/compact [focus]` | `/summarize` | Manually compact the session; optional `focus` biases what the summary preserves |
| `/done [note]` | `/checkpoint` | Write a task-boundary checkpoint; optional `note` becomes part of the handover record |

Both run a summarizer LLM call (5-15s); the next turn picks up from the summary with prior history sliced. See [Context management](/concepts/context-management/) for the design.

### Permissions

| Command | Aliases | Effect |
|---|---|---|
| `/permissions` | `/perms` | Show the current gate mode + active allow/deny patterns |
| `/allow <pattern>` | | Add an allow pattern to the live gate (and to `.agents/config.json` if writable) |
| `/deny <pattern>` | | Add a deny pattern (deny wins over allow) |
| `/allow bundle:<name>` | | Apply a pre-defined allow bundle (e.g., `dev_tools`) |
| `/replan` | | Revoke the active plan artifact and re-arm plan-first gating, so the next mutating call is denied until a fresh `record_plan`. Since v2.9 it archives **this** agent and session's plan — a subagent's or another tenant's newer plan is named and left alone. The gate flag clears either way |

Pattern grammar: `<tool>:<glob>` (e.g., `bash:git diff*`, `read_file:internal/**`). See [Permissions](/concepts/permissions/).

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
| `/subagent <name> <goal>` | `/sub` | Spawn a **configured** subagent by name against a goal (fire-and-continue; its report arrives on a later turn). Run `/subagent` with no arguments to list configured names. Ad-hoc inline personas are not offered here — curate them as `subagents[]` in config |

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
| **Ctrl+B** | Toggle the header / sidebar |
| **Ctrl+G** | Open the model picker (same as `/model`) |
| **Ctrl+X** | Expand the selected tool call (args + response detail) |
| **?** | Toggle the keybinding cheat sheet (same as `/keys`) |

### Transcript focus

The transcript takes the keyboard, so you can select, fold, and copy a single item instead of scrolling a wall of text. Press **Tab** to move focus out of the composer; **Enter** or **Esc** hands it back.

| Key | Effect |
|---|---|
| **Tab** | Move the keyboard between the composer and the transcript |
| **↑ / ↓** (or **k** / **j**) | Move the selection one item at a time |
| **Space** | Fold / unfold the selected item |
| **Shift+↑ / Shift+↓** | Scroll a line at a time *inside* a long item |
| **Shift+← / Shift+→** | Pan sideways over a wide table or diff (content that doesn't wrap) |
| **y** | Copy the selected item to the clipboard |
| **c** | Copy just the code blocks in the selected item |
| **g / G** | Jump to the first / last item (**G** resumes following the stream) |
| **Enter** / **Esc** | Return focus to the composer |

---

## Behavior notes

### Where a copy actually lands

`y` and `c` write to two clipboards, because a terminal session has two machines in it and neither one is a fallback for the other:

- An **OSC 52 escape** goes to the terminal emulator you are sitting in front of. This is the one that works over SSH, but many terminals decline it — Terminal.app has never implemented it, iTerm2 has it behind an off-by-default preference, and some web/relay terminals strip it in transit. The protocol has no acknowledgement, so nothing can tell you it was dropped.
- A **host clipboard write** goes to the machine the process is running on, using whichever helper is installed: `pbcopy` (macOS), `wl-copy` (Wayland), `xclip` / `xsel` (X11), or `clip.exe` (Windows and WSL). This one reports a result.

The footer tells you which succeeded. `copied 24 lines` means the host write confirmed. `copied 24 lines · osc52` means only the escape went out — either the box has no clipboard helper (a headless server, typically) or none is installed. `copied 24 lines · osc52 only (…)` means a helper was found and failed, with the reason.

For `core-agent-tui` in attach mode the host write is the useful one: that process runs on your laptop even when the agent is in a pod elsewhere.

### Untrusted tool output is escaped

Tool arguments, tool responses, file content, and bash stdout/stderr are stripped of ANSI escape sequences and have their remaining control bytes rendered as visible `\xNN` before anything reaches your terminal. A file containing `ESC[2J` shows you the bytes instead of clearing your screen. Tab and newline pass through untouched; SGR color in captured command output is dropped rather than shown as literal escape text.

### Cancellation semantics

Esc and Ctrl+C-once both cancel the current model turn. The turn unwinds cleanly — any tool call in flight runs to completion (you can't kill it from the operator side), but no new model call fires. The session continues; you can type a follow-up immediately.

### Typing while the agent is working

You can keep typing during a turn. Each Enter queues your input to the agent's inbox. When the current turn finishes, the agent auto-continues with the queued entries prefixed by a `↻` user message; the model decides whether to adapt the current task or capture each note with the `todo` tool. A soft cap of 10 consecutive auto-continues prevents runaway chains. The queue panel between chat and input mirrors what's pending; press Esc to dismiss queued entries.

### What `/model` lists — and what it doesn't

The candidate list is derived, not hand-maintained. It is the builtin pricing table (generated weekly from LiteLLM: every chat-capable, tool-calling, non-deprecated Gemini and Anthropic model) narrowed to the current generations — Gemini 3.x and newer, Claude 4.x and newer — with date-suffixed aliases (`claude-opus-4-7-20260416`) and the Mythos-class tier dropped as duplicates of the ids above them. The two entries at the top are the pinned defaults; the rest sort alphabetically within each family.

Narrowing the picker does **not** narrow what you can run. Anything the provider accepts still works via `--model <id>` or `model.name` in `.agents/config.json`, including older generations, date-pinned ids, and `-1m` long-context variants. Cost tracking follows: an id with no exact pricing row resolves by longest-prefix match, so `claude-opus-4-7-1m` bills at `claude-opus-4-7`'s rates. The picker is a curated short list for the common case, not an allowlist.

### Long-running slashes

`/compact`, `/done`, and `/btw` all fire LLM calls and take 5-15 seconds. The bottom toast (`▸ /<name> running…`) shows for the duration; an in-chat preamble row (`ℹ Capturing checkpoint summary…`, etc.) lands immediately so the dead time is visible. The final result message (success or error) appears below the preamble when the work completes.

### Slash visibility gating

`/done` and `/checkpoint` only appear in `/help` when `WithCheckpointer` was wired (default-on; disable with `--no-checkpoint`). Same for `/compact` + `--no-compact`. Operators who disable a mechanism don't see commands that would only error out.

---

## Where to go next

- **[Workflows](/run/interactive/workflows/)** — worked examples (code-reviewer, doc writer)
- **[Context management](/concepts/context-management/)** — `/compact`, `/done`, `/context` in depth
- **[Permissions](/concepts/permissions/)** — `/allow` + `/deny` pattern grammar
- **[Configuration](/reference/configuration/)** — pin the pieces above into `.agents/config.json`
- **[Attach mode TUI](/reference/attach-tui/)** — operator client for remote (attach-mode) agents has its own slash catalog
