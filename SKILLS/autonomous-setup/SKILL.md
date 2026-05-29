---
name: autonomous-setup
description: Walk a user through configuring an unattended core-agent — single-agent monitor or multi-agent team. Use when the user asks to "set up an autonomous agent", "run core-agent unattended", "configure a monitor", "build a multi-agent team", "deploy core-agent to production", "set up a long-running agent", asks how to bound budgets, asks about crash-resume, or describes a goal that obviously needs unattended execution (e.g., "watch the deploy", "monitor SLO breaches", "scan the fleet every hour").
---

When invoked:

1. **Decide if autonomous actually fits.** Many tasks the user thinks need unattended execution actually want interactive-with-attach. Ask: "Will a human need to make judgment calls during the run?" If yes, interactive + `--attach-listen` is usually better than fully autonomous.

2. **Define the goal crisply.** Autonomous runs follow goals strictly. Ambiguous goals produce either over-eager action or paralysis. Walk the user through:
   - What outcome they want (the success criterion)
   - What "done" looks like (the termination condition)
   - What constitutes an anomaly worth alerting about (vs noise to suppress)

3. **Pick single-agent OR multi-agent shape.** Heuristic:
   - **Single agent** — one focused task, one tool surface, one budget envelope. Most monitors fit here. Use `references/single-agent-monitor.md`.
   - **Multi-agent** — several specialists with different scopes, parent dispatcher coordinating. Use when concerns genuinely separate (infra vs apps, multi-tenancy, multi-cloud). Use `references/multi-agent-decomposition.md`.

4. **Bound the run.** Set the four budget caps with values the user is comfortable with. Tighter than they think they need; can always raise. Use `references/budgets-and-recovery.md` for the patterns.

