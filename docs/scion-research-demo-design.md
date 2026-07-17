# Scion remote-agent reference + parallel-research demo

Design doc for the v1.4.0 milestone. Untracked sibling to
`docs/background-subagents-design.md`,
`docs/scion-harness-improvements-design.md`,
`docs/cogo-core-agent-integration.md`,
`docs/docsy-migration-notes.md`.

## Context

v1.2.0 shipped `agent.RemoteAgentSpawner` as a consumer-pluggable
seam for out-of-process subagents but explicitly deferred the
reference implementation. v1.3.0 shipped the interrupt machinery
(`Agent.Inject`, `AutonomousHandle`, mid-turn REPL interrupt) that
makes harness embedding viable for Scion. The remaining gap on the
Scion-substrate axis is a working `RemoteAgentSpawner` against
Scion's Hub HTTP API, plus a demo scenario that actually exercises
the full v1.2.0 + v1.3.0 stack end-to-end against a real Scion
deployment.

Scope is intentionally tight: lifecycle taxonomy enrichment
(richer `sciontool_status` states) and a bigger multi-cluster K8s
monitoring demo are deferred to v1.5.0 once the small demo proves
the integration.

## What ships in v1.4.0

### 1. `extras/scion-remote-agent/` — `RemoteAgentSpawner` for Scion

New package implementing `agent.RemoteAgentSpawner`
(`agent/remote.go:33-46`) against Scion's Hub HTTP API
(`github.com/GoogleCloudPlatform/scion/pkg/hubclient`). Direct
import of `hubclient` rather than CLI shell-out: type safety,
streaming logs via SSE, no `scion` binary dependency on PATH at
runtime. The dep is heavy but lives in `extras/` under its own
`go.mod` — main core-agent library is unaffected.

```go
// extras/scion-remote-agent/spawner.go (sketch)
package scionremote

type Spawner struct {
    client     hubclient.AgentService
    projectID  string
    template   string                  // default template for spawned agents
    classifier func(CloudLogEntry) agent.RemoteAgentEvent
}

// New constructs a Spawner from env (SCION_AGENT_TOKEN,
// SCION_HUB_ENDPOINT, SCION_PROJECT_ID) or explicit options.
// Returns ErrNotInsideScion if neither env nor options provide
// enough config — caller should fall back to RefuseRemoteAgentSpawner.
func New(opts ...Option) (*Spawner, error)

func (s *Spawner) Spawn(ctx context.Context, spec agent.RemoteAgentSpec) (agent.RemoteAgentHandle, error) {
    req := &hubclient.CreateAgentRequest{
        Name:      spec.Name,
        ProjectID: s.projectID,
        Template:  s.template,
        Task:      spec.Goal,
        Notify:    true,
        Labels: map[string]string{
            "spawned-by": "core-agent",
            "parent":     parentAgentIDFromCtx(ctx),
        },
    }
    resp, err := s.client.Create(ctx, req)
    if err != nil {
        return nil, err
    }
    h := &handle{
        client:     s.client,
        agentID:    resp.Agent.ID,
        events:     make(chan agent.RemoteAgentEvent, 64),
        classifier: s.classifier,
    }
    go h.streamCloudLogs(ctx)
    return h, nil
}
```

`handle` implements `agent.RemoteAgentHandle`:

- `ID()` → Scion agent ID
- `Status(ctx)` → polls `client.Get(ctx, id).Phase`; maps Scion's
  phases (`running` / `stopped` / `failed` / `suspended`) onto
  `agent.RemoteAgentStatus`
- `Stop(ctx)` → `client.Stop(ctx, id)`
- `Events()` → channel populated by a goroutine that drains
  `client.StreamCloudLogs(ctx, id, opts, handler)` and maps each
  `CloudLogEntry` onto `agent.RemoteAgentEvent`

#### Log → event mapping

Scion's SSE log stream gives us
`{timestamp, severity, message, json_payload}` per log line from the
spawned container. Three classification strategies, in order of
robustness:

