# Budgets and recovery

Reference for the `autonomous-setup` skill. Fetch when sizing budgets, picking a failure policy, configuring crash-resume, or troubleshooting runaway cost.

## The four budget caps

```go
agent.WithMaxWallclock(d time.Duration)  // total wall-clock duration
agent.WithMaxTurns(n int)                // number of Run invocations
agent.WithMaxTokens(in, out int)         // cumulative token totals
agent.WithMaxCost(usd float64)           // cumulative dollar cost
agent.WithPerTurnTimeout(d time.Duration) // single-turn context.WithTimeout
```

The first four bound the run. The fifth bounds a single turn so one rogue tool call can't stall the entire run.

Always set all five. Each backstops the others:

- A wallclock cap protects against schedule_next_turn loops scheduled too far ahead
- A turn cap protects against the model thrashing without making progress
- A cost cap protects against the model picking expensive operations
- A token cap is mostly redundant with cost in well-priced models, but useful as a sanity check
- A per-turn timeout protects against one tool call (a network fetch, a long bash) hanging the run

## Picking values

Start tight. You can always raise. The reverse — discovering at 8am that the unattended agent burned $200 overnight — is bad.

| Use case | Wallclock | Turns | Cost | Per-turn |
|---|---|---|---|---|
| Short monitor (1 hour deploy watch) | 70min | 20 | $2 | 2min |
| Medium monitor (overnight 12h watch) | 14h | 200 | $10 | 5min |
| Long-running coordinator (24h+ team parent) | 26h | 1000 | $50 | 1min |
| Per-task subagent (focused investigation) | 30min | 30 | $1 | 2min |
| Per-task subagent (open-ended research) | 2h | 100 | $5 | 5min |

These are starting points. Tune based on observed run stats.

## Per-agent vs shared budgets

For multi-agent setups, every agent has its own budget. The parent has its own; each child has its own. **Never share one budget across the team** — one runaway specialist would starve all the others.

Total fleet spend = sum of per-agent budgets. Plan accordingly. A team of 10 agents at $5/day each is $50/day; if you only have $100/month allocated, you're underwater on day 2.

## Failure policy

What does the agent do when a turn fails? `agent.WithFailurePolicy(policy)`:

| Policy | Behavior | When to use |
|---|---|---|
| `FailFast` | First error stops the run | Critical workflows where partial progress is meaningless |
| `RetryWithBackoff(n)` | Retry up to n times with exponential backoff | Transient infrastructure errors (network blips, rate limits) |
| `ContinueOnError` (default) | Log + skip; next turn proceeds | Monitors where one failed scan shouldn't kill the run |

For monitors, the default (`ContinueOnError`) is right — a one-off failed `kubectl get` shouldn't end the run. For workflows that strictly depend on each step succeeding (deploy steps, migrations), `FailFast` is right.

## Crash-resume via `ResumeAutonomous`

If the process crashes mid-turn (OOM, SIGKILL, node failure), a fresh process can pick up the same session and continue:

```go
res, err := agent.ResumeAutonomous(ctx, model, sessionID, ...)
```

Requirements:

1. **Durable session storage.** Pass `agent.WithSessionService(...)` constructing an `eventlog`-backed service, OR use the CLI flag `--session-db`. Without durable sessions, the prior run's history is gone.
2. **Same session ID.** The session ID is the key; you must pass the same one the original run used. Common pattern: derive from a stable identifier like the cluster name or the goal hash.
3. **Same goal + same budgets.** Resume picks up the budget counters where they left off; cumulative caps continue across the resume.

For long-running unattended agents, wrap the run in a `for` loop:

```go
for {
    res, err := agent.ResumeAutonomous(ctx, model, sessionID, opts...)
    if errors.Is(err, agent.ErrSessionCompleted) {
        break
    }
    if err != nil {
        log.Printf("resume failed: %v; retrying in 60s", err)
        time.Sleep(60 * time.Second)
        continue
    }
    if res.StopReason == "completed" {
        break
    }
    // Otherwise (budget exhausted, transient error), resume continues.
}
```

