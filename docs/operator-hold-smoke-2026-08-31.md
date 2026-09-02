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

The token is never read off disk — both rigs keep it in the environment,
which is the same rule `dev/uat/attach/run.sh:37` states for the local one.

Local rig (`dev/uat/attach/run.sh`), which defaults `ATTACH_TOKEN` if unset:

```sh
export BASE=http://localhost:7777
export TOK="Bearer ${ATTACH_TOKEN:-uat-token-$(whoami)}"
```

GKE rig (`core-agent-demo-3`), after `./attach.sh` has the port-forward up:

```sh
source ~/projects/core-agent-demo-3/demo-tokens.env   # PLATFORM_TOKEN
export BASE=http://localhost:7778
export TOK="Bearer $PLATFORM_TOKEN"
```

Then, either way — read `APP` and `SID` off the daemon rather than guessing
them, because on a `multi_session` hub the SID is minted per incident:

```sh
curl -s -H "Authorization: $TOK" "$BASE/sessions" \
  | jq '.sessions[] | {app, sessionID, status}'
export APP=_______ SID=_______            # a tenant SID for §11
export S="$BASE/sessions/$APP/$SID"
```

Two helpers, used by every section below. `post` always sends
`Content-Type: application/json` because `pkg/attach/csrf.go:63` requires it
on every state-changing method — including a POST with **no body**, which
otherwise comes back **415** rather than doing anything:

```sh
post() { curl -s -X POST -H "Authorization: $TOK" -H 'Content-Type: application/json' "$@"; }
get()  { curl -s -H "Authorization: $TOK" "$@"; }
```

A long-running turn to interrupt. Anything that takes >30s and is visibly
mid-work; on the demo cluster, `./break-workload.sh bad-image` then asking
the agent to triage gives a real one with subagent delegation in it.

---

## 1 — Capability handshake

The handshake is **not** on `/status`. `StatusInfo` (`state.go:138`) carries
run-state only; the capability report is the first frame of the SSE stream —
`broadcaster.go:394`, *"Capabilities — required first frame per spec section
2.1"* — and the version field there is `protocol_version`, not `protocol`.

Two other shapes worth knowing before you start typing:

- There is no daemon-level `$BASE/status`. Everything below `/sessions` is
  session-scoped (`routeSession`, `handlers.go:205`), registered as
  `/sessions/{app}/{sid}/X` plus the shortcut `/sessions/{sid}/X`. Only
  `GET`/`POST /sessions`, `/whoami`, `/ui` and `/peers*` live at the root.
  `$BASE/X` returns a plain-text 404, which jq reports as `Invalid numeric
  literal` rather than as a missing route.
- Reading a *field that does not exist* is the quieter failure: jq prints
  `null` and exits 0. Two nulls means you are asking the wrong endpoint,
  not that the feature is off.

- [ ] `protocol_version` ≥ `1.5.0`
- [ ] `features.pause` is `true`, and `features.interrupt` is `true`
- [ ] With a daemon that predates the hold, the same frame carries no
      `pause` key — clients must read absent as off, not as unknown

```sh
# First data: frame off the stream, then quit. --max-time is not
# optional: `sed q` closes the pipe, but curl only notices on its NEXT
# write, so against an idle session with no further frames it prints the
# right answer and then hangs forever.
get "$S/events" --no-buffer --max-time 10 \
  | sed -n '/^data: /{s/^data: //p;q}' | jq '{protocol_version, features}'

# /status is the right call for run-state, just not for the handshake:
get "$S/status" | jq
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
# No body: the default is {"hold": true}. The Content-Type is still
# required — without it this is a 415, not an interrupt.
post "$S/interrupt" | jq
get  "$S/status" | jq '{state, paused_since, pause_reason, interrupted}'

# Watch the frames in another terminal for the pause event:
get "$S/events" --no-buffer | grep --line-buffered -E '^event: pause' -A2
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

```sh
post "$S/resume" -d '{"mode":"steer","steer":"drop the rollout, look at the CrashLoop first"}' | jq
post "$S/resume" -d '{}'                  | jq   # continue (the default mode)
post "$S/resume" -d '{"mode":"abandon"}'  | jq

