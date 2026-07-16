# Vertex context caching: eager system-prompt cache

Design doc for wiring Vertex explicit context caching around the
stable prefix of every generation call (SystemInstruction + Tools +
skill-reference content) so multi-turn sessions don't re-pay the
input-token cost of a 4–8k prefix on every turn.

**Status:** proposed (2026-07-16). Awaiting approval before
implementation.

**Tracking issue:** [#221](https://github.com/go-steer/core-agent/issues/221)

**Depends on:** #128/#130 shipped (structural MCP digest) — the
measurement question that gated this doc is now answered: MCP output
compression alone doesn't erase prefix-repeat cost. Cached prefix is
still the bigger lever for multi-turn sessions with fixed
system-instruction + tools + skills.

## Why this doc exists

Every turn in a core-agent session resends the same prefix:

- `SystemInstruction` — AGENTS.md contents, ~1–2k tokens
- `Tools` — every function schema (built-in + MCP tools), ~150–300
  tokens each × N tools. For the GKE-triage recipe this is ~3–5k
  tokens (GKE MCP alone contributes ~20 tools)
- Skill reference content — the fallback catalog + any skill-specific
  reference blocks eagerly loaded on skill invocation, ~1–3k tokens

Total prefix on GKE-triage today: **~4–8k tokens per turn, unchanging
across the session**. At `gemini-3.5-flash` rates ($1.50/M input) that
runs $0.006–0.012 per turn just for the prefix — dominates cost for
early turns before conversation history grows.

Vertex ships two caching flavors:
- **Implicit caching** (already benefit): opportunistic, prefix-based,
  partial reporting via `cachedContentTokenCount`. Works well when the
  prefix genuinely repeats byte-for-byte and the backend chooses to
  cache it. Not guaranteed.
- **Explicit caching** (this doc): operator creates a `CachedContent`
  resource once, references it by name on subsequent generation calls.
  Rate is 10% of input (per LiteLLM's `cache_read_input_token_cost`
  now wired in [`internal/pricing/builtin.go`](../internal/pricing/builtin.go)
  as of #259 Slice A). Guaranteed cache-hit on every referencing call.

**Expected impact** (GKE-triage recipe, per the plan's back-of-envelope
from [`backlog-cost-stack-2026-07-14.md`](backlog-cost-stack-2026-07-14.md)):
- Baseline post-#130: $0.05–$0.08/session
- + explicit prefix cache: **~$0.03/session** (~2× further reduction)

## Scope: v1 = system-prompt-only

Deliberate simplification vs the full #221 vision:

**In scope for v1:**
- Cache the fixed prefix at agent startup (SystemInstruction + Tools
  + skill reference content).
- Reference the cache handle on every subsequent generation call.
- TTL managed by daemon; refresh on session touch when remaining <
  30min.
- Fail-safe: any cache RPC error → structured stderr alert + fall back
  to uncached on that turn. Never break the session.

**Deferred (revisit only if telemetry justifies):**
- Conversation-history caching (prefix grows every turn; requires
  cache-invalidation-on-content-change lifecycle — much more state
  to manage)
- Cross-session cache sharing (per-session in v1; shared cache is a
  v2 concern if `#82` measurement shows cache-creation cost is
  meaningful across sessions)
- Dynamic prefix detection (any content that repeats > N turns gets
  cached automatically). Nice-to-have, not needed for the demo.

## Design

### Cache lifecycle

Three states, three transitions. Simple because we're only caching
content that's fixed at agent startup:

```
     agent.New()
         │
         ▼
    ┌─────────┐   create Vertex cache      ┌────────┐
    │  START  │──────────────────────────▶ │ ACTIVE │
    └─────────┘   (async, non-blocking)    └────┬───┘
                                                │
                                    session     │  TTL < 30min
                                    unregister  │  during session touch
                                    or panic    │
                                          ┌─────┴──────┐
                                          ▼            ▼
                                     ┌────────┐   ┌─────────┐
                                     │ DELETE │   │ REFRESH │
                                     └────────┘   └─────────┘
                                                       │
                                                       ▼
                                                   (back to ACTIVE)
```

- **START → ACTIVE**: on agent construction, background goroutine
  calls `caches.Create(...)` with prefix content. On success, stores
  the returned cache name on the agent. On failure, logs a structured
  warning and marks the agent as "no cache" — subsequent generation
  calls skip the cache reference field. Non-blocking: turn 1 must not
  wait for cache creation.
- **ACTIVE → REFRESH**: session touch (any turn) checks remaining TTL.
  If under 30min, background goroutine calls `caches.Update(...)` to
  extend by the daemon-configured TTL (default 6h).
- **ACTIVE → DELETE**: on session unregister (SSE disconnect + grace)
  or daemon shutdown, best-effort `caches.Delete(...)`. Not a
  correctness requirement — Vertex reaps expired caches — but keeps
  the account tidy.

### Cache content

The v1 cached block includes exactly the content that's fixed at
agent construction:

- `SystemInstruction`: `AgentBuilder.systemInstruction()` output — a
  single string composed from `AGENTS.md` + agent name + description.
- `Tools`: the full `[]*genai.Tool` slice built by
  `AgentBuilder.tools()`, which includes built-in tools, agentic
  wrappers, and every MCP tool discovered at Build time. Note:
  changing the MCP surface (a new MCP server added mid-session)
  invalidates the cache — v1 accepts this and rebuilds on next
  session; multi-session daemons that hot-reload MCP will need a
  cache-invalidation hook in v2.
- `SystemInstructionAdditions`: any skill catalog reference blocks
  that the agent's `SkillProvider` eagerly loads at startup. Skills
  that lazy-load references (loaded on-demand from within a turn) are
  NOT in the cache; that's expected — they're by definition not part
  of the fixed prefix.

Explicitly NOT in the cache:
- Conversation history (`Contents`) — grows every turn.
- Per-turn generation config overrides (temperature, max tokens) —
  passed on each call.

### Integration points

Three files touched:

1. **`pkg/models/vertex/vertex.go`** (or wherever we call
   `client.Models.GenerateContent` / `StreamGenerateContent`): accept
   an optional cache handle in the request config. When present, set
   `GenerateContentConfig.CachedContent = <cache_name>`. When absent,
   behavior identical to today.
2. **`pkg/agent/agent.go` (`agent.New` / `AgentBuilder.Build`)**: after
   building the tools slice + system instruction, kick off a
   background goroutine that calls a new `cacheManager.Init(ctx,
   systemInstruction, tools)`. Cache manager stores the resulting
   cache name; a getter is threaded through to `Generate` calls.
3. **`internal/vertexcache/manager.go`** (NEW): thin manager owning
   the cache-lifecycle state machine. Exposes `Init`, `Handle`,
   `Refresh`, `Delete`. All Vertex `Caches.*` RPCs live here.

### Config

New fields on `cfg.Model`:

```go
ContextCache *ContextCacheConfig `json:"context_cache,omitempty"`

type ContextCacheConfig struct {
    Enabled bool           `json:"enabled,omitempty"`        // default true
    TTL     Duration       `json:"ttl,omitempty"`            // default 6h
    Refresh Duration       `json:"refresh,omitempty"`        // default 30min
}
```

CLI kill switch: `--no-context-cache` (matches the `--no-mcp-digest`
pattern from #257 — a single flag operators can flip when debugging
a Vertex issue without editing config).

### Failure modes

Every failure path degrades to "no cache" for the affected turn,
never breaks the session:

- **`caches.Create` fails**: logged, agent runs uncached for its
  lifetime. No retry loop in v1 (retries hide real problems from
  operators; the structured log is enough).
- **`caches.Get`/`Update` returns NotFound mid-session** (backend
  reaped early or another process deleted): logged, cache handle
  cleared, agent runs uncached for remaining turns.
- **`GenerateContent` returns "cache not found"**: logged, retry once
  without the cache reference on the same turn. Matches the pattern
  used by #239 (empty-response retry).

## Verification

Three layers of test coverage:

1. **Unit** (`internal/vertexcache/manager_test.go`): state machine
   transitions on faked `Caches` client. Init happy path, Init error,
   Refresh under-threshold, Refresh over-threshold, Delete. Faked
   client validates the request shape (cache name in request, TTL
   values, referenced model matches session model).
2. **Integration** (`pkg/agent/agent_cache_test.go`): agent.New with
   cache enabled → verify the first Generate call carries
   `CachedContent`, subsequent calls carry the same handle, and a
   forced Create-error path degrades to no-cache without failing the
   agent.
3. **End-to-end** (manual + baseline capture): drive the same GKE-
   triage session used for the #82 baseline. Compare
   `/sessions/<sid>/usage` `overall.input_tokens_cached` before vs
   after. Target: prefix tokens (~4–8k × N turns) all reported as
   cached from turn 2 onward.

Append the numbers to `docs/backlog-cost-stack-2026-07-14.md`
measurements section — this is the follow-through gate on whether
#221 was worth its complexity.

## Non-goals

- **Not** a general prefix-detection framework. Only the fixed
  startup prefix. Dynamic detection = v2.
- **Not** cross-session cache sharing. Per-session ownership only.
  Requires a shared registry, TTL coordination, and multi-tenancy
  thinking that's out of scope.
- **Not** the LLM-subagent MCP wrap (that's #223 / `agentic-mcp-design.md`).
  Separate axis: this doc reduces prefix cost per turn; #223 reduces
  MCP-response cost per tool call. They compose cleanly.

## Related files

- Vertex model client: [`pkg/models/vertex/vertex.go`](../pkg/models/vertex/vertex.go)
- Agent construction: [`pkg/agent/agent.go`](../pkg/agent/agent.go)
- Pricing wiring (cache-read rate landed 2026-07-15): [`internal/pricing/builtin.go`](../internal/pricing/builtin.go)
- Backlog measurement gates: [`docs/backlog-cost-stack-2026-07-14.md`](backlog-cost-stack-2026-07-14.md)
- ADK Go SDK caching API reference: TBD — verify the SDK exposes
  `Caches.Create`/`Get`/`Update`/`Delete` before writing manager code.
  If the SDK is missing this surface, we need to fall back to raw REST
  against the Vertex API (`v1/projects/.../cachedContents`).
