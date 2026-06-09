# Transparent agentic wrapping for MCP tool calls

Design doc for routing MCP tool calls through a digesting subtask
at the toolset layer — invisible to the model — so large MCP
responses don't pollute the parent's context.

**Status:** proposed (2026-06-08). Awaiting approval before
implementation. v2.5 candidate.

**Tracking issue:** [#124](https://github.com/go-steer/core-agent/issues/124)

## Motivation

MCP tools are an unbounded source of context bloat in core-agent
today. The existing agentic-wrapper machinery (#118 shipped
default-on) protects the four built-in tools (`read_file`,
`fetch_url`, `grep`, `research`) but doesn't touch MCP at all
(`cmd/core-agent/agentic.go:68-103` — hardcoded names).

Two distinct bloat sources from MCP:

1. **Per-turn tool-declaration overhead.** N MCP tools × ~150-300
   tokens of description each, shipped on every turn. Provider-
   cached but not free. Out of scope for this doc.
2. **Uncapped tool responses.** Every byte the MCP server returns
   lands in the parent's context, raw, with no truncation
   (`pkg/mcp/namespace.go:104:renamedTool.Run` just passes the
   upstream response through). The response stays in context for
   every subsequent turn (history resend) until compaction fires.

Built-in tools have explicit output caps (`pkg/tools/grep.go:54`,
`pkg/tools/fetch.go:54`, `pkg/tools/bash.go:119`, `pkg/tools/file.go`).
**MCP tools have none.** A 13-row `gke_list_clusters` table burns
~2k tokens directly into context. A `gke_get_k8s_logs` call on a
chatty pod can burn 10k+. A long-session operator with three or
four MCP servers wired in (GKE + filesystem + GitHub + Linear,
say) accumulates this on every turn.

This problem is structurally the same as the one `agentic_read_file`
solves for built-in reads — but for MCP we can do better than the
existing pattern, because the existing pattern has its own known
failure mode that doesn't apply here.

### Why not just expose `agentic_mcp_*` as visible tools

The existing `agentic_*` design exposes both the bare tool
(`read_file`) and the wrapper (`agentic_read_file`) and uses tool
descriptions to nudge the model toward the wrapper. We just shipped
description hardening (PR #118 + the verify-loop wording) because
the model reaches for the bare tool anyway under pressure — issue
#59 is precisely "the model bypasses the wrapper."

For MCP this would compound:

- **Doubled tool surface.** N MCP tools × 2 (bare + wrapper) = more
  tokens of declarations on every turn.
- **Model discipline is unreliable** by the same #59 mechanism.
- **Operator config explosion.** Every MCP server's tool list
  would need wrapper variants generated and named consistently.

A transparent wrap at the toolset layer eliminates all three
problems at once. The model never sees a choice; it just calls
`gke_list_clusters` like always and the digesting happens
invisibly underneath.

## Goals

- **Transparent.** Model sees one tool, calls it normally. Digesting
  is a property of the wrapping pipeline, not a tool surface.
- **Size-bypassed.** Small responses pass through unmodified —
  wrapping cost > bloat cost for tiny payloads.
- **Operator escape hatch.** Per-server denylist for tools the
  operator wants raw (because they're small, latency-sensitive,
  or being debugged).
- **Audit-preserving.** Both the digest (parent's view) and the
  raw MCP response (subtask's view) are recorded, linked, and
  queryable. This is *better* than today's single-entry audit.
- **Default on.** Same posture as #118 — safe default, sharp tools
  available.

## Non-goals (v1)

- **Per-tool allowlist.** Operator says "only wrap these tools"
  instead of "wrap all except these." Plausibly useful but
  speculative; ship denylist first, revisit if telemetry shows
  operators want to opt in rather than opt out.
- **Intent capture.** Subtask reads parent's last N turns to digest
  with relevance. More powerful but adds context leakage parent →
  subtask. Revisit if telemetry shows generic preservation loses
  the wrong details.
- **Per-tool digest prompt customization.** Operator-tunable digest
  prompt per MCP tool. Pure configuration explosion; resist.
- **Per-tool small-model overrides.** Operator says "use Opus to
  digest *this specific* tool." Configuration explosion. Operators
  who want this can build a custom \`agentic_mcp\` package against
  the library API.
- **Bundled local LLM.** Quantized model in the binary. 100MB-1GB
  binary growth, CGo runtime, quality below Flash/Haiku. Don't.
- **Deterministic non-LLM digester.** Regex / structural pruning
  for known response shapes (e.g. strip verbose YAML fields).
  Niche; useful for specific MCP tools but not a general solution.
  Revisit if telemetry shows specific tools where structural prune
  beats LLM digest.
- **Reducing per-turn tool-declaration overhead.** Separate problem
  (operator-side tool-subset selection), separate doc.

## Dependencies

This design has two hard dependencies that should land first or
in parallel:

- **#122** — provider-aware default for `--agentic-small-model`. The
  cost-efficiency win of transparent wrapping requires a cheap-tier
  subtask model. Today operators have to set `--agentic-small-model`
  explicitly. With #122 closed, an Opus/Pro parent auto-routes
  subtasks to Haiku/Flash without any operator config. Without
  #122, this design "works" but every digest costs the parent's
  per-token rate — small cost win, full latency hit.
- **#120** OR **`--session-db` on** — empty tool text in transcripts.
  Without one of these, the post-mortem story degrades: parent's
  audit row shows "got a digest," but the subtask's raw MCP response
  is in transcript fields that are currently dropped. With
  `--session-db` on, the eventlog captures everything regardless of
  transcript fidelity. Either path closes the audit gap.

## Proposed design

Three knobs, in order of how often operators will touch them:

1. **Global flag (CLI + config)** — `--mcp-agentic-wrap=true|false`.
   Default true. Disables the entire pipeline when false; bare MCP
   passthrough behavior matches today.
2. **Global size threshold (CLI + config)** —
   `--mcp-agentic-wrap-threshold=8000`. Bytes. Default 8000
   (~2000 tokens of typical text). Responses below this bypass
   the subtask and pass through verbatim.
3. **Per-server denylist (`.agents/mcp.json`)** — `agentic_never`
   array of tool names per server. Listed tools always bypass,
   regardless of size.

### Pipeline shape

Wrapping happens at the toolset layer in
`pkg/mcp/lifecycle.go:Build`, after namespacing + gating:

```go
// Existing (today):
wrapped = withNamespace(ts, name)
if gate != nil { wrapped = coretools.GateToolset(wrapped, gate, "mcp") }

// Proposed:
wrapped = withNamespace(ts, name)
if gate != nil { wrapped = coretools.GateToolset(wrapped, gate, "mcp") }
if mcpAgenticEnabled {
    wrapped = withAgenticWrap(wrapped, agenticOpts{
        AgentGetter: agentGetter,
        Threshold:   thresholdBytes,
        DenyList:    spec.AgenticNever,
    })
}
```

The `agenticWrap` toolset is the new piece. Each tool it returns
has a `Run` implementation that:

1. **Recursion check.** If `tool.Context` carries the "I'm inside
   a subtask" marker (new context value), bypass — invoke the
   inner tool directly. Subtask-originated MCP calls don't
   re-wrap.
2. **Denylist check.** If `tool.Name()` is in the per-server
   `AgenticNever` list, bypass.
3. **Invoke inner tool** (today's bare MCP call path). Get the
   raw response.
4. **Size check.** If serialized response < threshold bytes, return
   it verbatim.
5. **Digest.** Call `Agent.RunSubtask` with:
   - **System prompt:** "You are a digesting subtask. Summarize the
     following tool response, preserving identifying values
     (names, IDs, URLs, statuses, counts, error messages). Keep
     all field names that look like primary keys. Discard verbose
     descriptions, redundant metadata, and visual formatting.
     Stay under 500 tokens."
   - **User message:** the serialized raw response.
   - **Inner tools:** none — the subtask is summarizing what it was
     handed, not re-fetching.
6. **Return** a synthetic map with the digest under a known key
   (e.g. `{"digest": "...", "truncated_from_bytes": N}`).

Step 1's recursion guard uses the same mechanism as today's
subtask context-carry (`pkg/agent/subtask.go:283:branchInjectingService`)
— inject a marker at `RunSubtask` entry, check it in the wrapper.

### Audit log shape

Already validated against the existing subtask infrastructure
(`pkg/agent/subtask.go:39-40`):

- **Parent's session branch** (`<parent>`): one event per MCP call —
  "called `gke_list_clusters`, got back this digest." Operator
  sees this in `/audit-log`, in transcripts, in the TUI tool list.
- **Subtask's session branch** (`<parent>:sub:agentic_mcp.gke.list_clusters`):
  full record of the raw MCP JSON-RPC call + raw response from
  the GKE server. Linked to parent by sessionID prefix.

Query API (`pkg/eventlog/eventlog.go:201:WithSessionTree`) returns
parent + every `:sub:%` descendant in one shot — `/audit-log` and
external tools get both halves linked.

Net: this is an **audit improvement**, not a regression. Today a
bare MCP call lands raw in the parent's context AND the audit log
as a single entry. After this lands, the audit log has both: what
the parent reasoned over (digest) and what actually went over the
wire (raw). Side-by-side comparison answers "did the digest
mislead the parent?"

### Configuration surface

#### CLI flags

```
--mcp-agentic-wrap=true|false           default: true
--mcp-agentic-wrap-threshold=BYTES      default: 8000
```

#### Config (`.agents/config.json`)

```json
{
  "mcp": {
    "agentic": {
      "enabled": true,
      "threshold_bytes": 8000
    }
  }
}
```

CLI flags override config; config overrides built-in defaults.

#### Per-server denylist (`.agents/mcp.json`)

```json
{
  "servers": {
    "gke": {
      "url": "...",
      "agentic_never": ["get_operation", "get_node_pool", "get_k8s_version"]
    },
    "filesystem": {
      "url": "...",
      "agentic_never": ["stat"]
    }
  }
}
```

Tools listed in `agentic_never` always bypass the wrapping pipeline,
regardless of size. Useful for:
- Known-tiny responses (avoid latency overhead).
- Tools where operators want raw responses for debugging.
- Tools whose response is already structured enough that an LLM
  digest can only lose information.

#### Slash visibility

`/context` (alias `/boundaries`) gains a line showing how many MCP
calls were wrapped vs. bypassed this session, and rolled-up subtask
spend across all MCP digests. Same pattern as today's "Subtasks:"
row introduced in PR #118.

## Implementation sketch

### Code locations

- **New file:** `pkg/tools/agentic/mcpwrap.go` — the `withAgenticWrap`
  toolset wrapper. Mirrors `namespace.go` / `gate.go` shape (passes
  through Declaration, wraps Run).
- **Modified:** `pkg/mcp/lifecycle.go:Build` — accept agentic opts,
  apply the wrap layer after namespacing + gating.
- **Modified:** `pkg/mcp/config.go` (or wherever ServerSpec lives)
  — add `AgenticNever []string` field.
- **Modified:** `cmd/core-agent/main.go` — new flags, threshold
  parsing, pass opts through to `mcp.Build`.
- **New:** recursion-guard context marker in `pkg/agent/subtask.go`
  (or `pkg/tools/agentic/`) — `WithSubtaskMarker(ctx)` /
  `IsSubtaskContext(ctx)`.

### Sequencing

1. **Land #122 first.** Without provider-aware small-model defaults,
   transparent wrapping silently routes through the parent model
   for operators who haven't configured `--agentic-small-model` —
   half-working state.
2. **Land the recursion guard.** Generic; useful beyond MCP.
3. **Land `withAgenticWrap` toolset wrapper** with the global enable
   flag + threshold (no denylist yet). Default off behind the flag
   until validated.
4. **Land the per-server denylist** in `.agents/mcp.json`.
5. **Flip default to on** once dogfooded (mirroring PR #118's
   approach — opt-in for one minor release, then default-on once
   real sessions have exercised it).

Each step is independently shippable.

### Telemetry to capture

Without this, we can't tune the threshold or evaluate whether the
denylist is being used as intended.

- Per-MCP-call event in the eventlog: `tool_name`, `raw_bytes`,
  `wrapped: bool`, `digest_bytes` (if wrapped), `bypass_reason`
  (if not — `under_threshold`, `denylisted`, `disabled`,
  `subtask_recursion`).
- Per-session rollup: total MCP calls, % wrapped, total raw bytes
  saved from parent context, total subtask cost.
- Surface in `/context` and `/stats`.

After a few weeks of real usage:
- Distribution of MCP response sizes (informs threshold tuning).
- Which tools are most often in denylists (might warrant being
  default-bypassed by core-agent).
- Subtask cost vs. context bytes saved (informs ROI argument).

## Open questions

1. **Threshold unit: bytes vs. tokens.** Bytes are deterministic
   and cheap to measure. Tokens are what the bloat cost is
   denominated in. Proposal: configure in bytes, document as
   "~4 bytes per token for typical text" so operators can reason
   in either unit. Implementation reads bytes directly off the
   serialized response; no tokenizer round-trip in the decision
   path.
2. **Serialization format for size check.** MCP responses are
   `map[string]any`. Do we measure JSON-serialized bytes
   (deterministic, slightly inflated) or estimate (fast, less
   accurate)? Proposal: JSON-serialize once, reuse for the digest
   subtask input — no double work.
3. **What digest format to return.** Plain markdown digest, or
   structured `{digest, raw_bytes, truncated}` map? Proposal:
   structured map. Lets the parent model see "this was 47KB
   compressed to 800 bytes," which is useful context for whether
   to call back with a narrower request.
4. **How aggressive should "preserve identifying values" be?** The
   digest prompt says "preserve names, IDs, URLs, statuses,
   counts, error messages." This is operator-tunable in principle
   (next-doc work) but for v1 a single fixed prompt is enough.
   Risk: the prompt isn't tight enough and digests lose load-
   bearing details. Mitigation: telemetry on "operator called
   bare tool to verify digest" pattern (visible as a follow-on
   call with the same args within K turns).
5. **Recursion guard scope.** Should the guard apply only to MCP
   wrapping, or to the existing `agentic_*` wrappers too? Today
   nothing prevents `agentic_research` from recursively calling
   `agentic_read_file` inside its own subtask. Probably fine in
   practice (each wrapper has its own model + budget), but worth
   thinking about. Proposal: generic guard, applied uniformly.
6. **Interaction with elicitation.** Some MCP tools elicit (ask the
   operator for input mid-call). If we wrap, the elicitation
   request comes from inside the subtask, not the parent. Does
   the elicitation surface still route correctly?
   `pkg/mcp/lifecycle.go:170:ElicitationHandler` is per-client;
   need to verify it survives the subtask boundary. Probably yes
   (same process, same handler), but worth a test.
7. **What about long-running MCP tools?** `tool.IsLongRunning()` is
   already plumbed through (`namespace.go:85`). If a tool is long-
   running, the subtask wait time may be unacceptable. Proposal:
   long-running tools default to bypass (same as denylist), with
   an explicit `agentic_always` per-server array for operators
   who want them wrapped anyway. Adds one knob; arguably worth it.

## Out of scope (revisit later)

- Per-tool allowlist (`agentic_only` array). Add only if telemetry
  shows operators want opt-in rather than opt-out.
- Per-tool small-model overrides.
- Per-tool digest prompt customization.
- Bundled local LLM as the digester.
- Deterministic structural digesters (JSON field pruning, YAML
  subfield stripping).
- Reducing per-turn MCP tool-declaration overhead (separate problem;
  belongs in an operator-side tool-subset selection doc).
- Caching identical MCP responses across turns to skip re-shipping
  (cache invalidation is its own design exercise).
- Cross-tool digest fusion ("digest the last K MCP calls together
  for a summary"). Plausibly useful for operators who run many
  related queries in a row; out of scope for v1.

## Addendum: Headroom-inspired extensions (post-v1)

[Headroom](https://github.com/chopratejas/headroom) (Netflix, Apache 2.0)
ships a local context-compression layer in front of any LLM. Its design
overlaps ours in goal but differs in primitive: instead of an LLM
subagent digester, it routes payloads to content-typed compressors
(AST-aware for code, structural for JSON, a fine-tuned small model
for prose) and keeps the originals locally so the model can fetch
them back via a retrieval tool ("CCR"). Reported 60-95% token
reduction on agentic workloads with accuracy preserved on
GSM8K/SQuAD/BFCL.

Two ideas from Headroom address known weaknesses in this design.
Neither is v1 scope; both should land as follow-ons once the v1
pipeline is dogfooded.

### CCR-style raw retrievability

Open question #4 names a real risk: the digest prompt isn't tight
enough and load-bearing details get summarized away. Today's
mitigation is "operator notices, calls the bare tool again" — which
costs a second MCP round-trip and may not be reproducible (list-style
endpoints with pagination cursors, time-sensitive queries, expensive
calls).

Proposal: when a response is wrapped, persist the raw bytes keyed by
the parent's tool-call ID (already unique, already in the eventlog)
and expose a new built-in tool:

```
mcp_retrieve_raw(call_id: string) -> { raw: string, bytes: int }
```

The synthetic digest map (`{digest, truncated_from_bytes, call_id}`)
already carries the call ID — the model has everything it needs to
ask for the original. The raw blob lives in
`pkg/eventlog` as a new field on the subtask's tool-result row
(already written for audit; we'd just expose it via a query).

Cost is small and additive: one new tool, one eventlog read path,
no change to the wrapping pipeline. It directly closes open question
#4 and removes the "subtask digest is one-way" complaint as a
blocker for shipping.

Storage policy: the raw is already in the eventlog when `--session-db`
is on. Retrievability inherits the eventlog's retention; no separate
GC story needed.

### Structural digester for shaped responses

The non-goals list (line 99-103) defers deterministic structural
digesters as "niche; useful for specific MCP tools but not a general
solution." Headroom's existence is evidence to revisit. In practice
most MCP responses are JSON-shaped (`gke_list_*`, GitHub API, Linear,
filesystem tools) — exactly the shape where structural pruning
(strip verbose description fields, collapse arrays past N elements
with a count, preserve identifier-shaped keys) is both cheaper and
more faithful than an LLM digest.

Proposal: insert a content router in front of the LLM subagent:

1. **JSON-shaped, recognizable schema** (response is a JSON object or
   array of objects with consistent keys) → structural prune. Drop
   long string values past M characters, summarize arrays past N
   elements as `{first: [...], last: [...], total: K, dropped: K-2N}`,
   preserve keys matching `*_id`, `name`, `status`, `*url*`, `error`.
   No LLM call.
2. **Code-shaped** (recognized by file extension hint or content
   sniff) → AST-aware compression (Go-only for v2; tree-sitter
   bindings for multi-language is its own scope decision).
3. **Prose / unknown** → existing LLM subagent path.

The router is dispatch logic, a few hundred LOC. The JSON pruner is
likewise small. Both are deterministic — testable without an LLM in
the loop, no per-call cost, no latency tail. The LLM subagent
remains for the cases where it's actually load-bearing.

Sequencing: ship v1 LLM-subagent-only first, instrument which MCP
tools hit it most often, then add the structural path for the
top-N tool shapes. Telemetry from the v1 rollout (see "Telemetry to
capture") directly informs which response shapes are worth a
dedicated pruner.

**Tracking issues:**
- [#128](https://github.com/go-steer/core-agent/issues/128) — `pkg/digest` library (umbrella).
- [#129](https://github.com/go-steer/core-agent/issues/129) — CCR retrievability via `retrieve_raw`.
- [#130](https://github.com/go-steer/core-agent/issues/130) — Structural JSON digester wired into both consumers.
- [#131](https://github.com/go-steer/core-agent/issues/131) — Docs: Headroom-as-MCP-server integration.

### Build vs. use vs. port (Headroom integration strategy)

Three options for getting Headroom's wins into core-agent:

1. **Use Headroom as-is.** Operators run the Python proxy or wire
   the MCP server form into `.agents/mcp.json`. Zero code in
   core-agent, but adds a Python runtime / subprocess to the
   operator's deployment story. Worth documenting as a supported
   integration regardless — operators who already run Headroom
   shouldn't have to choose.
2. **Full Go port under go-steer.** Reimplement ContentRouter,
   SmartCrusher, CodeCompressor (multi-language), Kompress-base
   inference, CacheAligner, CCR, image compression, `headroom
   learn` in Go. Significant scope; Kompress-base (the fine-tuned
   prose model) has no good Go inference path. Not recommended.
3. **Targeted port of specific pieces** as a separate `go-steer`
   library, consumed by core-agent and reusable by other Go agents.
   Recommended. Scope:
   - **In:** content router, structural JSON pruner, CCR-style local
     store (probably backed by eventlog when present, filesystem
     otherwise), cache-aligner-style prefix stabilizer.
   - **Out:** Kompress-base (we keep the LLM subagent path for prose),
     multi-language AST compression (consider Go-only as a follow-on),
     image compression, `headroom learn` (separate problem, lives
     closer to our shared-memory work).

A separate library matters because the CCR store + JSON pruner are
useful for non-MCP digesting too (grep output, bash output, file
reads — exactly what the existing `agentic_*` wrappers compress
today with an LLM). Packaging them as a library lets the same
primitives back built-in-tool digesting and MCP digesting, and
exposes them to anyone building Go agents on top of go-steer's
runtime layer.

Naming and home are TBD — could start as `pkg/digest/` inside
core-agent and extract once a second consumer exists, or stand up
the standalone library first if shared-memory or AX integration
also wants it. Either path is consistent with the project's "land
in-tree first, extract on second consumer" pattern.

## References

- #118 — agentic-tools default on (shipped). Sets the posture this
  design extends.
- #119 — per-model compaction threshold. Composes: this design
  reduces what reaches the parent's context, the threshold
  controls when what's left gets compacted.
- #120 — empty tool text in transcripts. This design depends on
  either #120 fixed or `--session-db` on for full audit fidelity
  in the no-DB case.
- #121 — small-tier startup warning. Independent; both apply.
- #122 — provider-aware default for `--agentic-small-model`. Hard
  dependency: without it, transparent wrapping's cost-efficiency
  win requires explicit operator config.
- `docs/context-management-design.md` — sets the substrate (compactor,
  subtasks, checkpoints, memory) this builds on.
- `docs/model-selection-design.md` — task-class flag + watchdog
  escalation. Orthogonal; both compose.
- `pkg/mcp/lifecycle.go:Build` — current MCP wiring; where the new
  wrap layer attaches.
- `pkg/mcp/namespace.go` — existing toolset-wrapper pattern this
  design mirrors.
- `pkg/agent/subtask.go` — `RunSubtask` primitive that does the
  actual digesting work.
- `pkg/eventlog/eventlog.go:201` — `WithSessionTree` query that
  preserves parent + subtask linkage in the audit log.
- [Headroom](https://github.com/chopratejas/headroom) — Netflix's
  local context-compression layer for LLM agents (Apache 2.0).
  Source of the CCR and structural-digester ideas in the addendum
  above.