# Both of these must be 400, not a silent no-op:
post "$S/resume" -d '{"mode":"steer"}'    -w '\n%{http_code}\n'
post "$S/resume" -d '{"mode":"banana"}'   -w '\n%{http_code}\n'
```

Notes:

## 4 — `/pause` holds without killing anything

- [ ] Mid-turn `POST $S/pause {"reason":"reviewing the plan"}` returns
      `paused: true, transitioned: true`
- [ ] The **running turn finishes normally** — it is "stop after this
      one", not a freeze. Confirm the final text arrives.
- [ ] The *next* turn is what waits
- [ ] `pause_reason` echoes the operator's string, not the default

```sh
post "$S/pause" -d '{"reason":"reviewing the plan"}' | jq
get  "$S/status" | jq '{state, pause_reason, interrupted}'
```

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

```sh
post "$S/pause" -d '{}' | jq '{paused, transitioned}'   # expect transitioned:false the 2nd time
post "$S/resume" -d '{}' | jq '{resumed}'               # expect resumed:false when not paused

# First-cause-wins: interrupt, then pause on top. reason must NOT change.
post "$S/interrupt" >/dev/null
post "$S/pause" -d '{"reason":"should not stick"}' >/dev/null
get  "$S/status" | jq '{pause_reason, interrupted}'     # operator-interrupt / true
```

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
- [ ] **`POST $S/inject` (default, waking) does *not* release the hold**
      (#878, protocol 1.11.0). It queues like any other message and the
      session stays `paused`. This item asserted the opposite when the
      checklist was written; the run found the old behavior let a
      k8s-lookout alert un-park a session nobody had resumed, so the
      rule inverted. Only `POST $S/resume` opens the gate.
- [ ] A **daemon restart** while parked: does the session come back
      parked, or running? Record whichever it is — the hold is in-memory
      and this is the one behaviour the design does not promise. ⚠ if it
      silently resumes work.

```sh
post "$S/inject" -d '{"message":"queued while parked","wake":false}' | jq
post "$S/wake"   -d '{}' | jq          # must start no turn
# Default wake:true. `woke:true` in the response is the WAKE firing, not
# the gate opening: the signal is buffered-1 and latches. The session
# must still read `paused`, with the SAME paused_since.
post "$S/inject" -d '{"message":"this one wakes"}' | jq
post "$S/resume" -d '{"mode":"steer","steer":"list verbatim every message that was waiting in your inbox"}' | jq
```

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

```sh
post "$S/interrupt" | jq '{interrupted, paused, running_subagents, stopped_subagents}'
post "$S/agents/cluster/stop" | jq          # 404 if that one isn't running
post "$S/interrupt" -d '{"stop_subagents":true}' | jq '{stopped_subagents}'
```

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

```sh
post "$S/slash/btw" -d '{"question":"what are you doing right now?"}' | jq
post "$S/slash/btw" -d '{"question":"how much has this cost?"}'      | jq

# Rate limit: 10/min, burst 5. Expect 429s partway through, with
# Retry-After on them.
for i in $(seq 1 15); do
  post "$S/slash/btw" -d '{"question":"ping"}' -o /dev/null -w '%{http_code}\n'
done
```

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

```sh
# The response `url` is already absolute (echoed from your Host header) —
# use it as-is, do not prefix $BASE onto it.
T=$(post "$BASE/sessions" -d '{}' | jq -r .url)
get "$T/events" --no-buffer --max-time 10 \
  | sed -n '/^data: /{s/^data: //p;q}' | jq '{protocol_version, features}'

