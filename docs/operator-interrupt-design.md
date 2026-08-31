# Operator interrupt, steer, and side questions

Design doc for making three existing operator-input surfaces —
`/interrupt`, `/btw`, and the inbox — behave the way operators
already expect them to behave, across all three consumers: the
embedded TUI, the remote (attach) TUI, and the raw HTTP/SSE API
that mast-web drives.

The target UX is Claude Code's:

- **ESC during a turn** stops the loop, holds it, and asks *"what do
  you want me to do instead?"* — and afterwards the loop **continues**,
  either with the new instruction injected or unchanged.
- **`/btw`** answers a side question against the live session — *"what
  file was that?"*, *"how much has this cost?"* — without polluting
  the conversation, without interrupting the turn, and without
  blanking or erroring out.

Neither works today. This doc explains why, locks the semantics, and
sequences the PRs.

`docs/operator-input-design.md` is the predecessor: it specified the
queue panel (A), auto-continue (B), `/btw` (C), and `/subagent` (D).
A–D all shipped. This doc covers what that design left unspecified —
interrupt/resume as a *state machine* rather than a one-way cancel —
plus the reachability and reliability defects found in the shipped
surfaces.

## Status today

### `/interrupt` is a one-way cancel with no way back

`Agent.Interrupt()` cancels the in-flight turn's context and returns.
That is the entire mechanism. There is no hold, no resume, and no
"what instead?" moment:

- After the cancel the driver (`runner.WakeLoop`, the autonomous loop,
  the REPL) goes back to waiting for the next wake. The agent is
  *idle*, not *paused*.
