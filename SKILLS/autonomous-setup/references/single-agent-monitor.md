# Single-agent monitor

Reference for the `autonomous-setup` skill. Fetch when the user is configuring a single autonomous agent (most common case — one focused task, one tool surface, one budget envelope).

## When this shape fits

- One subject (one deployment, one service, one queue)
- One scope (read-only OR a small, named set of mutating operations)
- One operator team that owns the outcome
- Goal can be expressed in a single coherent sentence

If any of those are split — e.g., the agent watches infra AND apps, or alerts route to different teams — consider the multi-agent shape (`references/multi-agent-decomposition.md`) instead.

## Worked example: "watch the deploy"

**Goal:** watch a Kubernetes Deployment, alert on unhealthy conditions, stay silent when healthy. Run for the duration of a release cycle (~1 hour).

### Step 1 — The goal sentence

```
Watch deployment `myapp` in namespace `prod` for the next hour. Every
~5 minutes, fetch its rollout status, pod conditions, and recent events.
Alert via report_alert when any of: pod in CrashLoopBackOff or
ImagePullBackOff for > 2 minutes; rollout stuck per kubectl rollout
status; ready replicas < desired for > 2 minutes; SLO error budget burn
rate > 2x normal. Stay silent when healthy. At the 60-minute mark,
call report_done with a summary of what was observed.
```

Every word is load-bearing:

- **"for the next hour"** — termination via wallclock budget. Crisp.
- **"Every ~5 minutes"** — cadence the agent should sustain via `schedule_next_turn`. Approximate; not strict.
- **"Alert via report_alert when any of: …"** — explicit trigger list. Not "alert on anomalies"; the model can't classify anomalies reliably without examples.
- **"Stay silent when healthy"** — explicitly states the no-op case. Without this, the model often posts status updates every wake.
- **"call report_done with a summary"** — explicit termination signal.

### Step 2 — `AGENTS.md` (project-scoped to this agent)

```markdown
You are a Kubernetes deployment monitor for the Acme platform.

## Your job

Watch the deployment named in your goal. Periodically fetch its state,
decide if it's healthy, and post a summary ONLY when something looks
unhealthy.

## When to alert

Use `report_alert` with a concise (3-5 sentence) summary when ANY of:

- Any pod in `CrashLoopBackOff`, `ImagePullBackOff`, or `Error` for > 2 minutes
- `kubectl rollout status` reporting a stuck rollout
- Ready replicas < desired replicas for > 2 minutes
- Container restart count increasing across consecutive scans
- Recent (last 5 min) Warning events at the deployment or pod level
- SLO error budget burn rate > 2x normal (use the SLO check skill)

Include the specific pod name(s) and event text in the alert body.

## When to scan

1. `kubectl get deployment <name> -n <namespace> -o json`
2. `kubectl get pods -l app=<name> -n <namespace> -o json`
3. `kubectl get events -n <namespace> --field-selector involvedObject.name=<name>`
4. `kubectl rollout status deployment/<name> -n <namespace> --timeout=10s`

Read-only operations. No delete, no apply, no scale.

## When to report_done

When the wallclock budget is nearly exhausted, call `report_done` with a
one-paragraph summary of what was observed during the run.

## What NOT to do

- Don't propose remediations. You're a watcher, not an actor.
- Don't post status updates when everything is healthy. Silence = good.
- Don't run `kubectl describe` repeatedly on the same resource — it
  floods the audit log. Use `get -o json` and parse.
- Don't restart the same investigation. If you already alerted about
  Pod X at T-5min, don't re-alert on the same state at T-now.
```

### Step 3 — `.agents/config.json`

```json
{
  "version": 1,
  "model": {
    "provider": "gemini",
    "name": "gemini-2.5-flash"
  },
  "permissions": {
    "mode": "allow",
    "allow": [
      "bash:kubectl get *",
      "bash:kubectl rollout status *",
      "bash:kubectl logs *"
    ]
  }
}
```

Notes:

