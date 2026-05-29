# Shared memory: a Memory interface + audit-derived recall + Redis AMS extras

Design doc for the cross-session + cross-agent memory layer.
Untracked sibling to [`attach-mode-design.md`](attach-mode-design.md),
[`peer-registration-design.md`](peer-registration-design.md),
[`scheduled-monitoring-design.md`](scheduled-monitoring-design.md),
[`bidirectional-mcp-design.md`](bidirectional-mcp-design.md),
[`code-mode-design.md`](code-mode-design.md). Brainstormed
2026-05-22 while comparing surface area against Hermes Agent,
Redis Agent Memory Server, OpenClaw (Mem0/Hindsight extensions),
and the Honcho user-modeling project.

## Context

Cross-session + cross-agent memory is the category that's
coalescing in the agent-framework space right now. The shape that
has emerged:

- **Two tiers.** Working memory (per-session, ephemeral, the
  current conversation) and long-term memory (persistent,
  searchable, typed: semantic / episodic / message / preference).
- **Hybrid retrieval.** Vector similarity + BM25 keyword, blended
  via an alpha parameter, with filters on user / namespace /
  type / timestamps.
- **MCP-native.** Tools like `create_long_term_memory` and
  `search_long_term_memory` exposed as MCP, so any MCP-capable
  agent can wire one in via `.agents/mcp.json` with zero
  framework integration.
- **Background extraction.** An LLM rolls up working memory into
  structured long-term records (Redis AMS calls this "memory
  strategies"; OpenClaw calls it "Dreaming"; Mem0 calls it
  "auto-capture").

Reference systems we surveyed:

- **Redis Agent Memory Server** (`github.com/redis/agent-memory-server`)
  — Apache 2.0, Python, pre-1.0 (v0.15.2). Two-tier + hybrid
  search + MCP server (stdio + SSE) + REST + Python SDK. Scopes
  by `user_id` + `namespace` + `session_id`. **No native
  agent-identity field** — multi-agent sharing is convention.
- **OpenClaw** default — file-based (`MEMORY.md` + daily logs +
  `DREAMS.md`) with `memory_search` / `memory_get` tools. Per-agent
  silos; the "your agents are strangers" gap is exactly what the
  shared-memory third parties target.
- **Hindsight** — extension that adds shared memory *banks* across
  OpenClaw agents.
- **Mem0** — moves memory out of the agent loop entirely:
  auto-capture after every turn, auto-recall before every turn,
  no `memory_search` tool call required.
- **Honcho** (Hermes-side) — "dialectic user modeling" — per-user
  persona that thickens across sessions.

Where we already stand:

- The **eventlog** (SQLite-backed, durable, append-only) records
  every model turn + every tool call. It is the ground truth for
  what happened.
- **Skills** are operator-authored markdown + structured tools —
  *never executable code*, by design (see `attach-mode-design.md`
  and the security thesis on permission-gating).
- **`peer-registration`** (v1.7+) gives us a hub that already
  knows the membership of a fleet — natural place to coordinate
  shared namespaces.
- **Attach-mode** is a working HTTP/SSE listener — natural place
  to expose a `/memory/search` operator endpoint.

What's missing is the connective tissue: an interface that
something *can* implement; an in-tree default that derives recall
from the eventlog; an extras adapter that talks to Redis AMS for
fleet-scale shared memory.

### The audit-derived-memory thesis

Every existing memory system treats **memory** and **audit /
observability** as separate write paths. The model "remembers"
into one substrate; the operator audits a different one. Those
two can diverge — the audit log shows the model did X but its
memory says it did Y.

In our model, the in-tree default (`memory.FromEventlog(handle)`)
makes that divergence structurally impossible:

> Recall comes from the same immutable event stream that auditors
> and live-tail observers see. Whatever the model "remembered" is
> exactly what the operator can prove it did.

This is the unique-in-market claim. The extras Redis AMS adapter
preserves the property by an additional rule: every `Remember`
call also lands as an eventlog entry, so even when the *recall*
substrate is external (Redis), the *audit trail* still
canonicalizes through the eventlog. Memory writes can not happen
without an audit record.