1. **Structured payload convention** (default). If the spawned
   agent's `report_alert` writes a log entry with
   `json_payload.kind="alert"` (and `.text` for the message), map
   cleanly. Easy when the spawned agent is also `core-agent` with a
   small log-emitter shim.
2. **String prefix fallback** (opt-in). Log lines starting with
   `[REPORT_ALERT]` map to `Kind="alert"`; `[REPORT_COMPLETED]` →
   `Kind="completed"`. Brittle but works for any agent that
   follows the convention.
3. **Lifecycle-only** (always-on safety net). Ignore log content;
   emit synthetic `started` / `completed` / `failed` events purely
   from status transitions. Always works; loses per-alert
   visibility.

v1.4.0 ships #1 as the primary path with #3 as the automatic
fallback when the spawned agent isn't core-agent. #2 is a
documented convention non-core-agent spawned agents can adopt.
`WithLogClassifier(...)` lets consumers override.

#### Authentication

Reads `SCION_AGENT_TOKEN` from env when running inside a Scion
container; passes via `X-Scion-Agent-Token` header on every Hub
call. Outside Scion (e.g. running locally for development), caller
supplies a token explicitly via `WithAgentToken(string)`.

### 2. `examples/scion-research-demo/` — the demo

Two agent definitions exercising the full v1.2.0 + v1.3.0 + v1.4.0
stack against a real Scion deployment.

**Orchestrator** (main agent, runs in the foreground Scion
container):

- Gets a task like: *"Look at the recent commits in directories A,
  B, and C. Summarize what changed, flag anything that needs deeper
  investigation, and propose follow-up actions."*
- Spawns 3 background subagents (in-process via `spawn_agent`),
  one per directory, each with `read_file` + `glob` + `grep` +
  `bash`.
- Aggregates their `report_alert` messages.
- When one of them reports "this needs deeper investigation" (the
  model decides based on what it finds), the orchestrator calls
  `spawn_remote_agent` → `scion-remote-agent` → Scion Hub spawns a
  sibling "investigator" container.
- Investigator runs in isolation with a richer tool set (full
  bash, network access, maybe MCP tools for tickets/docs).
- Investigator reports back via the SSE log stream.
- Orchestrator synthesizes a final report with proposed actions.

**Investigator** (the remote, spawned on demand):

- Slim agent definition focused on deep-dive: read files, run
  scripts, query external systems via MCP.
- Emits structured `report_alert` log entries (the convention) so
  the orchestrator's scion-remote-agent handle parses them cleanly.
- Calls `report_done` when finished.

Templates for both go in `examples/scion-research-demo/templates/`.
The README explains how to:

1. Stand up a local Scion Hub (`scion server start`).
2. Register the orchestrator and investigator templates.
3. Build a container image with `core-agent`, `sciontool`, and
   `tmux` on `PATH` (typically `FROM scion-base:latest` + one COPY
   of the `core-agent` binary — see [Scion adapter]({{< relref "/docs/reference/scion-adapter.md" >}})).
4. Start the orchestrator: `scion create orchestrator "summarize
   recent commits in agent/ runner/ tools/"`.
5. Watch it work: `scion logs orchestrator --follow`.
6. Mid-run interrupt to redirect:
   `scion message orchestrator "actually, focus on agent/ only"`.
7. Inspect the audit trail: query the eventlog SQLite database
   directly to see the full tree of events (parent + investigator
   sibling).

## Critical files

**New (in core-agent):**

- `extras/scion-remote-agent/go.mod` — own module so Scion's
  transitive dep tree (cloud.google.com/go, ent ORM, etc.) stays
  out of main core-agent.
- `extras/scion-remote-agent/spawner.go` — `Spawner` struct,
  `New`, `Spawn`, `Option` helpers (`WithAgentToken`,
  `WithHubEndpoint`, `WithProjectID`, `WithTemplate`,
  `WithLogClassifier`).
- `extras/scion-remote-agent/handle.go` — `handle` struct
  implementing `agent.RemoteAgentHandle`; the SSE-streaming
  goroutine.