- **Flash as parent.** A monitor doesn't need frontier reasoning. Flash is ~5x cheaper per token and handles "check state, decide alert" cleanly.
- **Allow-list is exactly the expected bash invocations.** Anything outside hits the `ask_user` refusal (from `--ask=auto`).
- **No `agentic-tools`.** A monitor does small reads (`kubectl get`), not bulk content digestion. The wrapper overhead doesn't pay off.

### Step 4 — The Go driver

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/go-steer/core-agent/v2/pkg/agent"
    "github.com/go-steer/core-agent/v2/pkg/models"
    _ "github.com/go-steer/core-agent/v2/pkg/models/gemini"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(),
        syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    provider, err := models.Resolve(nil)
    if err != nil { log.Fatal(err) }
    model, err := provider.Model(ctx, "gemini-2.5-flash")
    if err != nil { log.Fatal(err) }

    goal := `Watch deployment myapp in namespace prod for the next hour.
Every ~5 minutes, fetch its rollout status, pod conditions, and recent
events. Post a concise summary via report_alert when anything looks
unhealthy; otherwise stay silent. At the 60-minute mark, call
report_done with a summary.`

    res, err := agent.RunAutonomous(ctx, model, goal,
        agent.WithMaxWallclock(70*time.Minute),  // goal + buffer
        agent.WithMaxTurns(20),
        agent.WithMaxCost(2.00),
        agent.WithPerTurnTimeout(2*time.Minute),
    )
    if err != nil { log.Fatal(err) }
    log.Printf("stop: %s (turns=%d, cost=$%.4f)",
        res.StopReason, res.Turns, res.CostUSD)
    if res.StopReason != "completed" {
        os.Exit(1)  // CI / cron sees non-zero
    }
}
```

Run with `--session-db` so a crash doesn't lose the run:

```bash
go build -o watch-deploy ./cmd/watch-deploy
./watch-deploy
```

For production, wrap in your favorite scheduler — Kubernetes CronJob, systemd timer, etc. Pair with a sidecar tailing the audit log to your alerting system.

### Step 5 — Watching the run

In a separate terminal:

```bash
sqlite3 ~/.core-agent/sessions.db \
  "SELECT seq, author, json_extract(payload,'$.text') FROM agent_eventlog ORDER BY seq DESC LIMIT 20;"
```

Or use the attach mode — start the agent with `--attach-listen=:7777` and connect with `core-agent-tui http://localhost:7777` from a laptop to see live output.

## Variations

### Cadence

The `schedule_next_turn` tool lets the model defer its next wake. If the user wants a fixed cadence (every 5 min) vs adaptive (faster when anomalies present), instruct in the `AGENTS.md`:

```markdown
Default scan cadence is 5 minutes. When you've detected an unhealthy
condition, switch to 1-minute cadence until 3 consecutive scans return
healthy, then revert.
```

### Different subjects

For each subject (deployment / service / queue / etc.), the pattern is the same; the goal sentence + the `AGENTS.md` triggers change. The Go driver shape is identical.

### Multi-cluster

If watching N clusters with the same logic, run N driver instances. Each gets its own session DB row (or its own DB file). Don't try to share one driver across clusters — the agent can't context-switch reliably between subjects.

For shared coordination, see `references/multi-agent-decomposition.md`.

## Common failure modes

| Symptom | Likely cause | Fix |
|---|---|---|
| Agent runs but never alerts | Triggers in `AGENTS.md` too narrow OR conditions genuinely never tripped | Test with a deliberate failure; check the audit log for the gate's scan output |
| Agent alerts every scan | "Stay silent when healthy" rule missing OR too soft | Add the explicit rule; iterate `AGENTS.md` until it stops |
| Agent goes off-script (proposes remediations) | "Don't propose remediations" rule missing | Add to the don't-do list; for higher-stakes runs, remove the write tools via `tools.disable` |
| Budget exhausts before run completes | Cadence too tight OR per-turn input too large | Raise `--max-cost`/`--max-turns`, OR widen scan cadence, OR slim what the agent reads each scan |
| Agent hangs on first turn | `--ask=auto` not set; agent blocks on a permission prompt | Add `--ask=auto`; gate refuses cleanly so the model can adapt |