### Settled decisions (do not relitigate)

- **`Memory` interface in `package memory`, two implementations.**
  Same pattern as `tools.Scheduler` (in-tree
  `SleepScheduler`/`ExitOnDeferScheduler` + extras-able to NATS)
  and `agent.RemoteAgentSpawner` (interface in core, Scion impl
  in extras).
- **In-tree default: FTS5 + vector-free.** Derives recall from
  the existing eventlog. Pure SQLite (gojq + sqlite already in
  the dependency set, no new heavy deps). Single-binary
  deployment, zero ops. Trade: keyword + BM25 only — no semantic
  vector recall in-tree.
- **Extras: `extras/redis-memory/`.** Thin adapter over Redis
  AMS's MCP surface. Brings vector + hybrid search for the
  fleet/multi-agent case. Adds Redis + a separate Python process
  to the operator's deployment.
- **Memory payloads are markdown / structured text — never
  executable code.** Matches the skills-aren't-code thesis.
  Agent-authored memory is *content* the agent reads in a future
  turn, not *code* a future-self imports.
- **Memory kinds match Redis AMS verbatim: `semantic`,
  `episodic`, `message`, `preference`.** Clean adapter mapping
  wins over naming purity; we don't get to define this taxonomy
  alone, and divergence costs interop.
- **Multi-agent sharing via the peer-registration hub.** A
  fleet's hub assigns a shared `namespace` (default:
  `<hub-name>:<role-label>`); peer agents pick it up from the
  hub on register. No per-agent ACLs in v1 — within a namespace,
  all peers see all memories. Hub-led so it's discoverable, not
  configured-by-hand on every peer.
- **Every `Remember` / `Forget` goes through `permissions.Gate`.**
  Same gate that wraps every tool call. The `recall_memory` tool
  is permission-checkable like any other.
- **Every `Remember` emits an eventlog entry**, even when the
  recall substrate is external. Preserves the audit-derived-memory
  property uniformly across implementations.
- **No agent-authored *executable* memory.** Out of scope (and
  conflicts with the security thesis). Agent-authored *content*
  is fine.
- **No weight updates / RL.** "Self-improvement" via curation
  only, like Hermes.
- **No Mem0-style implicit auto-capture in v1.** Memory writes
  happen because the model called `persist_memory` (a tool), not
  because a background process scraped the turn. Revisit in v2
  once we have telemetry on adoption + a consumer asking.

## The `Memory` interface

```go
// package memory

// Kind classifies a memory item. Matches Redis AMS's vocabulary
// verbatim so the extras adapter is a one-to-one translation; the
// in-tree backend tolerates additional consumer-defined kinds via
// the Tags channel rather than expanding this enum.
type Kind string

const (
    KindSemantic   Kind = "semantic"   // durable facts ("user prefers metric units")
    KindEpisodic   Kind = "episodic"   // events at a point in time ("agent X ran kubectl apply at T")
    KindMessage    Kind = "message"    // conversation snippet ("user said: ...")
    KindPreference Kind = "preference" // tagged semantic memory ("topics: ['preferences']")
)

// Scope identifies where a memory item lives. SessionID is set for
// per-session items (Kind=Message commonly); namespace + agent-id
// scope long-term items. Empty SessionID means "long-term, not
// bound to a session."
type Scope struct {
    Namespace string // shared across a fleet, set by the hub
    AgentID   string // unique within Namespace; PeerRegistry.RegistrationID
    SessionID string // optional; "" = long-term
    UserID    string // optional; for per-user persona scoping
}

// Item is a single persistable memory. Body is markdown / plain
// text; never code. Topics are free-form tags used for filtering.
// EventDate is meaningful for Kind=Episodic.
type Item struct {
    ID        string    // implementation-assigned on Remember
    Kind      Kind
    Body      string
    Topics    []string
    EventDate time.Time // zero unless Kind=Episodic
    Scope     Scope
    CreatedAt time.Time // implementation-assigned
}

// Query filters Recall. All fields are AND-combined. TopK caps
// the result set; zero means implementation default (10 in tree).
type Query struct {
    Text         string   // hybrid: FTS5 in-tree, vector+BM25 in extras
    Kinds        []Kind   // empty = all
    Topics       []string // any-of
    Namespace    string   // "" = caller's
    AgentID      string   // "" = any agent in namespace
    UserID       string   // optional persona filter
    Since, Until time.Time
    TopK         int
}

// Memory is the abstraction. Lives in package memory; consumers
// inject one into agent.New via agent.WithMemory(m).
type Memory interface {
    Remember(ctx context.Context, item Item) (Item, error)
    Recall(ctx context.Context, q Query) ([]Item, error)
    Forget(ctx context.Context, id string) error
    Close() error
}
```