- `extras/scion-remote-agent/classify.go` — log-entry-to-event
  mapping (the three strategies; default to structured-payload).
- `extras/scion-remote-agent/env.go` — environment-variable
  auto-detection (`SCION_AGENT_TOKEN`, `SCION_HUB_ENDPOINT`,
  `SCION_PROJECT_ID`).
- `extras/scion-remote-agent/spawner_test.go` — uses Scion's own
  `httptest.Server` pattern (see `cmd/message_test.go:60`
  `newMessageMockHubServer`) to mock the Hub. Tests Spawn, Status,
  Stop, Events fan-out, and the three classification strategies.
- `examples/scion-research-demo/orchestrator/` — orchestrator
  agent definition (`agents.md`, `system-prompt.md`, sample
  config).
- `examples/scion-research-demo/investigator/` — investigator
  agent definition.
- `examples/scion-research-demo/templates/` — Scion templates for
  both.
- `examples/scion-research-demo/README.md` — full setup + run
  instructions.
- `dev/smoke/07-scion-remote-demo.sh` — smoke that spins up a
  local Scion Hub, runs the demo, asserts the investigator was
  actually spawned via the Hub API. Skips when `scion` binary
  isn't on PATH.
- `go.work` — wires both modules for local development.

**Modified:**

- `dev/ci/presubmits/build` and `test-unit` — also walk the
  `extras/scion-remote-agent/` module.
- `dev/ci/presubmits/verify-mod-tidy` — run `go mod tidy` in both
  modules; check both for cleanliness.
- `CHANGELOG.md` — `[1.4.0]` entry.
- `README.md` — feature bullet under "Subagents" and a pointer to
  the demo.
- `docs/site/content/docs/scion-adapter.md` — extend with a
  "Spawning sibling agents" section.
- `docs/site/content/docs/library-api.md` — extend the v1.2.0
  RemoteAgentSpawner section with a concrete Scion example.

## Reused (no changes)

- `agent.RemoteAgentSpawner` + `RemoteAgentHandle` interfaces
  (`agent/remote.go:33-46`) — the seam from v1.2.0.
- `agent.NewSpawnRemoteAgentTool` (`agent/remote.go`) — wired into
  the orchestrator's tool list.
- `agent.BackgroundAgentManager` (`agent/background.go`) —
  orchestrator uses it for the in-process subagents.
- Stock `cmd/core-agent` binary — runs unchanged as the container
  entrypoint for both orchestrator and investigator. Scion-shaped
  behavior comes from `pkg/hooks` (transient activity via
  `sciontool hook`) + the `sciontool_status` built-in tool (sticky
  states via `sciontool status`). No adapter binary required; the
  Scion harness bundle staged at `extras/scion/` (moves to Scion's
  `harnesses/core-agent/` on upstream adoption) tells Scion how to
  launch it. See [Scion adapter]({{< relref "/docs/reference/scion-adapter.md" >}}).
- Scion `hubclient.AgentService` (`pkg/hubclient/agents.go:34-101`)
  — direct import.
- Scion's `httptest.Server` mock pattern
  (`cmd/message_test.go:60`) — pattern crib for our tests.

## Module layout

`extras/scion-remote-agent/` ships with its own `go.mod` so the
`github.com/GoogleCloudPlatform/scion` direct dep and its heavy
transitive tree (cloud.google.com/go, ent ORM, etc.) stay out of
core-agent's main `go.mod`. Library consumers who don't use Scion
remain lean.

Operational implications:

- New `go.work` file at the repo root wires both modules for local
  development. `go build ./...` from inside either module traverses
  only that module.
- `dev/ci/presubmits/build` and `dev/ci/presubmits/test-unit` need
  to walk the extras module too — small change, one extra
  `(cd extras/scion-remote-agent && go build/test ./...)`
  invocation.
- `dev/ci/presubmits/verify-mod-tidy` runs `go mod tidy` in BOTH
  modules and checks both for cleanliness.

