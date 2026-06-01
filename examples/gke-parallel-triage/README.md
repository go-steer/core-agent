# `gke-parallel-triage` — config recipe for GKE incident triage via parallel fan-out

A **config-only** example: drop into a project, run `core-agent`, get a GKE
incident-triage agent that fans out one investigator per service in parallel
and synthesizes their reports into a root-cause analysis.

No Go code, no custom binary, no library wiring. The substrate (`cmd/core-agent`)
already does all the plumbing — this recipe just supplies the configuration.

## What ships in this example

```
.agents/
├── mcp.json        # GKE MCP server (read-only endpoint) via ADC
├── config.json     # Gemini Pro, ask-mode, pre-allowed read tools
└── AGENTS.md       # system instructions priming the parallel-fan-out pattern
```

That's it. The substrate reads these on startup; everything else is wired
by `core-agent` itself (MCP toolset discovery, `spawn_agent` family
registration, eventlog, attach mode, etc.).

## Prerequisites

1. **Install `core-agent`** (one binary, no other deps):

   ```bash
   go install github.com/go-steer/core-agent/cmd/core-agent@latest
   go install github.com/go-steer/core-agent/cmd/core-agent-tui@latest   # optional, for remote attach
   ```

2. **Google Cloud auth** for the GKE MCP server (Application Default Credentials):

   ```bash
   gcloud auth application-default login
   gcloud config set project <your-project-id>
   ```

   The MCP server lives at `container.googleapis.com/mcp/read-only` and
   accepts ADC tokens scoped to `container.read-only`.

3. **Gemini API key** (or Vertex AI access) for the model:

   ```bash
   export GEMINI_API_KEY=...
   ```

   To use Vertex AI instead, edit `.agents/config.json`:

   ```json
   "model": { "provider": "vertex", "name": "gemini-3.1-pro-preview-customtools" }
   ```

   To swap the parent to Claude Sonnet (better at synthesis at the cost of
   higher per-turn spend):

   ```json
   "model": { "provider": "anthropic-vertex", "name": "claude-sonnet-4-6" }
   ```

