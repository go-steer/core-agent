# pkg/digest — Local digesting primitives for Go agents

Design doc for a small, in-tree library that consolidates the
digesting primitives core-agent uses to keep large tool responses
out of the parent context.

**Status:** proposed (2026-06-09). Awaiting approval before
implementation. v2.6 candidate, post-#124 v1.

**Tracking issue:** [#128](https://github.com/go-steer/core-agent/issues/128)

## Motivation

core-agent will soon have digesting in two places that both reach
for the same primitive (an LLM subagent):

- `pkg/tools/agentic/agentic.go` — the existing wrappers for the
  four built-in tools (`read_file`, `fetch_url`, `grep`,
  `research`) that shipped default-on in #118.
- `pkg/mcp/lifecycle.go:Build` — the MCP wrap layer proposed in
  #124, which routes large MCP responses through a subtask.

LLM digesting is the right primitive for prose. It is the wrong
primitive for the common case: most MCP responses, most built-in
JSON tool output, and a meaningful share of grep/file output is
*shaped* — JSON objects, arrays of records, tabular text — where a
deterministic structural prune is cheaper, faster, and more
faithful than a small-model summary.

[Headroom](https://github.com/chopratejas/headroom) (Netflix,
Apache 2.0) shipped a local compression layer built around exactly
this insight: route by content type, use structural pruners for
shaped payloads, fall back to a small LLM for prose, and keep the
raw originals locally so the model can fetch them back via a
retrieval tool ("CCR"). It reports 60–95% token reduction on
agentic workloads with accuracy preserved on standard benchmarks.

We don't want to take a Python runtime dependency on Headroom, and
we don't want a fine-tuned prose model in our binary. But the
primitives — content router, structural pruner, CCR store — are
small, useful well beyond MCP, and worth a dedicated Go package
that both existing digest sites can share.

`pkg/digest` is that package.

## Goals

- **One package, three primitives.** Content router, structural
  JSON pruner, CCR-style raw retrieval store. Each independently
  useful and testable.
- **LLM-agnostic.** The package digests payloads. It does not
  import `pkg/agent`, does not know what an MCP tool is, does not
  reach for the model loop. Callers pass an `LLMFallback` function
  if they want one.
- **Two consumers from day one.** Both the MCP wrap layer (#124)
  and the existing `pkg/tools/agentic` wrappers swap in
  `digest.Process(...)` in place of their direct subtask calls.
  Anything that reduces parent-context bloat in MCP also reduces
  it for built-in tools.
- **Testable without an LLM.** Structural prune, router dispatch,
  and CCR store are pure / I/O code. CI doesn't need an API key
  to validate the digesting path.
- **In-tree first.** Same posture as shared-memory and other
  go-steer libraries: land in `pkg/digest`, extract to a standalone
  module when a second project (AX, future Go agents) actually
  wants it.

## Non-goals (v1)

- **Prose compression model.** No bundled fine-tuned model
  (Headroom's `Kompress-base` equivalent). Callers fall through to
  the LLM subagent path for prose. Reconsider only if Go inference
  for small models becomes practical *and* telemetry shows prose
  is a meaningful fraction of digested traffic.
- **Multi-language AST-aware code compression.** Tree-sitter
  bindings via CGo are heavy and fragile. Go-only AST compression
  (using `go/ast`) is plausible as a later add; multi-language is
  out.
- **Image compression.** Not core-agent's problem today.
- **Memory mining / `headroom learn` equivalent.** Adjacent but
  separate; this work belongs alongside shared-memory
  ([[project_shared_memory_design]]), not in the digesting library.
- **Cache-aligner / prefix stabilizer.** Real cost lever (provider
  KV cache hits) but orthogonal to digesting — belongs near the
  request-construction layer if it lands at all.
- **Standalone go-steer library.** Extract when a second consumer
  outside core-agent wants it; do not stand up a separate module
  preemptively.
- **Schema-aware pruners per MCP server.** The router pattern
  leaves room for these; v1 ships a single generic JSON pruner and
  lets telemetry drive any specialization.

## Dependencies

- **#124 (MCP wrap)** for the second consumer wiring point. The
  package itself doesn't depend on #124, but the most visible win
  (MCP-side compression) requires #124 to have landed.
- **`--session-db` / `pkg/eventlog`** for the eventlog-backed CCR
  store. When the eventlog is off, CCR degrades to a filesystem
  store or disables retrieval entirely (see open question 1).

No new external Go deps required for v1.

## Proposed design

### Package surface

```go
package digest

// Result is what Process returns to the caller.
type Result struct {
    Digest    string         // compressed payload (caller hands this to the model)
    Method    string         // "passthrough", "structural_json", "llm_fallback", "store_full"
    RawBytes  int            // serialized size of the original
    CallID    string         // opaque ID for CCR retrieval (empty if no Store)
    Metadata  map[string]any // pruner-specific stats (e.g. {"arrays_collapsed": 3})
}

// Options configure a single Process call.
type Options struct {
    // Threshold: payloads smaller than this bypass digesting entirely.
    Threshold int

    // Store: optional CCR backing. When non-nil, Process writes the raw
    // payload to the store and populates Result.CallID. When nil, no
    // retrieval is possible and CallID stays empty.
    Store Store

    // LLMFallback: optional prose digester. Called when the router cannot
    // dispatch to a structural pruner. When nil, payloads that would fall
    // through return Method == "passthrough" with Digest == raw.
    LLMFallback func(ctx context.Context, raw []byte) (string, error)

    // CallID: caller-provided identifier (e.g. tool-call ID). When empty,
    // Process generates one. Used as the Store key.
    CallID string
}

// Process digests payload according to opts. It never returns an error
// for content-shape reasons — pruner failures fall through to the LLM
// fallback or passthrough.
func Process(ctx context.Context, payload []byte, opts Options) (Result, error)
```

### Content router

Routes on a cheap content sniff:

1. Payload < `Threshold` → **passthrough** (verbatim).
2. Looks like JSON (first non-whitespace byte is `{` or `[`, parses
   successfully) → **structural_json**.
3. Otherwise → **llm_fallback** if `LLMFallback != nil`, else
   **passthrough** (truncated to a configurable max-size with a
   `...<N more bytes>` suffix so we never silently dump megabytes).

The router is intentionally shallow. Adding routes (Go AST,
schema-tagged JSON, etc.) is a later patch, not a v1 design
decision.

### Structural JSON pruner

Rules:

- **Preserve identifier-shaped keys** unconditionally. Match
  `id`, `*_id`, `name`, `status`, keys containing `url`/`uri`,
  `error`, `code`, `kind`, `type`. Configurable but with a sane
  default list.
- **Truncate long string values** past `MaxStringChars` (default
  500) → `"<truncated, N chars>"`. Identifier-key values are
  exempt.
- **Expand nested JSON strings** before truncating. If a long
  string starts with `{` or `[` and parses cleanly as JSON, the
  string is REPLACED with the parsed-and-recursively-pruned inner
  structure. This handles MCP servers whose native wire encoding
  wraps structured data as a JSON-string inside a JSON envelope
  (GKE MCP's `{"clusters":["<JSON obj>", ...]}` shape; any
  MCP `text-content` payload carrying JSON). Without this, the
  outer pruner sees an opaque long string and truncates the whole
  semantic content — model gets zero useful data. Falls back to
  the truncate-and-mark rule when the string doesn't parse.
  Depth-guarded via the same `MaxDepth` cap. Metadata bump:
  `nested_json_expanded: N`.
- **Collapse long arrays** past `MaxArrayElems` (default 20) →
  `{"_summary": true, "first": [...10 items...], "last": [...10
  items...], "total": K, "dropped": K - 20}`. Items inside the
  retained head and tail are pruned recursively.
- **Recurse into objects** with a depth cap (`MaxDepth`, default
  8). Subtrees deeper than the cap collapse to `"<truncated, deep
  subtree>"`.
- **Idempotent**: running the pruner twice produces identical
  output.

Pruner config (`MaxStringChars`, `MaxArrayElems`, `MaxDepth`,
identifier-key regex list) is set at package init with override
hooks, not per-call — operators don't tune this per-tool in v1.

### CCR store

```go
package digest

// Store is the CCR backing for raw payloads.
type Store interface {
    Put(ctx context.Context, callID string, raw []byte) error
    Get(ctx context.Context, callID string) ([]byte, error)
}
```

Two implementations:

- **`digest.FilesystemStore`** — directory + file-per-callID,
  caller chooses path. Default path under `os.TempDir()` per the
  project's tmp-not-$HOME convention. Bounded by a configurable
  max-total-size with FIFO eviction.
- **`digest.EventlogStore`** — backed by `pkg/eventlog`. Reuses
  the tool-call ID as the key and reads the raw payload from the
  existing tool-result row. No new tables; the row already
  carries the bytes when `--session-db` is on.

### Retrieval tool

A new built-in tool, `retrieve_raw`, is the model-facing CCR
surface:

```
retrieve_raw(call_id: string) -> { raw: string, bytes: int }
```

Exposed whenever a `Store` is wired up. Refuses with a clear error
when the call ID is unknown or the store is disabled. Subject to
the same gating as other built-in tools.

The synthetic digest map that consumers return to the model
includes the `call_id` already (so the model has everything it
needs to call `retrieve_raw` when a digest looks suspicious).

### Consumer 1: MCP wrap layer (#124 v2)

```go
// Today's #124 v1 sketch (LLM subagent only):
sub, _ := agentGetter().RunSubtask(ctx, digestPrompt, rawPayload)
return synthetic(sub.Text, len(rawPayload))

// After pkg/digest lands:
res, _ := digest.Process(ctx, rawPayload, digest.Options{
    Threshold:   thresholdBytes,
    Store:       eventlogStore,
    CallID:      toolCallID,
    LLMFallback: func(ctx context.Context, raw []byte) (string, error) {
        sub, err := agentGetter().RunSubtask(ctx, digestPrompt, raw)
        if err != nil { return "", err }
        return sub.Text, nil
    },
})
return synthetic(res.Digest, res.RawBytes, res.CallID)
```

Wins: JSON-shaped MCP responses get a deterministic prune for free
(no API call, no latency tail). Prose-shaped responses still hit
the subagent. Both paths populate the CCR store, so `retrieve_raw`
works for both.

### Consumer 2: pkg/tools/agentic wrappers

The four existing wrappers (`pkg/tools/agentic/agentic.go`) make
the same swap. `agentic_read_file` on a JSON config file gets a
structural prune for free. `agentic_grep` on a JSON corpus gets
the same. Where the content is genuinely prose (a markdown doc, a
research summary), the LLM fallback path runs as before.

This is a behind-the-scenes change — the tool surface doesn't
move, the prompts don't move, the model sees the same tools.

## Implementation sketch

### Code locations

- **New:** `pkg/digest/digest.go` — `Process`, `Result`, `Options`.
- **New:** `pkg/digest/router.go` — content-shape dispatch.
- **New:** `pkg/digest/pruner_json.go` — structural JSON pruner.
- **New:** `pkg/digest/pruner_json_test.go` — table-driven tests,
  no LLM in the loop.
- **New:** `pkg/digest/store.go` — `Store` interface +
  `FilesystemStore`.
- **New:** `pkg/digest/store_eventlog.go` — `EventlogStore`
  (depends on `pkg/eventlog`).
- **New:** `pkg/tools/retrieve.go` — `retrieve_raw` built-in tool.
- **Modified:** `pkg/mcp/lifecycle.go:Build` — wire `digest.Process`
  into the wrap layer after #124 v1 ships.
- **Modified:** `pkg/tools/agentic/agentic.go` — replace direct
  subtask calls with `digest.Process` using a fallback adapter.

### Sequencing

1. **Land `pkg/digest` skeleton** — `Process`, router, JSON
   pruner. Pure functions, no I/O, no LLM. Independently shippable.
2. **Land `Store` + `FilesystemStore`** — caller-chosen directory,
   bounded size. Still no eventlog coupling.
3. **Land `EventlogStore`** — depends on `pkg/eventlog` already
   shipping `--session-db`.
4. **Land `retrieve_raw` tool** — consumer of `Store`. Gated on
   the same allow-list as other built-ins.
5. **Wire into MCP wrap layer** — post-#124 v1; this is the
   #124-addendum CCR + structural work landing for real.
6. **Wire into `pkg/tools/agentic`** — last, because it touches
   shipped behavior; do it once telemetry shows the structural path
   is solid.

Each step is independently shippable and behind no feature flag
until step 5 (which inherits #124's flag posture).

### Telemetry

Same eventlog hooks #124 already specifies, plus:

- **Per-call:** `digest_method` (`passthrough`, `structural_json`,
  `llm_fallback`, `store_full`), `raw_bytes`, `digest_bytes`,
  `store_id` (if stored).
- **Per-session rollup:** distribution by method, total bytes
  saved, total LLM-fallback subtask cost, `retrieve_raw` call
  count.
- **Per-tool rollup** (after a few weeks): which MCP tools and
  which built-ins hit which path. Drives the decision on whether
  to add tool-specific pruners.

## Open questions

1. **Store backing when `--session-db` is off.** Options: silently
   degrade to `FilesystemStore` under `os.TempDir()`; disable CCR
   entirely (digests still work, `retrieve_raw` returns
   "unavailable"); make it an explicit `--digest-store=fs|eventlog|off`
   flag. Proposal: explicit flag, default `eventlog` when DB is on
   and `off` otherwise. Avoid silent fs writes the operator didn't
   ask for.
2. **Pruner config knobs surface.** `MaxStringChars`,
   `MaxArrayElems`, `MaxDepth` — operator-tunable, or fixed for
   v1? Proposal: fixed, with package-level overrides for tests.
   Add flags only if telemetry shows operators need them.
3. **Identifier-key regex list.** Default list (`id`, `*_id`,
   `name`, `status`, `*url*`, `error`, `code`, `kind`, `type`) is
   informed by common MCP responses. Open to additions; resist
   making it per-server-configurable in v1.
4. **`retrieve_raw` cross-session.** Should the tool be able to
   retrieve raw blobs from a *prior* session (eventlog has them)?
   Useful for resume / replay; risk of leaking stale context the
   model wasn't supposed to have. Proposal: session-scoped in v1,
   revisit when resume/replay flows demand it.
5. **AST-aware Go compression.** Cheap to add (`go/ast` is in the
   stdlib), useful for `agentic_read_file` on Go sources. Worth
   doing in v1 if it's truly small, or defer until structural JSON
   is dogfooded? Proposal: defer — it's a follow-on, the structural
   JSON path already covers most of the bloat.
6. **Idempotence and replay.** If a digest is replayed (e.g.
   during compaction summarization), do we re-prune or trust the
   stored digest? Proposal: trust — the digest is already the
   "compressed" form, and re-pruning prose is meaningless.

## Out of scope (revisit later)

- Multi-language AST compression (tree-sitter bindings).
- Prose compression model bundled in the binary.
- Image compression.
- Cache-aligner / prefix stabilizer (separate concern, separate
  layer).
- Memory mining / corrective-feedback writes to `CLAUDE.md`
  (belongs in shared-memory).
- Per-server schema-aware pruners (router pattern allows them
  later without breaking callers).
- Extracting `pkg/digest` to a standalone go-steer module (do this
  on the second external consumer, not pre-emptively).
- Cross-tool digest fusion ("summarize the last K digests
  together").
- Caching identical responses across turns to skip re-shipping
  (cache invalidation is its own design problem).

## References

- [Headroom](https://github.com/chopratejas/headroom) — Netflix's
  local context-compression layer (Apache 2.0). Direct
  inspiration; this design ports the load-bearing primitives
  (content router, structural pruner, CCR store) and skips the
  pieces that don't fit Go (`Kompress-base`, multi-language AST,
  image compression, `headroom learn`).
- `docs/agentic-mcp-design.md` — #124 design; the addendum at
  the bottom motivates this work.
- `docs/shared-memory-design.md` — adjacent; memory-mining work
  lives there, not here.
- `docs/context-management-design.md` — substrate (compactor,
  subtasks, memory) this composes with.
- `pkg/mcp/lifecycle.go:Build` — first consumer wiring point.
- `pkg/tools/agentic/agentic.go` — second consumer wiring point.
- `pkg/eventlog/eventlog.go` — backing store for the
  `EventlogStore` implementation.
- #118 — agentic-tools default on (shipped). Sets posture.
- #122 — provider-aware default for `--agentic-small-model`. Hard
  dep for the LLM-fallback path's cost story.
- #124 — Transparent agentic wrapping for MCP. First consumer of
  this library.
