# External state: pluggable session, control and memory stores for failover

**Status:** proposed, partly implemented. Phase 1 is shipping as the transcript seam
only — `eventlog.Transcript`, `eventlog.OpenTranscript`,
`pkg/statestore/{memory,conformance}` — with control, index and the
per-session state stores still on the local SQL database (see
Amendments). Phase 3, the Agent Engine transcript backend, is
`pkg/statestore/agentengine`, selected with `state.transcript.backend:
"agentengine"` in `.agents/config.json`, and it ships together with the
part of phase 2 a `state` block requires: the residency lease, the
self-fence and stop-on-loss (decisions 1–3), on the local control
store. The rest of phase 2 (plan, todo and gate state into the
transcript; the `409` owner hint) and phase 4 are still open; the
site's Sessions page says what survives a machine loss at this point.
Open questions 1–4 of the first draft were settled by the maintainer
on 2026-10-01 (decisions 13–16).

## Amendments

Recorded as the phases land; each names the decision it narrows.

- **2026-11-06 — Phase 1 introduces the Transcript seam only.** Control
  (lock, claims, boot log, acks, pauses, schedule) and index (session
  ACL) keep today's SQL code behind the unchanged `Handle` methods and
  `Handle.DB`, which therefore stays set on every Handle, until phase 4
  brings a second implementation. A Handle over a non-SQL transcript
  (`eventlog.OpenTranscript`) still needs a local SQL database for
  them. Additive; consistent with the #1192 compatibility table.
- **2026-11-06 — Interface placement and shape.** `Transcript` is
  declared in `pkg/eventlog`, which owns `Entry` and `QueryOption`, and
  aliased as `statestore.Transcript`; declaring it in `statestore`
  would make `eventlog` import `statestore` and `statestore` import
  `eventlog`. `Since` keeps the `Stream.Since` signature (the scope is
  in the options; `eventlog.ResolveQuery(...).Scope()` extracts the
  root) rather than a separate `root` argument, and the interface adds
  `Subscribe` for decision 4's in-process wake-ups.
- **2026-11-06 — Agent Engine backend details settled in code.**
  Sessions are scoped to `(app, user)` by `core_agent.app_name` in
  session state plus the service's `user_id`; a session found under a
  mapped ID that belongs to another app or user is reported not found.
  `raw_event` holds the event's own JSON at the top level with the seq
  and sidecar under one `coreAgent` key, so another ADK runtime reading
  `raw_event` as an Event still gets the event. An `AppendEvent` whose
  outcome is unknown (any status other than a definite rejection)
  invalidates the process's view of the tree, so the next seq is read
  from the store rather than reused; this is the residual window
  decision 3 accepts, named.
- **2026-11-06 — Phase 3 lands with phase 2's lease, not after it.**
  The #1192 compatibility table gates decision 1 on a `state` block
  being present, and phase 3 is what introduces one, so the lease ships
  with it rather than in phase order: `eventlog.WithResidencyLease`
  (set by the CLI for the `agentengine` transcript) takes a lease when
  the instance creates a session, when the multi-session registry
  builds a tenant (create or resume, via `Handle.AcquireResidency`), or
  on the first write to a tree never resident here — never on a read,
  so scans (auto-continue, `/parks`, auto-resume) claim nothing; a read
  of a tree another instance is writing may be served from this
  instance's cache until it becomes the writer. It covers the session's
  tree with its `:sub:`, `:digest` and `:mast-ops` trees, but not
  caller-chosen IDs that merely contain `:`. It is released on eviction,
  root delete (without a tombstone) and Close, and an evicted tree is
  tombstoned so a write still in flight is refused rather than
  re-taking it — until the session is resumed again in the same
  process, after which a straggler from the old generation is not
  distinguished (pinned). Every write is fenced on `SessionLock.ValidUntil` and
  `Lost()` and runs under a context deadline at the moment a successor
  could steal the row (decision 3), which also bounds the 429 retry.
  Taking a lease invalidates the transcript's cached view of the tree
  (`eventlog.TranscriptInvalidator`), since another instance may have
  written meanwhile. On loss a tenant is evicted; an unclaimed tree
  stops a single-session process and is only fenced in a multi-session
  daemon (decision 2). Two narrowings, both until phase 4: the lease is a row
  in the local `agent_run_lock` table, so it excludes only processes
  that share that SQLite file — single-writer across machines still
  comes from the deployment (one ax task per session, in shipmind) —
  and it is a separate row from the run lock `autonomous.Resume` and
  auto-continue take (key `residency:<root>`), so those callers are
  unchanged; merging the two is phase 2 proper. The `409` owner hint
  (decision 12) and plan/todo/gate state in the transcript are not
  here.