# Then re-run §2 and §3 against $T, and check isolation against $S:
post "$T/interrupt" >/dev/null
get  "$S/status" | jq '{state}'    # the other session must NOT be paused
```

Notes:

---

## Result

GKE run, 2026-08-31, demo-3 on `simian-test` / ns `kube-platform-native`:
daemon `ghcr.io/go-steer/core-agent:main-85abf50`, watcher
`lookout:v0.22.0`, content `v12`, model `gemini-3.7-flash`. Driven over
the `./attach.sh` port-forward on `localhost:7778`. §6 was later re-run
on `main-b265a03` after #879 landed — see the re-run at the end.

| Section | Verdict | Follow-up |
|---|---|---|
| 1 Capability handshake | pass | `protocol_version` 1.10.0; `pause` + `interrupt` both true. Old-daemon box needs a pre-hold binary — not testable here |
| 2 Interrupt parks | partial | idle-interrupt path exact: `interrupted:false` + `X-Interrupted: nothing-in-flight` + `paused:true`. Running-turn path not yet driven |
| 3 Resume dispositions | partial | continue + abandon pass, both 400s pass with named causes. Steer needs a real turn to judge |
| 4 `/pause` | partial | `transitioned:true`, operator string echoed, `interrupted` stays unset. Mid-turn "finishes normally" not yet driven |
| 5 Idempotence | pass | second pause `transitioned:false`; resume-when-unpaused `resumed:false`; first-cause-wins holds — a pause on top of an interrupt-hold does not overwrite the reason |
| 6 What may un-park | **fail — F3**, then **pass** on the re-run | See F3 and the re-run below |
| 7 Subagents | not run | needs a live delegation |
| 8 Remote TUI | not run | operator-driven |
| 9 Two clients | not run | |
| 10 `/btw` | **fail — F1** | real answers, live run-state, correct cost, 429 + `Retry-After` all pass; then the endpoint died permanently mid-run, taking `Compact` and `Checkpoint` with it. #874, fixed by #875 |
| 11 Tenant sessions | partial | create + handshake (`pause:true`) + cross-session isolation all pass. §2/§3 against the tenant, and lazy resume, not run |

### F1 — one empty part permanently bricks `/btw` for a session

Filed as [#874](https://github.com/go-steer/core-agent/issues/874),
fixed by [#875](https://github.com/go-steer/core-agent/pull/875).

After the §2–§6 sequence, every `/btw` call on `default` began returning
**500**, forever:

```
agent: AskSideQuestion: generate: failed to call model: Error 400,
Message: * GenerateContentRequest.contents[8].parts[0].data:
required oneof field 'data' must have one initialized field
```

A session created fresh answers `/btw` normally, so this is the
transcript, not the endpoint. `AskSideQuestion` re-sends the whole
history on every call, so once one malformed event is committed the
side-channel never recovers for that session. Restarting the daemon is
not expected to help either — demo-3 keeps its session DB on a PVC, so
the bad event is on disk, not in memory. Not directly tested; the only
recovery verified here is starting a new session.

Both history filters drop a content with *zero* parts and neither drops
a content whose parts are individually empty:

- `sessionHistory` (`pkg/agent/btw.go:345`) tests
  `len(ev.Content.Parts) == 0`, so a single empty `&genai.Part{}`
  survives.
- `normalizeToolPairs` (`pkg/agent/history_pairing.go:108`) explicitly
  returns `false` from `drop()` for a nil part, and its contract —
  *"contents left with zero parts are dropped"* — never contemplated a
  part that is present but carries no oneof.

Vertex rejects the request outright, so this is not a degraded answer;
it is a hard 500 on a surface whose whole design goal was that "the
model declined" and "the daemon is broken" must look different. This
lands squarely on §10's *"a declined answer is a 200, not a 500"* box.

**Blast radius, established after the first write-up of this finding.**
It is not only `/btw`. The daemon log carries the same 400 arriving on
the checkpointer's own schedule:

```
agent: pending checkpoint failed: agent: Checkpoint: generate:
failed to call model: Error 400, Message: *
GenerateContentRequest.contents[8].parts[0].data: ...
```

`Checkpoint` and `Compact` both run through `runSummarizer` →
`summarizerHistory` → `normalizeToolPairs`, the same choke point `/btw`
uses. So one bad part takes out the side-channel, compaction *and*
checkpointing together.

The agentic loop is **not** affected — it does not build its contents
through that path. Verified live: injecting a prompt into the poisoned
session returned 16 `agent` frames, a `turn-complete`, and the correct
answer. That asymmetry is why this was quiet enough to reach a live
cluster. A session looks perfectly healthy — it answers, it runs tools —
while three of its maintenance surfaces are dead and only the log says
so.

Not yet established: which write produced the empty part. The run that
preceded it was interrupt → pause-on-top → abandon → pause → inject
`wake:false` → wake → resume, so a cancelled turn committing a
contentless event is the obvious suspect and would make this an
interaction *between* the two halves of #799 rather than a bug in
either. Confirming it means reading the session DB in the pod.

Fix shape either way: treat a part with no initialized oneof as absent,
then drop contents left empty. That is defensive at the read side, which
is where it belongs — the history is already on disk and no
producer-side fix retroactively unbricks an existing session. #875 does
this with one predicate in `normalizeToolPairs`, which turns out to
cover all three callers; `sessionHistory`'s own filter needed no change.

### F2 — a created-but-never-run tenant session logs an error per tick

Not filed; recorded so the next reader does not chase it. The tenant
session created in §11 produces, once per auto-continue interval:

```
core-agent: session <sid>: auto-continue: read session:
database error while fetching session: record not found
```

Under `kubectl logs --tail` this reads as a tight loop, which is what it
looked like at first. It is not: counted over five minutes it fires
exactly once per interval. A session that exists in memory but has never
persisted an event has no row to read, and auto-continue says so every
time it wakes. Harmless, but it is log noise proportional to the number
of idle tenant sessions, and "record not found" is a poor way to say
"nothing has happened here yet."

### F3 — any wake-true inject releases an operator hold (#878)

§6's last box asserted that a default `POST /inject` *should* release
the hold, on the compatibility argument in
`docs/operator-interrupt-design.md`. The run inverted that box.

Observed in the TUI: an agent parked by operator interrupt printed a
bare

```
ℹ  Resumed.
```

that no operator had asked for, and the operator's follow-up
`/continue` answered `/continue: agent isn't held`. Bare "Resumed." is
`resumedSystemText("")` — a resume with empty mode, i.e. a direct
`Agent.Resume()`.

