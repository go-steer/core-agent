# Multi-agent decomposition

Reference for the `autonomous-setup` skill. Fetch when the user's workload genuinely needs multiple specialists with different scopes — typical for multi-tenant, multi-domain, or multi-cluster operations.

## When this shape fits

Use multi-agent when ALL of these are true:

- The work splits into clearly-separable concerns (infra vs apps; per-tenant; per-cluster)
- Each concern has its own appropriate tool surface (read-only vs full; cluster API vs app API)
- A coordinator role makes sense (route alerts, decide cross-cutting concerns, fan out)
- A single agent's `AGENTS.md` would become so long that attention dilutes

If concerns overlap heavily or one specialist would be near-empty, use a single agent with a richer skill catalog instead.

## The canonical shape: parent + specialists

The pattern from [gke-labs/kube-agents](https://github.com/gke-labs/kube-agents) (and applicable broadly):

```
platform (parent)
   │
   ├── operator (infrastructure specialist)
   └── devteam (application specialist)
```

- **Parent (`platform`)** — governance + dispatching. NO direct tool access for the underlying domain (no `kubectl`, no AWS API, etc.). It only has spawn tools (`spawn_agent`, `list_agents`, `check_agent`, `stop_agent`). Its job: provision specialists, route alerts, decide cross-cutting concerns.

- **Specialists (`operator`, `devteam`)** — focused tool surface for their scope. Spawned by the parent at runtime (one per cluster, one per tenant, etc.). Each runs in its own session with its own budget envelope.

**Key insight: scope separation lives in the tool surface, not in `AGENTS.md`.** `devteam` literally cannot mutate cluster state because its `mcp.json` excludes the mutating MCP endpoint. This is stronger than an `AGENTS.md` "don't do X" rule because the model can't propose what the tool doesn't expose.

## Per-agent configuration shape

```
.agents/
└── agents/
    ├── platform/
    │   ├── AGENTS.md
    │   ├── config.json          # spawn tools only
    │   └── templates/           # subagent templates
    ├── operator/
    │   ├── AGENTS.md
    │   ├── config.json          # full domain tool access
    │   ├── mcp.json             # mutating + read-only endpoints
    │   └── skills/
    └── devteam/
        ├── AGENTS.md
        ├── config.json          # read-only tool access
        ├── mcp.json             # read-only endpoints only
        └── skills/
```

Each agent's directory is self-contained. The parent uses `templates/` (or hardcoded spawn specs in its `AGENTS.md`) to describe how it spawns specialists.

## Parent `AGENTS.md` patterns

The parent is unusual — it has almost no domain tooling. Its `AGENTS.md` should:

```markdown
You are the [DOMAIN] custodian for the [ORGANIZATION] fleet. You don't
touch [DOMAIN ENTITIES] directly — you provision specialized agents that
do, and you watch what they do.

## Your responsibilities

- On startup: spawn one [SPECIALIST-1] per [SCOPE-1], and one
  [SPECIALIST-2] per [SCOPE-2].
- On alert escalation: receive `report_alert` from child agents. Route
  per the rules below.
- On budget exhaustion or unhealthy subagent: stop the failing subagent
  with `stop_agent` and spawn a fresh one with a wider envelope.

## Routing rules

- App-level alerts → ack, let the originating [SPECIALIST] continue
- Infra-level alerts → forward to the relevant [INFRA SPECIALIST]
- Cross-cutting concerns → handle directly, coordinate with affected specialists

## How you spawn

Use `spawn_agent` with these templates:

- [specialist-1]: load `templates/specialist-1.yaml`, pass <scope> in
  the goal. Budget: <wallclock>, <turns>, <cost>.
- [specialist-2]: load `templates/specialist-2.yaml`, pass <scope> in
  the goal. Budget: <wallclock>, <turns>, <cost>.

## What NOT to do

- Don't make [DOMAIN] API calls yourself. You have no MCP wiring —
  that's intentional. Delegate to specialists.
- Don't escalate [DOMAIN A] alerts to [DOMAIN B] specialists. Different
  scopes.
- Don't restart healthy subagents. Aggressive restarts churn audit logs.

## When to report_done

When all child agents have completed and no new dispatches are needed —
typically only on shutdown.
```

The parent's `config.json` only allows the spawn tools:

```json
{
  "permissions": {
    "mode": "allow",
    "allow": [
      "spawn_agent:*",
      "list_agents:*",
      "check_agent:*",
      "stop_agent:*"
    ]
  }
}
```

No bash, no MCP, no filesystem. Tight blast radius.

## Specialist `AGENTS.md` patterns

Each specialist is a focused autonomous agent (see `references/single-agent-monitor.md` for the single-agent shape). The differences from a standalone monitor:

1. **Scope is parameterized.** The specialist's goal includes the specific subject (cluster name, tenant, namespace) passed in by the parent at spawn time.
2. **Alert format includes escalation routing.** Each alert says "this is app-level" or "this is infra-level" so the parent can route correctly.
3. **Tool surface is structurally limited.** Read-only vs full per the specialist's scope.

Example (devteam — app specialist, read-only):

```json
// devteam/mcp.json
{
  "version": 1,
  "servers": {
    "platform-readonly": {
      "transport": "http",
      "url":       "https://api.example.com/mcp/read-only",
      "headers":   { "Authorization": "Bearer ${env:API_TOKEN}" }
    }
  }
}
```

Example (operator — infra specialist, full + read-only):

```json
// operator/mcp.json
{
  "version": 1,
  "servers": {
    "platform": {
      "transport": "http",
      "url":       "https://api.example.com/mcp",
      "headers":   { "Authorization": "Bearer ${env:API_TOKEN}" }
    },
    "platform-readonly": {
      "transport": "http",
      "url":       "https://api.example.com/mcp/read-only",
      "headers":   { "Authorization": "Bearer ${env:API_TOKEN}" }
    }
  }
}
```

The operator can investigate with `platform-readonly_*` tools and remediate with `platform_*` tools when warranted. The devteam literally cannot remediate — `platform_*` tools aren't in its surface.

## The Go driver

The parent runs via `RunAutonomous`; children are spawned dynamically via the `spawn_agent` tool from inside the parent's session.

```go
func main() {
    ctx, stop := signal.NotifyContext(context.Background(),
        syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    // Chdir to the parent's config directory so its config.json /
    // AGENTS.md / skills/ resolve correctly.
    if err := os.Chdir(".agents/agents/platform"); err != nil {
        log.Fatal(err)
    }

    provider, err := models.Resolve(nil)
    if err != nil { log.Fatal(err) }
    model, err := provider.Model(ctx, "gemini-3.1-pro-preview-customtools")
    if err != nil { log.Fatal(err) }

    goal := `Bootstrap the management team:

    Clusters: <cluster-1>, <cluster-2>, <cluster-3>
    Namespaces: <ns-1>, <ns-2>, <ns-3> (in each prod cluster)

    Spawn one [operator] per cluster and one [devteam] per (cluster,
    namespace). Then monitor: when subagents report alerts, route per
    your routing rules. Replace failed subagents within 60s of failure
    detection.`

    res, err := agent.RunAutonomous(ctx, model, goal,
        agent.WithMaxWallclock(24*time.Hour),  // long-running coordinator
        agent.WithMaxCost(50.00),
        agent.WithPerTurnTimeout(60*time.Second),
    )
    if err != nil { log.Fatal(err) }
    log.Printf("platform stop: %s", res.StopReason)
}
```

The parent's turns are mostly "check on N subagents, decide if anything needs attention." Cheap. The bulk of the cost is in the children.

## Cost model

For a 3-cluster × 3-namespace fleet (1 platform + 3 operators + 9 devteams = 13 agents), typical cost shape:

- Parent: ~$2-5/day (mostly idle, fires on alerts)
- Each operator: ~$5-10/day (scheduled patrols + CVE scans)
- Each devteam: ~$3-5/day (rollout watches + SLO checks per namespace)

Total: ~$60-110/day for the team. Use per-agent `--agentic-tools --agentic-small-model gemini-2.5-flash` to compress the per-agent cost ~30% on tool-heavy operations.

For larger fleets, the per-agent cost is bounded by budget caps; total cost scales linearly with agent count.

## Anti-patterns

| Pattern | Why it fails | Fix |
|---|---|---|
| Parent has direct tool access ("just in case") | Parent can take action without spec'd delegation; loses blast-radius safety | Parent gets ONLY spawn tools. Direct work goes through specialists. |
| Specialists share one budget | One runaway specialist starves the others | Each specialist has its own budget. Parent has its own budget. |
| Specialists alert without classification | Parent can't route; either picks wrong or escalates everything | Each alert names "app-level" vs "infra-level" vs "cross-cutting" in its body |
| Coordinator is itself an LLM agent making fine-grained calls | Cost balloons | Parent should be HIGH-LEVEL: dispatch, route, restart. Fine-grained work is the specialist's job. |
| Restart child on any failure | Audit log churns; transient failures masked | Replace only on persistent failure (≥ N consecutive). Use `list_agents` to see history. |
| All specialists run the same model | Cost inefficient | Match model to specialist load. Lighter monitors → Flash; deeper investigators → Pro. |

## When NOT to use multi-agent

| Scenario | Better approach |
|---|---|
| Single subject, single tool surface | Single agent with skills for sub-tasks |
| You think you need multi-agent but can't articulate scope separation | Stay single-agent; multi-agent without clear scope split is just overhead |
| Coordination would route ~1 alert/day | Single agent with logging |
| Team is < 3 specialists | Single agent with skills probably suffices |

The cognitive overhead of multi-agent (designing scope separation, alert routing, parent dispatching logic) is significant. Single-agent + skills handles a lot more workloads than people initially assume.
