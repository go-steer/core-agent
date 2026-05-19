# Scion harness improvements: soft interrupt + autonomous handle

Design doc for the v1.3.0 milestone. Untracked sibling to
`docs/background-subagents-design.md`,
`docs/cogo-core-agent-integration.md`,
`docs/docsy-migration-notes.md`.

## Context

core-agent is being positioned as a programmatic harness inside
Scion, competing with gemini-cli and Claude Code on the embedding
axis (not the human-TUI axis). Our substrate already beats them on
multi-provider, eventlog, autonomous + crash-resume, and dynamic
background subagents.

The biggest remaining gap for harness use is **interrupt plumbing**.
Today the Scion adapter scans stdin between turns
(`extras/scion-agent/main.go:265`), which means a long-running tool
call blocks message delivery for as long as the call takes. A
Scion orchestrator nudging the agent with a new priority or a
shutdown signal has to wait until the current turn ends.

The companion gap is **programmatic control over an autonomous
run** — `agent.RunAutonomous` is fire-and-forget; you can't pause,
resume, or inject mid-run from outside.

This release fixes both. The remote-spawner reference
(`extras/scion-remote-agent/`) and the hard-interrupt-with-redirect
semantics are explicitly deferred (see Deferred section).

## What ships in v1.3.0

Three coordinated pieces. They land in one release because they
share substrate and the docs / Scion-adapter rewrite touch all
three at once.

### 1. `Agent.Inject(message)` — soft interrupt for any consumer

A queue on `agent.Agent` that arbitrary callers (a harness goroutine
reading from stdin, an orchestrator wiring an HTTP endpoint, a test
harness) push messages onto. The pre-turn drain in `Agent.Run`
prepends them to the prompt the model sees on its **next turn**.

```go
// New on agent.Agent
func (a *Agent) Inject(message string) error
```

Format prepended to the next user prompt (sibling to the existing
`[Background reports]` block from v1.2.0):

```text
[Inbox]
- new message from orchestrator: deadline moved up to 14:00
- new message from orchestrator: pause file writes until further notice

---
<original user prompt or "continue">
```

Reuses the v1.2.0 alert machinery almost wholesale:

- Same drop-oldest backpressure pattern as
  `BackgroundAgentManager.pushAlert` (`agent/background.go:pushAlert`),
  so a stuck consumer can't deadlock a runaway producer.
- Same pre-turn drain pattern as `PrependPendingAlerts`
  (`agent/background_report.go`), extended to a second separate
  channel/queue so inbox and alerts each get their own formatted
  block.

Why separate from the alert channel:

- Different origin (external orchestrator vs internal subagent)
- Different format (`[Inbox]` vs `[Background reports]`)
- Different lifetime (alert channel lives on the manager; inbox
  lives on the agent itself, so consumers without
  `BackgroundAgentManager` still get Inject)

Companion: `Agent.InboxArrived() <-chan struct{}` — fires when a new
message lands, so the harness can decide to start a new turn instead
of polling.

### 2. `agent.StartAutonomous(...) (*AutonomousHandle, error)`

Programmatic control over an autonomous run. Returns immediately
with a handle; the run goroutine starts in the background.

```go
type AutonomousHandle struct { /* ... */ }

func StartAutonomous(ctx context.Context, build BuildFunc, goal string, opts ...AutonomousOption) (*AutonomousHandle, error)

func (h *AutonomousHandle) Pause() error
func (h *AutonomousHandle) Resume() error
func (h *AutonomousHandle) Stop() error                  // hard cancel via ctx
func (h *AutonomousHandle) Inject(message string) error  // delegates to underlying Agent.Inject
func (h *AutonomousHandle) Status() AutonomousStatus     // Running / Paused / Stopped / Completed / Failed
func (h *AutonomousHandle) Wait() (RunResult, error)     // blocks until terminal
```

Semantics:

- **Pause** sets a flag the loop checks before each turn. The
  currently-running turn finishes normally; the next turn waits on a
  cond variable until `Resume()`. Matches the per-turn checkpoint
  cadence the eventlog already uses
  (`agent/autonomous.go:219` `emitCheckpoint`). Synthetic pause +
  resume events are emitted to the eventlog
  (`Author="<binary>/autonomous"`, same convention as existing
  checkpoint events) so the audit trail records the gap.