This pattern auto-recovers from crashes. Pair with a process supervisor (systemd, Kubernetes Deployment with restartPolicy=Always) and the run survives across infrastructure events.

## Audit-log queries

`--session-db` creates `~/.core-agent/sessions.db` (or wherever you point it). The `agent_eventlog` overlay table indexes events by seq for cursor-style queries:

```sql
-- Most recent 20 events
SELECT seq, author, json_extract(payload, '$.text') AS text
FROM agent_eventlog
WHERE app_name = 'core-agent' AND session_id = '<your-session>'
ORDER BY seq DESC LIMIT 20;

-- All alerts the agent posted
SELECT seq, author, json_extract(payload, '$.text') AS text
FROM agent_eventlog
WHERE session_id = '<your-session>'
  AND json_extract(payload, '$.text') LIKE '%report_alert%'
ORDER BY seq DESC;

-- Cost across the session
SELECT
  SUM(CAST(json_extract(payload, '$.usage_metadata.prompt_token_count') AS INTEGER)) AS in_tokens,
  SUM(CAST(json_extract(payload, '$.usage_metadata.candidates_token_count') AS INTEGER)) AS out_tokens
FROM agent_eventlog
WHERE session_id = '<your-session>';
```

For programmatic access (Go), the `eventlog` package exposes a `Stream` API: `Since(seq)`, `Watch(seq)`, with filters by author + branch.

## What's NOT in budgets

- **Subtask cost rolls up to the parent's tracker** but doesn't have its own budget. If you want to bound subtask cost separately, use `agentic_*` wrappers with `--agentic-small-model` (sets the per-subtask model; budget bounded by `MaxTurns` per subtask, defaulting to 2-5 depending on wrapper).
- **MCP server costs** are external to the model budget. If your MCP server hits a paid API (Tavily search, etc.), those costs aren't tracked. Set provider-side budgets.
- **Background subagents** spawned via `spawn_agent` have their own budgets passed at spawn time. They don't share the parent's budget. List children with `list_agents` to audit.

## Diagnosing runaway cost

```bash
# Was the run within budget?
/stats

# Where did the tokens go (per-model breakdown)?
/context

# Audit-log: which turns spent the most?
SELECT seq, json_extract(payload, '$.usage_metadata.prompt_token_count') AS in_tokens
FROM agent_eventlog
WHERE session_id = '<sid>'
ORDER BY in_tokens DESC LIMIT 10;
```

Common runaway patterns:

| Pattern | Symptom | Fix |
|---|---|---|
| Cadence too tight | Turn count high relative to wallclock | Widen `schedule_next_turn` interval in AGENTS.md |
| Per-turn input grows linearly | First turn 5k tokens, 50th turn 250k | Enable compaction (default-on for autonomous); add `/done` triggers at task boundaries |
| Model retrying the same failed tool call | Same tool name repeated dozens of times | Add the failure mode to AGENTS.md's don't-do list; consider `FailFast` policy |
| Subtask budget too generous | Subtasks running 5+ turns each | Tighten `Budgets.MaxTurns` per wrapper |
| Parent is itself making fine-grained calls | High parent turn count in multi-agent setup | Refactor: parent should be HIGH-LEVEL only; fine work is the specialist's job |

## When to bump core-tui for live monitoring

For long-running unattended runs, pair with attach mode so an operator can drop in:

```bash
core-agent --session-db --attach-listen=:7777 --attach-token=ATTACH_TOKEN \
  -p "$GOAL"  # or run as a Go driver with the same flags
```

From a laptop:

```bash
ATTACH_TOKEN=... core-agent-tui http://localhost:7777
```

The operator sees live output, can `/interrupt` if the agent goes off-script, can `/inject` mid-run notes. The agent doesn't know whether anyone's watching — attach is read-then-optionally-write, doesn't change the run.
