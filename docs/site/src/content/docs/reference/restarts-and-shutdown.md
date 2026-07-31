---
title: Restarts and shutdown
---


What happens when a `core-agent` daemon stops — a K8s rolling upgrade, a config-change restart, a crash — and what survives it. Short version: with a durable session DB, restarts are boring. That's the design goal.

---

## What SIGTERM does

The daemon catches SIGTERM (and only SIGTERM — SIGINT belongs to the REPL's double-Ctrl+C flow) and cancels the process context. From there:

1. **In-flight turns are interrupted immediately.** There is no drain phase and no drain knob, deliberately: agent turns can run unboundedly long, so no timeout is "long enough", and a drain longer than the supervisor's kill timeout just invites SIGKILL mid-cleanup. Interrupted work is recoverable instead (see below).
2. **Teardown runs with bounded steps**: peer-hub deregistration (2s cap), attach listener drain (SSE streams hung up, then graceful HTTP shutdown — default 5s, tunable via [`attach.shutdown_timeout`](/reference/configuration/#attachshutdown_timeout)), background-subagent drain (5s, stragglers abandoned and logged), MCP stdio children (SIGTERM → 3s grace → SIGKILL, concurrently), then telemetry flush and Vertex context-cache cleanup (3s each).
3. **Message intake refuses instead of lying.** Once SIGTERM fires, `POST /inject` and `POST /wake` return `503` with `Retry-After` — a message accepted in that window would sit in an in-memory inbox and die with the process after the client got a success response. Clients redeliver after the restart; committed history is unaffected.
4. **The process exits 0.** Restart-on-exit is the supervisor's job (K8s `restartPolicy`), not an exit-code contract.

Worst case with defaults, the whole sequence takes **≈ 24 seconds** — inside Kubernetes' default `terminationGracePeriodSeconds: 30` with headroom. If you raise `attach.shutdown_timeout`, raise the grace period to keep that inequality true.

**No `preStop` hook is needed.** The container image runs the Go binary as PID 1 via an exec-form `ENTRYPOINT` (distroless, no shell, no tini/s6), so the kubelet's SIGTERM arrives unwrapped.

---

## What survives: the durability contract

Persistence is **per-event, during the turn** — not at turn end and not at shutdown. Every user message, model response, tool call, and tool result is committed to the session DB as it is produced. Nothing about durability depends on shutdown code getting a chance to run, which is why the contract holds equally for SIGTERM, SIGKILL, and OOMKill:

> **At most the in-flight model response or tool execution is lost. Everything already committed survives.**

Two preconditions:

- **`--session-db` must be on.** The default session store is in-memory; without the flag, a restart loses everything. See [Sessions and event log](/concepts/sessions/).
- **The DB must be on storage that survives the pod** — a PVC in K8s. Sessions, the event log, ACL rows, run locks, and digest state all live in that one SQLite file (or Postgres/MySQL for multi-writer deployments).

If a restart catches a turn between a persisted tool call and its result, the history is [repaired automatically on the next turn](/concepts/sessions/#interrupted-tool-calls-are-repaired-automatically) — no operator action.

One narrow caveat: the event log closes slightly before the background-subagent drain finishes, so a subagent that exits *during* the 5s drain window may fail to persist its final events. Committed state is unaffected.

---

## What resumes, and how

- **Interactive / attach sessions** resume **lazily**: the first request that touches a session after restart reconstructs its agent from the persisted ACL row and event log — same session ID, same history, same ACL. Nothing is scanned or re-run at boot. See [Multi-session → Session resume](/concepts/multi-session/#session-resume-v25).
- **Autonomous runs** resume **explicitly**: per-turn checkpoints (goal, continuation prompt, budgets, next wake time) are persisted as events, and the orchestrator that owns the process (K8s CronJob, supervisord, AX) calls `autonomous.Resume` on the next start. A stale run lock from a crashed process is stolen automatically after 30s. See [Autonomous → Crash-resume](/run/autonomous/operations/#crash-resume).
- **Interrupted turns can be finished automatically (opt-in)**: with [`agent.auto_continue`](/reference/configuration/#agentauto_continue) enabled, a session whose turn was cut off by the restart gets a synthesized "continue the task" turn — on first touch for attached sessions, and via a bounded boot-time scan for channel sessions nobody re-touches. Guarded by a freshness window, a per-boot cap, one-automatic-attempt-per-interruption semantics, and a crash-loop breaker (`agent_boot_log`); deliberately-interrupted turns (`POST /interrupt`) are never resurrected. Off by default: without it, a session interrupted mid-question resumes with intact history but waits for the next message.

  **K8s deployments should enable this** — rolling upgrades are exactly what it exists for; the recommended block is `{ "agent": { "auto_continue": { "enabled": true } } }` alongside a per-turn cost ceiling. Scope: multi-session daemons (lazy touch + boot scan) and single-user **headless** daemons (`--no-repl`, the `examples/gke-deploy` shape — its recipe enables it). Interactive REPL/TUI modes are excluded: a human is present to re-ask. The off-by-default posture is slated for revisit once the feature has production soak (#559).

---

## Kubernetes checklist

```yaml
spec:
  terminationGracePeriodSeconds: 30   # ≥ teardown budget (~24s with defaults)
  containers:
    - name: core-agent
      # exec-form ENTRYPOINT, binary is PID 1 — no preStop needed
      volumeMounts:
        - name: state
          mountPath: /data
      args: ["--session-db-path", "/data/sessions.db", ...]
  volumes:
    - name: state
      persistentVolumeClaim:
        claimName: core-agent-state
```

- PVC for the session DB — without it every other guarantee on this page is moot.
- `strategy: Recreate` (or a leader lock) with RWO volumes — two daemons must not share one SQLite file.
- Raise `terminationGracePeriodSeconds` in lockstep if you raise `attach.shutdown_timeout`.
