# Embedded TUI: `--local` mode + interrupt + queue panel

Design doc for the operator-facing embedded TUI experience. Builds
on the standalone `cmd/core-agent-tui/` from v1.8.0 by adding a
`--local` flag that spawns the agent + attaches in one command, a
new `POST /sessions/<sid>/interrupt` attach endpoint for ESC-to-break
across both local and remote modes, and a queue panel between the
viewport and input box that surfaces pending injects + their
lifecycle. Untracked sibling to
[`attach-tui-design.md`](attach-tui-design.md),
[`attach-mode-design.md`](attach-mode-design.md),
[`peer-registration-design.md`](peer-registration-design.md).

## Context

`v1.8.0` shipped `cmd/core-agent-tui/` as the remote-attach TUI.
For local interactive use today the operator runs two terminals
(`core-agent --attach-listen=:7777 ...` in one, `core-agent-tui
http://localhost:7777` in another) — fine for development but
ceremony-heavy for the "I just want a TUI session against a fresh
local agent" case. The standalone `core-agent` REPL (line-mode
input + ANSI-streamed output) still works but lacks scrollback,
distinct asst/user bubbles, glamour-rendered markdown, slash
commands, etc.

Three things change with this design:

1. **`core-agent-tui --local`** spawns the agent into the
   background with `--attach-unix-socket`, then attaches to it.
   One TUI codebase serves both local and remote.
2. **Queue panel** — a horizontal strip between the viewport and
   the input textarea showing pending injects + their statuses
   (queued / sending / sent / processing / acked / failed).
   Operators see at a glance what's in flight versus what's
   waiting; the input box doesn't have to be the only feedback
   surface.
3. **ESC interrupts the current turn.** New
   `POST /sessions/<sid>/interrupt` endpoint on the attach server
   propagates a context cancellation into the in-flight model
   call. Both local and remote TUIs use the same path. Between
   turns / autonomous sleeps it's a no-op (well-defined behavior,
   not a hang).

### Settled decisions (do not relitigate)

- **One TUI codebase.** `cmd/core-agent-tui/` is the single
  consumer; `--local` is just a spawn-and-attach mode that ends
  up at the same `attachclient.Stream` machinery as the remote
  case. No build-tag-on-`core-agent` or embedded variant. Settled
  during the earlier embedded-TUI debate; flipped 2026-05-23.
- **Two separate distroless images, one per binary.** Release
  artifacts ship both binaries as a pair (as v1.8.0 already
  does). Container images split: one distroless image carries
  only `core-agent` (agent-pod use case — small, fast pull,
  narrow CVE surface); a second distroless image carries only
  `core-agent-tui` (for `kubectl run -it core-agent-tui` style
  usage, sidecar deployment for in-pod debugging, or workstation
  pull via `docker pull`). Do NOT merge them into one image —
  keeps each image single-purpose and matches the binary
  separation. The v1.8.0 "default binary stays bubble-tea-free"
  invariant stands.
- **Line-mode REPL is not on the chopping block.** Default
  `core-agent` with no flags + a TTY keeps the existing
  line-mode REPL. The TUI is opt-in via `core-agent-tui --local`.
  Non-TTY / CI / piped-stdin environments stay on line-mode.
- **Queue panel applies to BOTH local and remote.** Same UI
  element; same lifecycle model. The local case has lower latency
  but otherwise behaves identically.
- **ESC cancels the in-flight model call, not the agent
  process.** Sessions, tools, event log, registered subagents all
  survive the cancel. Same `turnInterrupter` semantics the REPL
  already uses, just plumbed through HTTP for the remote case.
- **`/interrupt` is a real endpoint, not a `/wake` overload.**
  Wake fires the inbox-arrived signal; interrupt cancels the
  in-flight ctx. Distinct intents → distinct endpoints. Gated by
  `--attach-readonly` like `/inject` and `/wake`.
- **`--local` uses a unix socket, not a TCP port.** No port
  collision concerns, automatic cleanup, no firewall
  considerations. Socket path is `/tmp/core-agent-tui-<pid>.sock`.
