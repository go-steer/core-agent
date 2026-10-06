# Durable per-turn failures (#1258)

Box A2 (#1042) is settled as: *for one run, no failure class has more
entries in the daemon log than in the transcripts.* Two of the three
countable classes failed that in their plainest form on the 2026-10-06
fault batch (`main-24f2ae3`, `--max-turn-cost-usd=0.01`, drill A twice):
7 guardrail cuts and 7 turn errors in the daemon log, 3 and 3 in the
drill's live capture, **0 and 0** when the three sessions were replayed
from the server afterwards.

The cause is structural. A per-turn guardrail trip and a turn error each
reached exactly two surfaces: a log line, and a typed SSE frame
(`guardrail-trip`, `turn-error`) that the broadcaster publishes to
whoever is subscribed at that instant and never replays. The only
guardrail fact that reached the eventlog was a session **halt**
(`NewGuardrailTripEvent`, #643), and it is there so a restart can
restore it, not as a record. The drill's 3-of-7 was luck: it happened to
be attached for those turns.

A second gap, in the rig rather than the product: run 2's incident
opened two sessions and the drill captured one, so even a fixed daemon
would have failed A2 on that run.

## What counts, on each side

| Failure | Log line (`a2_count.py`) | Transcript record after this change |
|---|---|---|
| Per-turn cost trip, cut mid-turn (#1049) | `cost_ceiling guardrail cut the turn in flight` | `guardrail-turn-trip` row, `halted_turn: true` |
| Per-turn cost trip at the boundary | `cost_ceiling guardrail tripped:` | `guardrail-turn-trip` row, `halted_turn: false` |
| Turn-scoped watchdog cut (#1090), either place | `watchdog guardrail cut… / tripped:` | `guardrail-turn-trip` row |
| Any trip that halts the session (per-session ceiling, the third per-turn trip in a row, a session-scoped watchdog Critical) | `… cut the turn in flight` or `… tripped:` | the existing `guardrail-trip` halt row (#643), and no other |
| Refusal-storm cut (#1081) | `refusal_storm guardrail cut the turn in flight` | the existing `gate/refusal-storm` audit row |
| Turn error, any kind, including a guardrail's `canceled` | `session <id> turn: <err>` (wake loop) / `core-agent: turn: <err>` (REPL) | `turn-error` row |
| Turn refused at pre-flight (guardrail still tripped; pause gate's context died) | same turn line | `turn-error` row |
| Context-budget cut (#975) | its own line (not counted by `a2_count`) | the existing `context-reduction-degraded` row, `operation: turn-cut` — unchanged |

Every row is one append per failure, written at the same site that logs
it and emits its frame. A refusal storm is therefore one trip **and** one
turn error on both sides: one cut line and one turn line in the log; one
refusal-storm row and one turn-error row (`cut_by: refusal_storm`) in
the transcript. That keeps `a2_count`'s two classes disjoint and each
well-defined.

## The rows

Both new rows follow `guardrail_events.go` and
`context_reduction_events.go`: an event appended to the session's
eventlog with an `agent/…` author, the row's name in `InvocationID`, no
`Content`, and everything in `CustomMetadata`.

**`guardrail-turn-trip`**, `Author=agent/guardrail-turn-trip` — a trip
that left the session running.

| Key | Value |
|---|---|
| `source` | `agent` |
| `guardrail` | `cost_ceiling` or `watchdog` |
| `reason` | the operator-facing sentence, verbatim — the same text as the log line and the typed frame |
| `halted_turn` | `true` when the trip cut the turn in flight (a `canceled` turn error follows), `false` at the boundary |

**`turn-error`**, `Author=agent/turn-error` — one turn error.

| Key | Value |
|---|---|
| `source` | `agent` |
| `kind`, `code`, `message`, `retryable`, `hint` | `attach.ClassifyTurnError`'s `TurnError`, the same value the typed frame carries; `code` and `hint` omitted when empty |
| `prompt_id` | the turn's correlation handle; omitted for a pre-flight refusal, which never got one |
| `cut_by` | the guardrail kind whose cut produced the error (`cost_ceiling`, `watchdog`, `refusal_storm`), from `consumeGuardrailHalt`; omitted when no guardrail was involved |

The halt row (`guardrail-trip`, #643) gains the same `halted_turn` key
(`attach.NewGuardrailHaltEvent`). The fold ignores it. A client replaying
the session needs it for the same reason it needs the field on the live
frame: a halt that cut a turn is followed by a `canceled` turn error,
and core-tui absorbs that cancel only under a trip that says it cut the
turn. An older halt row has no key and reads as `false`.

Readers in `pkg/attach`: `GuardrailTurnTrip`, `GuardrailHaltRow`,
`TurnErrorRow`. Each matches author **and** name.

## Settled decisions (do not relitigate)

1. **Every per-turn trip and every turn error gets a row.** A2's wording
   says so, and a row per failure is what makes "log count ≤ transcript
   count" checkable at all. The cost is one small eventlog row per
   failure; a session that fails every turn already writes far more per
   turn than this.

2. **One row per failure, from one site.** `emitGuardrailTrip` already
   was the single site for the log line and the typed frame of every
   watchdog and cost trip (#1131). It now takes `haltedSession` and
   writes exactly one row: the halt row when the trip halted the
   session, the per-turn row otherwise. The `queueOutOfBandEvent` calls
   that used to sit in `maybeEnforceCostCeiling` and `maybeTripWatchdog`
   moved into it, so the two arms cannot drift and a halt is never also
   a per-turn row. The turn-error row is written beside the typed
   frame in `Run`'s cleanup. The refusal storm keeps its #1081 audit row
   rather than gaining a second one.

3. **A per-turn row has its own author, so it can never restore as a
   halt.** `FoldGuardrailEvents` keys on `agent/guardrail-trip`. A shared
   author plus a `scope` key would be correct in this binary and wrong
   in an older one rolled back over the same eventlog, which would fold
   every per-turn cut into a halt that needs an operator reset. A
   distinct author is rollback-safe by construction.

4. **Replay delivers the rows as ordinary `agent` frames; the typed
   frames stay live-only.** This is the #643 / #908 / #974 precedent and
   needs no new event type. Re-synthesizing typed frames on replay was
   rejected: `turn-error` is terminal, and replaying one into a client
   that attached mid-turn would end, in that client's state machine, a
   turn that is still running — and the exactly-one-terminal-frame rule
   and the #864 terminal barrier are both written about live turns.

5. **The typed frames name their row: `event_id`, protocol 1.19.0.**
   An attached client receives a failure twice — the typed frame and the
   row's `agent` frame — and must count it once. Both `GuardrailTrip`
   and `TurnError` gain an optional `event_id` holding the row's event
   ID. Additive and `omitempty`, so a minor bump; absent when the
   session has no eventlog (nothing was written, so nothing is named).
   Content matching (same guardrail + reason) was rejected: two identical
   cuts in one session are normal under a re-driving loop, and a
   multiset match on prose is exactly the kind of heuristic that breaks
   on a wording change.

6. **The rows are not conversation.** No `Content`, so ADK's contents
   processor (`buildContentsDefaultWithCallSource` skips an event with
   no content), compaction's `summarizerHistory`, tail repair, the
   approver's context and every `FinalText` collector skip them. No
   `LLMResponse.ErrorCode` / `ErrorMessage`: auto-continue's
   `classifyInterruptedTail` reads `ErrorCode` *before* it skips
   content-less rows, so a row carrying one would read as an in-band
   model error and change the re-drive decision. No
   `CompactionMetadataKey`, so `/context` stats and boundary slicing are
   untouched. No `UsageMetadata` or `side_usage`, so
   `RebuildTrackerFromEvents` books nothing. The rows are written out of
   band, never yielded from `Run`, so `collectFinalText` and the
   autonomous loop's substantiveness check cannot see them either.

7. **Written in the #565 window, before the terminal frame.** The rows
   go through `queueOutOfBandEvent`: a trip that fires mid-turn is
   parked until the cleanup's drain, after the runner has released its
   session handle. The turn-error row is queued just before that drain,
   so it is in the log before the typed `turn-error` goes out, and the
   terminal barrier delivers it to every attached client first. The
   drain is serialized end to end (`outOfBandDrainMu`, across the take
   and the appends): two drains racing — an operator's reset on an HTTP
   goroutine and a turn's cleanup — could otherwise each take a batch
   and append them in reverse, and a replay that shows a `canceled`
   before the trip that explains it renders wrongly.

8. **A refused turn gets a row and still no frame.** The driver logs a
   pre-flight refusal as a turn error, so A2 needs its transcript twin.
   It never opened a turn, so a terminal frame would terminate nothing;
   that has been the documented behaviour since #891 and stays.

9. **No session, no row.** A turn whose session does not exist (the
   runner fails before it), or an agent with no eventlog, has no
   transcript to write to; the log line stands alone. That is not an A2
   miss in any countable sense: A2 compares the log against the
   transcripts that exist. `RunWithContents` (the AX path) creates a
   fresh session per call, emits no frame and logs no turn line, so it
   is out of scope.

10. **core-tui renders the rows, first sighting wins.** The remote
    adapter projects a guardrail row to `GuardrailTrip` and a turn-error
    row to `TurnError`, exactly as their typed frames would, and drops
    whichever of the pair arrives second (keyed on `event_id`). The
    order differs by kind — a mid-turn trip's frame precedes its row, a
    turn error's row precedes its frame — so the dedup is symmetric. A
    replayed cut followed by its replayed `canceled` reproduces the live
    rendering, including core-tui's absorb of the cancel. Rows are
    rendered only from a daemon announcing 1.19.0+, because an older
    daemon writes halt rows with no `event_id` to pair against.

11. **`a2_count.py` counts rows and dedups by ID.** Transcript side:
    guardrail trip = halt rows + per-turn rows + refusal-storm rows +
    typed `guardrail-trip` frames whose `event_id` names no row; turn
    error = turn-error rows + typed `turn-error` frames likewise.
    Every `agent` frame is counted once across all inputs, keyed on its
    event ID, so a session passed both live and replayed counts once —
    which also fixes the latent double count of retry stamps and failed
    delegations across two captures of one session. The
    `turn-error kind == refusal_storm` rule is removed: no daemon ever
    produced that frame (the storm's turn error is `canceled`). A typed
    `guardrail-trip` with no `event_id` comes from a pre-#1258 daemon,
    which wrote the halt row but no per-turn rows; such a frame pairs
    with a halt row of the same guardrail and reason, each row once, so
    re-grading an old capture does not count one halt twice.

12. **The rig grades on server-side replays.**
    `dev/uat/gke-drill/replay_sessions.sh` lists every session on the
    hub whose `last_touched_at` falls in a window and saves each one's
    `?since=0` replay. A session touched in the window is a superset of
    those opened in it (the hub has no `created_at`), the safe direction.
    Idle sessions are skipped unless `--include-idle`: reading `/events`
    on one lazily resumes it, and a resume can run auto-continue. The
    status is read once from the list, so a session evicted in the
    seconds between the list and its replay is resumed by the replay;
    the helper is meant to run right after a batch, well inside the
    idle-eviction window.

13. **Replays are graded inside a window, on both sides.** A session
    touched in the window is a superset of those opened in it, and every
    replay reads from seq 0, so replays carry history from before the
    batch. For A2 that is the **masking** direction: a row with no log
    to answer it covers a log-only failure of the same class. So
    `a2_count.py --since/--until` drops `agent` frames by
    `event.Timestamp` and daemon-log lines by their `--timestamps`
    prefix, outside the window, and the row's note reports how many.
    Undated entries — the typed frames, which carry no timestamp, and
    any undated log line — are kept and counted in the note, because
    dropping them could only hide entries. `replay_sessions.sh` records
    the window in `window.env` and prints the exact invocation.

## Out of scope

- core-tui rendering of the refusal-storm row. Its turn error renders
  (`canceled`), but the storm's own reason has no typed counterpart and
  borrowing `GuardrailTrip` would offer a reset for a condition with
  nothing to reset (#1081's argument). A core-tui follow-up can render
  the row from its author.
- Lookout and other attach consumers. They receive the rows as `agent`
  frames they already tolerate; adopting `event_id` is theirs to do.
- Updating `core-tui/docs/sse-event-stream-protocol.md` for 1.19.0 —
  an upstream follow-up; the producer-side fixtures and the published
  attach reference are updated here.
- Subagent turn errors in `a2_count`. A child's rows land in the child's
  session; the daemon logs no turn line for them, so counting them
  would only add TRANSCRIPT-ONLY entries.
- A refused turn under the local `--tui` host or a headless `-p` run.
  It writes its row whatever the caller, but only the wake loop (the
  daemon, the drill) and the REPL log the `turn:` line `a2_count`
  matches, so there it is TRANSCRIPT-ONLY by construction.
- A daemon log that reports several errors for one turn. The wake loop
  logs every error `Run` yields; the turn has one `turnErr` and one row.
  ADK yields at most one error per run today, so this is theoretical.
- Restoring the per-turn trip streaks (#1049 / #1090) across a restart
  from these rows. The streaks are in-memory by design; the rows are a
  record, not state.
- Auto-continue's treatment of a guardrail-cut tail (it reads the tail
  shape, which a cut leaves mid-tool). Unchanged; these rows do not move
  that decision either way.
- Transcript export (#887). Not built; when it is, it renders the
  eventlog and so includes these rows with no further work.
