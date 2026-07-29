# `autonomous-handle` — Pause / Resume / Inject / Stop on a live run

## What this shows

The `autonomous.Handle` control API: `autonomous.Start`
launches the loop on a goroutine and returns a handle you steer from
outside. The example pauses the run mid-flight, queues a message with
`Inject` (it lands on the next turn's prompt as an `[Inbox]` block),
resumes, waits for the bounded run to finish, and calls `Stop`
(idempotent, safe after terminal).

No LLM credentials — the model is the echo mock wrapped in a small
`slowLLM` delay shim so the example has a wide-enough window to pause
between turns (real network latency gives you the same window).

## Run it

No credentials needed, runs offline:

```bash
go run ./examples/autonomous-handle
```

Takes a couple of seconds (three 400 ms echo turns plus the pause).

## Key APIs

- `autonomous.Start` — `github.com/go-steer/core-agent/v2/pkg/agent/autonomous`
- `autonomous.Handle.Pause` / `.Resume` / `.Inject` / `.Wait` / `.Stop` / `.Status`
- `autonomous.WithMaxTurns` / `autonomous.WithMaxWallclock` — run budgets
- `mock.NewEcho` — `github.com/go-steer/core-agent/v2/pkg/models/mock`

## What you should see

```
== autonomous.Start ==
  status: running
== Pause ==
  status: paused
== Inject ==
  queued: "priority changed: hello from the example"
== Resume ==
  status: running
== Wait ==
  reason:    max_turns_exceeded
  turns:     3
  finalText: "continue"
  status:    failed
```

(The echo agent never calls `report_done`, so the bounded run ends by
`max_turns_exceeded` rather than completing.)

## Next

- [`autonomous`](../autonomous/) — the blocking `autonomous.Run` form and the `report_done` gesture.
- [`autonomous-resume`](../autonomous-resume/) — durable checkpoints + resume across processes.
- API reference: [`docs/site/src/content/docs/embed/api.md`](../../docs/site/src/content/docs/embed/api.md)
