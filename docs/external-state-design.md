# External state: pluggable session, control and memory stores for failover

**Status:** proposed. No code yet. Open questions 1–4 of the first
draft were settled by the maintainer on 2026-10-01 (decisions 13–16).

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

11. **Memory stays the #948 `memory.Memory` interface.** Memory Bank and
    Redis AMS are adapters behind it, not ADK's `memory.Service`. ADK's
    interface returns no IDs, has no forget, and its Go Memory Bank
    client scopes only by user with a top-K of 3. The #948 settled
    decisions still apply, in particular that **every `Remember` /
    `Forget` emits a transcript event** whichever backend recalls it.
    The one exception is the auto-capture rule, which decision 15
    reopens.

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

    `shared-memory-design.md` carries a pointer to this decision.

16. **The stores are in-tree; the memory adapters follow #948.**
    - **In-tree, under `pkg/statestore/{sql,agentengine,redis,kubernetes}`:**
      the Agent Engine, Redis and Kubernetes transcript, control and
      index backends. GKE images are the main consumer, and an
      out-of-tree store would keep missing the conformance suite.
    - **In-tree:** the Memory Bank adapter, because it shares the
      `aiplatform` client with the transcript backend.
    - **`extras/redis-memory/`:** the Redis AMS adapter, as #948
      planned.

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
- **Memory Bank:** via `apiv1beta1.MemoryBankClient` (beta-only in Go):
  - `Remember` → `CreateMemory`, with scope `{app, user, namespace}`;
  - `Recall` → `RetrieveMemories` with `SimilaritySearchParams{TopK}`;
  - `Forget` → `DeleteMemory`.
  - `Kind` and `Topics` have no native field. See open question 1.
  - Opt-in consolidation → `GenerateMemories` with a
    `VertexSessionSource` (decision 15). Memory Bank reads the event
    `content`, which decision 6 populates.
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
6. **Memory:** #948 lands (in-tree FTS5). Then the Memory Bank adapter,
   with opt-in consolidation (decision 15). Then the Redis AMS adapter
   in `extras/`.
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
named elsewhere in `state`, so a deployment doesn't repeat it. The
existing `--session-db` / `--session-db-path` flags stay as shorthand
for "everything is sql at this path".

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

1. **Filtering Memory Bank by `Kind` / `Topics`.** Memory Bank has no
   field for either, and `RetrieveMemories` requires an exact scope
   match. One option is a scope key per kind (`{app, user, namespace,
   kind}`), which means one retrieve per requested kind. The other is
   storing kind and topics in the fact text and post-filtering on the
   client, which spends top-K on items that get thrown away. Lean: a
   scope key for `Kind`, post-filter on `Topics`. Settle in phase 6.
2. **Lease count limits.** Before phase 4 lands, check whether GKE
   Autopilot or cluster policies cap `Lease` objects per namespace, and
   what the apiserver write rate is at 50 resident sessions.
