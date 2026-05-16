# scion-agent

`scion-agent` runs [core-agent](https://github.com/go-steer/core-agent) inside [Scion](https://github.com/GoogleCloudPlatform/scion)'s container runtime. It is the Go counterpart of Scion's Python `adk_scion_agent` example, built on top of core-agent's library.

## What it does

- Loads the same `.agents/config.json`, MCP servers, skills, instruction file, and built-in tools as `core-agent`.
- Adds a `sciontool_status` ADK tool so the model can declare sticky lifecycle states (`ask_user`, `blocked`, `task_completed`, `limits_exceeded`) to Scion's hub.
- Emits transient activity (`thinking`, `executing`, `working`) to `$HOME/agent-info.json` automatically on each agent / tool boundary, so Scion's UI can render live progress.
- Accepts `--input <task>` to seed the first turn (Scion's harness appends this when starting an agent with a task) and then reads stdin for follow-up messages — `scion message <agent>` delivers them via tmux send-keys.
- **v1.3.0+: non-blocking message delivery.** Stdin is read in a background goroutine that pushes each line onto the agent's inbox via `Agent.Inject`. Messages arriving while a turn is in flight no longer block — they queue and land on the next turn's prompt as a `[Inbox]` block prepended above `"continue"`. Previously a 30-second tool call would delay every message by 30 seconds; now they queue immediately and get drained pre-turn.

Outside a Scion container the lifecycle hooks degrade to no-ops, so the same binary is usable for local development with no Scion runtime.

## Build

The Dockerfile builds the Go binary from source and lays it on top of `scion-base` (which provides `sciontool`, `tmux`, and the `scion` user):

```sh
# From the core-agent repo root, not from this directory.
docker build \
  --build-arg BASE_IMAGE=scion-base:latest \
  -t scion-core-agent \
  -f extras/scion-agent/Dockerfile .
```

You need a `scion-base` image available locally. Build it from the Scion repo first if you don't have one.

## Register the template with Scion

Copy the `templates/scion/` tree into Scion's templates directory (or point Scion at it directly):

```
templates/scion/
├── scion-agent.yaml                       # schema_version, default harness
├── agents.md                              # system instruction shipped with the agent
└── harness-configs/scion/config.yaml      # image, user, task_flag, args
```

Then create an agent in Scion that uses this template — see the Scion docs for the exact CLI.

## Run locally (no container)

```sh
go build -o /tmp/scion-agent ./extras/scion-agent
GOOGLE_API_KEY=… /tmp/scion-agent --input "list the files in this directory"
```

You'll see:

- Agent text streamed to stdout.
- `→ <tool>` / `← <tool>` lines on stderr as tools are called.
- `$HOME/agent-info.json` ticking through `thinking` / `executing` / `working` (tail it in another shell to watch live).
- After the model calls `sciontool_status("task_completed", "...")`, the file flips to `completed` (sticky).

When `sciontool` is not on `PATH`, the sticky-state subprocess calls become quiet no-ops — the rest of the agent still runs normally.

## Design notes

### Lifecycle hooks live in the adapter, not in core-agent

The adapter's `streamTurn` ranges over `agent.Run()`'s event stream and emits transient activity based on what it sees:

- before the loop: `WriteActivity("thinking")`
- on a function-call event: `WriteActivity("executing")`
- on a function-response event: `WriteActivity("thinking")`
- after the loop: `WriteActivity("working")`

This stays self-contained — no Scion-specific code in core-agent's library. If a future adapter needs *control-flow* callbacks (abort tool calls, substitute responses), we'll expose ADK's `BeforeToolCallback` etc. on `agent.New` then; today, no consumer needs that.

### Sticky vs transient

- **Transient** (high-frequency, observation-only): atomic write to `$HOME/agent-info.json`. Cheap.
- **Sticky** (low-frequency, hub-notifying): `sciontool status <type> <message>` subprocess. The model invokes these intentionally via the `sciontool_status` ADK tool — it owns the decision of when the task is "done" or it's "waiting on input."

`WriteActivity` reads the current activity first and refuses to overwrite a sticky one with a transient. This matches the Python adapter's semantics exactly.

### Env-var contract

| Var | Source | Purpose |
|---|---|---|
| `GOOGLE_API_KEY` | user / Scion | Gemini API key. |
| `GEMINI_API_KEY` | Scion's Gemini harness | Bridged to `GOOGLE_API_KEY` at startup if the latter is unset. |
| `ANTHROPIC_API_KEY` | user | Anthropic API key (when `model.provider: anthropic`). |
| `HOME` | container | Where `agent-info.json` lives. Falls back to `/home/scion`. |
| `WORKSPACE_ROOT` | Scion (optional) | Honored by core-agent's path-scope check via `cwd`. |

## What's not in this PR

- No Anthropic-specific path; the adapter uses whichever provider `.agents/config.json` selects.
- No subagent tool — that lands with core-agent's M3.
- No CI image build — the Dockerfile is a template; consumers build it locally with their own `scion-base`.
