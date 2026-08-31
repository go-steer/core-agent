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
- [ ] **`POST $S/inject` (default, waking) *does* release the hold** — the
      long-standing interrupt-then-inject client pattern must behave as it
      did before 1.5.0, so a TUI whose send path is `/inject` needs no
      resume call
- [ ] A **daemon restart** while parked: does the session come back
      parked, or running? Record whichever it is — the hold is in-memory
      and this is the one behaviour the design does not promise. ⚠ if it
      silently resumes work.

```sh
post "$S/inject" -d '{"message":"queued while parked","wake":false}' | jq
post "$S/wake"   -d '{}' | jq          # must start no turn
post "$S/inject" -d '{"message":"this one wakes"}' | jq   # default wake:true → releases
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
the `./attach.sh` port-forward on `localhost:7778`.

| Section | Verdict | Follow-up |
|---|---|---|
| 1 Capability handshake | pass | `protocol_version` 1.10.0; `pause` + `interrupt` both true. Old-daemon box needs a pre-hold binary — not testable here |
| 2 Interrupt parks | partial | idle-interrupt path exact: `interrupted:false` + `X-Interrupted: nothing-in-flight` + `paused:true`. Running-turn path not yet driven |
| 3 Resume dispositions | partial | continue + abandon pass, both 400s pass with named causes. Steer needs a real turn to judge |
| 4 `/pause` | partial | `transitioned:true`, operator string echoed, `interrupted` stays unset. Mid-turn "finishes normally" not yet driven |
| 5 Idempotence | pass | second pause `transitioned:false`; resume-when-unpaused `resumed:false`; first-cause-wins holds — a pause on top of an interrupt-hold does not overwrite the reason |
| 6 What may un-park | partial | `inject{wake:false}` and `/wake` both leave the gate shut; `/btw` confirmed inbox depth 1 while parked. Release-consumes-the-queued-message unverified (blocked by F1) |
| 7 Subagents | not run | needs a live delegation |
| 8 Remote TUI | not run | operator-driven |
| 9 Two clients | not run | |
| 10 `/btw` | **fail — F1** | real answers, live run-state, correct cost, 429 + `Retry-After` all pass; then the endpoint died permanently mid-run |
| 11 Tenant sessions | partial | create + handshake (`pause:true`) + cross-session isolation all pass. §2/§3 against the tenant, and lazy resume, not run |

### F1 — one empty part permanently bricks `/btw` for a session

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

Not yet established: which write produced the empty part. The run that
preceded it was interrupt → pause-on-top → abandon → pause → inject
`wake:false` → wake → resume, so a cancelled turn committing a
contentless event is the obvious suspect and would make this an
interaction *between* the two halves of #799 rather than a bug in
either. Confirming it means reading the session DB in the pod.

Fix shape either way: treat a part with no initialized oneof as absent
in both filters, then drop contents left empty. That is defensive at the
read side, which is where it belongs — the history is already on disk
and no producer-side fix retroactively unbricks an existing session.

#799 closes when §2, §3, §8 and §10 pass on the GKE run. The rest is
evidence for the follow-ups. §10 currently does not pass.
