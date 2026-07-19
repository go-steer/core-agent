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
    "github.com/go-steer/core-agent/pkg/agent"
    "github.com/go-steer/core-agent/pkg/eventlog"
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

## Schema

Two tables in the same database:

| Table | Owned by | Purpose |
|---|---|---|
| `events`, `sessions`, `app_states`, `user_states` | ADK's `database.SessionService` (we wrap it unchanged) | Per-event payloads, session identity, app- and user-scoped state with `_adk*` filtering |
| `agent_eventlog` | This package | Tiny overlay row per event: `seq INTEGER PRIMARY KEY AUTOINCREMENT`, the session triple, `event_id` (logical FK to `events.id`), `branch`, `author`, `timestamp` |

The `seq` column is the cursor `Stream.Since` and `Stream.Watch` operate on. ADK's events table doesn't expose monotonic ordering — its event IDs are timestamp-based strings — so the overlay is what makes "everything since seq N" semantics possible.

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
| `WithAuthorSuffix(suffix)` | Suffix-match on `Author`. Used internally by `ResumeAutonomous` to find checkpoints regardless of which binary emitted them (`/autonomous`) |
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

To prevent two processes from simultaneously running `ResumeAutonomous` against the same session, the package ships a small lock primitive that lives in the same database:

```go
lock, err := handle.AcquireLock(ctx, "core-agent", "alice", "long-task")
if err != nil { /* ErrSessionLocked when another holder is fresh */ }
defer lock.Release()
```

A background heartbeat goroutine refreshes the lease every 5 seconds. A lease is considered stale after 30 seconds without a heartbeat and is automatically stolen by the next acquirer (recovers from crashed processes). Concurrent attempts on a fresh lease return `eventlog.ErrSessionLocked` with the holder identifier in the error message for diagnostics.

`ResumeAutonomous` acquires the lock automatically. Plain `RunAutonomous` does not — fresh runs have no shared session to protect.

---

## Crash-resume

The session lock and the seq-numbered event log together support `agent.ResumeAutonomous`: a process that died mid-run can be restarted, and the new process picks up at the next turn from the same audit-log position.

See [Autonomous runs → Crash-resume](/run/autonomous/operations/#crash-resume) for the full pattern.

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
- **Multi-writer coordination across processes for plain `RunAutonomous`.** The session lock is acquired by `ResumeAutonomous` only; concurrent fresh runs against the same session ID are possible but not protected.
