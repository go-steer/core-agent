J# attach-tui: a bubble-tea TUI consumer for attach-mode

Design doc for the operator-facing TUI that consumes attach-mode
endpoints. Ships as its own binary (`cmd/core-agent-tui/`) so the
default `core-agent` stays distroless-clean. Untracked sibling to
[`attach-mode-design.md`](attach-mode-design.md),
[`peer-registration-design.md`](peer-registration-design.md),
[`fetch-url-design.md`](fetch-url-design.md),
[`shared-memory-design.md`](shared-memory-design.md).

## Context

Attach-mode (PR #11) shipped a working HTTP/SSE protocol and a
line-mode CLI consumer (`core-agent attach <url>` /
`core-agent ls <url>`). That's fine for "is this thing alive" but
rough for actual operator workflows:

- No scrollback — events scroll past, gone.
- Stdin input is interleaved with stream output instead of separated.
- Tool calls render as raw JSON-ish blobs; long tool results dominate
  the terminal.
- No surface for the agent's tool catalog, MCP servers, peers, or
  background subagents — operator has to know to hit `ls` separately
  or curl the right endpoint.
- Markdown / code that the model emits renders as plain text.

A real TUI closes this gap without needing to wait for a separate
product. The Bubble Tea chat example is the right reference shape —
~150 lines for the local-only case; ours adds an SSE client + a
handful of slash commands + glamour rendering + sets us at maybe
~600 LoC of bubble tea code on top of the existing attach SSE
client.

### Settled decisions (do not relitigate)

- **Separate binary at `cmd/core-agent-tui/`, not a build-tag
  subcommand of `core-agent`.** Earlier drafts of this doc
  proposed a `//go:build tui` subcommand for one-name discovery,
  but since the release pipeline ships two artifacts anyway,
  the tag mechanics (stub file, dispatch dance, `-tags tui` in
  CI, "rebuild with -tags tui" hostile UX in the default binary)
  bought nothing. Two binaries, two names: **`core-agent`** runs
  the agent; **`core-agent-tui`** is the operator's attach
  consumer. Resolved 2026-05-23.
- **Shared client code lives in `internal/attachclient/`.** The
  plumbing in `cmd/core-agent/attach.go` (URL parsing, bearer-token
  auth, `?since=N` reconnect, event decoding) moves to a small
  package importable by both `cmd/core-agent/` (the existing
  `attach`/`ls` subcommands) and `cmd/core-agent-tui/` (the TUI).
  No duplication.
- **Bubble Tea + Bubbles + Lipgloss + Glamour.** All from
  charmbracelet, one ecosystem, no impedance mismatch. Glamour for
  markdown / syntax highlighting. Lipgloss for the layout +
  bordering. Bubbles for the textarea + viewport + spinner
  primitives. No third-party render libraries.
- **Two-pane layout for v1, no tabs / multi-session split.**
  Session picker at startup → single session full-screen
  (scrollback chat + input box + status bar). Esc returns to picker.
  Multi-session tmux-style tiling is out of scope.
- **Read-only mode is structural.** When connected to an
  `--attach-readonly` listener, the input box is hidden and a
  banner reads "read-only — POST /inject disabled by server."
  No "looks editable but errors on send" footgun.
- **Markdown rendering is finalized-on-message-complete, not
  per-token.** While a model message streams, the partial body
  renders as plain monospace (so you can see tokens land); when
  the message closes, it re-renders through glamour. Live glamour
  per-token would re-parse the buffer hundreds of times per turn
  and would flicker.
- **All slash commands are local-state-only by default.** Anything
  that mutates server state (`/wake`, `/inject`, future `/pause`)
  goes through the existing attach endpoints. The TUI is a
  *consumer* of attach-mode, not a sidecar control plane.
- **Glamour theme defaults to `auto`** with `--theme` flag and
  `/theme` slash command for the muxer-aware override case
  (resolved 2026-05-23).
- **Session picker enumerates hub-local + peer sessions** when
  the launch URL is a hub. Each peer's `/sessions` is fetched in
  parallel at startup (bounded concurrency, 5-second per-peer
  timeout — a slow peer doesn't block the picker). On a non-hub
  URL, only the local listener's sessions appear. **Direct-jump
  shortcut**: `core-agent tui <url>/sessions/<sid>` skips the
  picker entirely and enters that session. Resolved 2026-05-23.
- **Status-bar identity shows whatever is available**: prefer
  `agent name` → fall back to `appName/sessionID`. `--alias
  <label>` flag overrides for operators who want a friendlier
  display. Resolved 2026-05-23.
- **Editor chain is `$VISUAL` → `$EDITOR` → `vi`** for `Ctrl+E`
  handoff. Windows / non-bash shells get the same fallback chain
  with no special-case logic — Windows operators set their own
  `$EDITOR=notepad.exe` (or whatever) before launching the TUI.
  Resolved 2026-05-23.
- **Tool gate state surfaces additively in PR A, hidden in TUI
  v1.** `permissions.Gate` grows a `Snapshot()` method returning
  `{allow, deny, mode}`; `GET /sessions/<sid>/tools` carries a
  `gate_state: "allowed|denied|prompted|<empty>"` field per
  tool. TUI v1 fetches but ignores the field — UI surface lands
  in v1.1 as a column in the `/tools` modal. Plumbing-first so
  the endpoint doesn't need a post-hoc retrofit. Resolved
  2026-05-23.

## Layout

```
┌─────────────────────────────────────────────────────────────────┐
│ core-agent  ●  hub.svc:7777 · sess-7f4e2a · monitor-prod        │  ← status bar (1 line)
├─────────────────────────────────────────────────────────────────┤
│ user │ what's the status of the canary?                         │
│                                                                 │
│ asst │ The canary deployment in prod is healthy.                │  ← scrollback pane
│      │   • 3/3 pods Ready                                       │    (viewport, takes
│      │   • last rollout: 2026-05-22 14:03 UTC                   │     all remaining
│      │                                                          │     vertical space)
│ ⚙    │ kubectl get pods -n canary -o json    (12.4 KB, 200 OK)  │  ← collapsed tool call
│                                                                 │     (Enter expands)
│ user │ wake up                                                  │
│                                                                 │
│ ⏱    │ scheduler defer: woke at T+45s                           │
├─────────────────────────────────────────────────────────────────┤
│ > _                                                             │  ← input box (3 lines
│                                                                 │     visible, grows
│                                                                 │     up to ~10)
└─────────────────────────────────────────────────────────────────┘
  /help · gemini-3.1-pro · in 12.4K · out 1.9K · $0.018              ← footer (1 line)
```

- **Status bar**: connected URL, session ID, agent name, connection
  health dot (●/○).
- **Scrollback pane**: bubbles' `viewport` over a styled chat log.
  PgUp/PgDn scroll; auto-scrolls to bottom on new events unless the
  operator has scrolled up.
- **Tool call rendering**: collapsed by default (icon + tool name +
  truncated args + status + size). Focus + Enter expands to show
  full args + result; Esc collapses. Errors render in red.
- **Input box**: bubbles' `textarea`. Enter sends; Shift+Enter adds a
  newline. Ctrl+E opens `$EDITOR` for long-form composition (Claude
  Code's pattern).
- **Footer**: live usage panel — model name, cumulative input tokens,
  cumulative output tokens, running cost estimate. Updates on every
  `model` event arrival. See "Token + cost display" below for the
  data flow. Reconnect countdown displaces the usage panel when a
  reconnect is in flight.

## Markdown + code rendering

Use [`glamour`](https://github.com/charmbracelet/glamour) — same
charmbracelet ecosystem as bubble tea. Standard markdown plus syntax
highlighting (via Chroma underneath). Glamour ships theme presets;
expose `auto` / `dark` / `light` / `notty` via `--theme` and `/theme`.

Streaming policy:

- While a model message is mid-stream, render the partial body as
  plain monospace inside a faint border, so the operator sees tokens
  arrive.
- On `partial=false` (the final delta of a message), re-render the
  full body through glamour and replace the in-place plain version.
- Tool results that are valid JSON pretty-print on display (we
  already have the structured value in the eventlog event).

## Slash commands

Two tiers:

### v1 (works against today's attach-mode surface)

| Command | What it does |
|---|---|
| `/help` | Lists commands + keybindings in a modal. |
| `/quit`, `/exit` | Cleanly exit (closes SSE, releases terminal). |
| `/clear` | Clear scrollback (purely visual — server log unchanged). |
| `/sessions` | Pop back to the session picker. |
| `/reconnect` | Force-reconnect the SSE stream (uses `?since=N` to resume losslessly). |
| `/wake` | `POST /sessions/<sid>/wake` — pierce a scheduler sleep. |
| `/inject <msg>` | Same as typing + Enter; useful for `/inject ` + paste of multi-line text. |
| `/theme auto\|dark\|light` | Switch glamour theme. |
| `/since <N>` | Reconnect with `?since=N` to replay from a sequence number. |
| `/peers` | When connected to a hub URL, modal with `GET /peers` output. Skips on a non-hub. |
| `/transcript [path]` | Save the current scrollback to a markdown file (default `/tmp/<sid>.md`). |

### v2 (needs new attach-mode endpoints — see next section)

| Command | Needs |
|---|---|
| `/tools` | `GET /sessions/<sid>/tools` — agent's full tool catalog (built-in + MCP + skills). |
| `/mcp` | `GET /sessions/<sid>/tools?source=mcp` — MCP tools only with server attribution. |
| `/subagents` | `GET /sessions/<sid>/agents` — background subagents spawned by this agent (BackgroundAgentManager state). |
| `/status` | `GET /sessions/<sid>/status` — running / paused / deferred + `next_wake_at`. |
| `/pause`, `/resume` | `POST /sessions/<sid>/pause` and `/resume` — control the autonomous loop. |
| `/spawn <prompt>` | `POST /sessions/<sid>/spawn` — operator-initiated background subagent (parity with the `spawn_agent` tool). |

The v2 commands are listed so the TUI doesn't ship them as
broken stubs. As each attach endpoint lands, the TUI flips the
corresponding command on.

## New attach-mode endpoints (proposed, additive)

For the `/tools`, `/mcp`, `/subagents`, `/status` commands above —
worth scoping here so it's part of one design conversation, even
though the endpoints would ship in a separate PR on the attach
side:

```
GET /sessions/<app>/<sid>/tools
  → [{name, description, source: "builtin|mcp|skill", server?: "<mcp-server>"}]

GET /sessions/<app>/<sid>/agents
  → [{id, name, status, started_at, parent_session_id, last_report?}]

GET /sessions/<app>/<sid>/status
  → {state: "running|paused|deferred|idle", next_wake_at?, current_tool?}
```

Read-only, gate-checked the same as `GET /sessions`. Pure
projections over in-memory state — no new persistence, no schema
churn. Estimated ~150 LoC additive on `attach/handlers.go`.

Pause/resume/spawn endpoints are deferred — they imply mutating
the agent's runtime state from outside, which intersects with the
existing `Agent.Inject` / `AutonomousHandle` surface and deserves
its own design conversation. Capture as "v3" rather than slipping
into this doc.

## Input affordances

- **Enter** sends; **Shift+Enter** newline.
- **Ctrl+E** opens `$EDITOR` with the current input as a buffer;
  on exit, the buffer becomes the input. Same pattern as Claude
  Code; great for paste-heavy or multi-paragraph prompts.
- **Up / Down** in an empty input cycle through history (last 50
  injects this session, in-memory only).
- **Tab** at the start of input shows slash-command completions
  in a popup; with text already typed, completes partial commands
  (`/sub<Tab>` → `/subagents`).
- **Ctrl+L** clears the scrollback (alias for `/clear`).
- **Ctrl+C** acts as a "cancel current selection" first (close
  modal, collapse expanded tool, etc.) and only quits on a
  second press within 1s — mirrors the REPL's double-Ctrl+C
  semantics.

## Token + cost display

The footer surfaces a live four-field usage panel:

```
gemini-3.1-pro · in 12.4K · out 1.9K · $0.018
```

Fields are **cumulative across the session** (matching how an
operator thinks about a single conversation's spend), not per-turn.
Per-turn breakdown is available on a tool-result expansion in v2,
but the headline number is "what has this session cost me so far."

### Data flow

- Each `model` event in the eventlog carries usage in
  `CustomMetadata` (`{input_tokens, output_tokens, ...}`) — same
  shape the existing `usage.Tracker` consumes for headless runs.
- On every `model` event the TUI receives via SSE, the model
  emits a `tea.Msg` that updates the cumulative `{in, out}`
  counters and triggers a re-render of just the footer.
- Cost is computed by calling the bundled `usage.PriceFor(modelName, cfg)`
  helper on the cumulative totals. Not authoritative for billing
  — informational only.
- **Model name** comes from the new `GET /sessions/<sid>/status`
  endpoint (added to PR A's `status` shape — see below). One
  fetch at session-enter; the TUI re-fetches if a `session_changed`
  event ever lands (none defined today, future-proofs against it).

### Endpoint shape touch-up

Earlier in this doc, `GET /sessions/<sid>/status` was sketched as
`{state, next_wake_at?, current_tool?}`. Extend to:

```
GET /sessions/<app>/<sid>/status
  → {
      state:        "running|paused|deferred|idle",
      model_name:   "gemini-3.1-pro-preview-customtools",
      next_wake_at: "...",   // optional
      current_tool: "..."    // optional
    }
```

Pure projection over already-in-memory session state — no new
plumbing in `agent.Agent`, just exposing what's already there.

### Numeric formatting

- `in/out`: SI suffix at thresholds — `<1000` → raw (`834`),
  `<1M` → `K` with one decimal (`12.4K`), `>=1M` → `M`
  (`1.4M`). Avoids the "what is `12483` even" parsing cost.
- `cost`: USD with two decimals at `$0.01+`, scientific
  (`$1.4e-3`) below that threshold. Operators eyeballing for
  "is this expensive" want the magnitude immediate.

### Display swap during reconnect

When the SSE stream drops, the usage panel swaps for the
reconnect countdown (`auto-reconnect 27s`) on the same line.
Once reconnect succeeds, the usage panel returns. Don't show
both — one line, one purpose.

## Reconnect + resume

SSE drops happen (network blips, hub restart). Behavior:

- **Detect**: on SSE error or stream EOF.
- **Banner**: footer shows "reconnecting in Ns…" with countdown.
  Backoff: 1s, 2s, 4s, 8s, max 15s.
- **Resume**: `?since=<last-seq>` — replay missed events first,
  then resume live stream. The lossless replay is already a
  property of the protocol; the TUI just has to honor it.
- **Scrollback survives** — we keep the full event log in memory
  for the session's lifetime, so reconnect doesn't lose history.

## Binary layout

Two `main` packages plus one shared internal package:

```
cmd/
├── core-agent/                  # existing agent binary
│   ├── main.go                  # dispatches to attach / ls / etc.
│   ├── attach.go                # existing — line-mode attach client
│   └── ...                      # everything that ships today
└── core-agent-tui/              # NEW — TUI binary
    ├── main.go                  # flag parsing + bubble tea entry
    ├── model.go                 # tea Model + Update + View
    ├── chat.go                  # chat-pane rendering (uses glamour)
    ├── input.go                 # textarea + history + editor handoff
    ├── status.go                # status bar + footer (usage panel)
    ├── slash.go                 # slash-command dispatch
    └── *_test.go

internal/attachclient/           # NEW — shared SSE / URL / auth code
├── client.go                    # parsedAttachURL, bearer auth, streamEvents
├── client_test.go
└── ...
```

Why `internal/attachclient/` rather than a sibling under `cmd/`:
the shared helpers aren't part of the public API (per the
stability promise in `CHANGELOG.md`), and Go's `internal/` rule
means only packages under `github.com/go-steer/core-agent/...`
can import it — exactly the scope we want. The existing
`cmd/core-agent/attach.go` shrinks as it moves logic into the
shared package; the TUI binary imports it as a peer.

The `core-agent` binary **never imports bubble tea, lipgloss,
glamour, or bubbles** — they only land in `cmd/core-agent-tui/`.
Verifiable with `go list -deps ./cmd/core-agent/`.

Testing:

- `go test ./...` — both binaries' tests run as part of the
  standard suite. No special invocations, no build-tag gymnastics.
- The TUI tests use bubble tea's testing utilities
  (`tea.NewProgram(..., tea.WithoutRenderer())` + scripted msgs)
  for deterministic snapshots; standard pattern.

## Release pipeline

Two artifacts per (OS, arch) tuple, one per `cmd/`:

```
core-agent_linux_amd64       # built from cmd/core-agent      — distroless, K8s, headless
core-agent-tui_linux_amd64   # built from cmd/core-agent-tui  — laptop operators
```

One extra row in the goreleaser matrix; no build-tag toggling.
Distroless container image bundles only `core-agent` (smaller
image, fewer transitive deps in scan reports). The TUI artifact
is laptop-only.

## Implementation sketch

About **600 LoC TUI + 100 LoC extracted shared client + 150 LoC
new attach endpoints + tests** total. Two PRs.

### PR A — attach-mode read-only endpoints (~150 LoC + tests)

- `attach/handlers.go` — three new handlers (`/tools`, `/agents`,
  `/status`) + table registration in the mux.
- `attach/integration_test.go` — round-trip tests.
- Lands first so the TUI doesn't ship dead `/tools` commands.

### PR B — TUI itself (~700 LoC + tests, separate binary)

- `internal/attachclient/` — extracted SSE / URL / bearer-auth
  helpers; `cmd/core-agent/attach.go` migrated to import from it.
- `cmd/core-agent-tui/main.go` — flag parsing, bubble tea entry.
- `cmd/core-agent-tui/{model,chat,input,status,slash}.go` — the
  actual TUI (see binary layout above).
- `cmd/core-agent-tui/*_test.go` — bubble tea scripted-msg tests.
- `.goreleaser.yml` — second `builds:` entry for `cmd/core-agent-tui`.
- `docs/site/content/docs/` — new "Attach TUI" page.
- CHANGELOG entry under `[Unreleased]`.

## Out of scope (v1)

- **Multi-session tiling** (tmux-style multiple chat panes). One
  session full-screen, picker between. Add later if a consumer asks.
- **Peer browser pane**. `/peers` modal is enough; a persistent
  sidebar is feature creep.
- **Diff view** between two model messages. Useful but niche.
- **`/copy` to system clipboard**. Cross-platform clipboard is a
  rabbit hole; operators can `/transcript` and copy from the file.
- **Vim mode / command mode**. Slash commands cover the same
  ground without a mode model.
- **Drag-and-drop file attachments**. Out of scope for attach-mode
  itself (would need a multipart upload endpoint).
- **Plugin / extension API**. Slash commands are not pluggable in
  v1; if a consumer wants custom commands, fork the file. Plugin
  systems are a meaningful complexity tax that early TUIs almost
  always regret shipping.
- **Mouse support** — bubble tea has it, but defaulting to off
  keeps keyboard discoverability paramount and avoids the "click
  did nothing" surprise on terminals without mouse-tracking.

## Future companion: WebUI

The same attach-mode endpoints that drive the TUI drive a WebUI
trivially. Server-Sent Events is natively a browser primitive
(`new EventSource(...)`); the read-only endpoints (`/sessions`,
`/tools`, `/agents`, `/status`, `/peers`) are plain JSON; the
write endpoints (`/inject`, `/wake`) are simple POSTs. A WebUI
is therefore **not a separate product** — it's a different
consumer of the same surface. Two ways it could land:

1. **Static SPA served by the agent itself.** `core-agent
   --attach-listen=:7777 --attach-webui` mounts a small embedded
   `embed.FS` of HTML+CSS+JS at `/` next to the existing API
   endpoints. Zero new deps for operators; build-tag gate it
   (`//go:build webui`) so default binary stays static-asset-free.
2. **Separate static-host deployment.** Point a hosted React/htmx
   app at any attach-mode URL with CORS configured. Operators
   running large fleets get a single dashboard against many
   hubs.

Captured here as the architectural property ("attach-mode is
UI-framework-agnostic") rather than as a near-term commitment.
**Out of scope for this design doc**; deserves its own when a
consumer asks. The TUI itself doesn't preclude either path —
both are additive consumers of the same protocol.

## Open questions

None — all resolved 2026-05-23. Settled-decisions section above
is authoritative. Future questions (post-implementation feedback,
v1.1 scope, additional slash commands) get appended below as
they surface, with the same `Resolved <date>: <decision>`
format so the doc stays a living record rather than a frozen
snapshot.

## Why two PRs and in this order

PR A (read-only endpoints) is small, additive, and useful on its
own — operators can `curl /sessions/<sid>/tools` from a shell
script even without the TUI. Lands first so the TUI doesn't have to
either ship broken stubs or hold the endpoint work in the same
review.

PR B (the TUI itself) is the bigger one but it's pure consumer
code — no risk to the existing attach surface. Build-tag isolation
means a bad TUI day doesn't affect the default binary.