Notes on shape:

- **No `Summarize` method.** Recall returns raw items; a separate
  `summarize_memories` tool (model-callable) takes a Query +
  asks the model to roll up matched items inline. That keeps the
  Memory interface narrow + lets the model decide when summary
  cost is worth paying.
- **`Forget` is permissive but audited.** Always emits an
  eventlog entry; the in-tree backend tombstones rather than
  hard-deletes so audit-driven recall sees a "memory X was
  forgotten at T" record.

## In-tree implementation: `memory.FromEventlog(handle)`

```go
m := memory.FromEventlog(eventlogHandle, memory.WithGate(gate))
agent.New(..., agent.WithMemory(m))
```

### Storage

- One additional SQLite virtual table: `memory_fts5` over a new
  `memories` table with `(id, kind, body, topics_json, agent_id,
  namespace, session_id, user_id, event_date, created_at,
  tombstoned_at)`.
- `INSERT INTO memories` is the canonical write; an `AFTER
  INSERT` trigger maintains the FTS5 index.
- **Every `Remember` also writes an eventlog row** with
  `Author="memory/remember"`, `CustomMetadata={memory_id: "..."}`.
  This is the audit-derived property: the eventlog is still the
  ground truth for "what happened," and `memories` is a
  query-optimized projection over it.
- **`Forget` writes an eventlog row** (`Author="memory/forget"`)
  and updates `tombstoned_at`; the FTS5 index excludes
  tombstoned rows from `Recall` results.

### Recall

- BM25 ranking from FTS5 + post-filter on Kind / Topics / time
  window / scope.
- No vector embeddings in-tree (deferred — `gojq` + `sqlite` are
  the only "real" deps today; adding a vector store would bring
  in a vendored embedding model or a network call per write).
  Consumers needing semantic recall use the Redis AMS adapter.

### Why this design

- **Zero new deps.** Pure SQL + FTS5, both already in our
  sqlite driver.
- **Audit-derived for free.** The `memories` table is *derivable*
  from the eventlog — a future migration could regenerate it by
  replaying `memory/remember` events. Memory and audit cannot
  diverge.
- **Single-binary deployment** unchanged. Distroless K8s pods
  still ship one Go binary; no Python sidecar, no Redis.

## Extras implementation: `extras/redis-memory/`

```go
import "github.com/go-steer/core-agent/extras/redis-memory/redismem"

m, err := redismem.NewMCP(ctx, redismem.Config{
    Endpoint:    "https://memory.svc:8000/mcp",
    Token:       os.Getenv("MEMORY_TOKEN"),
    Namespace:   "monitor-prod:cluster-a", // from hub
    AgentID:     hubRegistration.ID,
    EventLog:    handle, // for audit mirroring
})
agent.New(..., agent.WithMemory(m))
```

### Translation table

| `memory.Memory` call | Redis AMS MCP tool |
|---|---|
| `Remember(item)` (Kind=semantic/episodic/preference) | `create_long_term_memory` with `topics`, `entities`, `event_date` |
| `Remember(item)` (Kind=message) | `put_working_memory` |
| `Recall(q)` | `search_long_term_memory` with `search_mode=hybrid`, `hybrid_alpha=0.7` |
| `Forget(id)` | `delete_long_term_memory` |

### Audit mirroring