Reproduced deterministically against session
`01a058e6-b752-7cff-aa05-ce2aa38ef07b`:

```
park:                 {"interrupted":false,"paused":true}
status after park:    {"state":"paused","pause_reason":"operator-interrupt",
                       "paused_since":"2026-08-31T19:20:09Z"}
inject (wake=true):   {"woke":true,...}
status after inject:  {"state":"idle","paused_since":"0001-01-01T00:00:00Z"}
```

The guard was `caller.Identity != AutoContinueOriginator` — a denylist
of one, so the condition meant "any wake-true inject". In this
deployment that is not hypothetical: the lookout watcher's inject
envelope is `{"message": ...}` with no `wake` field
(`pkg/inject/injector.go:177-184`), so it defaults to true and every
alert it raises re-opened the gate.

Not subagent completion, which was the first suspect: `pushAlert` →
`wakeParent()` → `RequestWake` is a wake, and wakes never touched the
hold.

Fixed by #878: an inject queues and only `POST /resume` opens the gate
(protocol 1.11.0). §6's box is rewritten above to match. The specific
injector behind the *first* sighting was never identified — the daemon
logs no pause/resume transitions — but the mechanism is proven and the
watcher is sufficient to explain it.

### §6 re-run — pass, on `main-b265a03`

Same cluster and namespace, daemon re-pinned to
`ghcr.io/go-steer/core-agent:main-b265a03` (demo-3 commit `86204fd`;
watcher and content unchanged at `v0.22.0` / `v12`). Capability frame
now reports `protocol_version: 1.11.0`.

