---
title: Autonomous quickstart
---

15 minutes from `core-agent` installed → a working unattended agent that watches a real thing and reports back.

> **Prefer to have an agent walk you through this?** The [`autonomous-setup` skill](/run/skills-library/autonomous-setup/) covers the same material in workflow form, including the multi-agent decomposition path. Install once, then say "set up an autonomous monitor for me" and the agent walks the 9-step runbook with you.

## What "autonomous" means here

The interactive TUI assumes an operator at the keyboard reacting to model output. Autonomous mode flips that: you describe a goal up front, hand the agent a set of tools and a budget, then walk away. The agent works until it decides it's done, hits a budget cap, or you cancel it. The session log is the audit trail.

Concrete examples of what fits:
- Watching a CI build for failures and posting a triage summary to Slack
- Polling a service's health endpoint and creating an incident if SLO falls below threshold
- Running through a backlog of code-review requests overnight against a queue
- Periodically scanning a fleet for drift and proposing remediations

What doesn't fit autonomous mode: anything that needs real-time operator judgment in the loop. Use interactive mode for those, or use autonomous mode with [attach](/reference/attach-tui/) so an operator can drop in when needed.

## Before you start

- Completed [Getting started](/run/getting-started/) — provider credentials work, `core-agent -p "hello"` returns a response.
- Understand the four customization layers from [Interactive quickstart](/run/interactive/quickstart/) — `config.json`, `AGENTS.md`, skills, MCP. Autonomous mode uses the same files.

