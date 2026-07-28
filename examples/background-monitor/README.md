# `background-monitor` — background subagents, alerts, and the model-context drain

## What this shows

The in-process background-subagent pathway, end-to-end with no LLM
credentials. A `background.Manager` (attached to a parent agent via
`agent.WithBackgroundManager`) spawns two "monitor" children against
the echo mock; their alerts flow through both channels the substrate
offers:

- the `OnAlert` side channel, for immediate display (same shape the
  bundled CLI's REPL uses), and
- the pre-turn drain (`PrependPendingAlerts`), which prepends pending
  alerts to the parent model's next prompt.

The example calls `mgr.Spawn` directly to exercise the lifecycle
without an LLM round-trip; in a real deployment the spawn tools sit in
the parent's tool list and the model decides when to spawn.

## Run it

No credentials needed, runs offline:

```bash
go run ./examples/background-monitor
```

## Key APIs

- `background.NewManager` / `Manager.Spawn` / `Manager.OnAlert` / `Manager.PrependPendingAlerts` — `github.com/go-steer/core-agent/v2/pkg/agent/background`
- `background.WithProvider` / `WithMaxConcurrent` / `WithDefaultBudgets` — manager options
- `agent.WithBackgroundManager` — `github.com/go-steer/core-agent/v2/pkg/agent`
- `mock.NewEcho` — `github.com/go-steer/core-agent/v2/pkg/models/mock`

## What you should see

```
spawned: watch-cluster-a (branch=bg.watch-cluster-a, status=running)
spawned: watch-cluster-b (branch=bg.watch-cluster-b, status=running)
[hook] ↪ watch-cluster-a deferred: stopped: max_turns_exceeded
--- model would see ---
[Background reports]
- [watch-cluster-a] (deferred) stopped: max_turns_exceeded
...
what's the status of the monitors?
--- end ---
final handle states:
  watch-cluster-a -> deferred
  watch-cluster-b -> deferred
```

(The echo children end `deferred` because they hit their 1-turn
budget — that's the terminal alert pathway working.)

## Next

- [`scheduled-monitor`](../scheduled-monitor/) — Schedulers + `schedule_next_turn`, and the manager's default-scheduler wiring.
- [`with-subagent`](../with-subagent/) — inline (foreground) subagent delegation with a shared audit log.
- API reference: [`docs/site/src/content/docs/embed/api.md`](../../docs/site/src/content/docs/embed/api.md)