Parked an idle session, then walked the whole ladder without touching
`/resume`:

```
interrupt:                {"interrupted":false,"paused":true}
inject {wake:false}:      {"woke":false}
  status → paused, paused_since 2026-08-31T20:35:52.330254549Z
wake:                     {"woken":"default"}
  status → paused, paused_since UNCHANGED (no turn on the stream)
inject (default, wake:true): {"woke":true}
  status → paused, paused_since UNCHANGED
```

`woke:true` with the gate still shut is the whole shape of the fix: the
wake fired and **latched** (buffered-1), and the gate is a separate
thing that only `/resume` opens. Pre-fix, that third call zeroed
`paused_since` and the session went `idle`.

Then the half F1 had blocked — that the release consumes what was
queued. `POST /resume {"mode":"steer","steer":"list verbatim every
message that was waiting in your inbox…"}` produced one `[Inbox]`
block, in arrival order, with the operator's steer last:

```
[Inbox]
- from platform-oncall@example.com: UAT-A queued with wake false
- from platform-oncall@example.com: UAT-B default wake-true inject, standing in for a lookout finding
- from platform-oncall@example.com: [The operator interrupted you] …
```

Nothing dropped, nothing consumed by a turn that never ran, and the
wake-false and wake-true messages are indistinguishable once parked —
which is exactly what the 1.11.0 reference now promises. The model
answered the steer directly rather than resuming the old work (#857's
"a new question → answer it" branch).

`/btw` also answers again on this build (`POST /slash/btw` returned
live run-state), though this session's transcript is short and does not
reproduce F1's long-history conditions.

Two §6 boxes have no result from either run: auto-continue stand-down
over two intervals (the effective per-session cadence is the 10m
breaker window, and this session was interrupted while *idle*, so
there was nothing for the driver to continue), and
daemon-restart-while-parked. Neither is affected by #878.

#799 closes when §2, §3, §8 and §10 pass on the GKE run. The rest is
evidence for the follow-ups. §10's long-session case is still unproven.

---

## Result — 2026-09-02 re-run on `2.9.0-dev.4`

Same cluster and namespace, demo-3 on `simian-test` / ns
`kube-platform-native`: daemon `ghcr.io/go-steer/core-agent:2.9.0-dev.4`,
watcher `lookout:v0.23.0`, content `v12`, model `gemini-3.7-flash`.
Driven over the `./attach.sh` port-forward on `localhost:7878`; §8 driven
by the operator in `core-agent-tui`, everything else over `curl` with a
live SSE tap on `/events`.

This run was seeded by a real incident (`./break-workload.sh bad-image`
against `emailservice`), so the sessions are watcher-minted and
per-incident, not `default`.

