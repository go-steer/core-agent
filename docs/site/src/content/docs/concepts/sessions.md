---
title: Sessions and event log
---


By default, every `agent.Agent` uses ADK's in-memory session service: conversation history is alive only for the process lifetime, and a crash loses everything. For audit logs, replay, crash-resume, or any cross-restart workflow, wire a durable backend via the `eventlog` package.

This page covers when to use durable sessions, how to enable them, and what the substrate gives you.

---

## What you get

```go
import (
    "github.com/glebarez/sqlite"
    "github.com/go-steer/core-agent/v3/pkg/agent"
    "github.com/go-steer/core-agent/v3/pkg/eventlog"
)

handle, err := eventlog.Open(ctx, sqlite.Open("sessions.db"))
if err != nil { /* ... */ }
defer handle.Close()

a, _ := agent.New(m,
    agent.WithEventLog(handle),
    agent.WithTools(myTools),
)
```

The `*eventlog.Handle` bundles two things every consumer wants:

- A `session.Service` backed by the same database. Every event the agent emits — user inputs, model responses, tool calls, tool results, partials — lands in durable storage.
- A `Stream` for replay (`Since(fromSeq)`) and live-tail (`Watch(fromSeq)`) over those events, with monotonic `seq` numbers for cursor-style consumption.

`agent.WithEventLog(handle)` wires both into the agent in one call. Equivalent to `agent.WithSessionService(handle.Service)` plus a stash of the handle so you can reach back to `agent.EventLog()` for replay/watch without keeping a separate reference.

---

## CLI flags

The bundled CLI (`core-agent`) and any consumer fork opts into durable sessions through two flags:

```
--session-db                    persist sessions + audit log to a durable database (default off)
--session-db-path=PATH          override the database path (default: ~/.<binary>/sessions.db)
```

Either flag enables. The default path is derived from `os.Executable()`, so `core-agent` and forks each get their own directory automatically — no per-binary configuration needed.

### Attach mode implies durability

`--attach-listen` or `--attach-unix-socket` turns durable sessions on by itself. It is a precondition there, not a preference: the live-tail broadcaster pumps from the event log, `/events` replays from it, subagent history reads it, and guardrail state and auto-continue restore from it. A daemon without one has nothing to serve.

The implication only fires when neither flag was given — pass `--session-db-path` and the daemon uses your path. `--session-db=false` on an attach shape is a config error rather than a silent override: the implication is allowed to answer a question you did not ask, not to overrule an answer you gave. Startup says which happened:

```
core-agent: session db: /home/you/.core-agent/sessions.db [implied by attach mode; --session-db-path to relocate]
```

Interactive runs are unchanged: nothing is implied, and a run without a durable database now names what it is giving up rather than staying quiet about it.

```
core-agent: session db: off (in-memory) — no resume, no auto-continue, no guardrail
state across restart; this session is gone when the process exits. --session-db to persist.
```

One-shot `-p` and `--script` runs stay silent — they are ephemeral by definition, and there is no session to lose beyond the turn.

---

## Multi-driver

`eventlog.Open` accepts any GORM dialector:

```go
// SQLite (recommended for embedded use; pure-Go via glebarez/sqlite — no CGO).
eventlog.Open(ctx, sqlite.Open("sessions.db"))

// PostgreSQL.
eventlog.Open(ctx, postgres.Open(dsn))

// MySQL.
eventlog.Open(ctx, mysql.Open(dsn))
```

The CLIs wire SQLite by default. Library callers swap in any dialector — the rest of the API is identical.

For SQLite, WAL mode is enabled at startup (`PRAGMA journal_mode=WAL`) so concurrent readers can run alongside the single writer. Disable with `WithSkipWAL` for in-memory or read-only setups. For workloads that need true concurrent writers across processes, Postgres is the answer — same `eventlog.Open` API, swap the dialector.

---

## Agent Engine Sessions as the transcript (v3.2+)