- **Resume** signals the cond variable.
- **Stop** cancels the run's context. Current LLM call returns
  `Canceled`; loop exits; goroutine cleans up. Idempotent.
- **Inject** is a thin wrapper around the underlying Agent.Inject.
  The autonomous loop's continuation prompt is "continue" by
  default; injected messages get prepended naturally on the next
  turn.
- **Status** returns the current lifecycle state.
- **Wait** blocks until the run goroutine exits; returns the same
  `RunResult` shape RunAutonomous returns today.

`RunAutonomous` stays as a synchronous convenience that internally
calls `StartAutonomous(...).Wait()`. Existing callers don't change.

If a consumer needs immediate mid-turn interrupt, they use `Stop()`
(hard cancel via ctx.Cancel) rather than `Pause`. "Pause-and-resume
mid-turn" is the same design work as `Redirect` and will ship
together if a real consumer hits the seam.

### 3. Scion adapter rewrite to use Inject

Today `extras/scion-agent/main.go:257-282` is:

```go
for {
    if !scanner.Scan() { return ExitOK }   // blocking
    line := scanner.Text()
    runOneTurn(line)                       // also blocking
}
```

Replace with:

```go
// Stdin → inbox goroutine
go func() {
    sc := bufio.NewScanner(os.Stdin)
    for sc.Scan() {
        _ = ag.Inject(sc.Text())
    }
}()

// Main loop: wait for inbox arrival, run a turn, repeat
for {
    select {
    case <-ctx.Done():
        return ExitOK
    case <-ag.InboxArrived():
        runOneTurn("continue")   // Inject-drained pre-prompt fires
    }
}
```

Net effect: messages arriving during a turn no longer block. They
queue on the inbox; the next turn picks them up. Hard stop via
SIGINT/SIGTERM still works through the existing
`signal.NotifyContext` at line 98.

The `--input` flag still works — it becomes the first `Inject`
before the main loop starts.

## Critical files

**New:**

- `agent/inbox.go` — `Inject`, `inboxArrived` channel, queue type,
  drain helper, format function
- `agent/inbox_test.go` — drain semantics, drop-oldest backpressure,
  prepend formatting
- `agent/autonomous_handle.go` — `StartAutonomous`,
  `AutonomousHandle`, `AutonomousStatus`, all lifecycle methods
- `agent/autonomous_handle_test.go` — pause/resume across turns,
  stop unwinds the goroutine, inject during run lands on next turn

**Modified:**

- `agent/agent.go` — add `inbox` field on `Agent`; extend `Run` to
  drain inbox alongside alerts via the new helper. `Agent.Inject`
  and `Agent.InboxArrived` accessors.
- `agent/autonomous.go` — extract the loop body so `StartAutonomous`
  can host it in a goroutine; honor pause flag in the per-turn
  checkpoint block; emit pause + resume as synthetic events
  (`Author="<binary>/autonomous"` matching
  `agent/autonomous.go:106-122`).
- `agent/background_report.go` — keep `PrependPendingAlerts`
  background-only; new inbox drain is its own function. Both feed
  `Agent.Run`'s pre-turn prepend in order: alerts first, then inbox,
  then the original prompt.
- `extras/scion-agent/main.go` — full rewrite of the main loop per
  the snippet above.
- `extras/scion-agent/README.md` — update to describe the new
  stdin-to-inbox model and the non-blocking interrupt semantics.
- `cmd/core-agent/main.go` — no functional change; expose Inject via
  REPL slash command in a future release if useful.
- `CHANGELOG.md` — `[1.3.0]` entry covering all three pieces.
- `README.md` — feature bullet under harness positioning.
- `docs/site/content/docs/library-api.md` — new "Soft interrupt and
  programmatic control" section.
- `docs/site/content/docs/scion-adapter.md` — update.

## Reused (no changes)

- The pre-turn drain hook in `Agent.Run` (`agent/agent.go:Run`,
  post-v1.2.0) — already calls `bgMgr.PrependPendingAlerts`; just
  extends to also drain the inbox.
- Drop-oldest backpressure pattern from
  `BackgroundAgentManager.pushAlert` (`agent/background.go`).
- `RunAutonomous` budget options, retry policy, checkpoint emission
  (`agent/autonomous.go:357-476`) — `StartAutonomous` shares them
  via the same `AutonomousOption` slice.
- Eventlog Author convention `<binary>/autonomous`
  (`agent/autonomous.go:106`) for new pause/resume synthetic events.

