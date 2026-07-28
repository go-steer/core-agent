# `with-subagent` — parent + research subagent with a shared audit log

## What this shows

A parent agent that delegates to a `research` subagent via
`agent.WithSubagents`, end-to-end with no LLM credentials: two
scripted-mock providers replay JSONL transcripts (parent calls the
subagent as a tool, subagent answers, parent summarizes). Both agents
share one SQLite eventlog handle, so the subagent's events land in the
parent's session tree under `branch="research"` — the example finishes
by streaming the full audit log to show the branch-tagged rows.

## Run it

No credentials needed, runs offline:

```bash
go run ./examples/with-subagent
```

## Key APIs

- `agent.WithSubagents` / `agent.WithEventLog` / `agent.WithSession` — `github.com/go-steer/core-agent/v2/pkg/agent`
- `mock.NewScripted` — `github.com/go-steer/core-agent/v2/pkg/models/mock`
- `eventlog.Open` / `eventlog.WithSessionTree` — `github.com/go-steer/core-agent/v2/pkg/eventlog`

## What you should see

```
== parent run ==
  → research(map[request:what does the project ship])
  ← research -> map[...]
  text: The project ships an autonomous-run driver and a durable event log.

== full session tree (parent + every subagent) ==
  seq=1 branch=(root)     author=user
  ...
  seq=N branch=research   author=research
```

The subagent runs in a derived session row
(`parent-session:sub:research`); `WithSessionTree` returns parent +
descendants in one query.

## Next

- [`replay`](../replay/) — the scripted-mock provider on its own.
- [`background-monitor`](../background-monitor/) — parallel background subagents instead of inline delegation.
- Library guide: [`docs/site/src/content/docs/embed/guide.md`](../../docs/site/src/content/docs/embed/guide.md)
