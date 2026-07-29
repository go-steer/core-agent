# `replay` — offline agent loop from a recorded transcript

## What this shows

Driving the full agent loop with no LLM credentials by replaying a
recorded JSONL transcript through the `mock.NewScripted` provider. The
2-turn transcript is inlined as a constant and materialized to a temp
file; in lenient mode (`strict=false`) the provider plays responses
back in order regardless of the incoming prompt — pass `true` to
assert each request matches the recording (useful for catching
prompt-construction regressions in tests).

In real usage the transcript comes from a live run captured with the
bundled CLI's `--record-to` flag or `mock.NewRecorder` in library code.

## Run it

No credentials needed, runs offline:

```bash
go run ./examples/replay
```

## Key APIs

- `mock.NewScripted` — `github.com/go-steer/core-agent/v2/pkg/models/mock`
- `agent.New` / `agent.Agent.Run` — `github.com/go-steer/core-agent/v2/pkg/agent`

## What you should see

```
user: What is 2+2?
model: Replay turn 1: hello from the transcript.
user: And 3+3?
model: Replay turn 2: this came from the JSONL fixture.
```

The prompts deliberately don't match the recorded ones — that's
lenient mode doing its job.

## Next

- [`autonomous`](../autonomous/) — the same scripted provider driving `autonomous.Run`.
- [`with-subagent`](../with-subagent/) — two scripted providers, one per agent.
- Library guide: [`docs/site/src/content/docs/embed/guide.md`](../../docs/site/src/content/docs/embed/guide.md)