4. **A GKE cluster you can read.** The agent will only inspect what your
   ADC identity has IAM access to. For a sandbox cluster + a fake "broken"
   workload to drive the demo, see [Test cluster recipe](#test-cluster-recipe)
   below.

## Run

From this directory:

```bash
cd examples/gke-parallel-triage
core-agent
```

Lands in the in-process TUI. The agent has the GKE read tools available
plus the `spawn_agent` family for fan-out; `AGENTS.md` primes it to use
them in parallel for any incident-shaped prompt.

Drive it with prompts like:

```
> payments-prod is showing elevated 5xx — investigate all services in parallel
> there's been a deploy to checkout-svc in payments-prod; verify health across the namespace
> compare pod health between prod and staging in the orders namespace
```

The agent will:

1. List services in the namespace (one read-only tool call)
2. Spawn one investigator per service in a single assistant turn
3. Wait for each subagent's terminal report (each digest is ~3 bullets)
4. Synthesize across reports into a root-cause + mitigation suggestion

For a headless / scheduled setup (e.g. webhook from your alerting stack):

```bash
core-agent --no-repl --attach-listen=:7777 --session-db
# from your alerting webhook:
curl -X POST http://daemon:7777/sessions/<sid>/inject \
  -H 'Content-Type: application/json' \
  -d '{"message":"payments-prod degraded - investigate"}'
# operator attaches when they want:
core-agent-tui http://daemon:7777
```

## What's happening under the hood

The substrate wires three things from these config files:

| File | Drives |
|---|---|
| `mcp.json` | `pkg/mcp.Build` opens an HTTP MCP session to the GKE server, discovers tool schemas, registers them as `gke.list_k8s_api_resources`, `gke.get_k8s_logs`, etc. The namespace prefix comes from the server name in `mcp.json`. |
| `config.json` permissions.allow | `permissions.Gate` pre-allows the GKE read tools + spawn family so the operator isn't prompted on every call. Mutating MCP tools (apply, patch, delete) aren't even exposed at the URL `/mcp/read-only`, so they can't be invoked. |
| `AGENTS.md` | Prepended to the agent's system instructions. Tells the model the parallel-fan-out workflow it should follow for incident-shaped prompts. |

Plus `core-agent` itself wires the `spawn_agent` / `list_agents` /
`check_agent` / `stop_agent` tools through `BackgroundAgentManager`,
gives subagents the same tool catalog as the parent (so investigators
have GKE read tools), and runs them under per-spawn budgets (default
5 turns / 60 s wall-clock — overridable via `max_turns` and
`max_wallclock_seconds` args).

## Tuning

### Make subagents cheap

In `config.json`, set `agentic.small_model` (or pass
`--agentic-small-model gemini-2.5-flash` on the command line) so
investigators run on Flash while the orchestrator stays on Pro.
This was core-agent's headline v2.0 cost-efficiency lever.

### Scope to one cluster / region

The MCP server's tools accept cluster + location + project args.
You can either let the model figure it out from context (it will
ask if ambiguous) or hardcode them in `AGENTS.md`:

```
Default cluster: prod-us-central1 in project acme-payments-prod, region us-central1.
Always pass these to GKE tools unless the operator names a different cluster.
```

### Add cluster-write capability

For mitigation actions (rollback, scale), change the MCP URL to the
full endpoint and add the relevant tools to `permissions.allow`:

```json
"url": "https://container.googleapis.com/mcp"
```

This exposes `apply_k8s_manifest`, `patch_k8s_resource`, `update_cluster`,
etc. **Recommend leaving `permissions.mode: ask` for the mutating
tools** — keep the human-in-loop confirmation on anything that changes
cluster state. Operators can `/allow allow-always` on safe mutations
(specific deployment names) as they go.

### Multi-cluster fan-out

For "investigate this incident across 5 clusters in parallel" patterns,
either:

- **Single-orchestrator + per-cluster subagents** (works with this
  example as-is — change `AGENTS.md` to spawn one subagent per
  cluster instead of per service, each subagent then drills into
  that cluster's services). Limited by the orchestrator binary's
  reachability to all clusters.

- **Fleet-level** via the AX integration (`extras/ax-agent/`) — one
  orchestrator per cluster, AX coordinates the fan-out. See
  [`docs/ax-integration-audit.md`](../../docs/ax-integration-audit.md).

## Test cluster recipe

To drive this example without a real production cluster, spin up a
single-zone Autopilot cluster and deploy a deliberately-broken workload:

```bash
gcloud container clusters create-auto demo \
  --region us-central1 --project <your-project>

kubectl create ns payments-prod

# A "service" that fails to pull (typo in image)
kubectl -n payments-prod create deployment broken-svc \
  --image=gcr.io/distroless/static:nonexistent --replicas=2

# Two healthy services for contrast
kubectl -n payments-prod create deployment api-gateway --image=nginx --replicas=2
kubectl -n payments-prod create deployment checkout-svc --image=nginx --replicas=2
```

Then run `core-agent` against this directory and try:

```
> payments-prod has pods failing — investigate all services in parallel and report root cause
```

You should see the orchestrator spawn three investigators, three reports
come back, and a synthesis identifying `broken-svc` as the failing one
(with the specific `ErrImagePull` event).

## What this example does NOT do

- **No mutating MCP tools** — wired to `/mcp/read-only`. Intentional default for
  the triage use case.
- **No cluster-specific defaults** — every prompt needs to name (or
  default to) a cluster + location + project. Tune `AGENTS.md` if you
  want a single fixed cluster baked in.
- **No alerting integration** — the headless `--attach-listen` mode
  lets your alerting system inject prompts, but the integration
  itself (PagerDuty hook, etc.) is your code, not this example's.

## Compose with the rest of the substrate

- **Eventlog**: add `--session-db` to persist every triage session
  (the eventlog records each subagent's tool calls + outputs under a
  branch label, so you can post-mortem how the synthesis was reached).
- **Replay**: pair with `core-agent attach <url>` from another operator
  to watch a triage as it runs (LiveAgent observer mode — see
  [`docs/site/content/docs/reference/attach-tui.md`](../../docs/site/content/docs/reference/attach-tui.md)).
- **Scheduled monitoring**: wrap this same config under
  `agent.RunAutonomous` + a `Scheduler` to do periodic health sweeps
  rather than incident-only triage. See `examples/scheduled-monitor`
  for the substrate pattern.
