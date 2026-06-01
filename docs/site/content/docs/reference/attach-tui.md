---
title: Attach TUI
weight: 8
---

`core-agent-tui` is the operator-facing terminal UI for attach-mode — the remote client for an agent running elsewhere (workstation, K8s pod, peer-registered fleet member). It ships as a separate binary so the default `core-agent` stays distroless-clean (no terminal-rendering deps land in production K8s images). See [Configuration → attach]({{< relref "/docs/reference/configuration.md" >}}) for the listener-side config and the HTTP/SSE protocol it consumes.

For local interactive use, run `core-agent` directly — its in-process TUI is the default when stdin is a terminal. `core-agent-tui` is the remote client only.

## Why a separate binary

`core-agent-tui` is a thin shell over [`go-steer/core-tui`](https://github.com/go-steer/core-tui) (Bubble Tea + Glamour + Lipgloss live there now); the `core-agent` binary itself pulls in zero terminal-rendering deps. For the K8s use case — a long-running headless agent with `--attach-listen` — that distroless image stays tight. Splitting the operator surface into its own binary keeps both pieces single-purpose.

Two release artifacts:

```
core-agent_<os>_<arch>        # default — K8s, distroless, headless
core-agent-tui_<os>_<arch>    # for laptop operators
```

If you have Go installed: `go install github.com/go-steer/core-agent/cmd/core-agent-tui@latest`.

## Quick start

```bash
# 1. Bare invocation — stdin prompts for an attach URL.
core-agent-tui

# 2. Remote — point at a running agent's --attach-listen.
ATTACH_TOKEN=$(openssl rand -hex 32) \
  core-agent --no-repl --session-db --attach-listen=:7777 \
  --attach-token=ATTACH_TOKEN

core-agent-tui http://localhost:7777 --token=ATTACH_TOKEN
```

`--no-repl` runs `core-agent` as an attach-only daemon (no stdin REPL, no in-process TUI). Pair with `--session-db` so the eventlog persists — attach mode requires it for the live-tail broadcaster.

URL forms (same grammar as `core-agent attach`):

| URL | Behavior |
|---|---|
| `http(s)://host:port` | Hub form — TUI opens the session picker, enumerating local + peer sessions in parallel |
| `http(s)://host:port/sessions/<sid>` | Direct-jump — TUI skips the picker and enters that session |
| `http(s)://host:port/sessions/<app>/<sid>` | Qualified direct-jump |
| `unix:///path/to/socket` | Unix-socket hub |
| `unix:///path/to/socket/sessions/<sid>` | Unix-socket direct-jump |

## Flags

| Flag | Purpose |
|---|---|
| `--token=<ENVVAR>` | Name of the env var holding the bearer token (same indirection as `--attach-token` on the listener side). The secret never appears on the command line. |
| `--theme=auto\|dark\|light` | Force a glamour theme for markdown rendering. Empty = auto (terminal background detection via OSC 11). |
| `--alias=<label>` | Display label for the agent identity in the status bar. Defaults to the session ID. |
| `--version` | Print build identity (`core-agent-tui v2.2.0 (commit a1b2c3d4, built 2026-06-01T…)`) and exit. |

## Operator surface (slash parity with the in-process TUI)

`core-agent-tui` shares its operator surface with the in-process TUI — all the slash commands from the [in-process slash reference]({{< relref "/docs/cli/interactive/slash-reference.md" >}}) work end-to-end against a remote agent. Highlights:

| Command | Effect |
|---|---|
| `/help`, `/quit`, `/clear` | Standard housekeeping. |
| `/stats` | Cumulative token + cost totals, per-model breakdown. Pulls from the remote's `usage.Tracker`. |
| `/context` | Compactions, checkpoints, summarized chars, subtask cost. |
| `/memory` | Current `AGENTS.md` chain (project + user-global). |
| `/skills` | Loaded skills with trigger descriptions. |
| `/mcp` | Configured MCP servers and their status. |
| `/perms`, `/permissions` | Gate mode + active allow/deny patterns + per-session approval log. |
| `/allow <pattern>`, `/deny <pattern>` | Add patterns to the live gate (and to `.agents/config.json` if writable on the daemon side). |
| `/pricing`, `/pricing refresh`, `/pricing set <id> <in> <out>` | Inspect or override the pricing layer. |
| `/reload` | Re-walk memory + skills + MCP config on the daemon; surfaces per-surface results (`Memory: ✓`, `Skills: ✓`, `MCP: ✗` with errors inline). |
| `/compact [focus]`, `/done [note]` | Trigger summarization or task-boundary checkpoints on the remote agent. The TUI shows an in-chat preamble row during the 5–30 s round-trip. |
| `/btw <question>` | One-shot context-grounded side question. |
| `/subagent <goal>` | Spawn a background subagent on the remote agent (requires `--no-background-agents=false` daemon side). |
| `/tools`, `/subagents` | List the daemon's tool palette and active subagents. |
| `/interrupt` | Cancel the in-flight model turn on the remote. |
| `/reconnect` | Force-reconnect the SSE stream (resumes from `?since=<lastSeq>` — lossless). |
| `/wake` | Pierce a scheduler sleep on the remote. |
| `/sessions` | Pop back to the session picker. |
| `/transcript [path]` | Save the local scrollback to a markdown file (default `/tmp/<sid>.md`). |
| `/theme dark\|light` | Switch glamour theme; re-renders existing assistant messages. |

Sync slashes (`/context`, `/pricing`, `/reload`, `/perms`) hit the corresponding [attach read/mutation endpoints]({{< relref "/docs/reference/configuration.md" >}}) directly. Async slashes (`/compact`, `/done`, `/btw`, `/subagent`) flow through synchronous POSTs that block until the underlying agent operation completes; the remote TUI renders an in-chat preamble row at dispatch to bridge the 5–30 s gap.

## Observer mode (LiveAgent)

When the remote agent is running on its own — `agent.RunAutonomous`, scheduled background subagents, MCP-server-triggered activity, other attached operators' injects — the TUI surfaces every event in the chat scrollback as it happens. You don't have to type anything to see what the agent is doing; attaching is enough.

Operator typing still works: the prompt goes through `POST /inject` and the agent's response streams back through the same observer feed. The scrollback shows the full mixture — your prompts, autonomous turns, subagent activity — in order.

Reconnection is automatic. If the daemon dies (restart, SIGHUP, network drop), the TUI shows a transient error row, retries with exponential backoff (5 s → 30 s cap), and resumes from the last-seen event sequence when the daemon comes back. An operator typing during a backoff window pre-empts the sleep so the next attempt happens immediately. No need to kill the TUI and reattach.

The `Attached as observer` row at the top of the chat marks the start of the live feed.

## Permission prompts

If the remote agent runs in `ask` mode (the default), tool calls that aren't pre-allowed pop a modal in the TUI:

```
┌────────────────────────────────────────────────────────────────┐
│ bash wants to run:                                             │
│                                                                │
│   git push origin main                                         │
│                                                                │
│ [y] allow once     [s] allow session     [v] allow `git *`     │
│ [t] allow tool     [a] allow always      [n] deny              │
└────────────────────────────────────────────────────────────────┘
```

The decision round-trips to the daemon via `POST /perms/respond`; the tool call resumes on the remote side. Picking `a` (allow-always) also persists the pattern to the daemon's `.agents/config.json` so subsequent sessions don't re-prompt.

Operators who want zero prompts can pass `--yolo` to the daemon or pre-populate `.agents/config.json`.

## Layout

```
┌─────────────────────────────────────────────────────────────────┐
│ core-agent-tui  ●  scion  ·  ◇ gemini-3.1-pro-customtools       │  status bar
├─────────────────────────────────────────────────────────────────┤
│ user │ what's the status of the canary?                         │
│                                                                 │
│ asst │ The canary deployment in prod is healthy.                │  scrollback
│      │   • 3/3 pods Ready                                       │  (viewport)
│      │   • last rollout: 2026-05-22 14:03 UTC                   │
│                                                                 │
│   ⚙ kubectl get pods (12.4 KB, 200 OK)                          │  tool call
│                                                                 │
├─────────────────────────────────────────────────────────────────┤
│   ↻ "redeploy the canary"                                       │  queue panel
│   ↻ "check the rollout log"                                     │  (only when non-empty)
├─────────────────────────────────────────────────────────────────┤
│ > _                                                             │  input box
└─────────────────────────────────────────────────────────────────┘
  /help  in: 12.4K  out: 1.9K  $0.12   ↳ this turn $0.03            footer
```

### Queue panel

The strip between the scrollback and the input box renders any operator messages typed while the agent is mid-turn. On turn-end, all queued entries get auto-submitted as a single follow-up turn (with a `↻` marker), wrapped in a system-note framing block so the model knows they arrived mid-task. Soft cap of 10 consecutive auto-continues.

### Status bar

`<alias> · ◇ <model>` (or `<wordmark> · ◇ <model>` when no alias was set). The diamond marks the current model; switch with `/model`.

### Footer

`/help` shortcut + cumulative tokens + cumulative cost + last-turn cost. The last-turn cost is computed client-side from the daemon's cached pricing rates so the footer updates per event without an extra round-trip.

## Keybindings

| Key | Effect |
|---|---|
| **Enter** | Submit input (or run slash command). Mid-turn: queue for after current turn finishes. |
| **Shift+Enter** | Insert a newline in the input |
| **Esc** | Contextual: dismiss a modal if one's open; otherwise interrupt the in-flight turn. |
| **Ctrl+C** (once) | Cancel the in-flight turn |
| **Ctrl+C** (twice within 1s) | Quit the TUI |
| **Ctrl+D** | EOF — quit the TUI |
| **PgUp / PgDn** | Scroll the scrollback |
| **Ctrl+E** | Open `$EDITOR` with the current input buffer (fallback: `$VISUAL` → `vi`) |
| **r** (in picker) | Refresh the session list |

## Read-only mode

When connected to a listener started with `--attach-readonly`, the TUI still works for everything except writes:

- ✅ Session enumeration, live tail, observer mode, `/tools`, `/stats`, `/context`, `/memory`, `/skills`, `/mcp`, `/perms`, `/transcript`
- ❌ Sending messages (typing + Enter), `/wake`, `/inject`, `/interrupt`, `/allow`, `/deny`, `/reload`, `/compact`, `/done`, `/subagent`, `/pricing refresh|set`

Writes surface as red `✗` error lines in the scrollback (the server returns 403; the TUI shows the error rather than failing silently).

## Composition

- **Live stream**: SSE over `GET /sessions/<sid>/events`. Lossless replay via `?since=<seq>` so reconnects don't lose history. The adapter exposes [`coretui.LiveAgent`](https://github.com/go-steer/core-tui/blob/main/tui/agent.go) — core-tui's optional capability for hosts whose agent is observed via a continuous event stream rather than driven by per-turn `Run` calls.
- **Hub-and-spoke**: when the launch URL targets a peer-registration hub, the picker fans `GET /sessions` calls in parallel across the hub + every registered peer, with a 5-second per-peer timeout so a slow peer doesn't block the list.
- **Permissions bridge**: a background goroutine subscribes to `GET /perms/stream` (SSE) for pending prompts; each frame becomes a modal; the operator's decision posts to `POST /perms/respond` and the daemon's blocked `AskApproval` call unblocks.
- **Usage panel**: feeds from the same `CustomMetadata.input_tokens` / `output_tokens` shape that `usage.Tracker` consumes for headless runs. Updates on every model event.

For the full design rationale see [`docs/remote-tui-on-core-tui.md`](https://github.com/go-steer/core-agent/blob/main/docs/remote-tui-on-core-tui.md) and [`docs/remote-tui-observer-mode.md`](https://github.com/go-steer/core-agent/blob/main/docs/remote-tui-observer-mode.md).

## Debug logging

For diagnosing connection / render issues:

```bash
CORE_AGENT_TUI_DEBUG=/tmp/coreagent-tui.log core-agent-tui http://localhost:7777
# in another terminal:
tail -f /tmp/coreagent-tui.log
```

Pairs with `CORE_AGENT_DEBUG=<path>` on the daemon side for a two-file view of an attach session — adapter / bridge / broadcaster / SSE handler all log to whichever file each env var names. Silent unless the env var is set.
