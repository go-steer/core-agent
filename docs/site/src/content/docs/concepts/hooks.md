---
title: Hooks
---

`core-agent` can shell out to operator-configured commands whenever the agent crosses a tool, model, or turn boundary. This is a general-purpose observation mechanism — Scion is one consumer ([Scion adapter](/reference/scion-adapter/)), but any user can wire hooks to custom telemetry, notification services, project-specific triggers, or a local log tap.

The mechanism lives in `pkg/hooks` and is driven entirely from `.agents/config.json` — no code changes required to use it.

---

## Configuration

```json
{
  "hooks": {
    "tool-start":  [{"command": "logger -t core-agent 'tool starting'"}],
    "tool-end":    [{"command": "sciontool hook --dialect=core-agent", "timeout_seconds": 5}],
    "model-start": [{"command": "cat >> /tmp/core-agent-events.jsonl && echo >> /tmp/core-agent-events.jsonl"}],
    "agent-end":   [{"command": "curl -sX POST http://localhost:9000/turn-done -d @-"}]
  }
}
```

Each key is a hook event name (see table below). Each value is an ordered list of handlers. Handlers for the same event run **sequentially**, each getting a fresh JSON envelope on stdin. The `command` string is passed to `/bin/sh -c`, so pipes, redirections, and shell substitutions all work.

`timeout_seconds` bounds the wall-clock per handler; default is 10 seconds. A hung command is killed at the timeout — the agent's event stream is not stalled beyond that.

If a handler exits non-zero, the failure is logged to `core-agent`'s stderr and the next handler for that event fires. Hooks are observers, not veto-holders — they can't cancel the agent's work.

---

## Event vocabulary

| Event | When it fires | Envelope fields (in addition to `hook_event_name`) |
|---|---|---|
| `tool-start` | When the model calls a tool. Fires once per `FunctionCall` part in an event. | `tool_name`, `tool_input` |
| `tool-end` | When a tool returns its result. Fires once per `FunctionResponse` part. | `tool_name`, `tool_output` |
| `model-start` | When the model begins producing text — at turn start, and again after each tool boundary within a turn. Fires at most once per "thinking window." | — |
| `agent-end` | Once per turn, from the post-turn cleanup. | — |

Names deliberately match Scion's canonical event vocabulary so a `dialect.yaml` in a Scion integration is a trivial identity mapping. Consumers who don't care about Scion just get the same names.

Unknown event names in the config are rejected at startup — typos fail loudly.

---

## Envelope shape

Every command receives a JSON object on stdin with:

- `hook_event_name` — the string above (e.g. `"tool-start"`).
- Any event-specific fields from the table (e.g. `tool_name`, `tool_input`).

Example envelope for `tool-start`:

```json
{"hook_event_name":"tool-start","tool_name":"read_file","tool_input":{"path":"/etc/hosts"}}
```

Consumers can parse it however they like. If your handler is `sciontool hook --dialect=<name>`, the top-level field names match what Scion's `sciontool` auto-extracts.

---

## What hooks aren't

- **Not a permission gate.** Hooks can't refuse a tool call or substitute a response. Use the [permissions gate](/concepts/permissions/) for that.
- **Not a session archive.** For persistent event history use the [event log](/concepts/sessions/); hooks are for out-of-process side effects.
- **Not synchronous with the model.** Handlers run on the agent's tap loop, which is downstream of ADK's runner. They observe events after they've been persisted; they don't gate model calls.

---

## Programmatic use

Library consumers who want in-process observation (not shelling out) can skip the config file and wire `agent.WithEventHook(onEvent, onTurnEnd)` directly. See `pkg/agent/eventhook.go`. The `pkg/hooks` dispatcher uses the same option; a custom in-process observer sits alongside it (single-slot semantics — wrap two callbacks in one to compose).
