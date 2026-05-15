# Event log — implementation decisions log

A running record of design choices made while implementing `docs/eventlog-plan.md`. Each phase gets a section. Decisions are recorded as they're made; the design doc itself is updated only when something material changes.

## Phase 1 — Plug-in `session.Service`

Status: shipped on `main` (commit pending).

### What landed

- **`agent.WithSessionService(s session.Service) Option`** — the seam for swapping the default in-memory session backend with a durable one (or a test stub).
- **`(*Agent).SessionService() session.Service` accessor** — returns whatever Service the agent is using.
- **Default behavior preserved** — `agent.New(model)` with no option still constructs a fresh `session.InMemoryService()` per call, identical to prior semantics.
- **`agent/agent_test.go`** — five tests covering: nil-model rejection (preserved), default service is non-nil and per-instance, override is passed through by object identity, `WithSessionService(nil)` falls back to default rather than panicking, option order independence.
- **Package doc comment update** in `agent/agent.go` mentions the override path.

### Decisions made (with reasoning)

**Stored the service on the `Agent` struct (with public accessor) rather than only inside the constructor closure.**
The accessor is needed by the upcoming Phase 2 work (callers of `eventlog.Open(...)` will need to query the service for prior events) and useful for tests today. The cost is a single extra pointer field on `Agent`. No downside.

**`WithSessionService(nil)` falls back to the default rather than erroring or panicking.**
ADK's other With* options on this package treat nil/empty as "ignore" — keeping the convention. Also avoids surprise when a caller wires a Service from a function that returns nil on some configuration path.

**Default factory call (`session.InMemoryService()`) stays inside `New`, not pre-computed in `defaultOptions()`.**
`session.InMemoryService()` returns a fresh instance per call. If we cached it in `defaultOptions()`, every Agent built without an override would share state — a real bug. Constructing inside `New` (only when `o.sessionService == nil`) keeps the per-call freshness contract.

**No new package, no CLI flags, no docs site changes in Phase 1.**
The plan deliberately splits these into Phase 2 alongside the `eventlog/` package itself. Phase 1 is the smallest shippable seam — the option exists, but nothing in the repo wires durability through it yet. Library callers can already use `WithSessionService` to plug `database.NewSessionService(...)` from ADK directly if they want a quick durable backend before Phase 2 lands.

**Did not extend the `options` struct's TODO comment** about subagents.
That marker stays for the Phase 4 work; touching it here would be churn.

### What did NOT land in Phase 1 (deliberately)

- The `eventlog/` package — Phase 2.
- `WithEventLog(*eventlog.Handle)` convenience — Phase 2 (depends on the package existing).
- CLI flags `--session-db` / `--session-db-path` — Phase 2.
- ADK GORM driver dependency in `go.mod` — Phase 2 (no consumer of `database.NewSessionService` yet on the agent path).
- README / DESIGN / library-api doc updates — Phase 2 (small Phase 1 surface doesn't yet warrant user-facing docs; the changelog and the option's godoc cover it).

### Verification

```bash
go test ./agent/...        # 5 new tests + the existing autonomous suite, all pass
go vet ./...               # clean
go build ./...             # clean
for s in dev/ci/presubmits/*; do bash "$s"; done   # all 7 green
```

---

## Phase 2 — Event log substrate

Status: shipped on `main` (commit pending).

### What landed