## Phased delivery within v1.3.0

Single release, single tag. Internal commit boundaries for review:

1. **Phase 1 — `Agent.Inject` + inbox machinery.** `agent/inbox.go`
   + test + the `Run`-side prepend wiring. No external consumers
   wired yet. Verifies the core mechanism in isolation.
2. **Phase 2 — `StartAutonomous` + `AutonomousHandle`.**
   `agent/autonomous_handle.go` + tests + the autonomous-loop
   extraction. Refactor `RunAutonomous` to delegate to
   `StartAutonomous(...).Wait()` so existing call sites keep working.
3. **Phase 3 — Scion adapter rewrite.** Stdin goroutine + main loop
   redesign. Smoke against the existing Scion harness (see
   Verification).
4. **Phase 4 — Docs + tag.** CHANGELOG, library-api, scion-adapter
   docs. New smoke script `dev/smoke/06-scion-inject.sh` exercising
   the inject path.

## Verification

```bash
# Unit
go test ./agent/... -run TestInject
go test ./agent/... -run TestAutonomousHandle
go vet ./...
for s in dev/ci/presubmits/*; do bash "$s"; done

# Inject behavior under load — concurrent goroutines push messages
# while a long simulated turn runs; assert all messages land on the
# subsequent prompt in arrival order.
go test ./agent/... -run TestInject_ConcurrentProducers -race

# Real-LLM smoke for autonomous handle:
source /home/user/scripts/gemini.sh && unset GEMINI_API_KEY GOOGLE_API_KEY
go run ./examples/autonomous-handle/

# Scion adapter smoke (uses the existing Scion harness in
# /home/user/projects/scion):
cd /home/user/projects/scion
go run ./cmd/scion start --template=... --agent=core-agent ...
# In another shell:
scion message <agent> "first task"
# (Wait for the first turn to be in flight, then:)
scion message <agent> "actually, prioritize this other thing"
# Verify: the second message lands in the agent's next turn rather
# than waiting for the first to fully complete a separate per-line
# turn. Visible in the agent's [Inbox] block in the prompt and the
# eventlog rows.
```

New example `examples/autonomous-handle/main.go`: spawns an
autonomous run against the echo provider, pauses mid-run, resumes,
injects a message, asserts the message landed on the post-resume
turn's prompt. Credential-free, suitable for CI.

## Deferred (out of scope for v1.3.0)

- **`AutonomousHandle.Redirect(newGoal)`** — hard interrupt +
  restart with a new goal while preserving conversation context.
  Workaround in v1.3.0: `handle.Stop()` then
  `StartAutonomous(newGoal)` with the same agent (the eventlog
  carries history; the new run sees it). Promote to a first-class
  method when a consumer hits the seam.
- **`extras/scion-remote-agent/` reference RemoteAgentSpawner.**
  Requires a Scion-deployment decision (Hub HTTP API in
  `/home/user/projects/scion/pkg/hubclient/agents.go:156-185` vs CLI
  shell-out via `scion start`). Both work; the choice depends on
  whether the spawning agent runs inside the same Scion deployment
  as its children, has Hub credentials, etc. Plan separately once
  that's decided; the v1.2.0 `agent.RemoteAgentSpawner` interface is
  the seam, no core-agent changes needed to ship it.
- **Concurrent task multiplexing per container** — today one Scion
  container = one logical agent. If Scion ever wants to multiplex
  (cost optimization), we'd need session multiplexing. Not urgent.
- **Lifecycle status taxonomy enrichment** — `sciontool_status`
  currently emits four sticky states (`ask_user`, `blocked`,
  `task_completed`, `limits_exceeded` per
  `extras/scion-agent/sciontool.go:61`). A richer taxonomy
  (progress %, ETA, blocking-on-what) is worth doing but should be
  designed against what Scion's UI actually wants to display —
  coordinate with Scion folks first.
- **REPL `/inject` slash command** — interactive UX; library-only
  for v1.3.0. Add to the bundled CLI later if a real consumer wants
  it.

## Why not in this release

- **A full TUI / slash-command parity story with Claude Code or
  gemini-cli.** That's cogo's lane; see
  `docs/cogo-core-agent-integration.md`. This release focuses on the
  harness-substrate axis where we have a real edge.
- **`Redirect` as a first-class method.** Stop + restart is the
  workaround; promote when a consumer needs it.