| Section | Verdict | Follow-up |
|---|---|---|
| 2 Interrupt parks | pass | running-turn path now driven, which the first run could not do |
| 3 Resume dispositions | pass | continue, abandon and steer all fired against real turns |
| 4 `/pause` | pass | mid-turn "finishes normally" now proven — see the timings under §8 below |
| 7 Subagents | pass, bar one box | stop-already-stopped returns 200, not 404 — **F5**, filed as [#897](https://github.com/go-steer/core-agent/issues/897) |
| 8 Remote TUI | **fail — F4** | 9 of 12 boxes pass, 1 not run; the headline box fails. Filed as [core-tui#302](https://github.com/go-steer/core-tui/issues/302) + [#896](https://github.com/go-steer/core-agent/issues/896) |
| 9 Two clients agree | pass | |
| 10 `/btw` | pass | **F1 confirmed fixed.** Survived the full §2–§7 sequence *and* a 15-way concurrent hammer; returned live run-state (`State: Paused (paused (operator-interrupt))`, `Turn Cancellation: Yes`, `Turns Completed: 12`, `Cost So Far: $0.07`), rate-limited exactly (5×200 then 10×429 with `Retry-After: 6`), and persisted nothing |

§8 box by box: pass on Esc-while-idle-still-holds, type+Enter-steers,
`/continue`, `/abandon`, `/pause`-holds-without-cancelling,
slashes-dispatch-mid-turn, Esc-peels-innermost-surface-first, and
attach-to-an-already-parked-session. Fail on
Esc-during-a-turn-cancels-and-holds and on
banner-distinguishes-cancelled-from-gate-shut (F4).

The local in-process TUI box passes **by construction**, not by an
interactive run: `Pauser` is declined at
`cmd/core-agent/coretui_guards.go:124`, so `tui/update.go:1590` cannot
take the hold branch and Esc falls through to `cancelTurn` — no banner
to render and no call to 501 on. `go test ./cmd/core-agent/` covers the
decline. Not run at all: read-only attachment, which needs a viewer
identity not provisioned on this cluster.

### F4 — Esc mid-turn holds but never cancels

Filed as [core-tui#302](https://github.com/go-steer/core-tui/issues/302)
(client, and where the cheap fix lives) and
[#896](https://github.com/go-steer/core-agent/issues/896) (daemon).

The `## 8` headline box. Two runs against the same prompt, one keystroke
apart in intent, are indistinguishable on the wire except for a string.

The prompt is a delegation — *"delegate this to the cluster specialist
and wait for its findings before replying"* — which parks the parent in
`spawn_agent{wait: true}` for minutes. That is the widest possible
window and the case operator hold exists to serve.

`/pause`, injected 11:46:31.7:

```
11:46:42.1   pause  reason="operator-pause"    ← 10.4s into the turn
seq 135      functionCall record_plan          ← after the pause
seq 137      functionCall spawn_agent {cluster}
turn-complete  latency_ms: 140623
status         state: paused, paused_since 11:46:42
```

Correct, and exactly what §8's `/pause` box asks for: the gate shuts, the
in-flight turn finishes normally, no new turn starts.

**Esc**, injected 11:50:47.0:

```
11:50:59.2   pause  reason="operator interrupt"  ← 12.2s into the turn
seq 155      functionCall spawn_agent {cluster}  ← after the Esc
turn-complete  latency_ms: 238223
status         state: paused, paused_since 11:50:59
```

The operator pressed Esc 12 seconds in. The turn ran a further **226
seconds**, spawned the subagent, and delivered a 1,690-token report —
with the hold banner up the whole time. No `interrupted` flag on the
`PauseEvent` in either run.

The reason string is the only surviving trace of intent.
`operator interrupt` with a space is client-minted
(`core-tui tui/slash_builtin.go:299,310`); the daemon's own constant is
`operator-interrupt` with a hyphen (`pkg/agent/pause.go:33`) and a bare
`/pause` yields `operator-pause`. So the keystroke did reach
`holdCmd` — it just took the wrong branch.

Three links, each confirmed by reading the code and then by the run:

1. `AttachStatus()` never returns `AgentStateRunning`
   (`pkg/attachadapter/capabilities.go:193`, admitted in its own
   comment: *"running / deferred still need run-loop instrumentation
   that hasn't been wired"*). So `/status` and the SSE seed always say
   `idle`. **core-agent.**
2. core-tui receives `turn_state` and discards it — `tui/update.go:472`
   says so explicitly: *"Reserved for follow-up work that unifies push +
   in-band turn state."* **core-tui.**
3. so `spinnerActive` — set only by `beginLiveStretch()`, i.e. the
   operator's own inject from that client or an arriving partial chunk —
   stays false while the parent blocks on a subagent. `turnInFlight()`
   (`tui/view.go:712`) reads false, and `holdCmd` (`tui/pause.go:283`)
   picks `pauseCmd` over `interruptThenPauseCmd`. **core-tui.**

Blast radius is not a narrow race. `endLiveStretch` fires on every
commit (`Partial=false`), so the exposure is *both* the window before
the first token — every watcher incident, since a per-incident session
and its first turn are born together and no operator can be attached
before it — and every window after any commit while the turn continues.
The `spawn_agent{wait: true}` case above is the second window, and it is
the runaway-subagent scenario the feature was built for.

Cheapest correct fix is link 3: `holdCmd` guards on `turnInFlight()` for
no benefit. Interrupt-while-idle is already a defined safe no-op — the
daemon answers `X-Interrupted: nothing-in-flight` — so the client can
always take `interruptThenPauseCmd` and stop needing to know something
it structurally cannot know. Links 1 and 2 are still worth closing:
until `Interrupted` can be set truthfully, the banner cannot honour
§8's *"distinguishes a turn was cancelled from the gate just shut"* box
no matter what the client does.

### F5 — stopping an already-stopped subagent returns 200, not 404

Filed as [#897](https://github.com/go-steer/core-agent/issues/897).
§7's last box. Three places specify a 404 and the implementation
disagrees with all three:

- `pkg/attach/handlers_pause.go:142-144` — *"404 when no running
  subagent by that name is found … a 200 here would read as
  'stopped'"*.
- `pkg/attachadapter/pause.go:107-110` — *"reports false when the
  manager has no live subagent under that name (including one that
  already finished)"*.
- the §7 checklist item itself.

`pkg/agent/background/manager.go:1059-1082` instead documents the
opposite as deliberate: *"Stopping an already-stopped subagent is a
no-op that still reports true: the handle is still registered, so the
operator's intent was satisfied."* Observed: 200 with
`stopped: true`.

Either reading is defensible; shipping both is not. The operator-facing
question is whether "I stopped it" and "it had already finished" should
look the same, and the two comments answer it differently.

### F6 — a transient provider 400 is classified as a non-retryable config error

Filed as [#898](https://github.com/go-steer/core-agent/issues/898).
One sample, low severity, recorded so it is not mistaken for F1's shape.
During a window in which Vertex was also returning 429s, one turn failed
immediately after a `functionResponse` with:

```
{"kind":"config_error","code":"400","retryable":false,
 "hint":"Check the model provider config (model.vertex.location,
 model.name, GOOGLE_CLOUD_PROJECT ...)"}
```

Not poisoned history, which was the first suspect given F1: a plain-text
turn on the same session immediately returned `ack`, and a
single-tool-call turn then ran a full
`functionCall → functionResponse → text` cycle clean against the same
transcript. Vertex was returning `INVALID_ARGUMENT` transiently under
the same load producing the 429s.

The cost of the misclassification is that `retryable: false` forecloses
the retry that would have worked, and the hint sends the operator to
debug a configuration that is correct.

### Out of scope — `break-workload.sh restore` exits 0 on a failed undo

demo-3's, not core-agent's, but it cost time here. `restore` ran
`rollout undo`, timed out (*"1 old replicas are pending termination"*),
rewound to a revision that was itself broken, and **exited 0** leaving
`does-not-exist:v0-demo-break` deployed. Recovered with an explicit
`kubectl set image`. The failed-restore window also minted two junk
incident sessions. `restore` should verify the landed image and exit
non-zero if it is not the expected one.

### Verdict on #799

§2, §3 and §10 now pass on GKE. §8 does not: 9 of its 12 boxes pass and
1 is not run, but the headline box — *"Esc during a turn cancels **and** holds"* —
fails, and the `Interrupted` box fails structurally. By this document's
own closing criterion **#799 does not close on this run**; it closes
when core-tui#302 is fixed and §8's two failing boxes are re-driven.

The re-drive is cheap now that there is a reliable wide window: a prompt
that tells the parent to delegate and wait parks it in
`spawn_agent{wait: true}` for 140–240s, against turns that otherwise run
9–18s here because the persona reports after each tool cycle. Three
earlier attempts to widen the window with long-generation prompts all
died on Vertex 429s; the delegation shape costs a fraction of the tokens
for ten times the wall clock.