- **`--local` cleans up on TUI exit.** Spawned agent process is
  SIGTERM'd; socket file removed. Crash recovery: a stale socket
  + pid file on next launch is detected and either reused (if
  the agent is still alive) or cleaned up first.
- **Bare invocation shows a welcome screen.** `core-agent-tui`
  with no URL and no `--local` doesn't error — it opens a small
  landing screen with two choices: "Spawn a local agent" (the
  `--local` flow) and "Attach to a remote endpoint" (URL input
  field that flows into the session picker). Operators who
  already have a URL pass it as today; operators who don't know
  what they want yet still land somewhere useful instead of an
  error message. Same bubble-tea Model state, third mode
  alongside picker + chat.

## `--local` mode + bare invocation

```bash
core-agent-tui                                            # bare: welcome screen → pick local or remote
core-agent-tui --local                                    # spawn agent w/ defaults, attach
core-agent-tui --local --model=gemini-3.1-pro             # forward --model to spawned agent
core-agent-tui --local -- --provider=anthropic --p '...'  # pass args after `--` to the agent
core-agent-tui https://my-pod.svc:7777                    # direct remote attach (as v1.8.0)
```

### Welcome screen

When invoked with no URL and no `--local`, the TUI opens a small
landing screen instead of erroring:

```
┌─────────────────────────────────────────────────────────────────┐
│ core-agent-tui  ●  no endpoint selected                         │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│   How would you like to start?                                  │
│                                                                 │
│   ▸ Spawn a local agent          (--local equivalent)           │
│     Attach to a remote endpoint  (enter URL)                    │
│                                                                 │
│   ↑/↓ navigate · Enter select · q quit                          │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

Selecting "Spawn local" runs the same flow `--local` triggers
(temp socket, child agent, attach). Selecting "Remote" opens a
URL input field; on submit, the URL becomes the launch URL and
we enter the session picker as if `core-agent-tui <url>` were
invoked. Either way the operator ends up in chat without needing
to know argv conventions upfront.

This is a third bubble-tea mode alongside `picker` and `chat`
(call it `welcome`); same Update/View dispatch pattern.

### Spawn flow

1. **Generate a temp socket path**: `/tmp/core-agent-tui-<pid>.sock`
   (TUI's own pid, before we fork the agent).
2. **Generate a one-shot bearer token**: 32 random bytes, hex-
   encoded. Held in TUI memory only; passed to the agent via env
   `CORE_AGENT_TUI_LOCAL_TOKEN` so it never appears in argv.
3. **Spawn the agent** as a child process with:
   - `--session-db --session-db-path=/tmp/core-agent-tui-<pid>.db`
     (attach-mode requires an event log; in-memory wouldn't survive
     the socket round-trip nicely).
   - `--attach-unix-socket=<socket-path>`
   - `--attach-token=CORE_AGENT_TUI_LOCAL_TOKEN`
   - Anything else the operator passed after the `--` separator
     gets forwarded verbatim.
4. **Wait for the listener.** Poll `GET /sessions` on the socket
   with a 5s timeout. If the agent fails to bind, kill the process
   and surface the agent's stderr to the operator (we capture it
   to a pipe).
5. **Enter the TUI loop** as if invoked with the unix-socket URL
   directly. Session picker is skipped; we know there's exactly
   one session.

### Cleanup

- **Normal exit (Ctrl+C / `/quit`)**: TUI sends SIGTERM to the
  agent child, waits up to 3s for graceful shutdown, then SIGKILL.
  Socket file + session DB + pid file removed.
- **Crash recovery**: on launch, if `/tmp/core-agent-tui-*.{sock,pid}`
  exists for a still-living agent pid, refuse to spawn and prompt
  to attach or kill. If the pid is dead, clean up + proceed.
- **Operator wants to keep the agent running**: `--local --no-cleanup`
  leaves the agent + socket + DB in place on TUI exit. Same flag
  also prints the socket URL on exit so they can re-attach.

### Why unix socket over TCP

- No port allocation race (multiple operators can run `--local`
  simultaneously without coordinating ports).
- Automatic perms (0600 on the socket file means same-user only).
- No firewall / `/etc/hosts` interference.
- `lsof | grep core-agent-tui` shows the local pair clearly.

### Why a one-shot bearer token instead of no auth on a 0600 socket

Defense-in-depth. A malicious local process running as the same
user could open the socket; the bearer requirement means they'd
also need the env var contents. Cost is negligible (16 bytes of
randomness + a constant-time string compare per request).

## Queue panel

A 1- to 5-row strip between the viewport and the input textarea.
Empty queue: invisible (no chrome). Non-empty: shows each pending
injection with its status.

```
┌─────────────────────────────────────────────────────────────────┐
│ core-agent-tui  ●  ...                                          │
├─────────────────────────────────────────────────────────────────┤
│ user │ what's the canary status?                                │
│ asst │ Checking now...                                          │
│                                                                 │
│   ⚙ kubectl get pods (12.4 KB, 200 OK)                          │
├─────────────────────────────────────────────────────────────────┤
│ queue │  ⏳ "and the staging cluster?"   (queued)               │
│       │  ↑ "wake the monitor agent"      (sending)              │
├─────────────────────────────────────────────────────────────────┤
│ > _                                                             │
└─────────────────────────────────────────────────────────────────┘
```

### Lifecycle states

| State | When | Symbol |
|---|---|---|
| `queued` | TUI received the input, hasn't yet POSTed | `⏳` |
| `sending` | POST `/inject` in flight | `↑` |
| `acked` | server returned 200; agent inbox has it | `✓` |
| `processing` | next-turn drain consumed it (we see a `user` event in the SSE stream matching the text) | `…` |
| `done` | model emitted a response after this inject was processed (visual: removed from queue after a short delay) | (fade out) |
| `failed` | POST returned non-2xx OR connection error; surfaces error inline | `✗` |

### Mechanics

- Each Enter on the textarea creates a queue entry with state
  `queued`, immediately advances to `sending` as the
  `injectCmd` tea.Cmd fires.
- The TUI maintains a small ordered list keyed by client-generated
  inject IDs. Server doesn't know about IDs (yet — see open
  question 1); TUI matches `processing` by text equality against
  incoming `user` event bodies. Good enough for v1.
- Failures stick in the queue with the `✗` marker + an error
  message visible on focus. Operator can delete via `Esc` while
  the queue panel is focused.
- **Pause / resume**: `/queue pause` keeps new entries in `queued`
  state without auto-advancing; useful when the operator wants to
  type ahead but not flood the agent. `/queue resume` releases.

### Why a queue panel (not just "type more in the input box")

Three reasons surfaced by the v1.8.0 UAT and embedded-mode use:
- **Type-ahead clarity**: operator can fire `inject A`, then start
  typing `inject B` while A is still processing. Without the
  queue, B's status is invisible; the input box doesn't tell
  the operator whether their text reached the agent.
- **Failure visibility**: server errors / network blips today
  surface as a one-line red error that scrolls past in the
  scrollback. The queue keeps failed sends visible until
  acknowledged.
- **Interruption story**: ESC interrupts the *current turn* but
  doesn't clear the queue. Operator can interrupt A, then watch
  the agent move on to B without manual re-injection.

### Backport to v1.8.0's standalone TUI

Same UI element, same code path, lands in the same PR.

## ESC interrupt + `POST /sessions/<sid>/interrupt`

ESC behavior is contextual:

| Operator state | What ESC does |
|---|---|
| Typing in the input box (textarea has focus) | Clears the input buffer. Nothing posted. |
| Input box empty + a turn is in flight | POST `/sessions/<sid>/interrupt` — cancel the model call |
| Input box empty + no turn in flight | Nothing visible (no-op). Optional toast: "nothing to interrupt." |
| Autonomous loop deferred (between scheduler waits) | POST `/sessions/<sid>/interrupt` — sets a flag the next-turn check honors (no in-flight ctx to cancel) |

### New endpoint

```
POST /sessions/<app>/<sid>/interrupt    (also /sessions/<sid>/interrupt shortcut)
```

**Server side**: each agent holds a `cancelInFlight context.CancelFunc`
populated when `Agent.Run` enters its model-call goroutine, cleared
on Done. `/interrupt` checks if there's a non-nil cancel and
invokes it. Returns:

| Response | Meaning |
|---|---|
| `204 No Content` | Interrupt fired; in-flight model call will return ctx.Canceled |
| `204` with `X-Interrupted: nothing-in-flight` header | No turn in flight; no-op (TUI shows the no-op toast) |
| `412` | Session has no event log (same precondition as `/events`) |
| `403` | `--attach-readonly` is set (interrupt is a write to agent state) |
| `401` | Bearer/mTLS auth failed |

**Gate**: ReadOnly disables this endpoint — consistent with
`/inject` and `/wake`. Operator running in read-only auditor mode
shouldn't be able to break a turn.

**Audit**: each interrupt emits an eventlog row
(`Author="attach/interrupt"`, `CustomMetadata={source: "operator"}`)
so the operator's intervention is part of the audit trail.

### How the agent reacts

- **Mid-turn LLM call**: ctx.Canceled propagates through
  `model.GenerateContent`; the partial response is preserved in
  the eventlog as-far-as-it-got. The agent loop catches the
  cancellation, emits an `interrupted` event, returns the
  iterator. Existing REPL-side `turnInterrupter` already handles
  this shape; we're reusing the same plumbing.
- **Mid-turn tool call**: ctx.Canceled cancels the tool call.
  Tools that block on I/O (e.g. `bash`, `fetch_url`) return
  immediately with the cancel error. The agent reports the
  failure to the model — but since the model call itself has
  also been interrupted, the loop exits cleanly.
- **Between turns (autonomous sleep)**: there's no in-flight ctx
  to cancel. Instead, `interrupt` sets a `wantsStop` flag on the
  AutonomousHandle; the scheduler's next wake checks the flag
  and exits the loop with `StopReasonInterrupted` (new). REPL
  mode between turns: same no-op.

### Why a new endpoint instead of reusing `/wake`

Wake fires the inbox-arrived signal; the agent then runs a NEW
turn to process the inbox. Interrupt cancels the IN-FLIGHT turn.
Different intents; conflating them would force every wake to
also cancel any concurrent work, which is wrong for the inject-
during-running-turn case (operator's intent: "queue this for
after the current turn finishes," not "interrupt").

## Implementation sketch

About **400 LoC + tests** across both PRs.

### PR A — `/interrupt` endpoint + agent cancel-in-flight wiring (~150 LoC + tests)

- `attach/handlers.go` — new handler for qualified + shortcut
  `/interrupt` routes; ReadOnly gating; auth.
- `attach/integration_test.go` — interrupt-in-flight (using a
  stub registrant that exposes a cancel func), interrupt-when-
  idle, ReadOnly-rejects, audit-row-emitted.
- `agent/agent.go` — track `cancelInFlight` per agent; populate
  on Run entry, clear on Done. Expose `Interrupt() bool` (returns
  true if there was something to cancel) for the attach handler.
- `agent/agent_test.go` — interrupt cancels in-flight ctx;
  emits the `interrupted` event; subsequent Run/Inject still works.

### PR B — `--local` mode + welcome screen + queue panel (~280 LoC + tests, builds on PR A)

- `cmd/core-agent-tui/main.go` — `--local` flag, bare-invocation
  handling (no URL → welcome mode), spawn / socket / token /
  wait-for-bind dance, cleanup on exit.
- `cmd/core-agent-tui/welcome.go` — landing screen Model;
  ↑/↓ navigation; Enter on "local" triggers spawn flow, Enter
  on "remote" opens URL input → flows into picker.
- `cmd/core-agent-tui/model.go` — third mode (`welcome`)
  alongside picker + chat in the root dispatch.
- `cmd/core-agent-tui/queue.go` — queue model + render. Lifecycle
  state machine; matching `processing` by inbound user-event text
  (or by `request_id` once PR A ships that).
- `cmd/core-agent-tui/chat.go` — wire queue between viewport
  and textarea; ESC keymap on textarea / chat (clear vs interrupt).
- `cmd/core-agent-tui/queue_test.go` + `welcome_test.go` —
  state transitions, failure handling, pause/resume; welcome
  navigation + transitions.
- `internal/attachclient/client.go` — `Interrupt(ctx, sessionPath)`
  method.
- `docs/site/content/docs/attach-tui.md` — document `--local`,
  bare-invocation welcome screen, and the queue panel; add
  `/interrupt` to the slash-command table (operator can fire it
  explicitly via `/interrupt` too, for non-ESC cases).
- CHANGELOG entry under `[Unreleased]`.

## Out of scope (v1)

- **Mid-turn pause-and-resume**: ESC cancels. Resuming a partial
  turn is much harder (model state is gone; ADK iterator is
  closed). If a consumer asks, we'll design it then.
- **Queue editing / reordering**: queue entries are append-only
  in v1. Editing a `queued` entry before it's sent (or
  reordering) is nice but adds UI complexity.
- **Persisted queue across TUI restarts**: queue lives in TUI
  memory. A crashed TUI loses any not-yet-sent entries. (Server-
  side persistence would require a new `/queue` API surface.)
- **Multi-pane (multiple sessions in one TUI)**: still one
  session at a time. Esc returns to picker; multi-pane is
  v1.x polish.
- **Plugin / extension model**: queue lifecycle states are
  hardcoded.
- **Web-side interrupt UI**: the WebUI sibling design captured
  in `attach-tui-design.md` would inherit `/interrupt` for free.
  Not in scope to ship the WebUI itself.

## Open questions

1. **Server-side inject IDs vs client-side matching.** Today
   `POST /inject` returns `{injected, session}` with no
   server-issued ID. The queue panel matches `processing` state
   by comparing text equality against incoming `user` events.
   Good enough but breaks if the operator sends the same text
   twice — the second one matches the first's processing event.
   **Plan**: server returns a `request_id` on `POST /inject`;
   that ID flows back on the corresponding `user` event via
   `CustomMetadata.request_id`. TUI matches on ID, not text.
   Small additive change to attach surface; worth doing in PR A.
2. **Should ESC in the textarea clear OR interrupt by default?**
   Two competing instincts:
   - "ESC = back out" → ESC always clears input first; interrupt
     requires `/interrupt` or a different key
   - "ESC = stop the agent" → ESC always interrupts if a turn is
     in flight; clear input via Ctrl+U or similar

   **Plan**: contextual (as in the table above) — clears if
   textarea has text, interrupts if empty. Familiar pattern from
   Claude Code's REPL. Easy to flip if operators hate it.
3. **`--local` working dir.** The spawned agent inherits cwd from
   the TUI process. If the operator runs `core-agent-tui --local`
   from `/`, the agent has no `.agents/` to walk to. Add `--cwd`
   to override? Or just inherit + document?  Lean: inherit
   (consistent with how `core-agent` works today). Flag for v1.1
   if a consumer needs more.
4. **TUI exit when spawned agent has an active autonomous loop**:
   should TUI exit force-cancel the loop or detach + leave it
   running? Lean: force-cancel by default (operator can use
   `--no-cleanup` if they want it to keep running).
5. **`/queue` slash commands**: pause/resume/clear sufficient,
   or do we need `/queue ls` etc.? Lean: minimal v1 — the queue
   IS the panel. Slash commands only for state changes.

## Why two PRs

- PR A is small, additive on the attach surface, doesn't require
  TUI changes — useful on its own for scripted operators
  (`curl -X POST .../interrupt` to cancel a runaway turn).
- PR B is bigger and TUI-focused but doesn't touch the agent or
  attach package. Two reviewers, two diffs, clear boundaries.
