# Durable sessions + audit/replay event log — M3 plan

## Recommendation summary

Ship a new `eventlog/` package that pairs an append-only event log primitive (monotonic `seq`, `Since(seq)` replay, `Watch(seq)` live-tail) with a `session.Service` implementation backed by ADK's existing GORM-backed `database` package (multi-driver: SQLite, MySQL, Postgres). Wire it into `agent.Agent` via a new `WithSessionService` option (which replaces the hardcoded `session.InMemoryService()` at `agent/agent.go:173`) plus a higher-level `WithEventLog` convenience. Extend `agent.RunAutonomous` with checkpoint events and a `ResumeAutonomous` API so a crashed run can pick up at the next turn from the event log alone. Refresh `docs/subagents-plan.md` to drop the `tool/agenttool` wrapping in favor of a custom subagent runner that participates in the parent's event log under a branch-scoped path — so subagent events stream live into the audit log instead of being dropped.

This addresses the two highest-priority items on the M3 list:

1. **Subagents** (existing plan: `docs/subagents-plan.md`, refreshed in Phase 4 of this work)
2. **Durable sessions / crash-resume / audit logs / append-only event log (SQLite) with seq numbers / replay from event log; client can request "everything since seq N"**

This document covers (2), built such that (1) lands cleanly on top in the next milestone.

## Context

Today every `agent.Agent` uses ADK's in-memory session service (`session.InMemoryService()`, hardcoded at `agent/agent.go:173`). The session is alive only for the process lifetime; a crash loses everything. `docs/autonomous.md` and `docs/DESIGN.md:374-375` both call out crash-resume as deferred to "M3 file-backed sessions" — this is that work.

The user goals are stronger than just "persist on disk":

- **Audit log** — every event captured, queryable after the fact ("show me every approval gesture in run X")
- **Replay from N** — "everything since seq 1234" semantics, like AX's resumable streams
- **Live tail** — subscribers watch the log as new events land, blocking until the next event arrives
- **Crash-resume for autonomous** — `ResumeAutonomous(...)` reads checkpoint events from the log and continues at the next turn
- **Subagent participation** — subagent events appear in the parent's audit log, branch-scoped, so one log captures the full hierarchy of work

Design choices the user has weighed in on:

| Question | Choice |
|---|---|
| Storage shape | **Multi-driver via GORM** — pass-through ADK's existing dialector support for SQLite, MySQL, Postgres |
| Resume scope | **Include RunAutonomous crash-resume in this milestone** rather than deferring to a follow-up |
| Subagent participation | **Live streaming via custom subagent runner** — replace the existing plan's `agenttool` wrapping so subagent events land in the parent's log as they happen |

## Key insights from exploration

These shape the design and explain why scope is smaller than the goal list might suggest:

1. **ADK ships `google.golang.org/adk/session/database`** — a full GORM-backed `session.Service` with `NewSessionService(dialector, opts...)` and `AutoMigrate(svc)`. We do **not** rebuild durability from scratch; we wrap it.
2. **ADK's `session.Event` already has `Branch string`** — exactly the `agent_1.agent_2.agent_3` hierarchy that subagents need. No event-shape changes required for subagent participation. Comment from ADK's source: "Branch is used when multiple sub-agent shouldn't see their peer agents' conversation history."
3. **ADK's `session.Service.AppendEvent` is the one chokepoint** for "an event happened." If we wrap that, we get every agent event on the wire — user inputs, model responses, tool calls, tool results, partials.
4. **`agent/agent.go:173` hardcodes `session.InMemoryService()`** — the seam for plug-in persistence. Change it to read from an option, the rest follows.
5. **ADK's `session/database` schema does not give us monotonic seq numbers** — events have a string `ID` (timestamp-based) and a `Timestamp` field; ordering is by timestamp+ID, with no numeric cursor. For AX-style "since seq N" semantics we add a thin overlay table (`agent_eventlog`) with `seq INTEGER PRIMARY KEY AUTOINCREMENT` and a logical foreign-key reference to ADK's `events.id`.
6. **The existing `recording/` package operates at a different layer** — LLM-wire (per `GenerateContent` call). Keep both: recording for deterministic LLM replay through `mock.NewScripted`; eventlog for agent-event audit/replay. Document the distinction; do not unify.
7. **`session/transcript.go` writes a JSON snapshot at session end** — keep as a "human-friendly export" path. Does not interact with the new event log.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│ Consumer code: agent.New(m, agent.WithEventLog(stream), ...)│
└─────────────────────────────────────────────────────────────┘
            │
