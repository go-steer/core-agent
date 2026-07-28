# `basic` — minimal library agent in ~50 lines

## What this shows

The smallest useful embedding of core-agent as a Go library: resolve a
model provider from config, construct an `agent.Agent` with a one-line
instruction, and iterate the streaming event sequence from a single
`Run` call, printing partial text as it arrives. No tools, no session
persistence, no CLI — just the model round-trip.

## Run it

Needs real Gemini credentials:

```bash
GOOGLE_API_KEY=... go run ./examples/basic
```

from the repo root.

## Key APIs

- `agent.New` / `agent.WithInstruction` — `github.com/go-steer/core-agent/v2/pkg/agent`
- `agent.Agent.Run` — returns an `iter.Seq2[*Event, error]` you range over
- `models.Resolve` — `github.com/go-steer/core-agent/v2/pkg/models`
- `config.DefaultConfig` — `github.com/go-steer/core-agent/v2/pkg/config`
- blank import of `pkg/models/gemini` to register the provider

## What you should see

A single streamed answer, printed token-by-token as partials arrive:

```
The capital of France is Paris.
```

## Next

- [`streaming`](../streaming/) — same loop rendered with the
  `runner.WriteEvents` chat formatter, plus built-in tools.
- [`with-tools`](../with-tools/) — add a custom tool, MCP servers, and skills.
- Library guide: [`docs/site/src/content/docs/embed/guide.md`](../../docs/site/src/content/docs/embed/guide.md)