- New `eventlog/` package (`eventlog/eventlog.go`, `eventlog/sql.go`, `eventlog/service.go`).
- **`eventlog.Open(ctx, dialector, opts...) (*Handle, error)`** — multi-driver via GORM. SQLite tested in-tree; Postgres/MySQL work the same way (caller supplies their dialector). Default WAL mode for SQLite, configurable via `WithSkipWAL`.
- **`Stream` interface**: `Append`, `Since(fromSeq, opts...)`, `Watch(fromSeq, opts...)`, `Close`. Iterators are `iter.Seq2[Entry, error]`. Watch polls at 200ms by default (configurable via `WithWatchInterval`).
- **`QueryOption` filters**: `ForSession(app, user, sess)`, `WithBranchPrefix(prefix)`, `WithAuthor(name)`, `WithLimit(n)`. Branch prefix accepts both `.` and `/` separators (ADK uses `.` in its docstring; we match either since we don't yet know which the eventual subagent runner will pick).
- **`Handle` bundles** the `Stream` and `session.Service` with a `Close()` that releases both. `Service` is a wrapper around ADK's `database.SessionService` that delegates Create/Get/List/Delete unchanged and intercepts AppendEvent to also write the overlay row.
- **`agent.WithEventLog(*eventlog.Handle)`** convenience option: wires `Handle.Service` as the agent's session service and stores the Handle on the agent (accessible via `Agent.EventLog()` for callers that want replay/watch without holding a separate reference). `WithEventLog(nil)` is a no-op.
- **CLI flags** `--session-db` (bool) and `--session-db-path` (string) on `cmd/core-agent` and `extras/scion-agent`. Default path `~/.<binary>/sessions.db` derived from `os.Executable()` so forks/adapters get their own directory automatically. Either flag enables.
- **Tests**: 10 in `eventlog/eventlog_test.go` + 2 new in `agent/agent_test.go`. Cover Append seq monotonicity, Since tail / filters / limits, Watch block-then-yield, ForSession isolation, branch-prefix matching across separators, ADK CRUD pass-through, duplicate-event-ID rejection, closed-stream rejection, agent.WithEventLog wiring.
- **Docs**: `README.md` feature bullet, `docs/DESIGN.md` new section ("Durable sessions and audit/replay event log"), `docs/site/content/docs/library-api.md` new section ("Durable sessions and audit log"), `docs/eventlog-plan.md` updated with shipped status (decisions log = this file).

### Decisions made (with reasoning)

**Pure-Go SQLite via `glebarez/sqlite` (wrapping `modernc.org/sqlite`).**
Per the user's earlier choice. Keeps `CGO_ENABLED=0` builds working; ~10MB binary growth is acceptable for the value (durable sessions out of the box). For workloads where SQLite throughput matters more than CGO cleanliness, callers swap the dialector — the API doesn't change.

**Wrap ADK's `database.SessionService` rather than implement `session.Service` from scratch.**
ADK already handles the events / sessions / app_states / user_states tables with multi-driver `AutoMigrate`. Reimplementing would duplicate ~500 lines of state-management plumbing for no gain. Cost: we open a second `*gorm.DB` connection for our overlay table since ADK doesn't expose its connection. SQLite handles concurrent connections cleanly with WAL; other drivers similarly tolerate it.

**Two-write consistency, not single-transaction atomicity.**
Spanning a transaction across ADK's connection and ours would require reflection into ADK's private `*gorm.DB` or rebuilding the entire service. We accept eventual consistency: ADK writes first; if the overlay write fails after, the caller sees an error and can retry. The overlay's unique index on `event_id` makes the retry safe (idempotent insert). A reconciliation helper that finds events without overlay rows is a follow-up if a consumer reports drift.

**WAL mode by default for SQLite (`PRAGMA journal_mode=WAL`).**
Enables concurrent readers alongside the single writer. `WithSkipWAL()` disables for in-memory and read-only setups. Best-effort: a PRAGMA failure is silent because some SQLite distributions reject WAL on `:memory:`.

**Polling-based `Watch` at 200ms default.**
Native push (PostgreSQL LISTEN/NOTIFY, SQLite update_hook) would be lower-latency but adds driver-specific code. Polling is portable and the interval is tunable. Document the trade-off; revisit when a consumer reports actual latency pain.

**Default `--session-db` to off; opt-in.**
Per the user's earlier choice. Auto-defaulting durability would change existing CLI behavior and create files users may not expect. The flag is a single character (`--session-db`) so the friction is low.

**Default path `~/.<binary>/sessions.db` from `os.Executable()`.**
Per the user's earlier choice. Detects the running binary at runtime so forks (`scion-agent`, `ax-agent`, custom builds) each land in their own directory without per-binary configuration. Falls back to a hardcoded name if `os.Executable()` fails.

**Single DB file holding all sessions, not per-session DBs.**
Per the user's confirmation. The audit-log story (cross-session app/user state, branch-prefix queries spanning subagent hierarchies) practically forces this. SQLite's single-writer constraint is mitigated by WAL; the rare workload that genuinely needs concurrent writers points to Postgres, same `eventlog.Open` API.

**Branch prefix matches both `.` and `/` separators.**
ADK's docstring uses `agent_1.agent_2.agent_3`; the autonomous-plan's subagent description used `parent/child`. Until Phase 4 ships and pins the actual separator, the prefix matcher accepts either to avoid having to refactor the query API later. Cost: a slightly more permissive match. Acceptable.

**`Stream.Append` is on the public interface even though most callers go through `Service.AppendEvent`.**
The Stream wrapper handles overlay rows; the Service handles ADK delegation + overlay. Exposing Append directly lets advanced consumers write events into the log without going through ADK's session machinery — useful for testing and for the future subagent runner that may want to replay events into a child branch.

**No `eventlog.OpenSQLite(path)` helper.**
The CLI binaries import `glebarez/sqlite` and call `eventlog.Open(ctx, sqlite.Open(path))` directly. Adding an `OpenSQLite` helper would tie the eventlog package to a specific driver, complicating the multi-driver story. The two-line import is fine.

### What did NOT land in Phase 2 (deliberately)

- **`extras/ax-agent` flag wiring.** ax-agent lives only on the `axplore` branch, not `main`. Will land when axplore is rebased onto current main (Phase 2 included). Tracked as a follow-up.
- **`RunAutonomous` checkpoint events + `ResumeAutonomous`** — Phase 3.
- **Subagent integration** — Phase 4.
- **Native push for Watch** — deferred per the plan.
- **Compaction / pruning of the event log** — deferred per the plan.

### Verification

```bash
go test ./eventlog/... ./agent/...   # 10 + 7 tests pass
go vet ./...                         # clean
go build ./...                       # clean
for s in dev/ci/presubmits/*; do bash "$s"; done   # all green

# End-to-end smoke (real SQLite, echo provider, no creds):
DB=$(mktemp -u --suffix=.db)
go run ./cmd/core-agent --provider=echo --session-db --session-db-path="$DB" -p "hello"
# Then inspect the DB to confirm both ADK tables and agent_eventlog
# are populated with the user input + model response.
```

## Phase 3 — RunAutonomous resume

Status: shipped on `main` (commit pending).

### What landed

- **`eventlog.WithAuthorSuffix(suffix)`** QueryOption — suffix-match on the events table's author column. Used by ResumeAutonomous so checkpoints emitted by one binary (`core-agent/autonomous`) can be discovered by another (`scion-agent/autonomous`). Implementation is `LIKE '%suffix'`.
- **`eventlog.SessionLock`** primitive — `eventlog/lock.go`. New `agent_run_lock` table with composite primary key `(app_name, user_id, session_id)` plus holder identifier (`<host>/<pid>/<rand>`), `acquired_at`, `heartbeat_at`. `Handle.AcquireLock(ctx, app, user, session)` returns a `*SessionLock` whose background heartbeat goroutine refreshes `heartbeat_at` every 5 seconds. Stale-after-30-seconds steal logic recovers from crashed processes; `Release` is idempotent and uses a `WHERE holder = ourID` predicate so a race-stolen successor isn't accidentally deleted. Seven tests in `eventlog/lock_test.go` (acquire-twice-blocks, different-sessions-don't-block, release-allows-reacquire, release-idempotent, stale-gets-stolen, heartbeat-keeps-fresh, release-doesn't-delete-successor).
- **Checkpoint events** (`agent/checkpoint.go`): `checkpointPayload` struct + `emitCheckpoint(ctx, agent, payload)` helper. Author is `<binary>/autonomous` (from `os.Executable()`); ID is hex-encoded crypto/rand prefixed with `checkpoint-`. CustomMetadata holds `{turn, input_tokens, output_tokens, cost_usd, goal, continuation_prompt, stop_reason, done_detail, final_text}`. No-op when the agent has no event log wired (in-memory sessions can't survive a process restart, so the checkpoint would be lost anyway).
- **Per-turn checkpoint emission** added to `RunAutonomous`'s loop: fires after every clean turn (non-done, non-error). Final checkpoint emitted on every loop-exit path — completed, max_turns, wallclock, retry_aborted, context_cancelled — with `stop_reason` set.
- **`agent.UserID()`, `agent.SessionID()`, `agent.AppName()`** accessors — needed so the checkpoint emitter can call `session.Service.Get(...)` to fetch the live `session.Session` for AppendEvent. Stored on the `Agent` struct via the constructor.
- **`agent.ResumeAutonomous(ctx, build, ref, opts...)`** in `agent/resume.go` — the consumer-facing entry. New types `ResumeBuildFunc`, `SessionRef`. Acquires the session lock; reads the latest `/autonomous`-suffix checkpoint; short-circuits with the terminal state when `stop_reason == "completed"`; otherwise rebuilds RunResult totals and continues from the next turn. Releases the lock on exit (deferred).
- **Tests** in `agent/resume_test.go`: requires-build / requires-handle / requires-session-id, terminal-completed-returns-immediately (asserts LLM is never invoked), continues-from-mid-run (turns + tokens carry forward), budgets-carry-forward (max_turns already hit → no new turns), no-checkpoint-starts-at-zero, lock-blocks-concurrent. Plus `TestRunAutonomous_EmitsCheckpointPerTurn` against the agent test stubLLM.
- **Example** `examples/autonomous-resume/main.go` — Phase 1 runs 2 turns capped at MaxTurns(2); Phase 2 resumes against the same SQLite event log and completes the task on the next turn. End-to-end with no credentials via scripted mock provider.
- **Docs**: `docs/site/content/docs/library-api.md` extended with a "Crash-resume" subsection under "Autonomous runs" + a "Session lock" subsection under "Durable sessions and audit log".

### Decisions made (with reasoning)

**Only `StopReasonCompleted` triggers the resume short-circuit.**
The first draft short-circuited on any non-empty stop_reason. Caught in test: a Phase-1 run hitting `MaxTurns` would emit a final checkpoint with `stop_reason="max_turns_exceeded"`, and the resume would return that immediately rather than continuing. That's wrong — budget-exhausted runs are interruptions, not terminations; the consumer is supposed to be able to pass a bigger budget on resume. The fix scopes the short-circuit to `Completed` (model called done). Other stop reasons (`max_turns`, `max_tokens`, `max_cost`, `wallclock`, `retry_aborted`, `context_cancelled`) just provide carryover totals.

**`ResumeBuildFunc` is a separate type, not a unification with `BuildFunc`.**
Per the user's choice (option b in the open-questions discussion). Resume needs the session ID at construction time so the agent rejoins the right session via `agent.WithSession`. A separate type keeps RunAutonomous's existing signature unchanged. Cost: consumers writing both have two near-identical build functions; we accept that.

**Plain `RunAutonomous` does NOT acquire the session lock.**
The plan was ambiguous; I made a call. Locks are mainly to prevent concurrent ResumeAutonomous-vs-ResumeAutonomous (and ResumeAutonomous-vs-anything) races. Plain RunAutonomous starting fresh on a new session ID has no race partner. If a consumer concretely wants RunAutonomous to also acquire locks (e.g., to detect "did I already start this run from another process"), we'll add it as an option. Document the constraint.

**Cross-binary resume via `WithAuthorSuffix("/autonomous")`.**
Per the user's choice. The author is `<binary>/autonomous` derived from `os.Executable()`. Suffix matching means a run started under `core-agent` can be resumed under `scion-agent` or `ax-agent` without losing its checkpoint trail. SQL `LIKE '%suffix'` works across SQLite/MySQL/Postgres.

**No-checkpoint resume = turn-0 start.**
Per the user's choice. Useful for "take over this existing session and make it autonomous" scenarios. Not an error.

**Heartbeat: 5s interval, 30s staleness window.**
Standard ratio (6× cushion). Tunable in code; not exposed as an option in v1 because nobody has a use case yet. The conditional UPDATE (`WHERE holder = ourID`) means a long-paused process whose lease was stolen silently loses subsequent heartbeat updates — graceful degradation rather than confusing "ghost lease" behavior.

**Holder ID format: `<host>/<pid>/<rand>`.**
Hostname + PID identifies the process for diagnostics; 4 random bytes guard against reused PIDs (process A exits, process B starts with the same PID). Surfaces in `ErrSessionLocked` error messages so operators can find the holding process.

**Checkpoint-id prefix `checkpoint-`.**
ADK's events table uses the event's `ID` field as the storage primary key, so manually-constructed checkpoint events need explicit IDs. The `checkpoint-<hex>` format makes them visually distinct from runner-emitted event IDs (which are typically timestamp-based per ADK conventions). Hex of 16 random bytes for collision avoidance.

**`emitCheckpoint` is a no-op when the agent has no event log.**
Avoids spamming the InMemoryService with checkpoints that get dropped on process exit anyway. The downside: a consumer that uses `agent.WithSessionService(customDurableService)` instead of `agent.WithEventLog(handle)` won't get checkpoints — they'd need to wire the eventlog Handle explicitly. Acceptable for v1; document if it surprises someone.

**ADK logs "Event from an unknown agent: <binary>/autonomous" for each checkpoint.**
Cosmetic noise — ADK's runner sees our checkpoint events with an Author it doesn't recognize as a registered agent. Not an error; we don't try to suppress it. Future: investigate whether ADK exposes a way to register synthetic authors so the log goes away.

### What did NOT land in Phase 3 (deliberately)

- **Pause / resume mid-run.** The orchestrator-driven pattern (Scion, AX) covers it naturally — standalone semantics need more design.
- **Streaming subscriber for live tail of a running RunAutonomous.** `Handle.Stream.Watch(fromSeq)` already exists; consumers can wire their own subscribers without core-agent changes.
- **Lock acquisition for plain RunAutonomous.** See "decisions" above.
- **Non-default heartbeat / staleness intervals as Options.** No consumer asked.
- **Reconciliation tool for sessions whose ADK events succeeded but whose overlay rows failed.** Same eventual-consistency caveat as Phase 2; revisit if a consumer reports drift.

### Verification

```bash
go test ./agent/... ./eventlog/...    # 7 lock tests + 7 resume tests + carried-forward Phase 1/2 tests, all pass
go vet ./...                          # clean
go build ./...                        # clean
for s in dev/ci/presubmits/*; do bash "$s"; done   # all green

# End-to-end smoke (no creds, real SQLite):
go run ./examples/autonomous-resume
# Expected: Phase 1 hits max_turns_exceeded after 2 turns;
# Phase 2 resumes, runs 1 more turn, completes with reason=completed
# done_detail="resumed and finished".
```

## Phase 4 — Subagent runner refresh

Status: shipped on `main` (commit pending).

### What landed

- **`agent.NewSubagentTool(opts SubagentOptions) (tool.Tool, error)`** in `agent/subagent.go` — wraps an `*agent.Agent` as a tool the parent's model can call. Does NOT use ADK's `tool/agenttool`; runs the inner agent through its own ADK runner against a session.Service that injects `Branch` on every appended event.
- **`agent.WithSubagents([]*Agent) Option`** — convenience that registers each agent as a subagent tool. Resolved at the end of `New()` so it captures the parent's final session.Service + (app, user, session) triple.
- **`branchInjectingService`** — internal `session.Service` wrapper. CRUD pass-through; `AppendEvent` stamps `Branch` on events whose Branch is empty, preserves any pre-set branch (so nested subagents keep their deeper labels).
- **`agent.Agent.AgentName()`, `agent.Agent.Tools()`** accessors — needed for tool-name derivation and test introspection.
- **Depth context value** — `subagentDepthKey{}` carries recursion depth through `context.Context`. `CurrentSubagentDepth(ctx)` reads it; the tool handler refuses with a clear error message at depth >= MaxDepth (default 2).
- **Tests** in `agent/subagent_test.go` cover requires-Inner, requires-ADK-agent, defaults-name-to-Inner, name+description overrides, branch wrapper stamps + preserves, CRUD delegation, composeBranch matrix, depth context, WithSubagents registers tools / nil entries / order independence.
- **`examples/with-subagent/`** — end-to-end demo with two scripted-mock providers: parent calls `research`, subagent answers, parent emits a final summary. Inspects the audit log to show the branch-tagged events.

### Decisions made (with reasoning) — including a real architectural pivot from the plan

**Subagent runs in a derived session row, not the parent's.**
The plan said "the subagent runs through ADK's runner with the parent's session.Service and the parent's session ID, but with session.Event.Branch set to <parent>.<this>." Implementation surfaced a real bug: ADK's database session service has optimistic-concurrency checking via `last_update_time`. When the parent's outer runner is mid-stream and dispatches a subagent tool call, the subagent's runner writes to the session row — advancing `last_update_time`. When the parent's outer runner resumes and tries to AppendEvent, ADK rejects with `"stale session error: last update time from request (T0) is older than in database (T1)"`. The fix: subagent uses a derived session ID (`<parent>:sub:<branch>`) so the two runners write to different session rows. The events still land in the same database; audit queries find the subagent via `WithBranchPrefix("research")` across sessions.

The trade-off: queries scoped to `ForSession("parent-session")` no longer return subagent events. Consumers run two queries (parent session + branch-prefix across sessions) or omit ForSession. The decisions doc documents this; the example demonstrates both query shapes.

**Subagent code lives in `agent/`, not `tools/`.**
The plan said `tools/subagent.go` to match the existing tool wrapping pattern (LifecycleTool, ask_user). But subagent semantics are inherently agent-shaped — the `Inner *agent.Agent` reference is core to the API, and putting it in `tools/` would force either a circular import (agent → tools → agent) or splitting types across packages. `agent/subagent.go` keeps everything coherent; the `agent.WithSubagents` convenience is the natural shape.

**Branch separator: `.` (dot), matching ADK's docstring convention.**
ADK's `session.Event.Branch` docstring says `agent_1.agent_2.agent_3`. Confirmed by reading `internal/llminternal/contents_processor.go` which uses `strings.HasPrefix(invocationBranch, event.Branch+".")` for the LLM-request branch filter. The earlier eventlog `WithBranchPrefix` accepts both `.` and `/` (defensive — we hadn't pinned the separator yet). Phase 4 standardizes on `.`.

**ParentService / ParentAppName / ParentUserID / ParentSessionID on `SubagentOptions`.**
Public fields rather than internal — exposes the override points so consumers who construct subagent tools directly (not via WithSubagents) can wire shared session storage themselves. WithSubagents fills these in automatically; callers using NewSubagentTool standalone leave them empty and the subagent's own session.Service / triple is used.

**Subagent `AppendEvent` only stamps Branch when it's empty.**
A nested subagent invoked from inside another subagent will have a deeper branch label set by the inner-most wrapper. The outer wrapper must not overwrite it. The "stamp empty only" rule preserves the hierarchy.

**Depth cap default: 2.**
Same as the original subagent plan. A subagent calling another subagent calling another is the maximum nesting before things get hard to reason about. Override via `MaxDepth`.

**No filtering of subagent's input contents.**
Per ADK's contents-processor branch filter, when the subagent's runner builds its first LLM request, it includes events from the parent's session row whose Branch matches `subagent_branch` or is a prefix of it. Because we use a separate session row entirely, the subagent sees only its own session — fresh per call, no parent history bleeding in. This actually delivers context isolation more strongly than the plan's "shared session with branch filter" would have.

### What did NOT land in Phase 4 (deliberately)

- **Default research-safe tools** (read_file, list_dir, todo) for the subagent. The original plan suggested this — Claude Code's pattern. We didn't add it; the inner agent's tool list is whatever the consumer constructs it with. Easy to add in a follow-up if a CLI wants it.
- **`--enable-subagent` CLI flag.** Library-only feature for v1. The CLI doesn't auto-construct subagents.
- **Cross-session audit queries** that span parent + derived sub-sessions in one go. `WithBranchPrefix` across sessions is the workaround; a `WithSessionTree(parentID)` option could land in a follow-up.
- **Token / cost rollup** from subagent runs into the parent's `usage.Tracker`. The subagent's runner has its own internal tracking; surfacing it back through the tool result is non-trivial. Defer.
- **`agent.NewSubagentTool` in `tools/`** package per the original plan. See "decisions" for why agent/.
- **Refresh of `docs/subagents-plan.md`.** The old plan documented an `agenttool`-wrapped design; now superseded. Worth a follow-up to either delete or rewrite that doc to reflect the shipped design — the design has fundamentally changed.

### Verification

```bash
go test ./agent/... ./eventlog/...   # all pass (existing + new subagent tests)
go vet ./...                         # clean
go build ./...                       # clean
for s in dev/ci/presubmits/*; do bash "$s"; done   # all green

# End-to-end smoke (no creds):
go run ./examples/with-subagent
# Expected: parent calls research, subagent returns its answer,
# parent emits final summary, audit log shows parent events under
# session "parent-session" and subagent events under branch="research"
# (in derived session "parent-session:sub:research").
```