- **Mid-turn pause for the autonomous handle.** Same architectural
  question as Redirect; ships together.

---

## Addendum: mid-turn interrupt in the bundled REPL

Late in v1.3.0 cycle we added the **Claude Code / gemini-cli pattern**
to the bundled CLI's REPL: pressing **ESC** during an in-flight turn
cancels just that turn's ctx while preserving conversation history,
then drops back to the `> ` prompt. Pressing **Ctrl+C** does the
same; a second Ctrl+C within 1 second exits the REPL cleanly.

This is the interactive analog of the programmatic interrupts shipped
above (`Agent.Inject`, `AutonomousHandle.Stop`). Together they cover
both axes of "interrupt a running agent":

- **Programmatic** (harness, orchestrator, library caller):
  `AutonomousHandle.Stop` / `Pause` / `Inject`.
- **Interactive** (human at a terminal): ESC, single-Ctrl+C,
  double-Ctrl+C.

### Implementation summary

New `runner/interrupt.go` — package-private `turnInterrupter` that:

- Uses `golang.org/x/term`'s `MakeRaw` / `Restore` to put stdin in
  raw input mode during a turn. Stdout's termios is untouched, so
  model text streams normally.
- Spawns a key-reader goroutine that reads single bytes from stdin
  using `SetReadDeadline` to wake up periodically and check a stop
  channel — so the goroutine exits promptly when the turn ends, not
  just when the user types something.
- Tracks `firstCtrlCAt time.Time` for the double-Ctrl+C window
  (default 1 second; var, not const, so unit tests can shrink it).
- Emits `✕ interrupted` to stderr on the first ESC / Ctrl+C of a
  turn (deduplicated; multiple presses don't spam).
- Defers `term.Restore` so the terminal is always returned to cooked
  mode, even on panic.

`runner/repl.go` wraps each turn's `streamTurn` call in a
`turnInterrupter`. When stdin isn't a TTY (`*os.File` type assertion
fails, or `term.IsTerminal` returns false), the interrupter is
silently skipped and the REPL falls back to its legacy single-Ctrl+C-
exits behavior. The startup banner reflects which mode is active.

### Why cbreak was on the table but raw won

The first design used **cbreak** mode (disable ICANON + ECHO, keep
ISIG so the terminal driver still translates Ctrl+C to SIGINT) to
avoid two concerns:

1. Raw mode disables `OPOST` which mangles stdout newlines.
2. Raw mode disables `ISIG` so Ctrl+C arrives as a byte rather than
   a signal, requiring us to write our own translation.

Both turned out to be non-issues:

1. `OPOST` is on the **stdout** file's termios, not stdin's. Raw mode
   on stdin doesn't affect stdout. Verified.
2. Reading 0x03 (Ctrl+C) as a byte is the same code path as reading
   0x1b (ESC); the parse is one switch case. Removing the SIGINT
   handler simplified the code (no signal.Notify shenanigans).

`golang.org/x/term` exposes `MakeRaw` directly; no per-platform
termios manipulation. Single new direct dep, ships everywhere Go
supports.

### Tools mid-cancel

Same best-effort story as the autonomous Stop path. `bash` uses
`exec.CommandContext` and cancels its subprocess promptly. File ops
complete quickly so cancel-during-tool is rare. MCP tools depend on
the consumer's implementation; many honor ctx, some don't. Tools
that ignore ctx finish their in-flight work before the loop unwinds.

### What ADK does for us

When the turn ctx cancels mid-stream, the model's LLM call returns
`context.Canceled`. Any partial assistant text already streamed to
the session is **preserved** — ADK appends events to
`session.Service` as they arrive, not in a final batch. So the next
turn sees: previous user message + truncated assistant reply + new
user message. The model figures out the redirect intent naturally.
No synthetic "[user interrupted]" event needed for v1.3.0; if a real
consumer reports model confusion, we'd add one in a patch.

### Tests

11 unit cases in `runner/interrupt_test.go` exercising the state
machine without a real terminal: mock key source + controllable
clock. Covers single ESC, single Ctrl+C, double-Ctrl+C within
window, double-Ctrl+C outside window, hint dedup, banner dedup,
other-keys-ignored, nil-stdin reject, non-TTY reject, Close
idempotency. Real-TTY behavior is left to the manual smoke (see
verification in the v1.3.0 plan).