5. **Write the `AGENTS.md` with unattended discipline.** Autonomous AGENTS.md is *more* explicit than interactive — crisp success criterion, explicit don't-do list, bounded tool surface. Walk through:
   - Role + scope
   - When to alert (specific triggers)
   - When to report_done (the termination signal)
   - What NOT to do (the don't-do list is load-bearing without an operator in the loop)

6. **Set headless permission posture.** Default to `--ask=auto` + an `allow` list for expected calls. Use `references/single-agent-monitor.md` for the patterns.

7. **Wire durable sessions.** `--session-db` is non-negotiable for autonomous runs — without it, a process crash loses the run; with it, `ResumeAutonomous` picks up cleanly.

8. **Write the Go driver.** Autonomous mode runs from a small Go program calling `agent.RunAutonomous`. Generate it for the user; show what it does.

9. **Test the run end-to-end before deployment.** Don't deploy unattended without a successful local test that exercises the actual workflow.

## Triggers in detail

Beyond the description's verbatim list, also match on:

- "How do I keep core-agent running overnight"
- "Set up a cron-like agent"
- "Deploy core-agent to GKE / Kubernetes / a server"
- "Build a fleet of agents"
- "Multi-agent setup like the GKE team example"
- "How do I make the agent retry on failure"
- "Bound the cost of an unattended run"

## References

Fetch the relevant reference based on the user's path:

- **`references/single-agent-monitor.md`** — single-agent monitor pattern: one focused goal, one tool surface, scheduled wakes, alert flow. Read for ~80% of autonomous setups. Includes a complete worked example (watch a Kubernetes deployment).
- **`references/multi-agent-decomposition.md`** — multi-agent shape: when to split into parent + specialists, spawn/dispatch patterns, scope separation via tool surface, escalation flow. Read when the user is describing a multi-tenant or multi-domain workload that naturally decomposes.
- **`references/budgets-and-recovery.md`** — budget caps (turns, tokens, cost, wallclock), per-turn timeout, failure policy, crash-resume via `ResumeAutonomous`, audit-log queries. Read when sizing budgets or when the user is worried about runaway cost.

## Procedure: define the goal

The single biggest predictor of an autonomous agent working well is the crispness of its goal. Walk the user through articulating:

```
Watch [SUBJECT] for [DURATION]. Every [CADENCE], check [SPECIFIC THINGS].
Alert when [SPECIFIC TRIGGER CONDITIONS]. Stay silent otherwise.
At [TERMINATION CONDITION], call report_done with a summary.
```

Bad goal: "monitor the production environment."

Good goal: "Watch deployment `myapp` in namespace `prod` for the next 4 hours. Every 5 minutes, fetch its rollout status, pod conditions, and recent events. Alert when any pod is in `CrashLoopBackOff` or `ImagePullBackOff` for > 2 minutes, when ready replicas < desired for > 2 minutes, or when SLO error budget burn rate > 2x normal. Stay silent when everything is healthy. At the 4-hour mark, call `report_done` with a one-paragraph summary."

Help the user iterate to the second form before writing any code.

## Procedure: budgets

Always set all four:

```go
agent.WithMaxWallclock(4*time.Hour),  // upper bound on run duration
agent.WithMaxTurns(100),               // upper bound on Run invocations
agent.WithMaxCost(2.00),               // upper bound on $ spend
agent.WithPerTurnTimeout(2*time.Minute), // single rogue turn can't stall
```

Start tight. The user can raise if the run hits the cap legitimately. The reverse — finding out the unattended agent burned $100 overnight — is much worse.

For multi-agent setups, set budgets per-agent. The parent's budget is "manage the team for N hours"; each child's budget is "do your specific job within bounds." Don't share one budget across the whole team.

## Procedure: `AGENTS.md` for unattended use

Mandatory sections:

```markdown
You are <ROLE> for <SUBJECT>.

## Your job

<one paragraph stating what success looks like>

## When to alert

<specific trigger conditions — each one explicit>

## When to report_done

<specific termination condition>

## What NOT to do

- <anti-pattern 1, named specifically>
- <anti-pattern 2>
- <anti-pattern 3>

## Tool surface

<which tools the agent should/shouldn't use, and why>
```

The "What NOT to do" list is especially important. With no operator in the loop, the cost of an unintended action is high. Be aggressive: "Don't propose remediations. Don't run `kubectl describe` repeatedly. Don't post status updates when healthy."

## Procedure: permission posture

Two patterns:

**Pattern A (recommended for most monitors): `--ask=auto` + allowlist.**

```bash
core-agent --ask=auto ... # runtime flag
```

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

The `ask_user` tool is registered but cleanly refuses with "no user available" when there's no TTY. The agent adapts. The `allow` patterns cover expected calls; anything outside hits the refusal.

**Pattern B (fully-locked surfaces): `mode=yolo` + tight `tools.disable` + `path_scope`.**

```json
{
  "permissions": { "mode": "yolo" },
  "tools": { "disable": ["write_file", "edit_file", "delete_file"] }
}
```

Skip the gate entirely. The bash denylist + path_scope still apply. Use only when the tool surface is known-safe.

## Procedure: durable sessions

```bash
core-agent --session-db --session-db-path=/var/lib/myagent/sessions.db
```

Or in the Go driver, pass `agent.WithSessionService(...)` constructing an `eventlog`-backed service. Without durable sessions, a crashed process loses the run. With them, `ResumeAutonomous(ctx, sessionID, ...)` picks up cleanly.

For multi-agent runs, each subagent gets its own session — durable storage applies to all of them via the same DB.

## Procedure: the Go driver

```go
package main

import (
    "context"
    "log"
    "os/signal"
    "syscall"
    "time"

    "github.com/go-steer/core-agent/agent"
    "github.com/go-steer/core-agent/models"
    _ "github.com/go-steer/core-agent/models/gemini"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(),
        syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    provider, err := models.Resolve(nil)
    if err != nil { log.Fatal(err) }
    model, err := provider.Model(ctx, "gemini-3.1-pro-preview-customtools")
    if err != nil { log.Fatal(err) }

    goal := `<the crisp goal from step 2>`

    res, err := agent.RunAutonomous(ctx, model, goal,
        agent.WithMaxWallclock(4*time.Hour),
        agent.WithMaxTurns(100),
        agent.WithMaxCost(2.00),
        agent.WithPerTurnTimeout(2*time.Minute),
    )
    if err != nil { log.Fatal(err) }
    log.Printf("stop: %s (turns=%d, cost=$%.4f)",
        res.StopReason, res.Turns, res.CostUSD)
}
```

Walk the user through customizing each piece. The basic shape is stable; what changes is the goal, the budgets, the provider/model pick, and (for multi-agent) the spawn-tool wiring.

## When NOT to use this skill

- The user is configuring `core-agent` for **interactive** use. Use the `cli-setup` skill.
- The user is **embedding `core-agent` in a non-autonomous library context** (e.g., HTTP-served single-turn agents). Use the `library-embedding` skill.
- The user wants a one-shot run (`-p "..."`) not an unattended monitor. That's just CLI use; no autonomous machinery needed.
- The user has an existing autonomous agent and asks a specific failure question. Diagnose first; only walk the configuration runbook if the failure traces to misconfiguration.

## Output style

Conversational, sequential. Don't dump all 9 steps up front — that's overwhelming. Walk one decision at a time: confirm the goal shape before talking about budgets; confirm budgets before discussing AGENTS.md; etc.

When generating code (the Go driver), explain what each block does before writing. The user is going to run this unattended; they need to understand it well enough to trust it.

For multi-agent setups, do one agent at a time. Configure `platform` (or whatever the parent is), then one child, then another. Each child is a recursive call into this skill's procedure — same shape, narrower scope.