- The interrupt audit row (`Author = "attach/interrupt"`, #565) makes
  `classifyInterruptedTail` report not-interrupted, so auto-continue
  deliberately stands down. Correct — but it means nothing ever picks
  the work back up. The operator's only move is to type a fresh
  prompt, which runs as an ordinary turn with no framing that a human
  stopped the previous one.
- `attach.AgentStatePaused` (`pkg/attach/state.go:53`) is declared,
  documented as *"future, via /pause"*, and assigned nowhere in the
  tree. `autonomous.Handle` has working `Pause()` / `Resume()` /
  `Stop()` (`pkg/agent/autonomous/handle.go:218-288`) that no attach
  route exposes, and that only cover the autonomous driver anyway —
  the wake loop and the REPL don't go through a `Handle`.

Four defects on top of the missing state machine:

1. **Interrupt is single-shot per turn.** `Interrupt()` nils
   `cancelInFlight` before invoking it (`pkg/agent/agent.go:1768`).
   A second `/interrupt` while the first is still unwinding returns
   `{"interrupted": false}` + `X-Interrupted: nothing-in-flight`. When
   the first cancel doesn't bite promptly — a tool not honoring its
   context, a model call mid-retry — the operator is told nothing is
   running while the agent visibly keeps going. This is the single
   biggest contributor to *"it doesn't actually interrupt"*.
2. **Audit-row race.** `doInterrupt` calls `MarkInterruptPending()`
   *after* `AttachInterrupt()` returns (`pkg/attach/handlers.go:620`),
   but `drainInterruptAudit` runs in the interrupted turn's own
   cleanup. If cleanup wins the race the audit row lands a turn late,
   and auto-continue — which reads that row as *"deliberate kill,
   don't resume"* — can re-drive the very work the operator killed.
3. **Cancel doesn't reach background subagents.** `/interrupt`
   cancels the parent turn's context only. Declarative and background
   subagents (#626) run on manager-owned contexts. `Manager.Stop(name)`
   exists but has no attach route — `GET /subagents` and
   `GET /agents` are the only subagent endpoints. A runaway loop
   *inside* a subagent survives every interrupt the operator sends.
4. **Auto-continue can un-park a parked loop.** Not a bug today
   (nothing parks), but it becomes one the moment pause exists:
   `lockClassifyInject` injects a continue-note on a timer, and inject
   fires `RequestWake`.

### ESC and mid-turn slashes don't reach the agent

Both defects were in core-tui (v0.20.0 at the time of writing; the pin
is now v0.22.0), consumed here as a module pin.

5. **Every slash typed during a locally-driven turn is swallowed.**
   In `tui/update.go` the Enter handler checks `state == stateStreaming`
   and routes to `enqueueDuringStream(text)` **before** the `/`-prefix
   dispatch. Typing `/btw what file was that` while the agent works
   queues the literal string `"/btw what file was that"` into the inbox
   as an operator prompt and dispatches nothing. Same for
   `/interrupt`, `/compact`, `/done`, `/subagent`. This is precisely
   the moment `/btw` and `/interrupt` exist for.
6. **ESC is a no-op against a daemon-driven loop.** The ESC cascade's
   last arm requires `state == stateStreaming && cancelTurn != nil`,
   which only a *locally driven* turn sets. Attached to a daemon in
   LiveAgent/observer mode the TUI is in idle state, and there is no
   `RemoteInterrupter` arm on ESC. The Claude Code reflex silently
   does nothing; the operator has to know to type `/interrupt`.
   And at idle in *local* mode, `/interrupt` printed "no turn in
   flight" regardless, because the local adapter's
   `Interrupt() bool` in `cmd/core-agent/coretui_enabled.go` did
   not satisfy `coretui.RemoteInterrupter`'s `Interrupt(ctx) error`.
   Its doc comment claimed it "satisfies coretui.Interruptible" — an
   interface that has never existed in any core-tui release. The
   method satisfied nothing and was never called.

   **The adapter half is resolved** by
   [#803](https://github.com/go-steer/core-agent/issues/803): it is
   now `Interrupt(ctx context.Context) error`, pinned by a
   `var _ coretui.RemoteInterrupter` guard in
   `cmd/core-agent/coretui_guards.go` — the exhaustive per-adapter
   guard list from [#810](https://github.com/go-steer/core-agent/pull/810),
   not a line next to the method — and `false` from
   `agent.Agent.Interrupt` maps to an error so core-tui doesn't report
   a cancel that didn't happen. That is a correctness fix, not an
   operator-facing one on its own: the local gate is checked first and
   moves in lockstep with `stateStreaming`, so the capability arm is
   still only reached at idle or during the post-`finalizeTurn`
   unwind. **The ESC arm is still open** on the core-tui side
   ([core-tui#260](https://github.com/go-steer/core-tui/issues/260)),
   and it is the half that makes the fix pay — its cascade dispatches
   through `RemoteInterrupter` unconditionally.

### `/btw` returns blanks and infra errors

7. **30-second hard client timeout.** `POST /slash/btw` is synchronous
   by design (the handler blocks 5–30s while the model answers), but
   every TUI builds its attach client with `timeout == 0`, which
   `internal/attachclient/client.go:83` turns into a 30s
   whole-request deadline. A side question over a long history on a
   thinking model blows straight through it and surfaces as
   `context deadline exceeded` — the reported "infra error".
   `/compact` and `/done` ride the same 30s and are slower still.
8. **An empty model response is returned as an error.**
   `AskSideQuestion` returns `errors.New("agent: AskSideQuestion:
   model returned no text")` (`pkg/agent/btw.go:124`), which every
   surface renders as a failed call rather than "the model had
   nothing to say". Thought-only responses and safety-blocked
   responses both land here, with no finish-reason detail to
   diagnose which.
9. **`/btw` is not actually tool-less on Gemini/Vertex.**
   `AskSideQuestion` builds `&adkmodel.LLMRequest{Contents: history}`
   with a nil `Config`. `builtinsLLM.GenerateContent`
   (`pkg/models/gemini/builtins.go`) creates the Config and injects
   `google_search` / `url_context` into it; on a Vertex cached turn it
   also stamps `CachedContent` onto a request that already carries the
   full history. The design says "no tools" (`operator-input-design.md`
   §C) and the request shape doesn't enforce it.
10. **Rate limiting reads as failure.** `/btw` is cost-bearing and sits
    behind the token bucket (10/min, burst 5 — `pkg/attach/rate_limit.go`).
    A few quick side questions 429, and the 429 is not rendered as a
    rate-limit message.
11. **`/btw` can't actually answer "what's the status?"** The operator
    ask that motivates the feature — *"how much has this cost?"*,
    *"what are you doing right now?"* — is unanswerable from raw
    session history alone. The model gets the transcript and nothing
    about live run-state, cost, or the tool currently in flight.

### Inbox is healthy; two rough edges

The inbox does what it says: `InjectAs` queues with a prompt_id, emits
`inbox`/queued, fires `RequestWake`; `Run` drains it pre-turn and
prepends an `[Inbox]` block. Known gaps, both already filed:

- **#698** — no queue-without-waking. Every inject drives a turn.
- **#697** — the inject path uses the bare `[Inbox]` framing;
  the richer `FormatAutoContinueInbox` bundle guidance is
  auto-continue-only.

Both become more visible under pause (a queued steer must *not* drive
a turn until resume), so this design resolves them for the interrupt
path specifically.

## The state machine

One new concept: the agent has a **pause gate**. Paused means *no new
turn starts until someone resumes*. It is deliberately not "the
current turn freezes" — there is no safe suspend point inside a model
call, and pretending otherwise is how you get a UI that says paused
while tokens keep burning.

```
                    ┌──────────────────────────────────────┐
                    │                                      │
   ┌────────┐  wake/inject   ┌─────────┐   turn ends   ┌────┴───┐
   │  idle  │───────────────▶│ running │──────────────▶│  idle  │
   └────────┘                └────┬────┘               └────────┘
        ▲                         │
        │                    interrupt
        │                    (cancel + hold)
        │                         │
        │                         ▼
        │  resume{abandon}   ┌──────────┐   resume{steer|continue}
        └────────────────────│  paused  │──────────────────────▶ running
                             └──────────┘
                                  ▲
                             pause (no cancel;
                             current turn finishes)
```

Transitions:

| From | Event | To | Effect |
|---|---|---|---|
| running | `interrupt` (default, `hold=true`) | paused | cancel in-flight turn; audit row; gate closed |
| running | `interrupt{hold:false}` | idle | today's behavior — cancel, no gate |
| running \| idle | `pause` | paused | gate closed; a running turn finishes normally |
| paused | `resume{steer:"…"}` | running | inject steer w/ interrupt framing; gate open; wake |
| paused | `resume` (no steer) | running | inject continue-note; gate open; wake |
| paused | `resume{mode:"abandon"}` | idle | gate open; **no** inject, **no** wake |
| paused | `inject` from anyone | paused | queued, gate stays closed (#878; see below) |
| paused | `wake` / scheduler tick | paused | queued, gate stays closed |

### Where the gate lives

On `*agent.Agent`, not on `autonomous.Handle`. Every driver — the wake
loop, the autonomous loop, the REPL, the embedded TUI — funnels
through `Agent.Run`, and attach already holds the agent as its
`Registrant`. `autonomous.Handle.Pause/Resume` stay as they are and
delegate to the agent's gate so the two can't disagree.

```go
// pkg/agent/pause.go (new)

// Pause closes the gate. Idempotent; reports whether it transitioned.
func (a *Agent) Pause(reason string) bool

// Resume opens the gate. Idempotent; reports whether it transitioned.
func (a *Agent) Resume() bool

// InterruptAndHold cancels the in-flight turn AND closes the gate, in
// that order under one lock, so no wake can slip a turn in between.
// Returns (interrupted, paused).
func (a *Agent) InterruptAndHold(reason string) (bool, bool)

// PauseState is the projection every surface renders from.
func (a *Agent) PauseState() PauseState

type PauseState struct {
    Paused      bool
    Since       time.Time
    Reason      string // "operator-interrupt" | "operator-pause" | ...
    Interrupted bool   // a turn was cancelled entering this pause
}

// awaitResume blocks at the top of Run until the gate opens or ctx
// dies. Never holds a.mu while blocking (same discipline as
// autonomous.Handle.beforeTurn).
func (a *Agent) awaitResume(ctx context.Context) error
```

`Run` calls `awaitResume` as its **first** step — before the guardrail
restore, before the cost-ceiling preflight, before the inbox drain.
A paused agent must not drain its inbox: the queued steer has to
survive until resume so the resuming turn sees it.

Blocking inside `Run` is what makes the gate real for every driver at
once, and it costs nothing elsewhere: attach handlers never call
`Run`, so `/resume`, `/inject`, `/interrupt`, `/btw`, `/status`, and
the SSE stream all stay responsive while parked.

### Resume dispositions

`steer` and `continue` differ only in what gets injected, and both
land in the inbox so the normal drain path carries them:

- **steer** — `FormatInterruptSteer(text)`, a sibling of the existing
  `FormatAutoContinueInbox`:

  ```
  [The operator interrupted you]
  You were stopped mid-task by a human operator, who then said:
  - <steer text>

  Follow the operator's instruction. If it supersedes what you were
  doing, drop the old approach; if it adjusts it, adapt the next step.
  Do not silently resume the interrupted work.
  ```

- **continue** — `FormatInterruptContinue()`:

  ```
  [The operator interrupted you, then told you to carry on]
  A human operator stopped you mid-task and has now resumed you with
  no new instructions. Pick up where you left off. The last tool call
  may have been cancelled — re-check its state before assuming it
  completed.
  ```

  The "re-check" line matters: tail repair (#537) patches a dangling
  tool call into a well-formed history, but the *effect* of a
  cancelled `bash` or `write_file` is genuinely unknown.

- **abandon** — nothing injected, no wake. The interrupt audit row
  stands and auto-continue stays stood down, so the agent goes quiet.

### Inject while paused queues (revised by #878)

**As originally shipped (protocol 1.5.0–1.10.0):** an inject from
anyone other than `AutoContinueOriginator` opened the gate and woke the
loop, i.e. it *was* `resume{steer}` with the bare `[Inbox]` framing
instead of the interrupt framing. The argument was compatibility plus
UX: in Claude Code, typing after ESC resumes with your text, and the
common API pattern `POST /interrupt` then `POST /inject` kept producing
the same observable outcome, plus a paused window in between.

**Why that was wrong.** "Typing after ESC" is a human at a keyboard.
`POST /inject` is not — it is one door serving operators, chat
gateways, alert watchers and peer daemons alike, and the runtime cannot
tell them apart. `auth.Caller` carries `Identity`, `Labels`, `Admin`;
there is no human/machine bit and none could be added honestly, because
a machine can legitimately act under a person's identity. k8s-lookout's
watcher does exactly that: it injects as its `--owner`
(`platform-oncall@example.com` in the demo) asserted through the proxy
identity `sa:lookout-watch`, which is the same string the on-call
engineer authenticates as.

The exclusion list gave the shape of the defect away. `caller.Identity
!= AutoContinueOriginator` is a denylist of one, so what the condition
actually expressed was "any wake-true inject", not "an operator asked
to resume". Found live on the #799 smoke run: an agent parked by
operator interrupt came back with a bare `Resumed.` nobody had asked
for, and the operator's follow-up `/continue` answered *"agent isn't
held"*.

Proxying is not a usable discriminator either — a chat gateway proxies
for a real human whose message *should* release the hold. So the
authority is stated rather than inferred, and it lives in the verb:
`inject` queues, `resume` opens the gate.

**Current behavior.** An inject into a parked session queues the
message, emits `inbox`/queued, fires the wake signal (buffered-1, so it
latches rather than being spent against a shut gate), and leaves
`state: "paused"` intact. The operator's `resume` drains it alongside
their own instruction as one `[Inbox]` block, in arrival order — which
is strictly better than the old behavior, where an alert both jumped
the gate *and* was presented to the model as though the operator had
said it.

Migration for a client that relied on the implicit release: send
`POST /resume {"mode":"steer","steer":"…"}`. Still one call, and it
carries the interrupt framing, so the model is told its last turn was
killed rather than silently redoing the abandoned work.

Auto-continue gets two changes so it can't fight the gate:

- `lockClassifyInject` adds `a.Paused()` to its stand-down
  preconditions, alongside the existing `HasPendingOperatorInput`
  check (#624).
- Injects stamped `AutoContinueOriginator` never open the gate.

### Background subagents

Parking the parent while a subagent keeps burning tokens is
incoherent, but silently killing a subagent's work on an ESC press is
worse — subagent runs aren't resumable. So: make it **visible and
stoppable**, not automatic.

- `POST /interrupt` response gains `running_subagents: [{name, id}]`,
  and the paused banner renders "2 background subagents still
  running".
- `POST /interrupt` accepts `{"stop_subagents": true}` to also call
  `Manager.Stop` on each.
- New route `POST /sessions/{sid}/agents/{name}/stop` so the operator
  can kill one by name, exposing the `Manager.Stop` that already
  exists. Capability: `AgentStopper { AttachStopAgent(name string) (bool, error) }`.

## Wire protocol

The API is the contract mast-web builds against, so it is specified
here rather than derived from whatever the TUI happens to need.

Protocol version bumps **1.4.0 → 1.5.0** (additive: new endpoints, one
new event type, new optional response fields).

### `POST /sessions/{sid}/interrupt`

Request body (all fields optional; an empty body keeps the new
default):

```json
{ "hold": true, "stop_subagents": false }
```

Response:

```json
{
  "session": "s-123",
  "interrupted": true,
  "paused": true,
  "running_subagents": [{ "name": "cluster", "id": "bg-7" }]
}
```

`hold` defaults to **true**. `X-Interrupted: nothing-in-flight` is
still set when there was no turn to cancel — but with `hold=true` the
gate closes regardless, so an interrupt sent at idle still parks the
agent (the operator meant "stop", and the loop hadn't started yet).

### `POST /sessions/{sid}/pause`

```json
{ "reason": "operator-pause" }   →   { "paused": true, "state": "paused" }
```

Closes the gate without cancelling. A running turn finishes; the next
one waits. This is the "pause this agent" button in mast-web.

### `POST /sessions/{sid}/resume`

```json
{ "mode": "steer", "steer": "check the logs first" }
```

`mode` is `"steer"` | `"continue"` | `"abandon"`; omitted means
`"steer"` when `steer` is non-empty, `"continue"` otherwise.

```json
{ "resumed": true, "mode": "steer", "state": "running" }
```

`resumed: false` with 200 when the agent wasn't paused (idempotent, so
a double-click doesn't error).

### `GET /sessions/{sid}/status`

`StatusInfo` gains three fields and finally emits the declared
`AgentStatePaused`:

```json
{
  "state": "paused",
  "model_name": "gemini-3.5-pro",
  "paused_since": "2026-08-18T14:03:11Z",
  "pause_reason": "operator-interrupt",
  "interrupted": true
}
```

### SSE: new `pause` event

```
event: pause
data: {"state":"paused","reason":"operator-interrupt","interrupted":true,"at":"…"}

event: pause
data: {"state":"resumed","mode":"steer","at":"…"}
```

Added to `EventPause = "pause"`, `supportedEventTypes`, and the
`capabilities` event so a client can feature-detect. Attached clients
learn about pause without polling `/status` — required for mast-web
and for a second TUI watching the same session.

### Capability interfaces

```go
// pkg/attach/state.go
type PauseController interface {
    AttachPause(reason string) bool
    AttachResume(req ResumeRequest) (ResumeResponse, error)
    AttachPauseState() PauseInfo
}

type AgentStopper interface {
    AttachStopAgent(name string) (bool, error)
}
```

Absent capability ⇒ 501 on the POSTs (the established convention for
mutations), and `GET /status` keeps reporting the pre-1.5.0 state set.

## `/btw` fixes

Numbered to the defects above.

- **(7) Per-endpoint timeouts.** Drop the blanket 30s from the RPC
  client and set deadlines per endpoint: 30s for reads, **5 minutes**
  for the cost-bearing slashes (`btw`, `compact`, `done`, `replan`,
  `subagent`). The operator can already cancel an in-flight slash with
  ESC (`m.cancelSlash`), so the long deadline is a backstop, not a
  hang.
- **(8) Empty answer is a 200, not a 500.** `SideQueryResponse` gains
  `empty: bool` and `detail: string` (finish reason / block reason
  when the provider gave one). `AskSideQuestion` returns a typed
  `ErrSideQuestionEmpty` the handler maps to
  `{"answer":"", "empty":true, "detail":"finish_reason=SAFETY"}`.
  Surfaces render "(no answer — <detail>)" instead of an error modal.
- **(9) Actually tool-less.** `AskSideQuestion` sets an explicit
  non-nil `Config` and marks the context with a new
  `models.WithoutBuiltins(ctx)` — a sibling of the existing
  `models.WithoutPromptCache(ctx)` — that `builtinsLLM` honors by
  skipping both built-in injection and `CachedContent` stamping. The
  marker is opt-in per call site; only `/btw` adopts it in this train.
- **(10) 429 renders as a rate limit.** Surface `Retry-After` and map
  it to "rate limited — retry in Ns" in both TUI adapters rather than
  a generic failure.
- **(11) A status preamble so `/btw` can answer status questions.**
  Prepend a compact synthetic block to the side-question contents:

  ```
  [Session status at the time of this question]
  state: running (tool in flight: bash)
  model: gemini-3.5-pro · turns: 14 · cost so far: $0.42
  pending inbox: 1 · background subagents: 1 running (cluster)
  ```

  Cheap (a few dozen tokens), and it converts *"what are you doing
  right now?"* from unanswerable to trivially answerable. Sourced from
  the same projections `AttachStatus` / `AttachUsage` already build.

As shipped (PR 3), three details differ from the sketch above:

- **(7)** the blanket deadline stays. Rather than re-timing every
  endpoint, the client carries a *second* `http.Client` with the
  5-minute deadline and routes to it on the `/slash/` path segment;
  reads keep the configured RPC timeout. A ctx deadline inside `do()`
  was the obvious alternative and is wrong: `ListPeers` reads the
  response body after `do()` returns, so a cancel-on-return would
  truncate it. An operator who configured an RPC timeout *longer* than
  five minutes keeps theirs — the floor never shortens a deliberate
  choice.
- **(9)** the context marker is used even though `builtinsLLM` already
  exposes a `WithoutBuiltins() adkmodel.LLM` unwrap. The unwrap returns
  the bare inner model, which also drops `retryOnceOnEmpty` and
  `wrapEmptyTailDetection` — the two safety nets against exactly the
  blank answers this train exists to fix. Trading defect 8 for defect 9
  is not a fix. The marker also gates `cacheInit`, so a side question
  (no system instruction, no tools) can't seed a cache that every later
  turn then inherits.
- **(10)** the mapping lives in the client, not in the two TUI
  adapters. `attachclient` promotes a 429 into a `RateLimitError`
  carrying the parsed `Retry-After`, whose message already reads
  "rate limited by the daemon — retry in Ns"; both adapters render
  `err.Error()`, so neither can drift from the other. It embeds
  `*httpStatusError` and implements `Unwrap`, so the existing
  status-code classification (including `PermanentStreamErr`, which
  must keep calling 429 retryable) is unchanged.

`pkg/attach` cannot import `pkg/agent` (the dependency runs the other
way), so the agent's typed empty-answer error is restated by
`attachadapter` as `*attach.SideQueryEmptyError` on the way out. That
translation is the whole reason the handler can answer 200; a test
pins it, because losing it silently reverts defect 8.

Adversarial review turned up a hole in defect 8 that the original
analysis missed, and it was the *common* case rather than an edge one.
The Gemini adapter classifies a contentless turn as an **error**, not
as an empty response: `wrapEmptyTailDetection` synthesizes
`ErrEmptyResponse` for a bare `FinishReason=STOP` with no parts, and
`retryOnceOnEmpty` retries once and then surfaces it. That is correct
inside the agentic loop — a turn that produces nothing leaves the loop
with no next action and the session goes idle forever (#220) — but it
means the single most likely blank `/btw` answer never reached the new
typed-empty path at all. It arrived as a provider error and rendered as
one: a paragraph about silent safety filters and transient Vertex
faults, in front of an operator who asked a question. Fixing defect 8
without this would have left the reported symptom substantially intact
while every new test passed.

The fix keeps both readings of the same condition. `pkg/models` gains a
provider-agnostic `ErrEmptyResponse` sentinel; `gemini.ErrEmptyResponse`
wraps it (keeping its own diagnostic text, which the loop still logs);
and `AskSideQuestion` converts a match into `*SideQuestionEmptyError`
— after recording the usage the refusal cost, since the adapter
retried and the operator's cost line should say so. The loop's
behavior is untouched. A provider adapter added later opts in by
wrapping the same sentinel.

## core-tui changes

- **(5) Dispatch slashes during a turn.** Move the `/`-prefix check
  ahead of the `enqueueDuringStream` branch, gated by an allowlist of
  slashes that are safe mid-turn:

  | Safe mid-turn | Refused mid-turn |
  |---|---|
  | `btw`, `interrupt`, `resume`, `status`, `stats`, `context`, `agents`, `subagents`, `tools`, `help` | `compact`, `done`, `replan`, `clear`, `subagent` |

  The refused set mutates conversation state or writes a boundary and
  would race the live turn; they get "not while a turn is running —
  /interrupt first". `Agent.Compact` / `Checkpoint` already refuse
  mid-turn server-side (#355), so this makes the client agree with the
  server instead of silently queueing the text.

  Text that starts with `/` but isn't a known slash keeps queuing as a
  literal prompt, so this doesn't hijack ordinary input.

- **(6) ESC interrupts in every mode.** Add an arm to the ESC cascade
  after `cancelSlash`: if a `RemoteInterrupter` is wired, call it —
  whether or not the local `state == stateStreaming`. Local mode
  reaches it too once `coreAgentAdapter.Interrupt` is changed to
  `Interrupt(ctx) error`.

- **Paused UI.** New `statePaused`. On entering it the TUI renders a
  banner and repurposes the input line:

  ```
  ⏸ interrupted — what do you want me to do instead?
     enter to steer · /resume to continue as you were · /abandon to drop it
     (1 background subagent still running)
  ```

  Enter with text → `resume{steer}`. `/resume` → `resume{continue}`.
  `/abandon` → `resume{abandon}`. ESC again → dismiss the banner,
  leave the agent paused (so ESC never accidentally resumes).

- **`pause` SSE handling** so an attached TUI enters/leaves
  `statePaused` when *another* client interrupts the same session.

- **New capability interface** (duck-typed, so old adapters keep
  working):

  ```go
  type Pauser interface {
      Pause(ctx context.Context, reason string) error
      Resume(ctx context.Context, mode, steer string) error
      PauseState() (paused bool, since time.Time, reason string)
  }
  ```

## PR train

| PR | Repo | Scope | Depends on |
|---|---|---|---|
| 1 | core-agent | Pause gate on `Agent` (`Pause`/`Resume`/`InterruptAndHold`/`PauseState`/`awaitResume`), `Run` gate check, repeatable `Interrupt` (stop nil-ing `cancelInFlight`; let the gen-keyed `clearCancelInFlight` own it), audit-order fix (mark pending *before* cancel), auto-continue stands down while paused, interrupt framing helpers. No HTTP surface — but it carries the
`PauseEvent` / `PauseState*` / `ResumeMode*` declarations in
`pkg/attach/events.go`, because the gate emits pause events and
speaks resume modes at the agent layer. Nothing advertises them yet:
`protocolVersion` stays `1.4.0` and `pause` is out of
`supportedEventTypes` until PR 2 wires an emitter, so no client is
told about a surface that isn't there. | — |
| 2 | core-agent | Attach surface: `/pause`, `/resume`, `/interrupt` body + response, `StatusInfo` fields, `pause` SSE event, protocol 1.5.0, `PauseController`, `pause` capability flag, attachclient methods. Folds in what was PR 6 (`POST /agents/{name}/stop`, `stop_subagents`, `running_subagents`) — the subagent surface is three small methods behind the same `/interrupt` response shape, and splitting it would have shipped an interrupt response whose `running_subagents` field was permanently empty. **This is the mast-web contract.** | 1 |
| 3 | core-agent | `/btw` hardening: defects 7–11. | 1 |
| 4 | core-tui | Mid-turn slash dispatch + allowlist, ESC→interrupt arm, `statePaused` UI + steer prompt, `pause` SSE handling, `Pauser`. Release a tagged version. | 2 (wire shape only) |
| 5 | core-agent | Bump the core-tui pin, adapt `coreAgentAdapter` (`Interrupt(ctx) error`, `Pauser`) and `coretuiremote.Adapter`, docs (README + DESIGN + `docs/site/src/content/docs/`), live UAT. | 1–4 |

The train is linear: 1 → 2 → 3. PR 3 looked independent when this
plan was written — `/btw` hardening touches a different layer — but
the status preamble it prepends reports whether the session is
parked, which means `sideQuestionStatus()` calls PR 1's
`Agent.PauseState()`. Answering "what's going on right now?" without
mentioning that the loop is held would be the wrong answer, so the
dependency is the feature working, not an accident of packaging.

Two contract details settled during PR 2:

- **`AttachResume` takes the `auth.Caller`.** The steer text becomes a
  queued inbox message, and the inbox's last-write-wins originator
  decides who the resumed turn is attributed to. Without the caller,
  a resume would inherit whoever last drove the session.
- **`Agent.ResumeWith` queues before it opens the gate.** Resuming
  first leaves a window where a driver blocked in `awaitResume` starts
  an un-steered turn and the operator's instruction lands a turn late
  — against work they'd just redirected. (`injectAs` had a
  `releaseHold` axis to make that ordering expressible; since #878
  nothing releases, so the axis is gone and the ordering is simply what
  `ResumeWith` does.)
- **Hold against a pre-1.5.0 registrant degrades, it doesn't 501.** The
  turn is still cancelled and the response carries `X-Hold:
  unsupported` with `paused: false`. A stop that half-lands beats no
  stop, as long as the client isn't told a park happened.

## Validation

Each PR carries tests that fail on pre-fix code (repo convention):

- **Repeatable interrupt** — two `Interrupt()` calls during one turn
  both report true; the second is a no-op on an already-cancelled ctx.
- **Gate blocks a turn** — `Pause` then `Run` blocks; `Resume`
  releases; `ctx` cancellation releases with `ctx.Err()`.
- **Paused agent doesn't drain its inbox** — inject while paused, then
  assert the message is still queued (`PendingInboxCount() == 1`)
  until resume.
- **Auto-continue stands down while paused** — `lockClassifyInject`
  against a paused agent injects nothing.
- **Audit row lands before the resuming turn** — deterministic now
  that the pending flag is set pre-cancel.
- **No inject resumes, whoever sends it** (#878) — including one under
  an identity that looks like an operator's, which is the case a
  caller-identity check would wave through.
- **Wire round-trips** — `/interrupt` with and without `hold`,
  `/resume` in all three modes, idempotent double-resume, 501 when
  the capability is absent, `pause` event on the SSE stream.
- **Hold against a registrant without `PauseController`** — the turn is
  still cancelled, `paused` is false, and `X-Hold: unsupported` says so.
- **Status reports paused for a registrant that isn't a
  `StatusProvider`** — the handler folds the pause in centrally, so a
  third-party registrant can't report `idle` while parked.
- **Runaway subagent** — `/agents/{name}/stop` stops a live one and 404s
  on a name that isn't running; `stop_subagents` on `/interrupt` reports
  what it killed.
- **`/btw` empty answer is a 200** with `empty: true` — end to end: the
  agent's typed error, the adapter's translation across the
  package boundary, and the handler's 200. Paired at each layer with a
  genuine provider error that must still fail, so the fix can't be
  satisfied by swallowing errors.
- **`/btw` request carries no tools** — assert the `LLMRequest` seen by
  a fake `builtinsLLM` has an empty `Config.Tools` and no
  `CachedContent`, and that `cacheInit` never fires; plus an
  unsuppressed call in the same test that must still stamp, so the
  suppression is proven to be conditional.
- **Slash calls outlive the RPC deadline** — one server, one delay, two
  calls from the same client: the non-slash call times out, the slash
  call answers. Plus the routing table (`/slash/*` slow, `/inject`,
  `/interrupt`, `/resume`, `/wake` fast) so an endpoint rename can't
  silently move every RPC onto the five-minute deadline.
- **429 is typed** — `RateLimitError` with the parsed `Retry-After`,
  round-tripped through a real `SlashBtw` call; and it still unwraps
  to the underlying status error and still classifies as retryable.
- **A provider's empty-response error is an empty answer** — the
  Gemini-shaped `ErrEmptyResponse` reaches `/btw` as
  `ErrSideQuestionEmpty`, and the adapter's own error does not leak
  through; a transport failure in the same position still fails. Plus
  a pin in `pkg/models/gemini` that its sentinel wraps the shared one,
  since that wrap is the only thing making the conversion possible.
- **Status preamble** — the block leads the question's own user content
  (one appended content, as before), carries state / turns / cost /
  inbox, and reports `paused` when the agent is parked.

Live UAT (GKE recipe session, per the standing gate) covers the two
things unit tests can't: ESC mid-turn in the remote TUI actually
stopping a live model call, and a steer resume producing a visibly
different next step.

## Open questions

1. **Should `hold=true` really be the default on `/interrupt`?**
   Argued yes above (an interrupt at idle still means "stop"). The
   alternative — default `false`, opt in per client — is a smaller
   blast radius but leaves the TUI and mast-web configuring the same
   thing twice. *Settled yes;* the second half of the original
   argument, that inject-auto-resumes keeps the common API pattern
   behaviorally identical, was retired by #878 — that rule is gone and
   `POST /resume` is the pattern now.
2. **Pause persistence across daemon restart.** Currently in-memory:
   a restart resumes everything. Persisting it in the session row
   would be more honest for a long-parked session, at the cost of a
   schema field and a "why won't this agent start?" failure mode.
   Proposed: in-memory for now, revisit if operators hit it.
3. **Should `pause` also gate `/btw`?** Proposed no — asking questions
   about a parked agent is exactly when you'd want to.
4. **Cost ceiling interaction.** A session that parks at its cost
   ceiling and one that parks on operator interrupt both report
   `paused`. Worth a distinct `pause_reason` value
   (`"cost-ceiling"`) so mast-web can render them differently, but the
   ceiling path stays as-is in this train.
