# `autonomous` — autonomous.Run loop shape and the report_done gesture

## What this shows

Driving an agent through `autonomous.Run` end-to-end with no
LLM credentials. A scripted transcript stands in for the model: it
calls the driver's internal `report_done` lifecycle tool with
`state="done"`, then emits a final text summary. The driver loops,
detects the done call, and returns a structured `RunResult` (reason,
turn count, done detail, final text, duration).

The `build` callback is the key pattern: `autonomous.Run` hands you the
`extras` tool slice (carrying `report_done`) and you compose it with
your own tools when constructing the agent.

## Run it

No credentials needed, runs offline:

```bash
go run ./examples/autonomous
```

## Key APIs

- `autonomous.Run` / `autonomous.WithMaxTurns` — `github.com/go-steer/core-agent/v2/pkg/agent/autonomous`
- `autonomous.RunResult` — reason / turns / done detail / final text / duration
- `mock.NewScripted` — `github.com/go-steer/core-agent/v2/pkg/models/mock`
- `agent.New` / `agent.WithTools` — `github.com/go-steer/core-agent/v2/pkg/agent`

## What you should see

```
reason:      completed
turns:       1
done detail: summarized example.txt
final text:  Done. The project ships an autonomous-run driver.
duration:    <a few ms>
```

## Next

- [`autonomous-handle`](../autonomous-handle/) — Pause / Resume / Inject / Stop on a running autonomous loop.
- [`autonomous-resume`](../autonomous-resume/) — checkpointed resume after an interrupted run.
- API reference: [`docs/site/src/content/docs/embed/api.md`](../../docs/site/src/content/docs/embed/api.md)
