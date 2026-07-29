# `autonomous-resume` — checkpointed resume of an interrupted run

## What this shows

Crash-safe autonomous runs, end-to-end with no LLM credentials, in two
phases against one SQLite event log:

1. **Phase 1** — `autonomous.Run` capped at `MaxTurns(2)` to
   simulate an interruption; the scripted model never gets to call
   `report_done`. The driver emits per-turn checkpoint events into the
   event log.
2. **Phase 2** — `autonomous.Resume` with a
   `autonomous.SessionRef` pointing at the same log. It finds the
   latest non-terminal checkpoint, carries turn count + token totals
   forward, and a fresh scripted model finishes the task. A session
   lock (acquired automatically) keeps two resumers from clobbering
   each other.

## Run it

No credentials needed, runs offline:

```bash
go run ./examples/autonomous-resume
```

## Key APIs

- `autonomous.Run` / `autonomous.Resume` / `autonomous.SessionRef` — `github.com/go-steer/core-agent/v2/pkg/agent/autonomous`
- `eventlog.Open` — `github.com/go-steer/core-agent/v2/pkg/eventlog`
- `agent.WithAppName` / `agent.WithSession` / `agent.WithEventLog` — `github.com/go-steer/core-agent/v2/pkg/agent`
- `mock.NewScripted` — `github.com/go-steer/core-agent/v2/pkg/models/mock`

## What you should see

```
== Phase 1: autonomous.Run with MaxTurns(2) ==
[Phase 1] reason=max_turns_exceeded turns=2 done_detail="" final_text=""

== Phase 2: autonomous.Resume picks up at the next turn ==
[Phase 2] reason=completed turns=3 done_detail="resumed and finished" final_text=""
```

(A few "Event from an unknown agent ... checkpoint-..." log lines are
expected — that's ADK observing the driver's checkpoint events.)

Phase 2's turn count includes Phase 1's — that's the carried-forward
checkpoint state.

## Next

- [`autonomous`](../autonomous/) — the basic loop and `report_done`.
- [`autonomous-handle`](../autonomous-handle/) — in-process control of a running loop.
- API reference: [`docs/site/src/content/docs/embed/api.md`](../../docs/site/src/content/docs/embed/api.md)