Matches how `extras/ax-agent/` would land if ever promoted off the
`axplore` branch — separate modules per heavy adapter is the
pattern the codebase is settling into.

## Phased delivery within v1.4.0

Single tag at the end. Internal commit boundaries:

1. **Phase 1 — `scion-remote-agent` core.** Spawner + handle +
   classify + env + mock-Hub unit tests. New `go.mod` + `go.work`
   + presubmit script updates. Compiles + tests green; not yet
   wired into the demo.
2. **Phase 2 — Demo orchestrator + investigator.** Agent
   definitions, Scion templates, README. Validates the integration
   shape end-to-end.
3. **Phase 3 — Real-Scion smoke + docs.** `dev/smoke/07-...sh`,
   manual run against the local Hub, docs updates, capture demo
   transcript / screencast for the release notes.
4. **Phase 4 — Tag v1.4.0.**

## Verification

```bash
# Unit
cd /home/user/projects/core-agent
go test ./... -race
(cd extras/scion-remote-agent && go test ./... -race)
go vet ./...
(cd extras/scion-remote-agent && go vet ./...)
for s in dev/ci/presubmits/*; do bash "$s"; done

# Local Scion Hub (one-time setup)
cd /home/user/projects/scion
go run ./cmd scion server start    # starts Hub on localhost:$SCION_HUB_PORT

# In another shell: register the demo's templates
cd /home/user/projects/core-agent/examples/scion-research-demo
./register-templates.sh  # copies templates/ into Scion's templates dir

# Run the demo end-to-end
scion create orchestrator "summarize recent commits in agent/ and runner/"
# Watch orchestrator's logs:
scion logs orchestrator --follow
# Expect: orchestrator spawns 3 in-process subagents (visible as
# spawn_agent tool calls in the logs); when one flags something,
# orchestrator spawns an investigator via spawn_remote_agent
# (visible as a NEW agent in `scion list`).

# Verify the investigator was actually spawned in a separate
# container:
scion list
# Expect: orchestrator + investigator-<n> both running

# Inspect the unified audit trail (orchestrator + spawned in-process
# subagents share the same parent session in core-agent's eventlog;
# the investigator has its own Scion-side eventlog):
go run ./dev/smoke/cmd/inspect-grounding /tmp/orchestrator.db

# Mid-run interrupt
scion message orchestrator "actually, focus on agent/ only" --interrupt
# Expect: orchestrator's current turn cancels; new prompt with the
# "[Inbox]" block lands on the next turn; investigator may also
# get cancelled depending on orchestrator's decision.

# Clean up
scion stop --all
scion server stop
```

## Deferred (out of scope for v1.4.0)

- **Lifecycle taxonomy enrichment** (richer `sciontool_status`
  states beyond `ask_user` / `blocked` / `task_completed` /
  `limits_exceeded`). Designed-with-Scion-folks problem; defer to
  v1.5.0.
- **Multi-cluster Kubernetes monitoring demo**. The big realistic
  one. Promote to v1.5.0 once the small parallel-research demo
  proves the integration.
- **Investigator pause/resume across Scion suspend/restart**.
  Scion has suspend/resume; if the spawned investigator survives a
  Hub restart, we'd want core-agent's `ResumeAutonomous` to pick
  up. Defer.
- **Cross-Scion-environment portability**. v1.4.0 targets a
  single Scion Hub. Multi-Hub federation is out of scope.
- **CLI shell-out alternative** to the Hub HTTP path. Could be
  useful for environments where the agent container has `scion`
  on PATH but not the Hub credentials. Defer.

## Why not in this release

- **K8s monitoring demo** — bigger scope, needs test clusters,
  doesn't add anything the parallel-research demo doesn't already
  prove about the Scion integration itself.
- **Hub federation / multi-Hub spawning** — no consumer asking for
  it; would significantly complicate the spawner's auth + routing
  logic.
- **Cogo migration prototype** (from
  `docs/cogo-core-agent-integration.md`) — separate concern; the
  v1.4.0 Scion work doesn't unblock or block it.
