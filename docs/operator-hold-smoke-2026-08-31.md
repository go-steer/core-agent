# Operator hold smoke sweep — 2026-08-31

Scratch UAT checklist for the operator-input repair train
([#799](https://github.com/go-steer/core-agent/issues/799)): the hold
(#793 + #794), `/btw` hardening (#795), and the TUI half
(core-tui#260, shipped in v0.24.0 and pinned by #863).

Design of record: [`operator-interrupt-design.md`](operator-interrupt-design.md).
Wire contract: [attach-http reference §Interrupt, pause, and resume](site/src/content/docs/reference/attach-http.md).

#799's definition of done is *"the operator hold works end to end through
all three surfaces — local TUI, remote TUI, and the HTTP/SSE API that
mast-web consumes — and `/btw` answers questions about the live session."*
Everything below exists to make that sentence checkable. Nothing in this
file is automated: the unit and conformance tests already pin the wire
shapes, and what they cannot pin is whether an operator who hits Esc on a
real runaway loop actually gets it to stop.

Tick `[x]` as you go; jot observations inline. Anything ✗ becomes a
follow-up issue; anything ⚠ is "works but rough."

## Setup

- Daemon build: `_______` (want ≥ `v2.9.0-dev.3`; the hold landed in dev.1,
  `Pauser` on the remote adapter in #863)
- `core-agent-tui` build: `_______` (core-tui pin must be **v0.24.0**)
- Environment: ☐ local `--attach` daemon  ☐ GKE demo cluster (`core-agent-demo-3`)

The GKE run is the one that matters — a local daemon on a loopback socket
never reproduces the latency, the proxy, or the pod restarts. Do the local
pass first anyway; it is five minutes and it catches shape errors before
you spend a cluster.

```sh
export BASE=https://localhost:8443        # or the port-forward / ingress
export APP=core-agent
export SID=default                        # a tenant SID for §11
export TOK="Bearer $(cat /tmp/core-agent-uat/token)"   # never $HOME
export S="$BASE/sessions/$APP/$SID"
```

A long-running turn to interrupt. Anything that takes >30s and is visibly
mid-work; on the demo cluster, `./break-workload.sh bad-image` then asking
the agent to triage gives a real one with subagent delegation in it.

---

## 1 — Capability handshake

- [ ] `GET $BASE/status` (or `$S/status`) reports `protocol` ≥ `1.5.0`
- [ ] `features.pause` is `true`, and `features.interrupt` is `true`
- [ ] With a daemon that predates the hold, the same probe reports no
      `pause` key — clients must read absent as off, not as unknown

```sh
curl -sk -H "Authorization: $TOK" "$S/status" | jq '{protocol, features}'
```

Notes:

## 2 — Interrupt parks the loop

Start a long turn, then interrupt it.

- [ ] `POST $S/interrupt` with **no body** returns `interrupted: true`,
      `paused: true` (absent body means `{"hold": true}` — the default is
      the hold, not the old cancel)
- [ ] `GET $S/status` reports `state: "paused"` with `paused_since`,
      `pause_reason: "operator-interrupt"`, `interrupted: true`
- [ ] The turn actually stopped — no further assistant text, no further
      tool calls on the event stream
- [ ] **Nothing starts a new turn.** Watch for a full auto-continue
      interval plus slack (default cadence + 60s). This is the whole bug:
      pre-#793 the loop walked straight into the next turn.
- [ ] A `pause` SSE frame arrived on `GET $S/events`, shaped
      `{"state":"paused","reason":...,"interrupted":true,"at":...}`
- [ ] Interrupting again while parked is not an error, and does not
      un-park

```sh
curl -sk -X POST -H "Authorization: $TOK" "$S/interrupt" | jq
curl -sk -H "Authorization: $TOK" "$S/status" | jq '{state, paused_since, pause_reason, interrupted}'
```

- [ ] Interrupt while **idle**: `interrupted: false` with header
      `X-Interrupted: nothing-in-flight`, but still `paused: true`. An
      operator who hits stop on an idle agent meant stop.
- [ ] Repeatable: interrupt a turn, and while the first cancel is still
      unwinding send a second. It must not answer
      `{"interrupted": false}` — `cancelInFlight` used to be nil'd on use,
      which told the operator nothing was in flight when something was.

Notes:

## 3 — The three resume dispositions

From a parked session, one per run; re-park between each.

- [ ] **Steer** — `POST $S/resume {"mode":"steer","steer":"drop the rollout, look at the CrashLoop first"}`
      → `resumed: true`. The next turn carries the instruction under
      interrupt framing, and the model **does not silently redo** the
      abandoned work. Read the actual next turn; this is the claim most
      likely to be wrong in the field.
- [ ] **Continue** — `POST $S/resume` with an empty body → carries on
      where it left off (`mode` defaults to `continue`)
- [ ] **Abandon** — `POST $S/resume {"mode":"abandon"}` → gate opens,
      nothing is injected, and the agent **stays quiet** until something
      else drives it
- [ ] Each transition emitted a `pause` frame with `state: "resumed"` and
      the `mode` the operator chose
- [ ] `{"mode":"steer"}` with no `steer` text → **400**, not a silent
      empty steer
- [ ] An unknown mode → **400**

Notes:

## 4 — `/pause` holds without killing anything

- [ ] Mid-turn `POST $S/pause {"reason":"reviewing the plan"}` returns
      `paused: true, transitioned: true`
- [ ] The **running turn finishes normally** — it is "stop after this
      one", not a freeze. Confirm the final text arrives.
- [ ] The *next* turn is what waits
- [ ] `pause_reason` echoes the operator's string, not the default

Notes:

## 5 — Idempotence and first-cause-wins

- [ ] Second `/pause` → `paused: true, transitioned: false` (a quiet 200,
      so two operator surfaces racing one click is not an error)
- [ ] Resume when not paused → `200` with `resumed: false`
- [ ] Interrupt a turn (park with `interrupted: true`), then `/pause` on
      top. `pause_reason` stays `operator-interrupt` and `interrupted`
      stays `true` — a plain pause must not erase the fact that work was
      cancelled.
- [ ] The reverse order upgrades: `/pause` while idle, then a turn somehow
      starts and is interrupted → `interrupted` flips to `true`

Notes:

## 6 — What may and may not un-park

The gate is only real if the things that used to re-drive the loop now
respect it.

- [ ] **Auto-continue stands down** while parked, for at least two of its
      intervals. It must not un-park a loop a human parked.
- [ ] **A queued inbox message stays queued.** `POST $S/inject {"wake": false}`
      while parked, then check `pending inbox` (via `/btw`, §10) is
      non-zero and no turn ran. Resume and confirm that message is picked
      up by the turn the resume releases — not dropped, and not consumed
      by a turn that never happened.
- [ ] **`POST $S/wake` starts no turn.** The driver may enter `Run` and
      block in `awaitResume`, which is fine and invisible; what must not
      happen is a turn. Check the event stream, not the process.
- [ ] **`POST $S/inject` (default, waking) *does* release the hold** — the
      long-standing interrupt-then-inject client pattern must behave as it
      did before 1.5.0, so a TUI whose send path is `/inject` needs no
      resume call
- [ ] A **daemon restart** while parked: does the session come back
      parked, or running? Record whichever it is — the hold is in-memory
      and this is the one behaviour the design does not promise. ⚠ if it
      silently resumes work.

Notes:

## 7 — Background subagents

Needs a session that has spawned one; on demo-3 the `cluster` subagent
delegation during a triage does it.

- [ ] Interrupting the **parent does not stop subagents** — the
      `/interrupt` response lists them in `running_subagents`
- [ ] `POST $S/agents/{name}/stop` stops one by name → `{"stopped": true}`
- [ ] The same call for a subagent that is not running → **404**
- [ ] `POST $S/interrupt {"stop_subagents": true}` stops all of them and
      reports them in `stopped_subagents`
- [ ] The runaway case, which is why this exists: a subagent in a tool
      loop survives a plain parent interrupt, and `stop_subagents` is what
      actually ends it

Notes:

## 8 — Remote TUI (`core-agent-tui`)

- [ ] **Esc** during a turn cancels **and holds** — banner renders, and
      the status line does not read idle
- [ ] Esc when nothing is in flight still holds (no cancel, banner shows
      no work was lost)
- [ ] Banner distinguishes "a turn was cancelled" from "the gate just
      shut" (`PauseState.Interrupted`)
- [ ] **Type + Enter while held → steer**, not a queued prompt that blocks
      on the gate
- [ ] `/continue` (and `/cont`) releases and carries on
- [ ] `/abandon` releases and stays quiet
- [ ] `/pause` holds without cancelling
- [ ] **Slashes dispatch mid-turn** — pre-core-tui#260 every slash typed
      during a stream was swallowed by the enqueue branch
- [ ] Esc **backs out of the innermost surface first**: a modal, then the
      help sheet, then transcript focus, and only then the agent. Tab into
      the transcript mid-turn: the first Esc returns focus, the second
      cancels-and-holds.
- [ ] **Attach to an already-parked session.** Kill the TUI while held,
      reattach: the banner renders from the `GET /status` seed. Typed
      frames are live fan-out only, so a transition that happened before
      you connected appears in no replay — this is the case the seed
      exists for and the one most likely to regress.
- [ ] **Read-only attachment**: Esc reports the failure rather than
      showing a banner for a gate that never shut. An observer must not be
      able to park somebody else's agent.
- [ ] **Local (in-process) TUI**: Esc still plain-cancels. `Pauser` is
      deliberately declined there — nothing starts a turn but the operator
      pressing Enter, so there is no gate to open. Confirm no banner and
      no 501 noise.

Notes:

## 9 — Two clients agree

The point of emitting from the agent rather than the handler.

- [ ] Attach **two** TUIs (or a TUI and a `curl` on `/events`) to one
      session. Park from one; the other renders the hold.
- [ ] Resume with `mode: "steer"` from one; the other sees the mode, not
      just "something changed"
- [ ] Park from the **library/in-process** side if the deployment has one
      (an embedded TUI, `autonomous.Handle.Pause`) — remote operators must
      still see it

Notes:

## 10 — `/btw` answers about the live session

This is the half of #799 that is not the hold. All five defects were
against Gemini/Vertex, so run this one on a Vertex-backed session.

- [ ] `POST $S/slash/btw {"question":"what are you doing right now?"}`
      returns a real answer — not `context deadline exceeded`, not a
      provider error, not a blank
- [ ] The answer reflects **live run-state**, not just the transcript:
      state, model, turn count, cost so far, pending inbox depth, running
      subagents
- [ ] Ask it **while parked**: the answer says the session is held, and
      says whether a turn was cancelled on the way in
- [ ] `"how much has this cost?"` gives the tracker's number
- [ ] Asking mid-turn **does not interrupt the turn** and persists nothing
      to the event log
- [ ] A declined answer is a **200** `{"empty": true, "detail": "finish_reason=SAFETY"}`,
      not a 500 — "the model declined" must be distinguishable from "the
      daemon is broken"
- [ ] Hammer it past 10/min: **429** with `Retry-After`, rendered as a
      rate-limit message rather than an opaque status error
- [ ] No tool calls fire from a side question (tool-less by construction)

Notes:

## 11 — Multi-session tenant sessions

The third surface. `pkg/compose/multi_session.go` registers through
`attachadapter.New`, so a tenant session should get `PauseController` on
the same footing as the daemon's primary.

- [ ] `POST $BASE/sessions` to create one, then `features.pause` is true
      on **that** session's status
- [ ] The whole of §2 and §3 against `$BASE/sessions/$APP/<new-sid>`
- [ ] Parking one tenant does **not** park another — run two and check
- [ ] A tenant session rebuilt by lazy resume (restart the daemon, then
      touch the session) still reports `features.pause`

Notes:

---

## Result

| Section | Verdict | Follow-up |
|---|---|---|
| 1 Capability handshake | | |
| 2 Interrupt parks | | |
| 3 Resume dispositions | | |
| 4 `/pause` | | |
| 5 Idempotence | | |
| 6 What may un-park | | |
| 7 Subagents | | |
| 8 Remote TUI | | |
| 9 Two clients | | |
| 10 `/btw` | | |
| 11 Tenant sessions | | |

#799 closes when §2, §3, §8 and §10 pass on the GKE run. The rest is
evidence for the follow-ups.