This page works in a Go project (you'll write a small `main.go` to drive the agent). For autonomous use you call `agent.RunAutonomous` from your own code — it's not a CLI subcommand of `core-agent`.

---

## Step 1 — Scenario: "watch the deploy" (3 min)

We'll build a small monitor that watches a Kubernetes deployment, posts a brief status summary every 10 minutes for an hour, and surfaces anything that looks anomalous (pods crashing, image pulls failing, SLO breaches). The agent decides what's anomalous; we just bound how long it runs and how much it spends.

Goal as we'd describe it to the agent:

> *Watch deployment `myapp` in namespace `prod` for the next hour. Every ~10 minutes, fetch its rollout status, pod conditions, and recent events. If anything looks unhealthy — crash loops, image pull errors, replica count below desired, error-rate spikes in logs — post a concise summary. Otherwise stay quiet. Use the `report_done` tool to signal completion at the end.*

---

## Step 2 — The Go driver (5 min)

Drop this in `cmd/watch-deploy/main.go`:

```go
package main

import (
    "context"
    "log"
    "os"
    "os/signal"
    "syscall"
    "time"

    "github.com/go-steer/core-agent/pkg/agent"
    "github.com/go-steer/core-agent/pkg/models"
    _ "github.com/go-steer/core-agent/pkg/models/gemini"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(),
        syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    provider, err := models.Resolve(nil) // reads .agents/config.json
    if err != nil {
        log.Fatalf("resolve provider: %v", err)
    }
    model, err := provider.Model(ctx, "gemini-3.1-pro-preview-customtools")
    if err != nil {
        log.Fatalf("get model: %v", err)
    }

    res, err := agent.RunAutonomous(ctx, model,
        "Watch deployment myapp in namespace prod for the next hour. "+
        "Every ~10 minutes, fetch its rollout status, pod conditions, "+
        "and recent events. Post a concise summary when anything looks "+
        "unhealthy; otherwise stay quiet. Use report_done at the end.",
        // Budgets — autonomous always runs against bounds.
        agent.WithMaxWallclock(70*time.Minute),
        agent.WithMaxTurns(20),
        agent.WithMaxCost(2.00),
        // Per-turn timeout — one rogue tool call can't stall the run.
        agent.WithPerTurnTimeout(2*time.Minute),
    )
    if err != nil {
        log.Fatalf("autonomous run: %v", err)
    }
    log.Printf("stop reason: %s (turns=%d, input=%d, output=%d, cost=$%.4f)",
        res.StopReason, res.Turns, res.InputTokens, res.OutputTokens, res.CostUSD)

    if res.StopReason != "completed" {
        os.Exit(1) // CI / cron will see the non-zero
    }
}
```

`go build -o watch-deploy ./cmd/watch-deploy && ./watch-deploy` — and the agent is off.

**What this does:**

- Resolves the provider from `.agents/config.json` (same as the CLI does)
- Constructs a model handle
- Calls `agent.RunAutonomous` with a goal + four budget caps
- Logs the stop reason and totals when the run finishes

**Budget semantics:**

| Budget | Caps |
|---|---|
| `WithMaxWallclock` | Total wall-clock duration. Hard ceiling. |
| `WithMaxTurns` | Number of `Run` invocations the driver makes. The model gets at most N turns. |
| `WithMaxCost` | Cumulative dollar cost across all turns. Requires pricing to be wired (auto-detected from the model name). |
| `WithPerTurnTimeout` | Per-turn `context.WithTimeout`. One rogue turn can't stall the run. |

See [Operations](/run/autonomous/operations/) for the full budget reference, the lifecycle tool, failure policy, and crash-resume.

---

## Step 3 — The `AGENTS.md` for unattended use (3 min)

The unattended agent needs an `AGENTS.md` that's even more explicit than an interactive one. With no operator to course-correct, ambiguity in the prompt becomes either over-eager action or paralysis.

```markdown
You are a Kubernetes deployment monitor for the Acme platform.

## Your job

Watch the deployment named in your goal. Periodically fetch its state, decide
if it's healthy, and post a summary ONLY when something looks unhealthy.

## What "unhealthy" means

- Any pod in `CrashLoopBackOff`, `ImagePullBackOff`, or `Error` for > 2 minutes
- `kubectl rollout status` reporting a stuck rollout
- Ready replicas < desired replicas for > 2 minutes
- Container restart count increasing across consecutive scans (vs static)
- Recent (last 5 min) Warning events at the deployment or pod level

## How to scan

1. `kubectl get deployment <name> -n <namespace> -o json`
2. `kubectl get pods -l app=<name> -n <namespace> -o json`
3. `kubectl get events -n <namespace> --field-selector involvedObject.name=<name>`
4. `kubectl rollout status deployment/<name> -n <namespace> --timeout=10s`

Use `bash` for these. They're read-only — no `delete`, no `apply`, no `scale`.

## When to post

Use `report_alert` with a concise (3-5 sentence) summary when ANY of the above
unhealthy conditions trip. Include the specific pod name(s) and event text.

When the wallclock budget is nearly exhausted, call `report_done` with a
one-paragraph summary of what was observed during the run.

## What NOT to do

- Don't propose remediations. You're a watcher, not an actor.
- Don't post status updates when everything is healthy. Silence = good.
- Don't run `kubectl describe` repeatedly on the same resource — it floods
  the audit log. Use `get -o json` and parse.
```

The autonomous driver is strict about goal-following because there's no operator in the loop. Pay particular attention to:

- **Crisp success criterion.** "Call `report_done` at the end" gives the model an explicit termination signal. Without it, you're relying on budgets alone to stop the run.
- **Explicit don't-do list.** A monitor that decides to scale a deployment because "that would fix it" is exactly the failure mode unattended runs are notorious for.
- **Bounded tool surface.** Keep `kubectl` to read operations; if you need writes, gate them through a separate skill with explicit triggers.

For deeper prompt patterns see [Agent design → System instructions](/agent-design/system-instructions/).

---

## Step 4 — Headless permission posture (2 min)

Interactive defaults assume an operator is there to approve prompts. For unattended runs, you have two choices:

**Option A (recommended): `--ask=auto`.** The agent registers an `ask_user` tool. If the model tries to use it with no operator present (no TTY), the call cleanly refuses with "no user available" — the model sees the refusal and adapts. This is the right default for runs that *might* prompt occasionally but mostly proceed on their own.

**Option B: `permissions.mode = "yolo"`.** Skip the gate entirely. Use only for runs against a known-safe tool surface (e.g., your `kubectl` commands locked to read-only via `permissions.allow`). The `path_scope` and bash denylist still apply — yolo doesn't bypass them.

For most autonomous monitors, Option A is right. The exception is fully-locked-down workloads (read-only tool surface, no novel decisions) where you want zero gate friction.

Set the mode in `.agents/config.json`:

```json
{
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

This combines well with `--ask=auto`: the allow patterns cover the expected calls without prompting; anything outside hits the `ask_user` refusal cleanly.

---

## Step 5 — Run it + watch the audit log (2 min)

```bash
# In one terminal: run the monitor
./watch-deploy

# In another: tail the session log
core-agent --session-db --session-db-path=/tmp/watch-deploy.db &
sqlite3 /tmp/watch-deploy.db "SELECT seq, author, json_extract(payload,'$.text') FROM agent_eventlog ORDER BY seq DESC LIMIT 20;"
```

Every turn, every tool call, and every model response is appended to the durable event log. If the process crashes mid-turn, you can resume the same session via `ResumeAutonomous` and the agent picks up where it left off. See [Sessions and event log](/concepts/sessions/).

---

## Where to go next

- **[Operations](/run/autonomous/operations/)** — the depth reference: budgets, lifecycle tool, crash-resume, failure policy, audit-log queries, subagent composition
- **[GKE team scenario](/run/autonomous/gke-team-scenario/)** — multi-agent worked example: a platform parent + operator + devteam working together against real GKE workloads
- **[Context management](/concepts/context-management/)** — compaction + checkpoints make long unattended runs viable
- **[Sessions and event log](/concepts/sessions/)** — durable storage, replay, live tail, crash-resume
- **[Attach mode TUI](/reference/attach-tui/)** — let an operator drop into an unattended agent mid-run
- **[Library API → Autonomous runs](/embed/api/#autonomous-runs)** — full `RunAutonomous` reference + every option function