Every `Remember` first calls Redis AMS, then writes an eventlog
row matching the in-tree shape. If the AMS call fails, no
eventlog row is written and `Remember` returns the error — we'd
rather drop the memory than have an audit record of a write that
didn't happen.

### Scope translation

- `Scope.Namespace` → AMS `namespace`.
- `Scope.AgentID` is encoded into `topics` as
  `agent:<RegistrationID>` (AMS has no native agent field; this
  is the convention we adopt).
- `Scope.UserID` → AMS `user_id`.
- `Scope.SessionID` → AMS `session_id`.

## Multi-agent shared-namespace convention

When `peer-registration` (v1.7+) is active, the hub coordinates
shared-namespace assignment:

- Hub config holds a default `memory_namespace` (e.g.
  `monitor-prod`); peers receive it in their
  `RegisterAndHeartbeat` response (additive field on the
  existing `Peer` body — backward-compatible).
- Peers append `:<role>` from their own labels, yielding e.g.
  `monitor-prod:cluster-a`. Predictable + grep-able.
- Within a namespace, **all peers see all memories** in v1.
  Per-agent ACLs are out of scope; operators wanting separation
  run separate hubs or pick distinct namespaces.

A peer that has not registered (standalone agent) defaults to
`namespace = <appName>:local`. That keeps local dev usable
without forcing a hub.

## Composition with existing primitives

| Primitive | Interaction |
|---|---|
| **`eventlog`** | In-tree backend's storage substrate + universal audit substrate (extras backend mirrors writes here). |
| **`permissions.Gate`** | Every `Remember` / `Forget` goes through it. The `recall_memory` tool is gate-checked like any other. |
| **`attach-mode`** | New optional `GET /memory/search?q=...&kind=...&topk=...` endpoint on the listener for operator-side queries (mirrors Redis AMS's REST). Returns `[]Item` as JSON. ReadOnly mode disables nothing — search is read-only. |
| **`peer-registration`** | Hub coordinates `memory_namespace`; peers pick it up on register. |
| **`Scheduler`** | A future `consolidate_memories` autonomous loop can be scheduled to roll up `Kind=Message` items into `Kind=Semantic` periodically — the "Dreaming" pattern from OpenClaw, explicit and operator-controlled. Out of v1 scope; flagged here to make sure the interface doesn't preclude it. |
| **Built-in tools** | New `persist_memory(body, kind?, topics?)` + `recall_memory(query, kind?, topk?)` + `forget_memory(id)` registered when `agent.WithMemory(m)` is set. Tool descriptions include the cadence + scope guidance the way `schedule_next_turn` does. |

## Implementation sketch

About **600–800 LoC + tests** total, split across two PRs.

### PR #14 — in-tree (depends on attach-config / PR #13 landing)

- `memory/memory.go` — interface + types (~120 LoC).
- `memory/eventlog_backend.go` — FTS5 schema, INSERT triggers,
  Recall ranking, tombstoning, audit mirroring (~300 LoC).
- `memory/eventlog_backend_test.go` — round-trip, FTS5 ranking
  correctness, tombstone behavior, gate enforcement, eventlog
  audit-row presence (~250 LoC).
- `agent/agent.go` — `WithMemory(m)` Option + register the
  `persist_memory` / `recall_memory` / `forget_memory` tools
  (~80 LoC).
- `attach/handlers.go` — optional `GET /memory/search` (~60 LoC).
- `attach/integration_test.go` — `/memory/search` smoke (~80 LoC).
- `cmd/core-agent/main.go` — wire `WithMemory(memory.FromEventlog(handle))`
  when `--session-db` is set (~20 LoC).
- `docs/site/content/docs/` — new "Shared memory" page +
  CHANGELOG entry under `[Unreleased]`.

### PR #15 — `extras/redis-memory/` (stacks on PR #14)

- `extras/redis-memory/redismem/client.go` — MCP client +
  translation (~200 LoC).
- `extras/redis-memory/redismem/client_test.go` — translation
  table correctness against a mock MCP server (~150 LoC).
- `extras/redis-memory/example/` — minimal driver that spins up
  Redis AMS via docker-compose + a hub + two peers sharing a
  namespace + smoke that one peer's `Remember` is visible to the
  other's `Recall`.
- `dev/smoke/12-shared-memory-redis.sh` — the docker-compose +
  end-to-end smoke (~120 LoC).

## Open questions

1. **Namespace assignment policy.** Hub-led (decided above) vs.
   operator-set on every peer vs. label-driven. Lean: hub-led
   with a peer-side override flag (`--memory-namespace`) for
   the operator-needs-different-pool case.
2. **Bidirectional MCP intersection.** Once `core-agent` exposes
   itself as an MCP server (per `bidirectional-mcp-design.md`),
   does `recall_memory` get re-exported as a memory tool to
   *other* MCP clients? Probably yes for the in-tree case (free
   property, no new code); explicit opt-in for the extras case
   (we'd be republishing somebody else's memory service).
3. **Consolidation / Dreaming.** Scheduled rollup from
   `Kind=Message` → `Kind=Semantic` using the model itself. Punt
   to v2; design the `Memory` interface so it's additive (a
   `Consolidate(strategy)` method we add later doesn't change
   `Remember` / `Recall` shape).