- **2026-11-06 — The engine is named once, with memory.** Following
  `docs/memory-service-design.md` decision 4 (merged meanwhile),
  `state.transcript.agentengine.reasoning_engine` is optional and falls
  back to the top-level `agent_engine` block that Memory Bank reads, via
  `Config.ResolveAgentEngineResource`. This replaces the sketch's
  `state.transcript.agentengine.reasoning_engine` as the only source.
- **2026-11-06 — Agent Engine 429s follow #935's policy, narrowed to
  quota rejections.** A `ResourceExhausted` append is retried once after
  2s from a shared budget of three refilling one per 30s, the model
  retry's shape and defaults. `Unavailable` and `DeadlineExceeded` are
  not retried: their outcome is unknown, so a blind retry could write
  the event twice; they take the invalidate path above instead. The
  backoff runs inside the write's fence deadline, so it is cut short
  rather than landing after a successor; it does hold the process's
  write mutex while it waits, so other appends queue behind it for up
  to the backoff.
- **2026-11-06 — Decision 16, the SQL backend's location.** The SQL
  transcript is `eventlog.OpenSQLTranscript` in `pkg/eventlog`, not
  `pkg/statestore/sql`: it is the overlay and ADK service that package
  already owns, and moving it would move `eventlog.Open`'s internals
  with it. The other backends are under `pkg/statestore/` as decided.
- **2026-11-06 — Decision 4, as #1192 already noted:** the SQL Handle
  from `eventlog.Open` keeps polling, because a second process may be
  appending to the same SQLite file. Only a Handle from
  `OpenTranscript` is wake-up driven. The SQL transcript still fires
  in-process wake-ups so it passes the same conformance suite.

## Motivation

A core-agent daemon today keeps a session's durable state in one SQLite
file on the instance that runs it (`--session-db-path`, default
`~/.<binary>/sessions.db`). If that instance dies, the session can only
come back on an instance that can open the same file. In practice that
means the same pod with the same PVC. Resume (`session-resume-design.md`)
and auto-continue (`auto-continue-design.md`) both work, but only on
the same instance.

The goal is **fault tolerance, not concurrency**: a session must not
depend on one particular instance. When the instance running a session
dies, any healthy instance can pick the session up from external state.
Two instances never run the same session at the same time.

We want this without running a shared SQL database. The external stores
we need to support:

- **Vertex AI Agent Engine** Sessions and Memory Bank. Required.
- **Redis**, and the pattern of "a store we didn't write". The store
  layer must not bake in Google.

## What has to move, and what is fine where it is

The eventlog census (October 2026) found that `pkg/eventlog.Handle` is
four things sharing one `*gorm.DB`:

