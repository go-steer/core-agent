# Session resume on daemon restart

Design doc for the v2.5 follow-up to multi-session (#162 / `docs/multi-session-design.md`): sessions created via `POST /sessions` survive daemon restarts and resume transparently on the operator's next request.

**Status:** shipped in v2.5 (2026-07-01). Implemented across four merged PRs: ε.1 [#182](https://github.com/go-steer/core-agent/pull/182) (SessionACLStore + persistence), ε.2 [#183](https://github.com/go-steer/core-agent/pull/183) (SessionResumer + lazy resume + singleflight), ε.3 [#184](https://github.com/go-steer/core-agent/pull/184) (idle eviction sweep + cancel-on-evict), ε.4 (docs + smoketest + status flip). Operator-facing reference: `docs/site/content/docs/reference/multi-session.md` §"Session resume (v2.5+)". Tracking issue: [#178](https://github.com/go-steer/core-agent/issues/178).

## Motivation

The multi-session daemon shipped in v2.4 (α.1 + α.2 + β + γ + δ + the on-demand-creation spike #171) made `POST /sessions` real: each authenticated caller creates and owns sessions, isolation invariants hold, audit logs thread caller identity. But sessions live ONLY in the in-memory `attach.SessionRegistry`. When the daemon restarts — for config changes, image upgrades, crash recovery, K8s pod replacement — the registry resets to empty. Eventlog rows persist; the entries that connect them to live `*agent.Agent` instances do not.

Symptoms today:

- An operator's TUI reconnecting after a daemon restart gets HTTP 404 on `/sessions/<sid>/events` (the session ID was in the prior registry, not the new one).
- The TUI's reconnect loop retries forever (separate UX bug filed as [core-tui#51](https://github.com/go-steer/core-tui/issues/51)), but even with a clean 404 surface the operator has no recovery path other than relaunching with `--new-session` and losing all prior session context.
- Shared-session deployments (a Slack channel accumulating multi-day context, a Cloud Run pod restarted nightly by the platform team) are unworkable: every restart wipes operator state.

This is the gap that keeps multi-session from being production-grade. The conversation history is already durable in SQLite (`agent_eventlog` + ADK `events` tables); ownership / ACL state is the missing piece, and the resume orchestration is the missing primitive.

## Goals

- **Session continuity across daemon restarts.** A factory-created session survives restart. An operator's TUI reconnect Just Works — the same SessionID resolves; the same conversation history is visible; the same ACL applies.
- **No eager startup cost.** The daemon does NOT scan every session in the DB at startup. Resume is lazy — pay the cost when a session is first touched after restart, not for every session ever created.
- **No memory bloat.** Cold sessions don't sit in the registry indefinitely. An operator-configurable idle eviction removes them from memory; the next touch re-resumes from disk.
- **Backward compatibility.** Single-user / pre-multi-session deployments see zero behavior change. The startup-time agent's lifecycle is unchanged; only the multi-session-enabled path adds the resume layer.
- **Atomic ACL persistence.** When `RegisterOwned` runs, the ACL state is durably persisted in the same transaction as the registry insertion — no window where a session exists without its ACL.

## Non-goals (v2.5)

- **Auto-recovering an in-progress turn at restart.** If the daemon crashed mid-turn, the in-flight tool call is lost. The session resumes to the state visible in the eventlog; whatever the agent was doing at the moment of crash is gone. (ADK's `session.Service` will naturally pick up at the last committed event.) Turn-boundary semantics for crash recovery is a substantive separate concern; defer.
- **Cross-daemon session migration.** Sessions live in the daemon process that created them. Resuming on a different host is out of scope per `docs/multi-session-design.md` §"Non-goals" and remains so.
- **`DELETE /sessions` / hard-delete semantics.** Per design doc OQ #3 (deferred): soft-delete + sweep tool is the eventual plan. Resume's eviction is a sibling concept ("evict from memory; keep on disk") but doesn't itself delete anything from the DB. Hard-delete tracked separately.
- **ACL mutation API (`PATCH /sessions/<sid>/acl`).** v2.4 ships with ACLs set at session creation, no mutation. This design persists the same shape but doesn't add mutation; that's a follow-up.
- **Cluster-wide session registry.** A central registry shared across multiple daemons is what cross-daemon migration would enable. Per the previous bullet, out of scope.

## Conceptual model

### Persistence primitive: `agent_session_acl` table

A new GORM table on the eventlog database (the same SQLite/Postgres/MySQL file the eventlog already uses) holds the ACL state for every owned session. One row per session triple. Written transactionally with the session's first event so there's no window where an `agent_eventlog` row exists without an owning ACL row.

```go
// pkg/attach (or pkg/session-resume — TBD; see Open questions §6)

type sessionACLRow struct {
    AppName     string    `gorm:"not null;primaryKey"`
    UserID      string    `gorm:"not null;primaryKey"`
    SessionID   string    `gorm:"not null;primaryKey"`
    Owner       string    `gorm:"not null;index:idx_session_acl_owner"`
    // JSON-encoded []string for slice fields — keeps the table
    // primary-key shape simple and queryable without a join table.
    // Slices are operator-edited via a future ACL mutation API
    // (out of scope here); querying "who can see this session" at
    // resume time means deserializing the JSON, which is cheap for
    // the small slice sizes we expect (single-digit identities).
    ViewersJSON      string    `gorm:"type:text"`
    ContributorsJSON string    `gorm:"type:text"`
    CreatedAt        time.Time `gorm:"not null"`
    LastTouchedAt    time.Time `gorm:"not null;index:idx_session_acl_last_touched"`
}

func (sessionACLRow) TableName() string { return "agent_session_acl" }
```

**Why a sidecar table and not eventlog metadata:**
- Eventlog metadata is per-event attribution (caller / proxy_by). ACL is per-session state — different granularity, different mutation cadence.
- Resume needs a fast "give me all rows where SessionID = X" query. A dedicated indexed table gives O(1) lookup; scanning the eventlog for a synthetic "session-created" event would be O(N) over the whole log.
- The schema can evolve independently — ACL fields can grow without re-flowing eventlog metadata.
- It composes cleanly with future `PATCH /sessions/<sid>/acl` (just update the row).

**Indexes**: composite primary key on `(AppName, UserID, SessionID)` for the lookup-by-triple path; secondary indexes on `Owner` (for "list sessions I own" queries) and `LastTouchedAt` (for the eviction sweep).

### Resume primitive: `attach.SessionResumer`

The pluggable callback that turns a "session in DB but not in memory" lookup miss into a registered entry. Construction-time wiring lives in the daemon (cmd/core-agent); the attach server consults it from `Lookup` / `LookupSingle` when the in-memory registry doesn't have the requested triple.

```go
// pkg/attach (new — sibling to SessionFactory)

// SessionResumer reconstructs a session that exists on disk but
// not in the current daemon's in-memory registry. Called by
// Registry.Lookup / LookupSingle on miss when a SessionResumer is
// configured. Implementations read the persisted ACL row to learn
// the original Owner Caller, then invoke the daemon's
// SessionFactory closure with that Caller + the explicit
// SessionID. The returned Registrant is added to the in-memory
// registry under the original ACL.
type SessionResumer interface {
    Resume(ctx context.Context, app, user, sid string) (Registrant, auth.SessionACL, error)
}
```

The implementation in `cmd/core-agent`:

```go
// In cmd/core-agent/multi_session.go (sibling to buildSessionFactory).

func buildSessionResumer(deps sessionFactoryDeps) attach.SessionResumer {
    return &sessionResumer{
        factory: deps,
        // The shared sidecar table connection; reuses eventlog's DB.
        store: deps.aclStore,
    }
}

func (r *sessionResumer) Resume(ctx, app, user, sid) (attach.Registrant, auth.SessionACL, error) {
    // 1. Look up ACL row by triple.
    row, err := r.store.Get(ctx, app, user, sid)
    if err != nil { return nil, auth.SessionACL{}, err }  // sql.ErrNoRows → "session not found"
    // 2. Materialize the original Caller from the row.
    caller := auth.Caller{Identity: row.Owner}
    // 3. Reproduce the agent using the same factory closure, but
    //    with the explicit SessionID (vs. minting a new one).
    ag, err := r.factory.reproduceAgent(ctx, caller, sid)
    if err != nil { return nil, auth.SessionACL{}, err }
    return ag, row.ACL(), nil
}
```

`reproduceAgent` mirrors `buildSessionFactory`'s closure but accepts an explicit SessionID instead of minting one. ADK's `session.Service` + the durable eventlog automatically reattach the prior conversation history when you point them at the same triple.

### Concurrency primitive: per-triple resume lock

Two TUIs reconnecting to the same session at the same time after a restart would race the resume path — both miss the registry, both call the resumer, both construct an agent, the second `RegisterOwned` fails with `ErrSessionExists`. To dedupe:

```go
// In pkg/attach.SessionRegistry.

// resumeFlight tracks in-flight resume attempts per triple so
// concurrent Lookup misses share one resume call (singleflight
// pattern). Per-triple key avoids contention across sessions.
resumeFlight singleflight.Group
```

`Lookup` on miss becomes:

```go
func (r *SessionRegistry) Lookup(app, sid string) (*Entry, error) {
    // Existing fast path: check the in-memory map.
    if entry, ok := r.findInMemory(app, sid); ok {
        return entry, nil
    }
    if r.resumer == nil {
        return nil, ErrSessionNotFound
    }
    // Singleflight per triple: concurrent misses for the same
    // session collapse to a single resumer call.
    key := app + "/" + sid
    result, err, _ := r.resumeFlight.Do(key, func() (any, error) {
        return r.resumeAndRegister(ctx, app, sid)
    })
    if err != nil {
        return nil, err
    }
    return result.(*Entry), nil
}
```

### Lifecycle primitive: idle eviction sweep

Once sessions persist, the registry grows unboundedly without an eviction policy. Solution: a background goroutine sweeps the registry on an interval; entries idle past `attach.multi_session.session_idle_timeout` are removed from memory (NOT deleted from disk). The next touch lazily re-resumes them.

```go
// In pkg/attach.SessionRegistry, started by Server.Bind.

func (r *SessionRegistry) sweepIdle(ctx context.Context, idleAfter time.Duration) {
    ticker := time.NewTicker(idleAfter / 4)  // sweep at 1/4 the idle window
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done(): return
        case <-ticker.C:
            cutoff := time.Now().Add(-idleAfter)
            r.evictBefore(cutoff)
        }
    }
}
```

Each registry entry tracks `LastTouchedAt` (updated on every Lookup hit, every Inject, every event broadcast). Evicting an entry removes it from memory + persists the updated `LastTouchedAt` to the ACL row so the next sweep doesn't immediately re-evict if the session re-resumes briefly.

**Per-session goroutine cleanup on evict**: the per-session wake loop spawned by `buildSessionFactory` (`runSessionWakeLoop` in `cmd/core-agent/multi_session.go`) needs to exit cleanly when the session is evicted. The lock-free path: pass a per-session cancel function to the registry; the registry calls it on evict; the wake loop's `<-ctx.Done()` branch fires.

## Resume flow (end-to-end)

Sequence for a TUI reconnecting after daemon restart:

```
1. Daemon starts; SessionRegistry is empty; SessionResumer is wired.

2. Operator's TUI sends GET /sessions/<sid>/events.

3. Handler calls h.reg.LookupSingle(sid):
   a. byTriple miss.
   b. Singleflight.Do(sid) → resumeAndRegister:
      i.   sessionResumer.Resume(ctx, app, user, sid)
      ii.  aclStore.Get(app, user, sid) → row{Owner=alice@..., ACL=...}
      iii. reproduceAgent(ctx, alice, sid) → *agent.Agent constructed
           with the same factory shape as new sessions but with
           an explicit SessionID (so ADK reattaches the existing
           conversation history from the eventlog).
      iv.  Registry.registerWithACL(ag, row.ACL()) → new *Entry.
      v.   Spawn the per-session wake loop (same as factory does).
      vi.  Return (entry, nil).
   c. Caller sees a normal Lookup hit.

4. Authorize alice against the resumed ACL → 200 OK.

5. Stream starts; SSE events flow. Prior conversation history is
   visible because ADK's session.Service reads the same DB.

6. Operator types a new prompt. Inject queues; wake loop picks it
   up; agent.Run executes; events broadcast normally.

7. Session continues as if the daemon never restarted.
```

Total operator-visible latency cost on resume: one DB query + one `agent.New` call (~50ms typically) on the first reconnect after restart. Subsequent requests hit the in-memory entry directly.

## Per-substrate impact

### `pkg/attach.SessionRegistry`

- Add `resumer SessionResumer` field (optional; nil means no resume).
- Add `resumeFlight singleflight.Group` for dedup.
- Modify `Lookup` / `LookupSingle` to consult resumer on miss.
- Add per-entry `LastTouchedAt time.Time`; update on every Lookup hit.
- Add `evictBefore(t)` sweep entry point + background sweep goroutine wiring.
- Add `Unregister` callback that signals per-session cancel func (added separately on the Entry).

### `pkg/attach.SessionACLStore` (new)

- GORM-backed CRUD over `agent_session_acl` table.
- `Put(ctx, row)` — upsert. Written transactionally with `RegisterOwned`.
- `Get(ctx, app, user, sid)` — lookup for the resumer.
- `Delete(ctx, app, user, sid)` — for future hard-delete.
- `Touch(ctx, app, user, sid, when time.Time)` — bump LastTouchedAt without rewriting the rest.
- `ListByOwner(ctx, owner string)` — for "show me all sessions I own" UX.
- `ListVisibleTo(ctx, caller auth.Caller)` — returns every ACL row the caller can `SessionRead` (Owner / Viewer / Contributor / Admin-sees-all). Powers `GET /sessions` for the persisted-but-evicted half of the list (the in-memory half comes from `Registry.List()`; the two are unioned + deduped by the handler).

### `pkg/eventlog`

- Existing `Open` adds `agent_session_acl` to the AutoMigrate list alongside `agent_eventlog`.
- New `Handle.SessionACL` field exposing the `SessionACLStore` (so cmd/core-agent can pass it through to attach's resumer).

### `cmd/core-agent`

- Build the `SessionACLStore` from the eventlog handle.
- Pass it into `sessionFactoryDeps` so both the factory (writes on `RegisterOwned`) and the resumer (reads on `Lookup` miss) share the same connection.
- Wire `attach.NewServer` with `Options.SessionResumer = buildSessionResumer(deps)`.

### `pkg/attach.SessionFactory` (existing) — modest extension

- `RegisterOwned` becomes a 2-step transaction: insert into byTriple + upsert into `agent_session_acl`. If either fails, both roll back. Already wrapping in a mutex; add the DB write inside the same critical section.

## Config surface

Two new fields under `attach.multi_session`:

```jsonc
{
  "attach": {
    "multi_session": {
      "enabled": true,
      // ... existing fields ...

      // Whether to enable session resume from the persisted ACL
      // table on Lookup miss. Default: true when multi_session is
      // enabled. Set to false for ephemeral / test deployments
      // where session continuity isn't wanted.
      "session_resume": true,

      // Maximum idle duration before an in-memory registry entry
      // is evicted (still resumable from disk). "0s" or omitted →
      // default 24h. Set higher for shared-session deployments
      // where conversations span days; lower for tight-budget
      // pods where memory is precious.
      "session_idle_timeout": "24h"
    }
  }
}
```

No new CLI flags. The config-only surface matches the existing multi-session knobs and keeps the operator-facing complexity contained.

## Migration story

**Existing v2.4 deployments** (multi-session enabled, no resume):
- On first restart with the v2.5 binary, the `agent_session_acl` table is created by AutoMigrate. The table is empty.
- Existing sessions in the eventlog don't have ACL rows → they're NOT resumable (the resumer's `Get` returns `sql.ErrNoRows`, treated as `ErrSessionNotFound`).
- New sessions created post-upgrade write ACL rows and are resumable as expected.
- Operators with critical existing sessions get a one-time manual migration tool (see "Out of scope" — track separately): `core-agent admin backfill-acl --owner=<who-to-attribute>` that walks the eventlog and writes inferred ACL rows.

**Fresh v2.5 deployments**: no migration needed; ACL rows exist from the first `RegisterOwned`.

**Single-user / pre-multi-session deployments**: no behavior change. `session_resume` defaults to false when `multi_session.enabled` is false (the resumer is never wired).

## Implementation phases

### Phase 1 — SessionACLStore + persistence (PR ε.1)

- New `agent_session_acl` GORM model + table.
- `pkg/attach.SessionACLStore` CRUD interface + implementation.
- `RegisterOwned` writes to it; `Unregister` deletes from it.
- AutoMigrate wired in `eventlog.Open`.
- Tests: round-trip Put/Get/Delete/Touch/ListByOwner; concurrent Put; DB-error paths.

Estimate: ~300 LoC + ~250 LoC tests.

### Phase 2 — SessionResumer + lazy resume (PR ε.2)

- `attach.SessionResumer` interface.
- `cmd/core-agent/multi_session.go` `buildSessionResumer` implementation.
- `Registry.Lookup` / `LookupSingle` consult resumer on miss with singleflight dedup.
- `reproduceAgent` helper that's the `reproduceAgent` counterpart of `buildSessionFactory`'s agent construction — same options, explicit SessionID.
- Tests: resume after restart restores the same triple + ACL; concurrent Lookup misses share one resume call; resume failure surfaces as 404 (not 500); registry still empty after restart until first touch.

Estimate: ~400 LoC + ~350 LoC tests.

### Phase 3 — Idle eviction (PR ε.3)

- Per-entry `LastTouchedAt`.
- Background sweep goroutine started by `Server.Bind`.
- Per-session cancel-on-evict that signals the wake loop to exit cleanly.
- Tests: evicted session re-resumes on touch; evict doesn't break an actively-streaming TUI (the entry is touched continuously); cancel fires the wake loop's done channel.

Estimate: ~250 LoC + ~200 LoC tests.

### Phase 4 — docs + smoketest + recipe update (PR ε.4)

- Hugo reference page update (`docs/site/content/docs/reference/multi-session.md`) — new "Session resume" section covering when sessions persist, when they evict, how to configure the idle timeout, what an operator should expect after a daemon restart.
- Smoketest extension: kill the daemon mid-flight, restart, verify alice's TUI reconnects + the prior conversation history is visible.
- CHANGELOG v2.5.0 entry.
- This design doc's status flipped from "proposed" → "shipped in v2.5".

Estimate: ~300 LoC docs + smoketest extension.

**Total**: ~1,250 prod + ~800 tests across 4 PRs. Larger than the multi-session spike but smaller than the original multi-session substrate (#162).

## Open questions (resolved)

All 8 design questions are resolved with explicit decisions below. Implementation can begin once the doc is approved.

### 1. `GET /sessions` and persisted-but-evicted sessions

**Resolved: yes — listing includes idle sessions.** Returns the UNION of in-memory registry entries AND persisted ACL rows the caller can read; resume itself only fires on `Lookup`/`LookupSingle` (specific-session access).

Reasoning:
- Listing is metadata-only — no agent reconstruction, no I/O beyond the indexed ACL query.
- `agent_session_acl` has an index on `Owner`; per-caller queries are O(log n). Admin's full scan at realistic scale (thousands of sessions) is sub-millisecond.
- The alternative (in-memory-only listing) means an operator post-restart sees "0 sessions" — actively bad UX. Right behavior: alice sees her sessions, sees they're idle, clicks in, resume fires on the lookup that follows.

Wire shape — `sessionDescriptor` gains two fields:

```go
type sessionDescriptor struct {
    AppName       string    `json:"app"`
    UserID        string    `json:"user"`
    SessionID     string    `json:"sessionID"`
    HasEventLog   bool      `json:"has_event_log"`
    Status        string    `json:"status"`         // "active" (in-memory) | "idle" (persisted-only)
    LastTouchedAt time.Time `json:"last_touched_at"`
}
```

`Status` lets the TUI render the distinction (e.g. `● 3 active · ◆ 7 idle`). `LastTouchedAt` is useful for ordering and operator GC decisions. `ListAuthorized(caller)` becomes a two-source union, deduped by triple, filtered through `Authorize`.

### 2. Resume failure (factory error) handling

**Resolved: HTTP 500 with the underlying error in the response body. No broken-session bookkeeping in v2.5.**

Surface format:

```
HTTP/1.1 500 Internal Server Error
Content-Type: application/json

{"error":"resume failed: agent.New: <underlying cause>"}
```

Reasoning:
- Most resume failures are transient (MCP server hiccup, ADC token refresh blip, a downstream API rate-limit). Operator retries; if the underlying issue clears, the next request succeeds.
- A "marked broken" flag in the ACL row adds complexity without solving the underlying issue — and risks marking sessions broken that would have worked on retry.
- The informative body lets operators diagnose without grepping daemon logs. (Daemon also logs `resume failed for session X: <err>` at WARN; pairs naturally with [#179](https://github.com/go-steer/core-agent/issues/179)'s `--log-file`.)
- Side-channel guard: the body MUST NOT distinguish "session doesn't exist" (404) from "session exists but factory failed" (500) by content. The 404 body stays `attach: session not found`; the 500 body says `resume failed:` — different surfaces, can't be conflated by an attacker probing SessionIDs.

If resume failures become operator pain in practice (one bad MCP server causing many 500s, daemon log floods), broken-session bookkeeping lands as a v2.6+ enhancement.

### 3. Sidecar table vs eventlog metadata for ACL persistence

**Resolved: sidecar table (`agent_session_acl`).** Spec'd in §"Persistence primitive" above.

Reasoning (final):
- Eventlog metadata is per-event attribution (`caller`, `proxy_by`); ACL is per-session config. Different granularity, different mutation cadence.
- Resume needs `WHERE SessionID = X` to be O(1). Sidecar with composite-PK index delivers; eventlog scan is O(N) over the audit log.
- The sidecar schema can evolve independently — ACL fields (Viewers, Contributors, future `display_name`, future `quota_class`) grow without forcing eventlog metadata migrations.
- Composes cleanly with future `PATCH /sessions/<sid>/acl` (just `UPDATE` the row).

### 4. Wake-loop lifecycle interaction with eviction

**Resolved: per-session cancel func on `*Entry`; eviction invokes it; the loop exits cleanly.**

Mechanism:

1. When `RegisterOwned` runs (factory OR resumer), the caller passes a `cancelOnEvict context.CancelFunc` along with the agent.
2. The registry stores it on `*Entry.cancelOnEvict`.
3. `Registry.Unregister` (called by eviction OR by an explicit DELETE in the future) invokes the func before removing the entry.
4. The wake loop's outer ctx is derived: `loopCtx, cancelOnEvict := context.WithCancel(daemonCtx)`. Two cancel sources — daemon shutdown (`daemonCtx.Done`) and per-session evict (`cancelOnEvict`) — share one `<-loopCtx.Done()` branch in the wake loop's select.

Code sketch (factory and resumer share this):

```go
loopCtx, cancelOnEvict := context.WithCancel(deps.daemonCtx)
go runSessionWakeLoop(loopCtx, ag, deps.tracker, deps.model.Name(), deps.pricingRate)
return ag, cancelOnEvict, nil  // caller hands cancelOnEvict to registerWithACL
```

The existing wake loop code is unchanged — it already selects on `<-ctx.Done()`. Cleanup is automatic.

### 5. Migration tool for existing v2.4 sessions

**Resolved: ship a two-mode `core-agent admin backfill-acl` command in Phase 4. Operators on fresh v2.5 don't need it; v2.4 → v2.5 upgrades use it once.**

Two modes:

- **`--owner=<identity>`** — single-tenant attribution. Walks the eventlog, writes ACL rows for every session triple with `Owner = <identity>`. One command for the "I'm the only operator" case.

- **`--infer-from-eventlog`** — multi-tenant attribution via the existing α.2 Metadata sidecar. For each session triple in the eventlog, scan its events; the most common (or first) `Metadata["caller"]` value becomes the inferred owner. Operator runs `--dry-run` first to review the inferred mapping, then re-runs without `--dry-run` to commit.

Pre-v2.4 sessions (no Metadata) infer as `--owner=unknown@local` (or whatever the operator passed as a fallback) so they're at least admin-only-accessible after migration rather than lost.

The infer mode is elegant — the α.2 audit data is exactly the source of truth for "who created what session." Phase 4 land timing lets us include it in the v2.5 CHANGELOG migration notes.

### 6. Package placement for the resumer code

**Resolved: `SessionACLStore` lives in `pkg/attach`; `SessionResumer` interface in `pkg/attach`, implementation in `cmd/core-agent`.**

Reasoning:
- `SessionACLStore` is a thin GORM CRUD layer with no cmd-level dependencies — clean fit in `pkg/attach` alongside `SessionRegistry`. Tests for the store don't need to spin up a full daemon.
- `SessionResumer` interface is small (`Resume(ctx, app, user, sid) (Registrant, SessionACL, error)`) — belongs in `pkg/attach` so handlers can consult it without importing `cmd/core-agent`.
- `SessionResumer` IMPLEMENTATION needs `sessionFactoryDeps` (model, gate template, tools, eventlog handle, MCP servers, …) which are cmd-level. Moving them into `pkg/attach` would force `pkg/attach` to depend on every package the daemon depends on — bad layering. Pattern mirrors `buildSessionFactory` today.

### 7. Resume in single-user mode

**Resolved: no — single-user crash-resume already works via ADK's `session.Service`.**

The single-user path uses `Register` (not `RegisterOwned`), writes no ACL row, has the constant "default" SessionID. On daemon restart, the new agent is constructed with the same SessionID; ADK's `session.Service` reattaches the prior conversation history from the DB automatically. No separate resume machinery needed.

Simpler invariant: **"ACL row exists ⟺ session is resumable via the multi-session resumer."** Operators wanting multi-session-style resume (custom session IDs, ownership, audit threading) flip `multi_session.enabled: true` even with one user.

### 8. ACL mutation evolution

**Resolved: schema accommodates mutation; `PATCH /sessions/<sid>/acl` API deferred to v2.6+.**

The `agent_session_acl` table's `ViewersJSON` and `ContributorsJSON` columns are mutable; the store's `Put` rewrites the row, and a future `UpdateACL(ctx, app, user, sid, viewers, contributors []string)` method writes a partial update via `UPDATE … SET viewers_json = ?, contributors_json = ? WHERE …`. No schema change needed when the mutation API lands.

When mutation does land, ACL changes will write a synthetic event to the eventlog (Author=`attach/acl-mutation`, CustomMetadata={`who`, `field`, `before`, `after`}) so the audit trail of "who changed permissions when" lives alongside the conversation trail. Threading through `Authorize` for the mutating call is straightforward — `ActionSessionAdmin` already exists in the matrix and gates Owner+Admin only.

## Security considerations

- **ACL table contents are sensitive.** Identities (potentially employee emails, customer IDs) of owners / viewers / contributors. The session DB already requires file-mode protection in the same shape `users.json` does (document this in the operator reference). Same retention concerns apply.
- **Resume implicitly trusts the ACL row.** A row with `Owner: "alice@..."` reconstructs alice's session even if alice was offboarded after the row was written. Operators offboarding users should also revoke their session ownership — track as a v2.5+ feature, or document as a known limitation tied to the "user-management CLI" non-goal from the original design.
- **DB corruption / tampering.** Hand-editing the ACL table to swap Owner would let an attacker reconstruct sessions under their identity. The DB is already a high-trust artifact (the eventlog is the audit log); same posture extends to the ACL table.
- **Resume failures are not a side channel.** The 404 returned on resume failure (or a 500 on factory error) MUST NOT distinguish "session doesn't exist" from "session exists but factory failed" — both surface as `404 session not found`. Otherwise an attacker can probe SessionIDs and learn which exist.

## Out of scope (deferred to v2.6+)

- Cross-daemon session migration (per multi-session design doc).
- `DELETE /sessions` hard-delete + soft-delete semantics.
- `PATCH /sessions/<sid>/acl` ACL mutation API.
- Auto-recovering an in-progress turn on crash (turn-boundary semantics).
- Pre-flight resume of "warm" sessions (active in last N hours) at daemon startup for faster first-reconnect.
- User-management CLI (`core-agent users add/remove`) — already deferred in the multi-session design.
- A per-session quota model that uses ACL as the key.

## Dependencies and related work

- **[#162](https://github.com/go-steer/core-agent/issues/162) / multi-session substrate** — landed in v2.4. This builds on `SessionRegistry`, `RegisterOwned`, `SessionACL`, the factory closure pattern.
- **[#171](https://github.com/go-steer/core-agent/pull/171) / on-demand session creation** — `POST /sessions` + the `SessionFactory` closure that this design's `SessionResumer` mirrors.
- **[core-tui#51](https://github.com/go-steer/core-tui/issues/51)** — 404-aware TUI reconnect. Once resume ships, the 404 case is rare enough that the upstream fix is a polish item, not a blocker. Both should be tracked together.
- **[#179](https://github.com/go-steer/core-agent/issues/179) / `--log-file`** — operator observability for resume failures (a daemon log line "resume failed for session X: <reason>" is more useful than a silent 404 in the audit log).
- **`docs/multi-session-design.md`** — the parent design. This doc updates two of that doc's "Out of scope" entries (session deletion semantics — partially addressed via eviction; resume not previously specified).

## When this lands

- Phase 1: ~3 days (sidecar table + store + RegisterOwned/Unregister integration)
- Phase 2: ~4 days (Resumer + lazy lookup + singleflight)
- Phase 3: ~2 days (eviction sweep + per-session cancel-on-evict)
- Phase 4: ~2 days (docs + smoketest extension + CHANGELOG)

~2 weeks of focused work. Phases 1+2 are the substrate; 3+4 are polish + ship.

The whole stack is independently mergeable: Phase 1 ships a useful primitive (ACL persisted even without resume) that operators can query directly via SQL; Phase 2 turns on the user-visible resume; Phase 3 keeps memory bounded; Phase 4 makes it discoverable. Each PR can land + ship in v2.5.minor releases without forcing the others.