┌───────────▼─────────────────────────────────────────────────┐
│ agent.Agent (existing) — uses configured session.Service    │
│ + new: agent.WithSessionService(s) and agent.WithEventLog(s)│
└───────────┬─────────────────────────────────────────────────┘
            │ session.Service interface (ADK)
┌───────────▼─────────────────────────────────────────────────┐
│ eventlog.Service — wraps ADK's database.SessionService;     │
│ on every AppendEvent, also writes to our seq table          │
└───────────┬─────────────────────────────────────────────────┘
            │
   ┌────────┴─────────┐
   ▼                  ▼
┌─────────┐   ┌──────────────────┐
│ ADK     │   │ eventlog.Stream  │
│ events  │   │ (seq, replay,    │
│ table   │   │ watch, since N)  │
└─────────┘   └──────────────────┘
   (handled by ADK; we don't touch the schema)
```

Two tables in the same DB:
- `events` (ADK's, untouched) — full event payload, the source of truth
- `agent_eventlog` (ours, added via AutoMigrate) — `seq INTEGER PRIMARY KEY AUTOINCREMENT`, plus session keys (app_name, user_id, session_id), event_id (logical FK to events.id), branch, author, timestamp. Tiny table, indexable by `(app_name, user_id, session_id, seq)` and `(branch, seq)`.

Reads:
- `Stream.Since(fromSeq, opts)` — `SELECT seq, event_id FROM agent_eventlog WHERE seq > ? ...`, then JOIN/lookup full events from ADK's events table. Returns `iter.Seq2[Entry, error]`.
- `Stream.Watch(fromSeq, opts)` — same query, polling loop with configurable interval (default 200ms; tune later). Cancellable via ctx.

## Phased delivery

Internal phasing keeps the work shippable in increments — each phase lands as its own commit set on `main`.

### Phase 1 — Plug-in `session.Service`

**Smallest reasonable shippable unit. ~1 day.**

- `agent/agent.go` — replace hardcoded `session.InMemoryService()` with a configurable option:
  ```go
  func WithSessionService(s session.Service) Option
  ```
- Existing behavior preserved when no option is set (still defaults to `InMemoryService`).
- Tests pin: option override works; default unchanged.
- No new package yet — just the seam.

### Phase 2 — `eventlog/` package: stream + GORM-backed service

**~3–4 days.**

New package `github.com/go-steer/core-agent/eventlog`. Public API:

```go
// Stream is the append-only event log primitive.
type Stream interface {
    // Append writes ev to the log under sess. Returns the assigned seq.
    Append(ctx context.Context, sess session.Session, ev *session.Event) (seq int64, err error)

    // Since returns events with seq > fromSeq, in order, bounded by
    // current end-of-log. Apply filters via QueryOption.
    Since(ctx context.Context, fromSeq int64, opts ...QueryOption) iter.Seq2[Entry, error]

    // Watch returns events with seq > fromSeq, in order, blocking
    // for new events until ctx is cancelled. Same opts as Since.
    Watch(ctx context.Context, fromSeq int64, opts ...QueryOption) iter.Seq2[Entry, error]

    // Close releases resources.
    Close() error
}

type Entry struct {
    Seq   int64
    Event *session.Event
}

type QueryOption func(*queryOpts)
func ForSession(appName, userID, sessionID string) QueryOption
func WithBranchPrefix(prefix string) QueryOption
func WithAuthor(name string) QueryOption
func WithLimit(n int) QueryOption
```

Constructor:

```go
// Open returns a Stream and a session.Service backed by the same
// underlying database. AutoMigrate is run once on first call.
//
// dialector is GORM's standard dialector (e.g.
// sqlite.Open("path.db"), postgres.Open(dsn), mysql.Open(dsn)).
// Multi-driver support comes from ADK's database service plus
// our overlay table being plain SQL via GORM.
func Open(ctx context.Context, dialector gorm.Dialector, opts ...Option) (Stream, session.Service, error)

type Option func(*openOpts)
func WithWatchInterval(d time.Duration) Option   // default 200ms
func WithGORMOptions(o ...gorm.Option) Option
```

Convenience wired into `agent`:

```go
// In agent/agent.go (Phase 2 ships this too):
func WithEventLog(stream eventlog.Stream, svc session.Service) Option
```

Implementation notes:
- The internal `eventlog.Service` wraps ADK's `database.NewSessionService` with a synchronous `AppendEvent` that writes to ADK's events table first (so the event has its assigned ID), then inserts the seq row. Failure of the seq insert after a successful event insert is a hard error (transaction-wrapped via GORM).
- Tests use SQLite in-memory (`sqlite.Open(":memory:")`) — no test fixtures, no temp dirs.
- Multi-driver smoke: tests against Postgres are gated behind a build tag (`-tags pg_integration`), CI runs SQLite only.

### Phase 3 — RunAutonomous crash-resume

**~3–4 days** (small revision upward to cover the session-lock primitive).

Three new pieces:

#### 1. Checkpoint events

Each turn boundary, the autonomous driver appends a synthetic `session.Event` to the event log as a checkpoint:

- `Author = "<binary>/autonomous"` where `<binary>` comes from `os.Executable()` (matches the convention used elsewhere — `core-agent/autonomous`, `scion-agent/autonomous`, etc.)
- `CustomMetadata` carries the structured payload: `{turn int, input_tokens int, output_tokens int, cost_usd float64, goal string, continuation_prompt string, stop_reason string}`
- `stop_reason` is empty for per-turn checkpoints, set to one of the `StopReason` values for the final checkpoint emitted when the loop exits (completed / max_turns / max_tokens / max_cost / wallclock / context_cancelled / retry_aborted)

Per-turn checkpoints fire *after* each turn completes. The final checkpoint fires on loop exit regardless of cause. ResumeAutonomous reads the latest checkpoint and either returns the terminal state immediately (if `stop_reason` is set) or continues from `turn + 1`.

#### 2. Resume API

```go
// ResumeAutonomous reads the most recent checkpoint event from the
// session's event log, reconstructs RunResult totals, and continues
// from the next turn. The build function receives the original
// sessionID so it can construct an agent that resumes the same
// session via agent.WithSession.
//
// If no checkpoint events exist for the session, the run starts from
// turn 0 with whatever event history the session already has loaded —
// "make this existing session autonomous from here" is a valid use.
//
// If the latest checkpoint has stop_reason set (terminal state),
// ResumeAutonomous returns that state immediately without
// constructing the agent or running any turns.
func ResumeAutonomous(ctx context.Context, build ResumeBuildFunc, ref SessionRef, opts ...AutonomousOption) (RunResult, error)

// ResumeBuildFunc is RunAutonomous's BuildFunc with sessionID added.
// The consumer's build implementation is expected to call
// agent.WithSession(ref.UserID, sessionID) so the constructed agent
// reuses the resumed session.
type ResumeBuildFunc func(extras []tool.Tool, sessionID string) (*Agent, error)

// SessionRef identifies the session to resume. Handle provides both
// the session.Service and the eventlog.Stream needed to look up
// checkpoints; the (AppName, UserID, SessionID) triple identifies
// which session within the database.
type SessionRef struct {
    Handle    *eventlog.Handle
    AppName   string
    UserID    string
    SessionID string
}
```

Checkpoint discovery filters by author **suffix** `/autonomous` rather than an exact author match, so a run started from `core-agent` can be resumed by `scion-agent` (or vice versa) without losing its checkpoint history. Hand-offs across processes are a real use case.

#### 3. Session lock

To prevent two concurrent ResumeAutonomous calls from clobbering each other's writes, eventlog ships a tiny lock primitive that uses the same database:

```go
// SessionLock holds an exclusive lease on (app, user, session) for
// the lifetime of the autonomous run. The lease is heartbeated every
// 5s; a lock is considered stale if its last heartbeat is older than
// 30s, allowing recovery from crashed processes.
type SessionLock struct{ /* ... */ }

func (h *Handle) AcquireLock(ctx context.Context, app, user, session string) (*SessionLock, error)
func (l *SessionLock) Release() error
```

Implementation: a tiny `agent_run_lock` table with primary key `(app_name, user_id, session_id)` plus `holder` (process identifier — pid + hostname), `acquired_at`, `heartbeat_at`. AcquireLock attempts an INSERT; on unique-constraint violation it checks `heartbeat_at` — if stale, steals the lock; otherwise returns a clear "session locked by <holder>" error. A background goroutine refreshes `heartbeat_at` while the run executes.

Both `RunAutonomous` (when called against a SessionRef) and `ResumeAutonomous` acquire the lock. Plain `RunAutonomous` calls without a SessionRef are unchanged — no lock taken (no shared session to protect).

#### Behavior summary

On resume:
- Acquire SessionLock; abort if held by another live process.
- Read `Stream.Since(0, ForSession(...))` and find the last event whose Author has suffix `/autonomous`.
- If found and `stop_reason != ""`: return reconstructed RunResult immediately, release the lock.
- If found with empty `stop_reason`: re-derive turn counter + token/cost totals; continue with `prompt = continuation_prompt`.
- If no checkpoint event at all: start from turn 0 with the session's existing history loaded.
- Honor all `AutonomousOption` budgets; they're evaluated against the cumulative resumed totals.
- Release the lock on exit (deferred).

#### Files

- `eventlog/lock.go` — `agentRunLockRow` GORM model + `AcquireLock`/`Release`/heartbeat goroutine
- `eventlog/lock_test.go` — acquire-twice-fails, stale-steal, heartbeat-refresh, release-clears
- `agent/autonomous.go` — checkpoint emission per turn + final checkpoint, `ResumeAutonomous`, `ResumeBuildFunc`, `SessionRef`
- `agent/autonomous_test.go` — resume-from-mid-run, resume-from-terminal-returns-final-state, no-checkpoint-starts-at-zero, cross-binary-author-match, lock-blocks-concurrent-resume, budgets-carry-forward
- `examples/autonomous-resume/main.go` — drives a small RunAutonomous against `--provider=scripted` with a tight turn cap, then ResumeAutonomous picks up where it left off (no actual process crash needed; the budget cap simulates the interruption)
- `docs/site/content/docs/library-api.md` — extend the autonomous section with a "Crash-resume" subsection
- `docs/eventlog-decisions.md` — Phase 3 implementation record

### Phase 4 — Subagent integration via custom runner (replaces existing subagents-plan)

**~5–7 days. Substantially refactors `docs/subagents-plan.md`.**

The existing plan wraps ADK's `tool/agenttool`, which creates a fresh in-memory session per subagent call. **Subagent events would be invisible to the parent's event log** — incompatible with the audit-log property the user wants.

This phase replaces that approach:

- New `tools/subagent.go` implements subagent-as-tool **without** `agenttool`.
- The subagent runs through ADK's runner with the **parent's `session.Service`** and **the parent's session ID**, but with `session.Event.Branch` set to `parent_branch + "/" + subagent_name`.
- ADK's branch system isolates conversation history along branch lines (per `session.Event` doc: "Branch is used when multiple sub-agent shouldn't see their peer agents' conversation history") — so the subagent doesn't see the parent's history, but its events still land in the same persistent log.
- Result: subagent events appear in the parent's event log, live, in order, distinguishable by `WithBranchPrefix` queries.

Library API (refresh of the prior plan):

```go
// Same shape as the prior plan, but now constructs without agenttool.
func tools.NewSubagentTool(opts SubagentOptions) (tool.Tool, error)

// Convenience on agent.
func agent.WithSubagents(agents []*agent.Agent) Option
```

Refresh `docs/subagents-plan.md` in the same milestone — strike the "wrap agenttool" sections, replace with the branch-scoped runner approach. The other decisions (default research-safe tools, depth cap, gate inheritance, etc.) are preserved.

This phase is its own commit-train; consumers using just Phases 1–3 don't need it.

## Critical files

**New:**
- `eventlog/eventlog.go` — Stream interface + Entry + Open + WithWatchInterval + WithGORMOptions
- `eventlog/sql.go` — GORM model `agentEventRow`, AutoMigrate registration, Append/Since/Watch implementations
- `eventlog/service.go` — `session.Service` wrapper that delegates to ADK's database.SessionService + writes the overlay row
- `eventlog/eventlog_test.go` — SQLite in-memory tests for all interface contracts
- `eventlog/service_test.go` — tests against ADK's session.Service interface contract
- `eventlog/postgres_test.go` (`//go:build pg_integration`) — Postgres integration smoke

**Modified (Phase 1):**
- `agent/agent.go` — add `WithSessionService` option; replace hardcoded `InMemoryService()` with the configured one (default unchanged)
- `agent/agent_test.go` — pin: default behavior unchanged; option override works

**Modified (Phase 2):**
- `agent/agent.go` — add `WithEventLog(stream, svc)` convenience that calls `WithSessionService` internally
- `cmd/core-agent/main.go` — add `--session-db=PATH` flag (SQLite path; empty = current in-memory behavior)
- `extras/scion-agent/main.go` — same flag (parity with cmd/core-agent)
- `extras/ax-agent/main.go` — same flag (parity)
- `docs/site/content/docs/library-api.md` — new `## Durable sessions and event log` section after `## Recording LLM turns`
- `docs/DESIGN.md` — short subsection under "Built-in tools" or new sibling: "Durable sessions and audit/replay event log." Cover: why two tables (ADK + overlay), why multi-driver via GORM, why polling for Watch (not native NOTIFY), seq-number semantics
- `README.md` — one bullet under Features: "Durable sessions + audit log — `eventlog.Open(...)` returns a SQLite/Postgres-backed `session.Service` plus a Stream with monotonic `seq`, `Since(seq)` replay, and `Watch(seq)` live-tail."

**Modified (Phase 3):**
- `agent/autonomous.go` — add checkpoint-event emission per turn; add `ResumeAutonomous` + `SessionRef` + `ResumeContext` types
- `agent/autonomous_test.go` — add resume tests
- `docs/site/content/docs/library-api.md` — extend the autonomous section with a "Crash-resume" subsection
- `examples/autonomous-resume/main.go` — new example showing the checkpoint+resume flow with SQLite

**Modified (Phase 4):**
- `docs/subagents-plan.md` — refresh per Phase 4 description above
- `tools/subagent.go` (new) — branch-scoped runner implementation
- Plus tests, docs, etc. per the refreshed subagent plan

## Reused pieces (not re-implementing)

- `google.golang.org/adk/session/database.NewSessionService` — durable session core, multi-driver
- `google.golang.org/adk/session/database.AutoMigrate` — schema setup
- `google.golang.org/adk/session.Service` interface — what we wrap
- `google.golang.org/adk/session.Event.Branch` — subagent hierarchy (Phase 4)
- `gorm.io/gorm` — already a transitive dep through ADK's database package
- `gorm.io/driver/sqlite` — likely already transitive; pin if not

## Tests

Phase 1:
- `TestAgent_DefaultUsesInMemoryService` — pin existing behavior
- `TestAgent_WithSessionService_OverridesDefault` — custom Service used by Agent.Run

Phase 2:
- `TestStream_AppendAssignsMonotonicSeq` — multiple appends, seq strictly increasing
- `TestStream_SinceReturnsTail` — given log of N events, `Since(K)` returns N-K events in order
- `TestStream_WatchBlocksUntilAppend` — Watch starts, no events; Append fires; Watch yields it; cancel ctx; Watch returns
- `TestStream_ForSessionFiltersOtherSessions` — two sessions in same DB, queries don't bleed
- `TestStream_WithBranchPrefixFilters` — events with Branch="a/b/c" match prefix "a", don't match prefix "z"
- `TestService_DelegatesCRUDToADK` — Create/Get/List/Delete all call through to inner ADK service; `AppendEvent` writes both tables
- `TestService_AppendEvent_TransactionAtomic` — simulate seq insert failure, assert ADK event is rolled back (or surface the consistency rule clearly)
- `TestOpen_AutoMigrateIdempotent` — Open twice on the same DB doesn't error

Phase 3:
- `TestRunAutonomous_EmitsCheckpointPerTurn` — count of checkpoint events == Turns
- `TestResumeAutonomous_ContinuesFromLastCheckpoint` — interrupt after 2 turns, resume, finish; total turns == 2 (interrupted) + new turns
- `TestResumeAutonomous_BudgetsCarryForward` — pre-crash totals count toward budget on resume
- `TestResumeAutonomous_TerminalStateIsFinal` — resuming a Completed run returns the stored RunResult, doesn't re-run

Phase 4 (subagent): per refreshed `docs/subagents-plan.md`.

## Verification

```bash
cd /home/user/projects/core-agent

# Phase 1
go test ./agent/...

# Phase 2
go test ./eventlog/... ./agent/...
go vet ./...
go build ./...
for s in dev/ci/presubmits/*; do bash "$s"; done

# Phase 2 smoke (no creds, real SQLite file):
go run ./examples/autonomous # current example; unchanged
go run ./cmd/core-agent --session-db=/tmp/session.db -p "hello"
sqlite3 /tmp/session.db "SELECT seq, author, branch FROM agent_eventlog ORDER BY seq;"
# Expected: rows for user input, model response (multiple partials),
# any tool calls/results, in seq order.

# Phase 3 smoke:
go run ./examples/autonomous-resume   # crashes mid-run, restarts
# Expected: second invocation picks up at the right turn, completes.

# Phase 4 verifications: in the refreshed subagent plan.
```

## Out of scope (deferred)

- **Native push for Watch (PostgreSQL LISTEN/NOTIFY, SQLite update_hook)** — polling is good enough for v1; add native push if subscriber latency or DB load becomes a real complaint.
- **Compaction / pruning of the event log** — auditors typically want full retention; if storage growth becomes an issue, ship a `eventlog.Prune(olderThan time.Time)` helper later.
- **Cross-session event log queries** (e.g. "every approval gesture across all runs in the last 24h") — add as need arises; the schema supports it (omit `ForSession` in the query) but no API surface today.
- **Encrypted-at-rest event log** — defer to filesystem/DB-level encryption.
- **Streaming subscribers over the network** — no gRPC/HTTP bridge for the event log in this milestone. Internal use only. Add an `extras/eventlog-server/` adapter later if a consumer wants AX-style remote tail.
- **Multi-writer coordination across processes** — this milestone assumes one writer per session at a time. Multiple readers are fine. If a consumer wants true multi-writer (two agents appending to the same session concurrently), revisit; SQLite makes this hard, Postgres makes it manageable.
- **Recording-eventlog unification** — keep both. Recording is the LLM-wire primitive; eventlog is the agent-event primitive. They're at different layers.

## Risk register

- **GORM's seq autoincrement portability.** SQLite and Postgres both support `INTEGER PRIMARY KEY AUTOINCREMENT` semantics through GORM, but the type details differ. Verify both before declaring multi-driver support.
- **Watch polling overhead.** 200ms default polling × N watchers × M sessions = potentially significant load. Document the tunable; if it becomes a problem, ship the native push path.
- **ADK schema evolution.** ADK's `events` table may change between minor versions. Our overlay table doesn't depend on column shape, just on `events.id` being stable. Worst case: re-pin the ADK version and migrate.
- **Subagent refactor (Phase 4) is a real architectural shift** from the existing subagent plan. Worth a re-read of that plan as part of Phase 4 to confirm nothing else regresses.

## When the deferred pieces become active

- **Native push for Watch** — when a subscriber complains about latency or DB load. Postgres path (LISTEN/NOTIFY) is the easier first move; SQLite update_hook needs CGO.
- **Network-streaming adapter** — when a second remote consumer wants AX-style live tail (Scion's approval channel, an external dashboard). Lives under `extras/eventlog-server/` analogously to `extras/scion-agent/`.
- **Multi-writer coordination** — when a real multi-process-per-session use case arrives. Probably means moving the seq column into a dedicated sequence object on Postgres + advisory locks; on SQLite, accepting the single-writer constraint.
