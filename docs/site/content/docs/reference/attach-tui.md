---
title: Attach TUI
weight: 8
---

`core-agent-tui` is the operator-facing terminal UI for attach-mode. It ships as a separate binary so the default `core-agent` stays distroless-clean (no terminal-rendering deps land in production K8s images). See [Configuration → attach]({{< relref "/docs/reference/configuration.md" >}}) for the listener-side config and the HTTP/SSE protocol it consumes.

## Why a separate binary

The TUI pulls in [Bubble Tea](https://github.com/charmbracelet/bubbletea), [bubbles](https://github.com/charmbracelet/bubbles), [lipgloss](https://github.com/charmbracelet/lipgloss), and [glamour](https://github.com/charmbracelet/glamour) (plus their transitive deps). For the K8s use case — a long-running headless agent with `--attach-listen` — those deps are pure bloat. Splitting into `cmd/core-agent` and `cmd/core-agent-tui` keeps the agent binary Bubble Tea-free (verifiable with `go list -deps ./cmd/core-agent`) while still shipping a polished operator surface for laptop use.

Two release artifacts:

```
core-agent_<os>_<arch>        # default — K8s, distroless, headless
core-agent-tui_<os>_<arch>    # for laptop operators
```

If you have Go installed: `go install github.com/go-steer/core-agent/cmd/core-agent-tui@latest`.

## Quick start

Two ways to start (v2 removed the local-spawn path — use `core-agent` directly for local interactive use; its bare TTY invocation now lands in the in-process TUI):

```bash
# 1. Bare invocation — welcome screen prompts for a URL.
core-agent-tui

# 2. Remote — point at a running agent's --attach-listen.
ATTACH_TOKEN=$(openssl rand -hex 32) \
  core-agent -p "watch the date forever" --session-db --attach-listen=:7777 \
  --attach-token=ATTACH_TOKEN

core-agent-tui http://localhost:7777 --token=ATTACH_TOKEN
```

For local interactive use, run `core-agent` directly — the Bubble Tea TUI lifted from cogo (see [`embedded-tui-design-v2.md`](https://github.com/go-steer/core-agent/blob/main/docs/embedded-tui-design-v2.md)) is in-process now. `core-agent-tui` is the remote client only.

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
| `--theme=auto\|dark\|light\|notty` | Glamour theme for markdown rendering. `auto` detects from the terminal background. Override with `/theme` at runtime. |
| `--alias=<label>` | Display label for the agent identity in the status bar. Defaults to `<appName>/<sessionID>` (or just `<sessionID>` for the unqualified case). |

`--local` and `--no-cleanup` were removed in v2. For local interactive use, run `core-agent` directly — the TUI is in-process now.

## Welcome screen

Invoking `core-agent-tui` with no URL opens a landing screen with a single input box that accepts slash commands:

```
core-agent-tui  ●  no endpoint selected
────────────────────────────────────────
  Type a command to get started:

    /attach <url>            attach to a remote endpoint
    /help                    show all commands
    /quit                    exit

  > /attach http://localhost:7777_

  Enter run · Esc clear/quit · Ctrl+C quit
```

- `/attach <url>` accepts any of the URL forms above. A bare URL with no `/attach` prefix is also accepted as a convenience: `http://localhost:7777` is treated as `/attach http://localhost:7777`.
- `/help` expands an extra hint pane below the input showing the URL grammar and chat-mode commands.

Errors surface inline under the input box (e.g. connection errors when `/attach` can't reach the endpoint).

You can also return to this screen at any time from a chat session with `/welcome` or switch endpoints directly with `/attach <url>`.

**`/spawn` was removed in v2.** Earlier versions could spawn a local `core-agent` subprocess; v2 removed that path because `core-agent` now ships its own in-process TUI. For local interactive use, run `core-agent` directly. Typing `/spawn` in core-agent-tui surfaces an error pointing you at the new path.

## Layout

```
┌─────────────────────────────────────────────────────────────────┐
│ core-agent-tui  ●  core-agent/sess-xyz  ·  http://localhost:7777│  status bar
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
│   ↑ "redeploy the canary"   (sending)                           │  queue panel
│   ✓ "check the rollout log" (acked)                             │  (only when non-empty)
├─────────────────────────────────────────────────────────────────┤
│ > _                                                             │  input box
└─────────────────────────────────────────────────────────────────┘
  /help · gemini-3.1-pro · in 12.4K · out 1.9K · $—                   footer
```

Tool calls collapse to a single icon-prefixed line by default; the design captures expand-on-focus as v1.1 polish.

### Queue panel

The strip between the scrollback and the input box renders one row per inject the operator submitted, each labeled with a lifecycle glyph:

| Glyph | State | Meaning |
|---|---|---|
| ⏳ | queued | submitted; POST `/inject` hasn't fired yet |
| ↑ | sending | POST in flight |
| ✓ | acked | server returned 200; in the agent's inbox |
| … | processing | matching `user` event observed in the SSE stream |
| · | done | model emitted a response; fades from the panel after ~2s |
| ✗ | failed | POST returned non-2xx or network error |

The panel is empty (and consumes zero rows) when no entries are in flight. Failed rows include the error message and stick around until you focus the panel and press **Esc** to dismiss.

## Slash commands

Type `/` followed by a command name in the input box and press Enter.

| Command | Effect |
|---|---|
| `/help` | Print the command list + keybindings into the scrollback. |
| `/quit`, `/exit` | Leave the TUI cleanly. |
| `/clear` | Clear the local scrollback (server log is untouched). |
| `/sessions` | Pop back to the session picker. |
| `/welcome` | Pop all the way back to the welcome landing screen (lets you switch between local-spawn and remote without restarting). |
| `/attach <url>` | Disconnect from the current endpoint and attach to a new one. Accepts the same URL forms as the CLI. |
| `/interrupt` | `POST /interrupt` — cancel the in-flight model turn. Server returns `interrupted: false` when nothing was running (you'll see "no in-flight turn" in the log). |
| `/reconnect` | Force-reconnect the SSE stream (resumes from `?since=<lastSeq>` — lossless). |
| `/wake` | `POST /wake` — pierce a scheduler sleep. |
| `/inject <msg>` | Same as typing + Enter; useful for `/inject ` + paste of multi-line text. |
| `/theme auto\|dark\|light\|notty` | Switch glamour theme; re-renders existing asst messages. |
| `/tools` | List tools available to this agent (with source + gate state). |
| `/subagents` | List background subagents. |
| `/status` | Show model + run state. |
| `/peers` | List peers when connected to a hub. |
| `/transcript [path]` | Save the scrollback to a markdown file (default `/tmp/<sid>.md`). |

## Keybindings

| Key | Effect |
|---|---|
| **Enter** | Submit input (or run slash command) |
| **Shift+Enter** | Insert a newline in the input |
| **Ctrl+E** | Open `$EDITOR` with the current input as a buffer (fallback chain: `$VISUAL` → `$EDITOR` → `vi`) |
| **PgUp / PgDn** | Scroll the scrollback |
| **Esc** | Contextual: clears the input box if it has text; otherwise sends `/interrupt` to cancel the in-flight model turn. From the welcome or picker screens it quits. |
| **Ctrl+C** | Quit |
| **r** (in picker) | Refresh the session list |

## Read-only mode

When connected to a listener started with `--attach-readonly`, the TUI still works for everything except writes:

- ✅ Session enumeration, live tail, `/tools`, `/status`, `/agents`, `/peers`, `/transcript`
- ❌ Sending messages (typing + Enter), `/wake`, `/inject`, `/interrupt`

Writes surface as red `✗` error lines in the scrollback (the server returns 403; the TUI shows the error rather than failing silently).

## Composition

- **Live stream**: SSE over `GET /sessions/<sid>/events`. Lossless replay via `?since=<seq>` so reconnects don't lose history.
- **Hub-and-spoke**: when the launch URL targets a peer-registration hub, the picker fans `GET /sessions` calls in parallel across the hub + every registered peer, with a 5-second per-peer timeout so a slow peer doesn't block the list.
- **Permissions audit**: `/tools` surfaces each tool's `gate_state` (`allowed` / `denied` / `prompted` / `denied-allow-mode`) sourced from `permissions.Gate.Snapshot()` — operators can see what's gated without consulting the source.
- **Usage panel**: feeds from the same `CustomMetadata.input_tokens` / `output_tokens` shape that `usage.Tracker` consumes for headless runs. Updates on every model event.

For the full design rationale see [`docs/attach-tui-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/attach-tui-design.md).

## Future

The same attach-mode endpoints that drive this TUI would drive a WebUI trivially — SSE is a browser primitive, the read endpoints are JSON, the writes are POSTs. Captured as a "future companion" in the design doc; not in scope for v1 of the TUI.
