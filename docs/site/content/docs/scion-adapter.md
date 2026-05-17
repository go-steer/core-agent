---
title: Scion adapter
weight: 11
---


[`extras/scion-agent/`](https://github.com/go-steer/core-agent/tree/main/extras/scion-agent) packages core-agent for [Scion](https://github.com/GoogleCloudPlatform/scion)'s container runtime. It is the Go counterpart of Scion's Python `adk_scion_agent` example — same lifecycle contract, built on the core-agent library.

The adapter is opt-in. The core library and bundled CLI work standalone; you only build `scion-agent` when you want to deploy core-agent as a Scion-managed agent.

---

## What it adds on top of `core-agent`

- **`--input <task>` flag.** Scion's harness appends this when starting an agent with an initial task. From v1.3.0 the value is pushed onto the agent's inbox via `Agent.Inject` before the main loop starts; the first turn then drains it as the first `[Inbox]` block.
- **Non-blocking inbox loop (v1.3.0+).** A background goroutine reads stdin and pushes each line onto the agent's inbox via `Agent.Inject`. The main loop waits on `Agent.InboxArrived()` and runs a turn with prompt `"continue"`; the per-turn drain prepends queued messages as an `[Inbox]` block. Messages arriving while a turn is in flight no longer block — they queue immediately and land on the next turn's prompt. (Previously the binary scanned stdin between turns, so a 30-second tool call delayed every message by 30 seconds.)
- **Transient activity emission.** On every agent / tool boundary the adapter writes `thinking`, `executing`, or `working` to `$HOME/agent-info.json` so Scion's UI can render what's happening live.
- **Sticky lifecycle states via a `sciontool_status` tool.** The model invokes this tool to declare `ask_user`, `blocked`, `task_completed`, or `limits_exceeded`. The tool shells out to Scion's `sciontool` binary so the hub gets notified.

Outside a Scion container (no `sciontool` on `PATH`, no writable `$HOME`) the lifecycle hooks degrade to no-ops, so the same binary is usable for local development.

---

## Build and deploy

```bash
# From the core-agent repo root, not from extras/scion-agent/.
docker build \
  --build-arg BASE_IMAGE=scion-base:latest \
  -t scion-core-agent \
  -f extras/scion-agent/Dockerfile .
```

You need a `scion-base` image available locally — build it from the Scion repo first if you don't have one.

Then register the template tree under `extras/scion-agent/templates/scion/` with Scion (it has the `scion-agent.yaml`, `agents.md`, and `harness-configs/scion/config.yaml` Scion expects).

---

## Run locally without a container

```bash
go build -o /tmp/scion-agent ./extras/scion-agent
GOOGLE_API_KEY=… /tmp/scion-agent --input "list the files in this directory"
```

You'll see the agent stream its response to stdout, tool calls on stderr, and `$HOME/agent-info.json` ticking through `thinking` / `executing` / `working`. After the model decides the task is done and invokes `sciontool_status("task_completed", ...)`, the file flips to `completed`.

---

## Design notes

### Lifecycle hooks live in the adapter, not in core-agent

The adapter wraps `agent.Run()` with its own ~30-line `streamTurn` and emits transient activity by inspecting the event stream:

- before the loop: `WriteActivity("thinking")`
- on a `FunctionCall` event: `WriteActivity("executing")`
- on a `FunctionResponse` event: `WriteActivity("thinking")`
- after the loop: `WriteActivity("working")`

This keeps every Scion-shaped concept in `extras/scion-agent/`. core-agent's public API gains nothing for this — if a future adapter needs *control-flow* callbacks (abort a tool call before it runs, substitute a tool response), we'll expose ADK's `BeforeToolCallback` etc. on `agent.New` then. Today, no consumer needs that, so we don't add the API surface.

### Sticky vs transient

| Kind | Examples | Mechanism | Frequency |
|---|---|---|---|
| Transient | `thinking`, `executing`, `working` | atomic write to `$HOME/agent-info.json` | per agent / tool boundary |
| Sticky | `ask_user`, `blocked`, `task_completed`, `limits_exceeded` | `sciontool status <type> <message>` subprocess | invoked intentionally by the model via the `sciontool_status` tool |

`WriteActivity` reads the current activity first and refuses to overwrite a sticky state with a transient. Matches the Python adapter's semantics exactly.

### Env-var contract

| Var | Set by | Purpose |
|---|---|---|
| `GOOGLE_API_KEY` | user / Scion | Gemini API key. |
| `GEMINI_API_KEY` | Scion's Gemini harness | Bridged to `GOOGLE_API_KEY` at startup if the latter is unset. |
| `ANTHROPIC_API_KEY` | user | Anthropic API key (when `model.provider: anthropic`). |
| `HOME` | container | Where `agent-info.json` lives. Falls back to `/home/scion`. |

The adapter inherits all of core-agent's other env-var conventions (Vertex Gemini, Vertex Anthropic, etc.) — see [Providers]({{< relref "providers.md" >}}).

---

## Spawning sibling agents from inside Scion

A core-agent running inside one Scion container can spawn **sibling** Scion containers using the v1.5.0 [`extras/scion-remote-agent/`](https://github.com/go-steer/core-agent/tree/main/extras/scion-remote-agent) module — a reference implementation of `agent.RemoteAgentSpawner` against Scion's Hub HTTP API. The parent's model calls `spawn_remote_agent` and a new Scion container appears (provisioned from whatever template you pass), with its log stream classified into events on the parent's alert pipeline.

The module lives in its own Go module so Scion's heavy transitive deps stay out of the main core-agent library. Wiring sketch:

```go
import (
    "github.com/go-steer/core-agent/agent"
    scionremote "github.com/go-steer/core-agent/extras/scion-remote-agent"
)

spawner, err := scionremote.New(
    scionremote.WithTemplate("research-investigator"),
)
// errors.Is(err, scionremote.ErrNotInsideScion) → fall back to
// agent.RefuseRemoteAgentSpawner so local development still works.
remoteTool, _ := agent.NewSpawnRemoteAgentTool(spawner, bgMgr)
```

See the [Remote (out-of-process) subagents]({{< relref "library-api.md#remote-out-of-process-subagents" >}}) section in the library-API guide for the full surface, and [`examples/scion-research-demo/`](https://github.com/go-steer/core-agent/tree/main/examples/scion-research-demo) for an orchestrator-spawns-investigator scenario built on this spawner.

---

## See also

- [`extras/scion-agent/README.md`](https://github.com/go-steer/core-agent/blob/main/extras/scion-agent/README.md) — fuller README with env-var table and design notes.
- [`extras/scion-remote-agent/`](https://github.com/go-steer/core-agent/tree/main/extras/scion-remote-agent) — reference `RemoteAgentSpawner` for spawning sibling Scion containers.
- [`examples/scion-research-demo/`](https://github.com/go-steer/core-agent/tree/main/examples/scion-research-demo) — orchestrator + investigator end-to-end demo.
- [`docs/DESIGN.md`](https://github.com/go-steer/core-agent/blob/main/docs/DESIGN.md) — the **Adapters** section explains why `extras/` exists and what a future adapter would look like.
- [Scion `adk_scion_agent` example](https://github.com/GoogleCloudPlatform/scion/tree/main/examples/adk_scion_agent) — the Python adapter we modeled this on.