| Concern | Today | Needs from a backend |
|---|---|---|
| **Transcript**: ADK `session.Service` plus the overlay `Stream` (`Append`/`Since`/`Watch`, `Entry.Seq`, `Metadata`) | ADK `database` service plus the `agent_eventlog` overlay, written in ADK's transaction (`atomic.go`) | Ordered append, read back in order, query by author, branch and session tree |
| **Control**: run lock (`lock.go`), continuation claims (`claim.go`), boot log (`bootlog.go`) | SQL tables, CAS via `RowsAffected` and a conditional upsert | Compare-and-swap with a TTL |
| **Index**: session ACL (`attach.SessionACLStore`) | gorm table, `ListVisibleTo` via `LIKE` over JSON | Key-value get/put plus owner and visibility listing |
| **Memory**: `memory.Memory` (#948, not shipped) | n/a | Remember, recall and forget, plus semantic search |

Things that survive a restart today because they are written to the
transcript: guardrail trips (#643), autonomous checkpoints, the digest
store, and the `finish_reason` that auto-continue classifies on.

Things that are **lost** when the instance dies, and what this design
does with each:

| State | Where | Disposition |
|---|---|---|
| Plan file | `<agentsDir>/plans/plan-<seq>.md`, local disk (`record_plan.go`) | **Moves.** Written as a transcript event; the file stays as a convenience copy. |
| Todo list | process memory (`tools/todo.go`) | **Moves.** Snapshot event per update, folded back on resume. |
| Per-session gate state (`planRecorded`, session allow-list) | re-derived fresh (`multi_session.go:461`) | **Moves.** Derived from the plan and grant events on resume. |
| Inbox / inject queue | process memory (`agent/inbox.go`) | **Accepted loss**, as it is today across a restart. The 10-minute claim TTL already covers the gap. |
| Pending permission prompts | `PromptBroker.pending` | **Accepted loss.** The turn that asked is dead. Auto-continue re-drives it and the prompt is asked again. |
| In-flight turn | goroutine | **Accepted loss.** Auto-continue handles it, as it does today. |
| Background subagents | goroutines | **Accepted loss.** Out of scope. |

## Settled decisions (do not relitigate)

1. **One writer per session, enforced by a lease the owning instance
   holds for as long as the session is loaded.** Today the run lock is
   taken only by auto-continue and autonomous resume. Under this design
   the registry acquires it when a session becomes resident (create or
   resume) and releases it on eviction or shutdown.
   `ErrSessionLocked` on a resume means "another live instance owns
   it".

2. **Losing the lease stops the session; it does not just log.** Today
   nothing reads `SessionLock.Lost()`. The registry will watch it.
   When the lease is lost, the instance cancels the in-flight turn,
   stops the wake loop and evicts the session without writing anything
   more.

3. **Self-fencing by local deadline, because Agent Engine cannot fence
   writes.** `AppendEvent` on Agent Engine carries no precondition or
   etag, so the store cannot reject a stale writer. The holder
   therefore tracks `validUntil = lastSuccessfulBeat + staleAfter -
   margin` on its own monotonic clock and refuses to append once that
   time has passed, even if the coordinator can't be reached. Backends
   that *can* fence (SQL, Redis via a Lua compare-and-append) also
   check a fencing token. The risk that remains (a process paused
   longer than `margin` between its deadline check and the write
   landing) is accepted and documented, not hidden.

4. **Live tailing is in-process; the store is only for catch-up.** With
   one writer, every live event for a session is produced on its owner,
   and attach observers connect to that owner. `Watch` becomes an
   in-process fan-out fed by `Append`, and `Since` reads the store for
   replay. No backend needs a server-side tail, so we don't need Redis
   `XREAD BLOCK` or Agent Engine polling. The SQL backend's 200ms poll
   goes away with it.

5. **`Entry.Seq` stays an int64 that increases monotonically within a
   root session tree. It is assigned by the lease holder, not the
   store.** The SSE `?since=N` cursor is visible to clients, so its
   meaning can't change. Uniqueness across *all* sessions is dropped.
   No consumer depends on it: every `Since` call is scoped with
   `ForSession` / `WithSessionTree`. On resume the new owner reads the
   tree's highest seq and continues from there. SQL keeps its global
   autoincrement, which still satisfies the per-tree rule.

6. **The event record is written whole, in one write.** The overlay
   table exists because ADK's schema has no room for our seq and
   metadata. External backends store the full ADK event JSON plus
   `{seq, metadata}` in one record:
   - Agent Engine: `SessionEvent.raw_event`, with `content` also
     populated so Memory Bank can read the session.
   - Redis: one stream entry.

   That makes `atomic.go`'s two-write problem disappear rather than
   moving to another backend.

7. **We write our own Agent Engine transcript backend on
   `aiplatform/apiv1beta1.SessionClient`; we do not use ADK's
   `session/vertexai`.** ADK's client loses data in the round-trip,
   even at v1.7.0:
   - it rewrites event IDs;
   - it drops partials, `UsageMetadata`, `FinishReason`, most
     `Actions`, `FileData` and `CodeExecutionResult`;
   - before v1.6.0 it dropped `FunctionCall.ID`, which breaks Anthropic
     tool_use pairing on resume.

   Our resume rebuilds the usage tracker and the auto-continue
   classifier from exactly those fields. ADK's code is the reference
   for request shapes; we don't depend on it.

8. **Session IDs are mapped, not constrained.** Agent Engine IDs must
   match `[a-z][a-z0-9-]{0,62}`:
   - `rwc-<hex>` IDs pass unchanged;
   - UUIDv7 IDs (which can start with a digit) and derived IDs
     (`<sid>:sub:<n>`, `<sid>:digest`) are not valid.

   Backends that need it map our ID to
   `s-<base32(sha256(id))[:40]>`, deterministically, and store the
   original ID and the root ID in session state. Nothing above the
   store layer sees the mapped ID.

9. **The four concerns are four interfaces, chosen independently in
   config.** The SQL backend implements all four, so today's
   single-binary default doesn't change. A deployment picks one backend
   per concern:

   | Deployment | transcript | control | index | memory |
   |---|---|---|---|---|
   | default (today) | sql | sql | sql | sql-fts (#948) |
   | GKE + Agent Engine | agentengine | kubernetes | kubernetes | memorybank |
   | Redis everywhere | redis | redis | redis | redis-ams |

   Agent Engine provides no compare-and-swap, so it can never serve
   **control**. This is why the concerns can't be one interface.

10. **A shared conformance suite is the contract.**
    `pkg/statestore/conformance` holds table-driven tests that every
    backend runs: ordering, author/branch/tree filters, seq continuity
    across a handover, lease steal after staleness, `Lost()` on steal,
    claim semantics, ACL visibility, and a round-trip of every event
    field resume depends on. We learned this the hard way (#553): ADK's
    in-memory and database services differed on stale writes, and that
    is how two projects shipped the same bug. The Agent Engine
    conformance run is provider-gated, like the Vertex tests already
    are. Redis runs hermetically against `miniredis`. Kubernetes runs
    against `envtest`'s real apiserver, not client-go's fake clientset,
    because the fake doesn't enforce `resourceVersion` conflicts and the
    lease's CAS is the property under test.

11. **Memory follows [`memory-service-design.md`](memory-service-design.md)
    (superseding #948's single-scope `memory.Memory` draft).** Memory
    Bank, SQLite FTS5, and external stores implement `memory.Backend`
    behind a multi-scope `memory.Router` (`0..1` private `agent` mount +
    `0..N` named `shared:<pool>` mounts), rather than ADK's
    `memory.Service` directly (`memory.AsADKService` bridges third-party
    ADK tools). Every completed `Remember`, `Forget`, and
    `ConsolidateSession` still emits a `memory/*` transcript event into
    whichever `session.Service` is wired. Conversation consolidation on
    Memory Bank defaults to `DirectContentsSource` (with an optional
    `VertexSessionOrigin` fast path to `VertexSessionSource` when
    transcript and memory share a `ReasoningEngine`), decoupling Memory
    Bank from session-store choice.

12. **No request proxying.** If a request reaches a non-owner while a
    live lease exists, the instance answers `409 Conflict` with the
    owner's advertised attach URL (stored in the lease record) and a
    `Retry-After`. Routing is the ingress's or client's job. If the
    lease is stale, the receiving instance steals it and resumes,
    through the existing lazy-resume path.

13. **On GKE, control is the Kubernetes `Lease` backend, not Redis.**
    The target deployment (GKE + Agent Engine) runs no extra service:
    one `coordination.k8s.io/v1` `Lease` per resident session. The
    accepted cost is one apiserver write per resident session per
    heartbeat (5s). That fits tens of sessions per daemon. A deployment
    that needs thousands uses the Redis control backend instead. Redis
    is still built, after Agent Engine and Kubernetes.

14. **The index lives on the control backend whenever the transcript is
    Agent Engine.** Agent Engine can't filter sessions by viewer or
    contributor, so `ListVisibleTo` would mean a `ListSessions` scan per
    user. On Kubernetes the index is labels on the session's `Lease`:
    - `owner.core-agent.dev/<h>`;
    - `viewer.core-agent.dev/<h>`;
    - `contributor.core-agent.dev/<h>`.

    `<h>` is a truncated hash of the caller ID, because label keys are
    length- and charset-limited. Each listing is then one label-selector
    query. Title and timestamps go in annotations. The Lease outlives
    eviction: releasing the lease clears `holderIdentity`, not the
    object. The object is deleted when the session is deleted, so the
    index survives restarts just as the SQL table does.

15. **Memory Bank consolidation is opt-in, off by default. This reopens
    #948's "no auto-capture in v1".** With
    `memory.memorybank.consolidate: "session_end"`, the daemon calls
    `GenerateMemories` with the session as its source when a session
    ends (explicit close or delete; idle eviction does not count,
    because an evicted session is still live). Two rules keep #948's
    reasons for refusing auto-capture intact:
    - **It is audited.** The operation's result (every created,
      updated or deleted memory ID and its fact) is written as a
      `memory/consolidate` transcript event. An operator can therefore
      see every memory they didn't know was written, which was #948's
      stated objection.
    - **It is never implicit.** The default is `"off"`. A recipe that
      wants it says so, and `/status` shows it.

    [`memory-service-design.md`](memory-service-design.md) specifies the full
    consolidation contract (`"off" | "turn_end" | "session_end"`, targeting the
    private `agent` mount by default unless `consolidation_target` names a
    writable shared pool, via `DirectContentsSource` or `VertexSessionOrigin`).

16. **The stores are in-tree; the memory adapters follow [`memory-service-design.md`](memory-service-design.md).**
    - **In-tree, under `pkg/statestore/{sql,agentengine,redis,kubernetes}`:**
      the Agent Engine, Redis and Kubernetes transcript, control and
      index backends. GKE images are the main consumer, and an
      out-of-tree store would keep missing the conformance suite.
    - **In-tree (`pkg/memory`, `pkg/memory/memorybank`, `pkg/memory/sqlfts`):**
      the driverless core/router/in-memory store (safe for the slim embed
      ceiling), the Memory Bank adapter sharing the `aiplatform` client
      with the Agent Engine session backend, and the local SQLite FTS5
      backend.
    - **`extras/redis-memory/`:** the Redis AMS adapter.

## Interfaces (sketch)

```go
// package statestore

type Transcript interface {
    Service() session.Service // ADK CRUD; AppendEvent assigns Seq + runs the fence check
    Since(ctx context.Context, root string, fromSeq int64, opts ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error]
    HighestSeq(ctx context.Context, root string) (int64, error)
    Close() error
}

type Control interface {
    AcquireLease(ctx context.Context, key SessionKey, advertise string) (Lease, error) // ErrSessionLocked{Owner}
    ClaimContinuation(ctx context.Context, key SessionKey, interruptedAt time.Time) (bool, error)
    ReleaseContinuationClaim(ctx context.Context, key SessionKey) error
    RecordBoot(ctx context.Context, b Boot) (BootID, error)
    UpdateBootAttempted(ctx context.Context, id BootID, n int) error
    RecentBoots(ctx context.Context, since time.Time) ([]Boot, error)
    Close() error
}

type Lease interface {
    Token() uint64               // fencing token; monotonic per key
    ValidUntil() time.Time       // local self-fence deadline (decision 3)
    Lost() <-chan struct{}
    Release(ctx context.Context) error
}

// Index is today's attach.SessionACLStore, moved behind the store layer.
type Index = attach.SessionACLStore
```

`eventlog.Handle` stays as the type that wires these together, so its
callers barely change: `Handle.Stream` and `Handle.Service` keep their
shapes. `Handle.DB` goes. Its only two external users construct the
ACL store, and they get `Handle.Index` instead. `IsSessionNotFound`
stops matching `gorm.ErrRecordNotFound` and matches
`session.ErrNotFound`, which needs ADK ≥ v1.7.0 (see phase 0).

## Backend notes

**Agent Engine (transcript, memory).**
- **Setup:** one ReasoningEngine per deployment, created without
  deploying code (`ReasoningEngineClient.CreateReasoningEngine`). Its
  resource name goes in config. We don't infer it from the app name.
- **Sessions:** are created with our mapped ID. ADK's client waits on
  creation by polling `GetSession` for up to about 30s. We wait on the
  operation instead, so `POST /sessions` isn't held up by a polling
  loop.
- **Reads:** `ListEvents` with `order_by=timestamp`, paginated. Our seq
  is the authoritative order, and timestamps only narrow the query.
- **Index:** not served here (decision 14). The original and root IDs
  are still written to session state (decision 8), so a session can be
  traced from the Agent Engine console.
- **Retention:** set to an explicit `ttl`, at least the 24h minimum.
  It doesn't follow the idle eviction (eviction only drops the session
  from memory).
- **Memory Bank:** via `apiv1beta1.MemoryBankClient` (beta-only in Go; full
  RPC and scope mapping in [`memory-service-design.md`](memory-service-design.md)):
  - `Remember` → `GenerateMemories` (`DirectMemoriesSource`, default
    deduplicating consolidation) or `CreateMemory` when `WriteVerbatim` is
    requested, scoped to discrete `agent` (`scope_kind="agent"`, `agent_id`)
    or `shared:<pool>` (`scope_kind="shared"`, `pool`) mounts;
  - `Recall` → `RetrieveMemories` (`SimilaritySearchParams{TopK}`) fanned
    out concurrently across mounted scopes via `memory.Router`;
  - `Forget` → `GetMemory` scope verification followed by `DeleteMemory`;
  - Opt-in conversation extraction → `GenerateMemories` via
    `DirectContentsSource`, taking a zero-copy fast path to
    `VertexSessionSource` when the active session implements
    `memory.VertexSessionOrigin` on the same `ReasoningEngine` (decision 15).
- **Quotas:** apply per project and region. A 429 goes through the same
  transient-retry policy as the model calls (#935).

**Redis (transcript, control, index).**
- **Transcript:** one stream per root tree (`XADD` with an explicit ID
  `<seq>-0`). That makes seq the stream ID, and Redis rejects any
  non-increasing ID, which is a free monotonicity check.
- **Fenced append:** a Lua script compares the lease token, then
  `XADD`s.
- **Lease:** `SET NX PX` plus an `INCR` fencing token. Renew and release
  are compare-and-`PEXPIRE` and compare-and-`DEL` in Lua.
- **Claims:** a conditional upsert in Lua.
- **Boot log:** a sorted set scored by time.
- **Index:** hashes plus sets per owner, viewer and contributor.
- **Client:** `github.com/redis/go-redis/v9`.
- **Memory:** an adapter over the Redis Agent Memory Server REST API, as
  `shared-memory-design.md` already planned.

**Kubernetes (control and index).** `coordination.k8s.io/v1` `Lease`
per session:
- CAS via `resourceVersion`;
- `leaseTransitions` as the fencing token;
- `holderIdentity` and an annotation for the advertised URL.

This needs no extra service on GKE. Its cost is apiserver load: one
heartbeat per resident session every 5s. That is fine for tens of
sessions per daemon and wrong for thousands (decision 13). Index labels
are described in decision 14. Claims go in an annotation on the session's
Lease (`interruptedAt`, holder, claimedAt), written with the same
`resourceVersion` CAS. The boot log is a single fleet-wide ConfigMap,
trimmed to the breaker's window on every write. RBAC is a kustomize
component granting
`get/list/watch/create/update/patch/delete` on `leases` and the one
ConfigMap in the daemon's namespace. That matches how gated-apply ships
its RBAC.

## Phases

0. **ADK v1.2.0 → v1.7.0 bump.** It stands on its own: `session.ErrNotFound`
   and the struct-conversion fixes. It is a prerequisite only for the
   `IsSessionNotFound` change.
1. **Seams, no behaviour change.** Introduce `pkg/statestore` and its
   interfaces, plus the conformance suite. Move the SQL code behind it,
   and take the ACL store off `*gorm.DB`. Replace `Watch` polling with
   in-process fan-out (decision 4). Every existing test stays green.
2. **Lease held for the whole time a session is resident** (decisions
   1–3 and 12):
   - registry acquire and release;
   - `Lost()` consumed;
   - the self-fence check in `AppendEvent`;
   - `409` plus an owner hint.

   Also: plan, todo and gate state move into the transcript. SQL is
   then already failover-correct for anyone sharing a SQLite file over
   a shared volume. That is not a supported deployment, but it is a
   useful test.
3. **Agent Engine transcript backend,** with provider-gated
   conformance.
4. **Kubernetes `Lease` control and index backends** (decisions 13–14),
   plus envtest conformance and the RBAC component. After this phase
   the GKE + Agent Engine stack is complete.
5. **Redis backend** for transcript, control and index, with `miniredis`
   in CI.
6. **Memory:** Phases 1–4 of [`memory-service-design.md`](memory-service-design.md)
   (`pkg/memory` core + `InMemory` + `Router`, gated tools + subagent scope
   binding + stateless `--memory-ref` / `agent_engine` wiring,
   `pkg/memory/memorybank` with opt-in consolidation, then `pkg/memory/sqlfts`
   and `extras/redis-memory/`).
7. **Failover UAT** in `dev/uat/failover/`: two daemons share a backend;
   kill the owner mid-turn and assert:
   - the other instance resumes within `staleAfter`;
   - auto-continue re-drives the turn;
   - the SSE cursor carries on without a gap or a repeat;
   - the dead owner, revived via SIGSTOP/SIGCONT, writes nothing.

   Run it on GKE with Agent Engine and Kubernetes leases, two replicas,
   killing a pod. Plus a `dev/smoke/` hermetic version on miniredis.

Each backend phase also adds:
- `state.*` config and flags,
- `env.yaml` entries, with credential env names ending in `_env` (#764),
- a site page update and a CHANGELOG bullet.

## Config surface (sketch)

```json
{
  "state": {
    "transcript": { "backend": "agentengine",
                    "agentengine": { "reasoning_engine": "projects/p/locations/us-central1/reasoningEngines/123",
                                     "session_ttl": "720h" } },
    "control":    { "backend": "kubernetes", "kubernetes": { "namespace_env": "POD_NAMESPACE" } },
    "index":      { "backend": "kubernetes" },
    "memory":     { "backend": "memorybank", "memorybank": { "consolidate": "off" } },
    "lease":      { "heartbeat": "5s", "stale_after": "30s", "advertise_url": "https://$(POD_IP):8443" }
  }
}
```

`index` and `memory` inherit the connection block of the same backend
named elsewhere in `state` (or the shared top-level `agent_engine` block
defined in [`memory-service-design.md`](memory-service-design.md), where full
`memory` scope and subagent configuration lives), so a deployment doesn't
repeat the `ReasoningEngine` resource name. The existing `--session-db` /
`--session-db-path` flags stay as shorthand for "everything is sql at this
path".

## Out of scope

- Two instances running one session at the same time, and any form of
  multi-writer merge.
- Proxying requests to the owner (decision 12).
- Moving a turn mid-flight. A dead owner's turn is re-driven by
  auto-continue, not resumed.
- Background subagents surviving the death of their parent's instance.
- Moving the inbox out of process (revisit if the claim-TTL gap turns
  out to matter in practice).
- Migration tools that copy sessions between backends.
- ADK `app:` / `user:` state prefixes. Nothing in the tree uses them.
- Cross-region replication. That is the store's job, if it offers it.
- Postgres/MySQL dialector selection. The package doc claims support,
  but there is no driver in `go.mod`. That is a separate small fix, not
  this design.

## Open questions

1. **Filtering Memory Bank by `Kind` / `Topics` and multi-agent sharing.**
   *Settled in [`memory-service-design.md`](memory-service-design.md):*
   Memory Bank's exact-match `scope` (`<= 5` keys, no wildcards) maps to
   explicit private (`scope_kind="agent"`, `agent_id`) and shared
   (`scope_kind="shared"`, `pool`) mounts queried via concurrent fan-out
   on `memory.Router`, with topics stored on `Memory.Topics` and
   post-filtered when needed.
2. **Lease count limits.** Before phase 4 lands, check whether GKE
   Autopilot or cluster policies cap `Lease` objects per namespace, and
   what the apiserver write rate is at 50 resident sessions.
