# Cost-reduction plan: post-v2.7.0-dev.2 (2026-07-14)

Session handoff. Filed after wrapping the v2.7.0-dev.N demo drive on 2026-07-14. Fresh session should pick up from **first action** at the bottom.

## Context

The v2.6 GKE-troubleshoot demo drive surfaced that the recipe's cost per triage session is **$0.28** (10 turns, 181k input tokens). At 100 incidents/day that's ~$840/mo — high enough that operators will notice, low enough that they haven't yet. We shipped all recipe-breaking bugs in the v2.7.0-dev.N series (see `git log v2.6.0..main`); cost is now the biggest remaining ticket.

Four cost levers were surfaced. They stack multiplicatively:

| Lever | Cuts what | Est. reduction | Issue | Task |
|---|---|---|---|---|
| Per-turn UsageMetadata on `/usage` | (attribution, not cost) | 0× (measurement) | #222 | #82 |
| `pkg/digest` skeleton (JSON pruner, store) | Foundation | 0× (enables next) | #128 | #83 |
| Structural pruner in MCP wrap + `retrieve_raw` safety net | JSON-shaped MCP responses (GKE MCP is 100% JSON) | 5-10× on MCP outputs, zero latency | #130 + #129 | #84 |
| Vertex context caching | Stable prefix repeated every turn | ~3-4× on prefix cost | #221 | #85 |

**Not in stack** (parked): #124/#223 (LLM subagent MCP wrap) — with #130 shipped, subagent digest is the marginal case for prose-only MCP responses. Revisit only if telemetry shows a meaningful prose slice.

## Sequencing decision: Option 3 — meta-first

Agreed 2026-07-14. Rationale: the demo's cost problem is dominated by **MCP output size**, not prefix-repeat waste. #130 is deterministic (fewest failure modes), lands fastest per line of code, and gives measurement data to decide whether #221's lifecycle complexity is worth 2-3 days.

Rough back-of-envelope (GKE-triage recipe):
- Baseline: $0.28/session
- + #221 alone (fixed prefix cached): ~$0.15
- + #130 (GKE MCP JSON structurally pruned before context): ~$0.05 to $0.08
- Combined #130 + #221: ~$0.03

If #130 gets us to <$0.05 alone, #221 saves ~$0.02 more — probably not worth the lifecycle-management complexity. If #130 lands at $0.10-$0.15, ship #221.

**Sequence:**
```
#82 (measure) → #83 (foundation) → #84 (wire + safety) → measure again → #85 (empirical decision on #221)
```

## Settled design decisions

**#221 Vertex context caching (if we do it):**
- **Cached block**: `SystemInstruction` (AGENTS.md) + `Tools` (all schemas incl. MCP) + skill reference content. NOT conversation history.
- **Creation**: eager on session-register (background goroutine, don't block turn 1)
- **Sharing**: per-session in v1. Revisit shared-cache only if #82 shows cache-creation cost is meaningful.
- **TTL**: match session idle timeout (default 6h). Refresh on session touch when remaining < 30min. Delete on unregister.
- **Failure modes**: never break the session. Any `Caches.*` error → structured stderr alert + fall back to uncached. Same "log/recover/persist" pattern as #239 empty-response retry.
- **Backend prereq**: empirically confirm gemini-3.5-flash caching works on both Vertex + direct Gemini API. One `Caches.Create` call, check non-error. ~5 min experiment before writing any lifecycle code.

**#128 pkg/digest:**
- Router: `passthrough` under threshold, `structural` for JSON-shaped, `llm_fallback` for prose/unknown
- Structural JSON pruner: preserve identifier-shaped keys (apiVersion/kind/metadata.name/etc.), preserve small arrays, collapse long strings, truncate arrays > N with `"…N more"` marker, depth cap on recursion
- Store interface: `FilesystemStore` + `EventlogStore` implementations
- Zero I/O in the pure functions — testable without a session DB
- Design: `docs/digest-design.md`

**#130 wiring:**
- Wire `digest.Process` into `pkg/mcp/lifecycle.go:Build` first (biggest cost surface for GKE recipe)
- Also wire into the four `pkg/tools/agentic` wrappers using the same primitive
- LLMFallback = `agent.RunSubtask` (existing subagent digester used today by agentic wrappers)
- Telemetry: `digest_method` per call, per-tool rollup in `/context` or `/stats`

**#129 retrieve_raw:**
- New built-in: `retrieve_raw(call_id: string) -> {raw: string, bytes: int}`
- Backed by `EventlogStore` (from #128)
- Synthetic digest map returned to model includes `call_id` — model has the retrieval key inline
- Ships bundled with #130 (aggressive pruning without a retrieval escape hatch = silent quality regressions)

**Cross-cutting:**
- **Feature flag**: default-on with a kill switch (matches #217 OTel pattern). No `--enable-*` flags.
- **Attribution surface**: `/sessions/{sid}/usage` endpoint first; SSE push later only if TUI wants a live breakdown chart.

## Parked / non-goals

- Cross-provider cache portability (Vertex ↔ direct-API) — no, provider-specific
- Cache warmth persistence across daemon restarts — no, ephemeral for v1
- Operator-tunable pruner rules — no, per #130 issue body
- LLM subagent MCP wrap (#124/#223) — parked pending #130 telemetry
- Cross-session cache sharing (#221) — v2 concern only

## First action tomorrow

Start task **#82** (`#222` per-turn UsageMetadata):

1. Check current `/sessions/{app}/{sid}/usage` handler surface (`pkg/attach/handlers_operator.go`, look for `usageQualified` / `usageShortcut` / `doUsage`)
2. Look at how usage data is currently accumulated (`Registrant.AttachUsage()` — see `UsageProvider` interface)
3. **Empirical check first**: instrument a test run against real Vertex to answer "is `cachedContentTokenCount` per-hit or cumulative-cache-state?" Prior observation of `cached > prompt` suggested cumulative — need to know before we design the attribution schema.
4. Extend the response shape:
   - `input_tokens_cached` + `input_tokens_uncached`
   - `cost_usd_estimated` + `cost_usd_uncached_reference` (delta = cache savings)
   - Per-turn array with same fields per-turn
5. Ship as its own small PR, test with the demo session so we have baseline data for #83+#84 comparison

Expected scope: 1-2 hours. Small, mechanical, unblocks everything else.

## Related files (starting points)

- `pkg/attach/handlers_operator.go` — endpoint definitions
- `pkg/attach/registry.go` — `Registrant` interface + `UsageProvider`
- `pkg/mcp/lifecycle.go` — where #130 wiring lands
- `pkg/tools/agentic/agentic.go` — existing subagent digester pattern to align with
- `pkg/models/gemini/` — where #221 lifecycle would live
- `docs/digest-design.md` — the Headroom-inspired design doc
- `docs/agentic-mcp-design.md` — #124's design (parent of #129/#130)

## Reference: today's related shipped PRs

- v2.7.0-dev.2 tag: image assets built + published
- #238 overlay + workflow guard
- #239 gemini bare-STOP detect + auto-retry
- #240 k8s-event-watcher informer re-list dedup
- #241 SessionSwitcher + core-tui v0.10.0
- #242 version fallback bump + presubmit
- #244 DELETE /sessions endpoint (#81)
- #245 CI fix for version-fallback fetch-depth