A session whose only copy is a SQLite file lives and dies with the machine (or pod, or sandbox) that holds the file. `state.transcript.backend: "agentengine"` moves the **transcript** — the ADK session record every turn appends to — into [Vertex AI Agent Engine Sessions](https://cloud.google.com/vertex-ai/generative-ai/docs/agent-engine/sessions/overview), a managed store authenticated with Application Default Credentials. A new process on a new machine, given the same session ID, resumes from it.

```json
{
  "state": {
    "transcript": {
      "backend": "agentengine",
      "agentengine": {
        "reasoning_engine": "projects/my-project/locations/us-central1/reasoningEngines/1234567890",
        "session_ttl": "720h"
      }
    }
  }
}
```

**What you need.** One Agent Engine instance (a *reasoning engine*) per deployment, created without deploying any code; its full resource name goes in `reasoning_engine`. The principal running core-agent needs `roles/aiplatform.user` on the project (or a narrower role that grants `aiplatform.sessions.*` and `aiplatform.sessionEvents.*`), and egress to `{location}-aiplatform.googleapis.com`.

**The local database stays.** `state.transcript.backend` other than `sql` *implies* `--session-db` exactly as attach mode does: the run lock, continuation claims, boot log, guardrail state, spend ledger, approval decisions, pause tokens and session ACLs still live in the SQLite file. Pass `--session-db-path` to say where. The startup log names both:

```
core-agent: session db: /workspace/.core-agent/sessions.db [implied by state.transcript.backend=agentengine; --session-db-path to relocate]
core-agent: session store: agentengine projects/…/reasoningEngines/1234567890 (transcript); local db holds control and state
```

**What survives the loss of the machine, and what does not (v3.2).**

| State | Survives |
|---|---|
| Transcript: every event, its `seq`, the caller sidecar, session state | **Yes** — read back from Agent Engine on the first `Get` |
| Spend ledger | Yes — rebuilt from the transcript's priced events at the current catalog, by the multi-session resume path (which today needs the ACL row; see below) |
| Run lock, continuation claims, boot log | Not needed — a new machine has nothing to hand over |
| Guardrail state (trips, budget extensions), approval decisions and parked calls, pause/interrupt tokens, MCP digest raw payloads | **No** — SQLite-only. The guardrail restore reads the local state row and the local overlay index, neither of which exists on a new machine, so a tripped watchdog or cost ceiling is forgotten. The next phase of [#1192](https://github.com/go-steer/core-agent/issues/1192) moves these into the transcript. |
| Session ACLs (`attach` multi-session) | **No** — and the attach server's lazy resume reads the ACL row to decide who may open a session, so on a machine with a fresh SQLite file `GET /sessions/{app}/{sid}` answers 404 even though the transcript is in Agent Engine. Resuming there works through the library (`eventlog.OpenTranscript` + `Service.Get`, `autonomous.Resume`) until the index moves off SQLite (#1192 phase 4). |

What this phase delivers, then, is a transcript that outlives the machine and a library path to it. Do not make `agentengine` the production default for a deployment that relies on attach-level resume or on the approval/pause rows surviving a restart.

**How it stores events.** Each Agent Engine `SessionEvent` carries the whole ADK event as JSON in `raw_event` — so `UsageMetadata`, `FinishReason` (via the `finish_reason` stamp), `FunctionCall.ID`, compaction summaries and everything else resume reads back survive the round-trip — plus a `coreAgent` block with the per-tree `seq` and the sidecar. The typed envelope fields (author, text and function parts, state deltas, branch) are populated too, so the Agent Engine console and Memory Bank can read the session. Session IDs the service's charset would reject (UUIDv7 IDs starting with a digit, `<id>:sub:…` subagent sessions) are mapped deterministically on the wire; the original IDs live in session state and nothing above the store sees the mapped form.

**Reads cost no RPC.** One process is the writer for a session, so once a session tree is loaded (one `ListSessions` plus one `ListEvents` per member) the in-memory copy is authoritative: `Get`, `Since`, `Watch` and `HighestSeq` read it. `Watch` is wake-up driven here rather than polled.

**`seq` after a restore.** Sequence numbers are per root session tree and continue where the store left off, so an attached client's `?since=` cursor from before the restart stays valid.

**Quota and failure.** The project default is 300 `appendEvent` calls per minute; twelve tool-heavy agents will exceed it, so request an increase before a fleet goes live. An append the service refuses fails the turn's event with the status intact (a 429 is `ResourceExhausted`); nothing is written anywhere else — there is no fall-back to local. An append whose outcome is unknown (the reply was lost) makes the process re-read the tree from the store before assigning the next `seq`, so a lost reply cannot leave a duplicate behind.

**TTL.** Every session is created with `session_ttl` (default 30 days, service minimum 24 hours) and the expiry is refreshed on append once less than half of it remains.

Library callers reach the same backend directly: `agentengine.Open` returns an `eventlog.Transcript`, and `eventlog.OpenTranscript(ctx, transcript, sqlite.Open(path))` builds the `Handle`. `pkg/statestore/conformance` is the contract every transcript backend passes.

---

## Schema

`eventlog.Open` manages ADK's session tables plus `pkg/eventlog`'s durable overlay and state tables in the same database:

| Table | Owned by | Purpose |
|---|---|---|
| `events`, `sessions`, `app_states`, `user_states` | ADK's `database.SessionService` (we wrap it unchanged) | Per-event payloads, session identity, app- and user-scoped state with `_adk*` filtering |
| `agent_eventlog` | `pkg/eventlog` | Monotonic overlay row per event: `seq INTEGER PRIMARY KEY AUTOINCREMENT`, the session triple, `event_id` (logical FK to `events.id`), `branch`, `author`, `timestamp`, and sidecar `metadata` |
| `agent_guardrail_state` | `pkg/eventlog` | Per-session durable watchdog and cost-ceiling halt flags, reasons, and cumulative operator-granted `budget_extension_usd` (`GuardrailStore`, `#1360`) |
| `agent_budget_spend` | `pkg/eventlog` | Per-session cumulative token and USD spend ledger (`SpendStore`, `#1359`) |
| `agent_run_lock` | `pkg/eventlog` | Distributed per-session and fleet lease locks with heartbeat expiry (`AcquireLock`, `#1361`) |
| `agent_continuation_claim` | `pkg/eventlog` | Auto-continue attempt claims and retry rate-limiting per session (`ClaimContinuation`) |
| `agent_boot_log` | `pkg/eventlog` | Daemon boot-attempt history backing the crash-loop circuit breaker |
| `agent_approval_decisions` | `pkg/eventlog` | Durable operator approval and denial records for parked mutating tool calls (`#1400`) |

The `seq` column is the cursor `Stream.Since` and `Stream.Watch` operate on. ADK's events table doesn't expose monotonic ordering — its event IDs are timestamp-based strings — so the overlay is what makes "everything since seq N" semantics possible.

Both rows are written in one transaction (v3.0+). Because the overlay is the index every `Since` and `Watch` consumer reads, an event with no overlay row isn't late — it's permanently invisible to live tail, attach replay and every transcript built on them. `AppendEvent` used to write the two independently, so a crash, OOM kill or node eviction landing between them produced exactly that. The overlay insert now runs inside ADK's own event transaction, so the two rows commit together or not at all; if the overlay write fails, the event is rolled back with it and the caller sees the error, which is the recoverable failure of the two. On a database where that wiring can't be established the log falls back to the older two-write path rather than refusing to open.

---

## Replay

```go
// Replay a session from the start.
for entry, err := range handle.Stream.Since(ctx, 0,
    eventlog.ForSession("core-agent", "local", "default")) {
    if err != nil { /* ... */ }
    fmt.Printf("seq=%d author=%s\n", entry.Seq, entry.Event.Author)
}
```

Each `Entry` carries the assigned `Seq` plus the rehydrated `*session.Event`. `Since` returns when caught up to the current end of log.

Context-management boundary events (v2.0+) are regular events with `CustomMetadata["compaction"]` set: `"summary"` for compaction summaries, `"checkpoint"` for task-boundary checkpoints. They appear in the audit log inline with the rest of the conversation; the slicing that hides earlier history from future model requests is applied at request-construction time, not by mutating the event log. Filter for them with a custom predicate on `entry.Event.CustomMetadata`, or use `WithAuthor` if your agent name is the only one writing them in this session. See [Context management](/concepts/context-management/) for the design.

Filters available:

| Filter | Effect |
|---|---|
| `ForSession(app, user, session)` | Restrict to one session triple. Queries without it scan across every session in the database — useful for audit dashboards, slow on a busy database |
| `WithSessionTree(app, user, parent)` | Returns events for `parent` and every derived sub-session (`<parent>:sub:%`). The one-query alternative to running `ForSession(parent)` plus `WithBranchPrefix(branch)` separately. Use this when you want the full audit trail of one logical "run." |
| `WithBranchPrefix(prefix)` | Match events whose `Branch` field begins with `prefix`. Subagent runners set `Branch="<parent>.<sub>"` so e.g. `WithBranchPrefix("research")` returns every research subagent's events across sessions. See [Library API → Subagents](/embed/api/#subagents) |
| `WithAuthor(name)` | Exact-match on the event's `Author` |
| `WithAuthorSuffix(suffix)` | Suffix-match on `Author`. Used internally by `autonomous.Resume` to find checkpoints regardless of which binary emitted them (`/autonomous`) |
| `WithLimit(n)` | Cap the result set |

---

## Live tail

`Watch` blocks for new events as they're appended:

```go
for entry, err := range handle.Stream.Watch(ctx, lastSeq,
    eventlog.ForSession("core-agent", "local", "default")) {
    if err != nil { /* ... */ }
    handleLive(entry)
}
```

Polls the database every 200ms by default. Tunable via `eventlog.WithWatchInterval(d)` at `Open` time — smaller values reduce latency at the cost of database load; larger values do the opposite.

Cancel `ctx` to stop. Native push (PostgreSQL `LISTEN/NOTIFY`, SQLite `update_hook`) is deferred — polling is the v1 floor across all dialectors.

---

## Session lock

To prevent two processes from simultaneously running `autonomous.Resume` against the same session, the package ships a small lock primitive that lives in the same database:

```go
lock, err := handle.AcquireLock(ctx, "core-agent", "alice", "long-task")
if err != nil { /* ErrSessionLocked when another holder is fresh */ }
defer lock.Release()
```

A background heartbeat goroutine refreshes the lease every 5 seconds (`eventlog.WithLockHeartbeat(interval, staleAfter)` overrides the defaults). A lease is considered stale after 30 seconds without a heartbeat and is automatically stolen by the next acquirer (recovers from crashed processes). Concurrent attempts on a fresh lease return `eventlog.ErrSessionLocked` with the holder identifier in the error message for diagnostics.

If a heartbeat tick discovers either that another holder stole the row (`RowsAffected == 0`) or that database errors have prevented a heartbeat refresh for at least the **loss window** — `staleAfter` minus one heartbeat interval, 25 seconds at the defaults — `SessionLock.Lost()` closes and the heartbeat loop stops. The window is deliberately one interval shorter than the 30-second point at which a newcomer may steal the row: a fast-failing database is only noticed on the next tick, so without the margin a holder could believe it still owned the lease for up to one interval after a successor had taken it. The margin assumes holders share NTP-grade clocks; skew between hosts eats into it directly.

`autonomous.Resume` selects on `lock.Lost()`, cancels any in-flight turn or scheduled-wake wait, and returns `eventlog.ErrLeaseLost` (`StopReasonContextCancelled`) so the process exits and a restart re-acquires. What happens to the final checkpoint depends on *why* the lease was lost: after a steal nothing is written, because the session now belongs to the successor; after a stall, `Resume` re-proves ownership with one conditional heartbeat (`SessionLock.Reaffirm`) and, if the row is still its own — the single-replica case, where a database outage longer than the loss window had nobody to hand the lease to — writes the final checkpoint so the partial turn's accounting survives. Either way the run ends; `Release` is bounded by the heartbeat timeout so a still-stalled database cannot hold the exit open.

**HA posture: single-replica with safe restart.** `SessionLock` is a cooperative lease for rolling restarts, crash recovery, and preventing accidental concurrent `autonomous.Resume` invocations against the same `(AppName, UserID, SessionID)`. It is not a fencing token on every event or tool side-effect: run at most one active replica per session (for example a single-replica `Deployment`, a `StatefulSet` ordinal, or one Kubernetes `Job` per session) rather than an active-active pool competing for the same session ID.

`autonomous.Resume` acquires the lock automatically. Plain `autonomous.Run` does not — fresh runs have no shared session to protect.

---

## Crash-resume

The session lock and the seq-numbered event log together support `autonomous.Resume`: a process that died mid-run can be restarted, and the new process picks up at the next turn from the same audit-log position.

See [Autonomous runs → Crash-resume](/run/autonomous/operations/#crash-resume) for the full pattern.

### Interrupted tool calls are repaired automatically

Events are committed as they are produced: the model response carrying a tool call lands in the database before the tool runs. If the process dies mid-tool (SIGKILL, OOMKill, pod replacement) — or the turn is cancelled mid-tool by an interrupt or daemon shutdown — the history can end with a tool call that never received its result. Model providers reject such a history outright.

The agent heals this on its own: at the start of every turn it scans recent history for its own unanswered tool calls and appends a synthesized error result ("tool execution was interrupted…") for each. The repair is a real, durable event — audit and replay see a consistent history — stamped with `kind: tool_tail_repair` in its event metadata so it is distinguishable from a genuine tool result. The model sees the interruption notice and can re-issue the call if the result still matters. No operator action is required.

### Spend survives a resume at the price it was billed

In multi-session mode (`compose.ReproduceAgent`), every priced model call a session makes — its own turns, delegated and `spawn_agent` subagent turns, and side calls such as the auto-mode approver — is appended to `agent_budget_spend` (`eventlog.SpendStore`, on the same database) at the moment it is billed, with the cost the catalog in force charged. When a session is resumed after eviction or a restart, its `usage.Tracker` is restored from those rows verbatim rather than re-priced from the event stream, so `/stats`, the per-model breakdown and the point where `--max-session-cost-usd` trips are the same numbers the session had when it was last live — even if `/pricing/refresh` changed the catalog in between. Event-backed rows are keyed by ADK event ID, so a turn billed twice lands once. A session created before the ledger existed has no rows and is rebuilt from its events at the current catalog, which seeds the ledger for its next resume; the session's stderr line reports which source was used (`spend restored from ledger` or `from events`).

---

## Recording vs event log

Two related but distinct logging layers ship in the repo:

| Package | Logs | When to use |
|---|---|---|
| `recording` | LLM-wire requests + responses (one entry per `GenerateContent` call) | Capturing a real session for offline replay against `mock.NewScripted` |
| `eventlog` | Agent-level events (one entry per `session.Event`) | Audit log, durable session, replay, crash-resume |

They operate at different layers and compose transparently — wrapping the LLM with `recording.NewRecorder` is independent of wiring the agent with `WithEventLog`.

---

## Consistency model

`AppendEvent` writes to ADK's events table first (so the event has its assigned ID), then to the overlay so it picks up a seq. The overlay has a unique index on `event_id`, so a retry of the same event is a no-op rather than a duplicate. Spanning a single transaction across both layers is not done in v1 — surfaced overlay-write errors let callers retry safely.

---

## Pricing

The CLIs add the `glebarez/sqlite` driver — pure-Go, ~10 MB binary growth. CGO-free builds keep working. For workloads where SQLite throughput matters more than CGO cleanliness, swap the dialector to `mattn/go-sqlite3` (CGO) — the rest of the API is unchanged.

---

## What's deferred

- **Native push for `Watch`** (Postgres `LISTEN/NOTIFY`, SQLite `update_hook`). Polling is good enough for v1; native push lands when a consumer reports actual latency pain.
- **Compaction / pruning.** Auditors typically want full retention; if storage growth becomes an issue we'll ship `eventlog.Prune(olderThan time.Time)`.
- **Cross-session app/user state mutations in the overlay.** The state tables are owned by ADK; their changes don't get seq numbers today. Add when a consumer needs cross-session audit trails.
- **Encrypted-at-rest event log.** Defer to filesystem/DB-level encryption.
- **Streaming subscribers over the network.** No gRPC/HTTP bridge in v1; an `extras/eventlog-server/` adapter would land if a consumer wants AX-style remote tail.
- **Multi-writer coordination across processes for plain `autonomous.Run`.** The session lock is acquired by `autonomous.Resume` only; concurrent fresh runs against the same session ID are possible but not protected.
- **Control, index and per-session state outside SQLite.** With an Agent Engine transcript these still live in the local file; moving them (and a residency lease held for the whole time a session is loaded) is the remaining work of [#1192](https://github.com/go-steer/core-agent/issues/1192).