4. **Embedding-free vs. embed-on-write.** In-tree skips
   embeddings to stay zero-dep. If a consumer needs semantic
   recall without taking on Redis AMS, the next step is a
   `memory.WithEmbedder(e Embedder)` option + a vector column.
   Skip in v1; revisit on first ask.
5. **Cross-namespace search.** A supervisor agent that wants to
   recall across all monitor namespaces — `Query.Namespace="*"`?
   Lean: yes, gate-checked separately (`permission:
   memory:cross-namespace`). Worth deciding before PR #14
   freezes the Query shape.
6. **Honcho-style user personae.** `Scope.UserID` is on the
   interface; an `extras/honcho/` adapter could implement a
   richer per-user model on top. Captured but not in scope here.

## Out of scope (v1)

- **Weight updates / RL.** "Self-improvement" is curation only;
  no online training, no fine-tuning.
- **Mem0-style implicit auto-capture.** Writes happen because the
  model called `persist_memory`. A background "capture every
  turn" mode is interesting but adds a class of "memory you
  didn't know was written" failures that operators will have to
  reason about. Revisit on consumer ask.
- **Per-agent ACLs within a namespace.** Within a shared
  namespace, every peer sees every memory. Operators wanting
  separation pick distinct namespaces or run separate hubs.
- **Agent-authored *executable* memory.** Bodies are markdown /
  plain text. No skill-import-time-Python class of attack.
- **Honcho persona modeling.** Adapter in `extras/honcho/` if a
  consumer asks; punt out of core.
- **Messaging-platform delivery of memory events.** We don't have
  a gateway; not our category.
- **Cross-cluster federation.** Hubs don't share memory namespaces
  with peer hubs in v1; an operator with two clusters runs two
  Redis AMS deployments or pre-coordinates namespaces by hand.

## Why this puts us in a unique position

The headline claim, once shipped:

> **core-agent is the only agent framework where shared memory
> is *derived from* the same eventlog that audits behavior — not
> a separate write path. Whatever the model "remembered" is
> exactly what the operator can prove it did.**

Concretely, the unique combination is:

1. **Multi-agent topology already structured** — peer-registration
   hub-and-spoke (v1.7+) plus AX for the distributed-runtime case.
2. **Audit-grade eventlog** — every model turn and every tool
   call already lands in SQLite, queryable, durable.
3. **Permission gate** wraps every memory write the same way it
   wraps every tool call.
4. **Single-binary in-tree backend** for the simple case;
   **MCP-native extras adapter** for the fleet case. No
   forced-in-Python dependency, no forced-in-Redis dependency
   for consumers who don't need them.

No other framework in the comparison set (Hermes, OpenClaw +
Mem0/Hindsight, Redis AMS standalone, Honcho standalone) has all
four. Hermes and OpenClaw have memory but no structural audit
property and no built-in fleet coordination. Redis AMS and Mem0
have memory + retrieval but are runtime-agnostic — they don't
know about the agent's audit trail or its peers. We sit at the
intersection.
