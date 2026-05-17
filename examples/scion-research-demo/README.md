# scion-research-demo

End-to-end demonstration of core-agent's [v1.2.0 `RemoteAgentSpawner`
seam](../../docs/site/content/docs/library-api.md) wired into Scion. An
**orchestrator** agent runs in one Scion container, fans research work out
to in-process subagents, and **escalates** any flagged finding to a sibling
Scion container (an **investigator**) by calling `spawn_remote_agent`. The
investigator runs in isolation, reports back through Scion's log stream, and
the orchestrator synthesises a final report.

This demo exercises the full v1.2.0 + v1.3.0 + v1.5.0 stack together:

- `agent.BackgroundAgentManager` — in-process subagents (v1.2.0)
- `agent.RemoteAgentSpawner` — out-of-process subagents (v1.2.0)
- `extras/scion-remote-agent` — the Scion-backed `RemoteAgentSpawner`
  implementation (v1.5.0, this directory's reason for existing)
- Soft-interrupt + mid-turn REPL interrupt (v1.3.0) — the orchestrator can
  receive `scion message <agent>` mid-turn and redirect its work

## What runs where

```
                ┌──────────────────────────────────────────────┐
                │  Scion Hub (docker-compose or local cluster) │
                └──────────────────────────────────────────────┘
                          ▲                       ▲
                          │ HTTP/SSE              │ HTTP/SSE
                          │                       │
   ┌──────────────────────┴───────────┐   ┌───────┴─────────────────────┐
   │  research-orchestrator container │   │  research-investigator       │
   │  (image: scion-research-demo)    │   │  container (spawned by the   │
   │  binary: research-orchestrator    │──▶│  orchestrator on demand)     │
   │                                  │   │  image: scion-research-demo  │
   │  - BackgroundAgentManager        │   │  binary: scion-agent          │
   │  - spawn_agent (in-process)      │   │                              │
   │  - spawn_remote_agent ──────────────▶│  - read_file/glob/grep/bash  │
   │  - scionremote.Spawner            │   │  - emits [REPORT_ALERT] /   │
   └──────────────────────────────────┘   │    [REPORT_COMPLETED] lines  │
                                          └──────────────────────────────┘
```

Both containers come from the **same** Docker image (one Dockerfile, two
binaries). The harness-config picks which binary to launch per template.

## Prerequisites

- A working Scion checkout at `$SCION_SRC_DIR` (default: `~/projects/scion`).
  This demo currently uses a local `replace` directive in
  `extras/scion-remote-agent/go.mod`; drop the replace once Scion publishes
  a tagged release.
- Docker with [BuildKit](https://docs.docker.com/build/buildkit/) enabled
  (default in Docker 23+). The build wrapper uses
  `--build-context scion-src=…` to stage the Scion source.
- A Scion base image (`scion-base:latest`) providing `sciontool`, `tmux`,
  and the `scion` user. Built from your Scion checkout — see Scion's docs.
- A model provider configured the way core-agent expects. The simplest is
  Gemini via `GEMINI_API_KEY`; see [`cmd/core-agent`](../../cmd/core-agent/)
  for the full set.

## One-time setup

### 1. Build the demo container image

```bash
cd /path/to/core-agent

# Defaults: SCION_SRC_DIR=~/projects/scion, BASE_IMAGE=scion-base:latest,
# IMAGE_TAG=scion-research-demo:latest. Override via env vars if needed.
examples/scion-research-demo/build.sh
```

This stages the Scion source as a BuildKit named context, then builds a
single image that ships **both** binaries (`research-orchestrator` and
`scion-agent`).

### 2. Register the templates with Scion

```bash
# Default: ~/.scion/templates. Override via TEMPLATES_DIR.
examples/scion-research-demo/register-templates.sh
```

Two templates appear:

- `research-orchestrator` — uses the `research-orchestrator` binary
- `research-investigator` — uses the stock `scion-agent` binary with a
  customised `agents.md`

Both point at the `scion-research-demo:latest` image you just built.

### 3. Start a local Scion Hub

Pick whichever path your Scion deployment supports — `docker-compose up`,
`scion server start`, a kind cluster, etc. The orchestrator container
reads `SCION_HUB_ENDPOINT`, `SCION_AGENT_TOKEN`, and `SCION_PROJECT_ID`
from its environment to talk to the Hub; Scion's harness sets these for
agents it manages.

## Running the demo

```bash
# Replace <repo> with whatever workspace path Scion mounts at /workspace.
scion create research-orchestrator \
  "look at recent commits in agent/, runner/, and tools/; \
   summarize what changed and flag anything that needs deeper investigation"
```

Watch what happens:

```bash
# Logs from the orchestrator container — tool calls visible on stderr.
scion logs research-orchestrator-<N> --follow
```

You should see, roughly:

1. The orchestrator emits a brief `todo` plan, then calls
   `spawn_agent("commits-agent", goal="summarize agent/ commits, ...")`
   three times (one per directory).
2. The subagents call `read_file` / `bash` (`git log -p`, etc.) and
   eventually `report_alert` / `report_completed`.
3. The orchestrator reads the alerts on its next turn and **decides**
   whether anything needs deeper investigation. If yes:
   `spawn_remote_agent("investigator-1", goal="<the focused question>", ...)`
4. A new Scion container appears in `scion list` — that's the investigator.
5. The investigator emits `[REPORT_ALERT]` lines as it digs; the
   orchestrator's `scionremote.Spawner` classifies each one as an alert and
   delivers it to the orchestrator's inbox on the next turn.
6. When the investigator emits `[REPORT_COMPLETED] ...` and calls
   `sciontool_status("task_completed", ...)`, the orchestrator sees a
   terminal alert.
7. Orchestrator synthesises the final report and calls
   `sciontool_status("task_completed", ...)`.

### Mid-run interrupt (v1.3.0)

Redirect the orchestrator mid-turn:

```bash
scion message research-orchestrator-<N> "actually, focus only on agent/" --interrupt
```

The orchestrator's current turn cancels (v1.3.0's soft-interrupt); on the
next turn the `[Inbox]` block prepends your message and the model adjusts.
Any investigator the orchestrator has already spawned keeps running unless
the orchestrator explicitly stops it.

### Inspecting the audit trail

```bash
# Orchestrator's own session + every in-process subagent shares one
# eventlog database when --session-db is on.
scion exec research-orchestrator-<N> -- \
  bash -lc "ls ~/.research-orchestrator/"

# Pull the file out and replay it with your favourite SQLite tool.
scion cp research-orchestrator-<N>:/home/scion/.research-orchestrator/sessions.db /tmp/
sqlite3 /tmp/sessions.db 'select branch, count(*) from agent_eventlog group by branch'
```

The investigator container has its own eventlog (Scion-side); join them by
agent ID via the Hub's API.

## Local development without Scion

The orchestrator binary works outside any Scion environment — `scionremote.New()`
returns `ErrNotInsideScion` when `SCION_HUB_ENDPOINT` / `SCION_AGENT_TOKEN`
/ `SCION_PROJECT_ID` are unset, and the orchestrator falls back to
`agent.RefuseRemoteAgentSpawner(...)`. `spawn_remote_agent` calls then
return a clean tool-result error and the model adapts — useful for testing
the orchestration logic without standing up a Hub.

```bash
cd /path/to/core-agent
export GEMINI_API_KEY=...

cd extras/scion-remote-agent
go run ./cmd/research-orchestrator \
  --provider gemini -m gemini-2.0-flash \
  --input "look at recent commits in agent/ and runner/; flag anything noteworthy"
```

`spawn_agent` works (in-process); `spawn_remote_agent` refuses cleanly.

## What's deferred

- **Lifecycle taxonomy enrichment** (richer `sciontool_status` states beyond
  `ask_user` / `blocked` / `task_completed` / `limits_exceeded`) — pending
  feedback from Scion folks on what their UI wants to display.
- **Multi-cluster K8s monitoring demo** — the bigger realistic variant.
  Wait for this small demo to prove the integration, then promote.
- **Investigator pause/resume across Scion suspend/restart** — needs
  coordination with Scion's suspend/restart story and core-agent's
  `ResumeAutonomous`.
- **Federation across Scion Hubs** — single-Hub only for now.

See [`docs/scion-research-demo-design.md`](../../docs/scion-research-demo-design.md)
for the full v1.5.0 design rationale.
