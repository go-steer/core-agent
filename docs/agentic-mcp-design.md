# Transparent agentic wrapping for MCP tool calls

Design doc for routing MCP tool calls through a digesting subtask
at the toolset layer — invisible to the model — so large MCP
responses don't pollute the parent's context.

**Status:** revised (2026-07-16). Original design proposed 2026-06-08
under closed issue [#124](https://github.com/go-steer/core-agent/issues/124).
Reopened as [#223](https://github.com/go-steer/core-agent/issues/223)
now that #130's structural digest has shipped and reframes the
scope: the LLM subagent wrap becomes the *complement* to structural
digest, not a replacement.

**Tracking issue:** [#223](https://github.com/go-steer/core-agent/issues/223)
(supersedes closed #124)

## 2026-07-16 update — composition with structural digest

The original doc assumed no structural digest existed and proposed
LLM subagent wrap as the sole line of defense against MCP output
bloat. Since then, #130 (`pkg/digest` structural JSON pruner) and
#84 (wrap `digest.Process` into MCP tools) have shipped. This
changes the design in three concrete ways:

1. **Structural is the fast path.** Any MCP response that's JSON-
   shaped and passes structural pruning under the threshold never
   invokes a subagent. This is the vast majority of today's MCP
   surface (GKE MCP is 100% JSON; kube-mcp is 100% JSON). LLM wrap
   only fires for prose responses and JSON responses that stay
   over-threshold after structural pruning.
2. **Compose over `digestingTool`, not `renamedTool`.** The wrap
   stack (outer to inner) becomes:
   `agenticMCPTool` → `digestingTool` → `renamedTool` → upstream MCP
   tool. Structural runs first; the LLM wrap sees what structural
   couldn't reduce.
3. **Reuse `retrieve_raw` as the shared escape hatch.** Both digest
   paths (structural and LLM) can persist raw responses via the same
   `digest.Store` shipped in #128 (steps 3 + 4). The main model
   already knows about `retrieve_raw` — no new tool surface.

Model choice: MCP-specific override with fallback to the general
`--agentic-small-model` resolution used by built-in wrappers
(`cmd/core-agent/main.go` around `models.ResolveSmallModel`). Two
levels of override for operators who want to tune MCP wraps
independently from built-in wraps:

1. **MCP-specific**: `cfg.MCP.AgenticWrapModel` (new field). Wins
   when set. Motivation: MCP responses can have a genuinely
   different shape/complexity than built-in-wrapped surfaces
   (a structured `gke_get_k8s_logs` response vs an arbitrary
   `fetch_url` body), and operators may find a different tier
   works better for one but not the other.
2. **General small-model**: `--agentic-small-model <id>` flag (or
   equivalent config). The same knob already used by built-in
   wrappers today. Falls back to this when the MCP-specific field
   is empty.
3. **Provider cheap-tier default**: `gemini-2.5-flash` for
   Gemini/Vertex, `claude-haiku-4-5` for Anthropic. Falls back to
   this when neither operator override is set.
4. **Parent inherit**: providers without a cheap tier (echo,
   scripted) inherit the parent's model — no cost benefit but no
   correctness break either.

Implementation shape: extend `models.ResolveSmallModel` to accept
an optional first-preference override, or add a thin
`models.ResolveMCPSmallModel(provider, mcpSpecific, agenticSmall)`
sibling that layers the MCP field in front. Either way, the
resolved value is threaded into the `agenticMCPTool` factory the
same way `resolvedSmallModel` is already threaded into
`buildAgenticTools`. Startup log gets a second line so operators
see the resolved MCP wrap model separately from the built-in wrap
model (`"agentic MCP wrap: gemini-2.5-flash (mcp-specific override)"`).

Economics at the default cheap tier: `gemini-2.5-flash` runs
~$0.075/M input / $0.30/M output. A 5k-token prose response
digested to a 500-token summary costs ~$0.0005. Break-even after
one subsequent turn where the digest replaces the full response in
history resend.

The rest of the doc below (from #124's original 2026-06-08 draft)
still applies to the LLM-wrap component. Read it with the layered
composition above in mind — the "when to route through subagent"
gate is now "when structural digest can't reduce the response
under threshold," not "always for MCP."

## 2026-07-17 update — savings telemetry + OTel spans

The existing "Telemetry to capture" section (below) sketches per-
call eventlog metadata but predates the shipping of #128 (structural
digest) and doesn't cover OTel. This update makes the observability
story concrete for both paths — structural (shipped) and agentic
(this PR) — so the value the cost-reduction stack is actually
delivering is visible to operators and dashboards.

Two surfaces, one measurement path:

1. **Eventlog metadata** → surfaced in the attach TUI (`/stats`,
   per-tool-call footer) and in transcripts.
2. **OTel spans + attributes** → surfaced in any OTLP consumer
   (Jaeger, Honeycomb, GCP Cloud Trace).

Both are populated from the same measured points in the wrapper
pipeline — no double instrumentation.

### The `digest.Savings` shape

Extend `pkg/digest.Result` with an optional `Savings` field. Present
on both structural and agentic paths; absent (`nil`) only on the
passthrough path where we deliberately skipped digesting because the
payload was already small.

```go
// pkg/digest
type Savings struct {
    // Path: "passthrough" | "structural" | "agentic". Matches the
    // Method* constants but named separately since Savings can be
    // omitted on passthrough (no bytes to save) while Method is
    // always set.
    Path string

    // Byte counts of the payload before and after digesting.
    // Deterministic; measured on serialized JSON.
    OriginalBytes int
    DigestBytes   int

    // Token estimates. Standard 4-char-per-token heuristic — cheap,
    // no tokenizer round-trip, accurate to ±15% for typical mixed
    // content. Precise counts via provider tokenizer are available
    // but not worth the round-trip for a metric.
    OriginalTokensEst int
    DigestTokensEst   int

    // Agentic path only (zero on structural / passthrough): the
    // subagent's own LLM call. Populated by the caller (the MCP
    // wrapper) after invoking the small-tier subagent, since
    // pkg/digest doesn't own the subagent itself.
    SubagentInputTokens  int
    SubagentOutputTokens int
    SubagentModel        string
}
```

`pkg/digest.Process` populates `Path`, `OriginalBytes`, `DigestBytes`,
`OriginalTokensEst`, `DigestTokensEst` from what it can measure
locally. The caller (the MCP wrapper for #223, or `agentic_read_file`
et al when we retro-fit) fills in `Subagent*` fields on the agentic
path from the subagent's `ResponseUsage` before writing the eventlog
metadata / OTel attributes.

**Cost computation happens at display time, not at digest time.** The
digest struct carries counts; `usage.Tracker` (or the display layer)
looks up pricing via the existing layered-pricing chain and produces
a dollar figure. This keeps `pkg/digest` free of the pricing/model
dependency graph and lets a single price change re-price historical
digests correctly.

### Cost math

**Structural path** (no subagent, pure input-token reduction):

```
savings_tokens = OriginalTokensEst - DigestTokensEst
savings_cost   = savings_tokens × parent_model.input_rate
```

The parent model's input rate is used because the saved tokens are
what the parent would have paid for on this turn's input and every
subsequent turn's history resend.

**Agentic path** (subagent cost offsets the parent-input savings):

```
parent_input_saved   = (OriginalTokensEst - DigestTokensEst) × parent_model.input_rate
subagent_input_cost  = SubagentInputTokens × subagent_model.input_rate
subagent_output_cost = SubagentOutputTokens × subagent_model.output_rate
net_savings          = parent_input_saved - subagent_input_cost - subagent_output_cost
```

Break-even is when `parent_input_saved > subagent_input+output_cost`.
For a typical GKE MCP response (10k tokens raw, 800 tokens digest,
Sonnet/Opus parent, Haiku subagent), net savings run 90–95% of
gross — the subagent cost is nearly noise. Small responses can
break even negative; the size threshold guard (line 207 below)
prevents this.

**Cumulative session savings** roll up in `usage.Tracker`: one
counter per (session, path). `/stats` shows them alongside the
existing per-model breakdown.

### Display

Three surfaces, ranked by operator value:

1. **Per-tool-call footer** in the chat stream (in-process TUI +
   remote TUI). Emitted whenever `Savings.Path != "passthrough"`:

   ```
   ↪ mcp/gke_get_k8s_resource: 12.4k → 2.1k tokens (agentic, saved ~$0.008)
   ↪ mcp/gke_list_clusters:     4.8k → 0.9k tokens (structural, saved ~$0.014)
   ```

   Immediate operator feedback; visible during live drives so the
   cost-reduction infra proves itself in real time.

2. **`/stats` session totals** — new "Digest savings" block:

   ```
   Digest savings this session
     Structural: 41.2k tokens saved  (~$0.14)
     Agentic:    43.0k tokens saved  (~$0.17 net after subagent cost)
     Total:      84.2k tokens saved  (~$0.31)
   ```

3. **`/context`** — annotate the context-budget breakdown with
   "reduced from N via digest infrastructure" so operators can see
   what would have been in context if digesting were off.

Labeling matters: these are *hypothetical* savings ("what it would
have cost without digest infra" minus "what we actually spent"). Real
session spend is still `actual`. Label the `/stats` block as
"savings vs. no-digest baseline" so the number is honest — this is
the exact question operators ask ("is this infra earning its keep?").

### OTel spans + attributes

Per MCP tool call, spans nest:

```
mcp.tool_call                              (parent, new)
├── mcp.http_call                          (existing, from otelhttp)
└── digest.process                         (new)
      └── subagent.llm_call                (new, agentic path only)
```

Same measurement points as the eventlog metadata — one code path
emits both. `mcp.tool_call` is the wrap layer; `mcp.http_call` is
the underlying `renamedTool.Run` HTTP round-trip (already
otelhttp-instrumented via #217's foundation, PR #237).

Attributes use `core_agent.*` namespace so they're easy to filter
in trace UIs. Where OTel semantic conventions cover LLM operations
(`gen_ai.*`), we emit those too for tooling that has LLM-aware
dashboards (Honeycomb's LLM view, GCP Vertex AI Trace, etc.).

**`digest.process` span:**

- `core_agent.digest.path` = `passthrough` | `structural` | `agentic`
- `core_agent.digest.original_bytes` / `digest_bytes`
- `core_agent.digest.original_tokens_est` / `digest_tokens_est` /
  `savings_tokens_est`
- `core_agent.digest.savings_cost_usd_est` (agentic only, since we
  have full cost math there; structural gets computed at display
  time from the parent model's rate)
- `core_agent.mcp.server_name` / `core_agent.mcp.tool_name` (so
  savings can be sliced by which MCP tool)

**`subagent.llm_call` span** (agentic only):

- `core_agent.subagent.model`
- `core_agent.subagent.input_tokens` / `output_tokens` / `total_tokens`
- `core_agent.subagent.cost_usd`
- `gen_ai.request.model` = same as `subagent.model` (OTel semconv)
- `gen_ai.usage.input_tokens` / `output_tokens` (OTel semconv)
- `gen_ai.system` = provider name (`google.gemini` / `anthropic`)

**Span kind:** internal for `digest.process` (no network egress at
the span level); client for `subagent.llm_call` (crosses provider
boundary). `mcp.tool_call` is internal (the wrap layer); the child
`mcp.http_call` is client (HTTP egress).

### Bundling with #223

The observability wiring is *not* a follow-up PR — it lands in the
same PR as the wrapper implementation. Rationale:

- Same measurement points → same code traversal. Two PRs would
  require re-touching the wrapper code and re-reasoning about where
  measurements attach.
- The value-add of #223 is invisible without the savings surface.
  Landing the wrapper without the display means the demo story
  ("we saved 84k tokens this session") isn't proveable until the
  follow-up ships.
- Structural-only savings (backfill for `pkg/digest.Result` on the
  already-shipped structural path) drops out of the same code with
  no extra effort — the `Savings` struct is populated by
  `pkg/digest.Process` regardless of which path the router chose.

### Progress against #217

#217 asks for end-to-end distributed tracing across k8s-event-watcher
→ daemon → MCP/Vertex. The current state after #237 (foundation) +
this PR is:

- ✅ k8s-event-watcher → daemon (traceparent propagation, done in
  #237)
- ✅ Daemon → MCP HTTP (otelhttp, done in #237)
- ✅ Daemon → MCP tool call → digest → subagent LLM (this PR)
- ⏳ Daemon → Vertex/Gemini SDK calls (not this PR — needs
  instrumenting the genai SDK path, tracked separately in #217)

Add a comment on #217 when this PR merges, listing the two remaining
Vertex/Gemini SDK legs so the outstanding scope is visible.

### Post-drive validation

Same v2.6 GKE-troubleshoot fixture the issue body references
(session `019f5bff-ca1a`, four sequential `gke_get_k8s_resource`
calls, 16k → 21k prompt-token growth by turn 8). After this PR
lands, re-run the fixture and verify:

- Each of the four MCP calls emits a per-tool footer with
  measurable digest.
- `/stats` at end-of-session shows a `Digest savings` block with
  the four contributions summed.
- OTel export (LM otel-collector locally) shows the span tree
  above with populated attributes.
- Cumulative `promptTokenCount` stays near baseline (~15k) instead
  of climbing to 21k — the fixture's cost baseline was the design
  target.

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

## 2026-07-17 addendum — route `retrieve_raw` through the agentic wrap

The `retrieve_raw` escape hatch (shipped in #128 steps 3+4) closes
the "digest looks suspicious" gap by letting the model fetch the
full un-digested payload back into context. As shipped, it hands
back the raw bytes verbatim — same cost as if the wrap had never
fired for that call.

Field-observed on the 2026-07-17 demo drive: the model would call
`retrieve_raw` in exactly the cases where the digest was working
correctly for the majority of the response but had lost a specific
detail — and pay the full cost of re-inflating everything. On a
28k-token GKE `describe` response where the model needed ONE
truncated field, `retrieve_raw` adds all 28k tokens back to
context.

### Proposal

When `retrieve_raw` fires, route the raw payload through the
existing `DigestOptions.LLMFallback` closure BEFORE returning to
the model. This turns the escape hatch from "give me everything
verbatim" into "give me a higher-fidelity digest, produced by a
subagent with more headroom than the structural pruner has."

Wire shape:

- Same LLM subagent hook already wired for the second-chance path
  in `pkg/digest.Process` (agentic MCP wrap, #223 Phase 3). Reuse
  the closure — no new configuration surface.
- `retrieve_raw`'s handler in `pkg/tools/retrieve.go` gets an
  optional `LLMFallback` field on `RetrieveRawOptions`. When set:
  handler fetches raw from Store, then invokes fallback with a
  higher token budget than the wrap's per-call default (the model
  is asking specifically because it wants MORE than the digest
  gave it — the subagent should have room to expand).
- Fallback is opt-in via `--mcp-agentic-wrap-llm` (already the
  gate for the wrap's own LLM path); off by default matches the
  wrap's shipped posture.
- When off, `retrieve_raw` returns raw verbatim — same behavior
  as today. No regression.

### Trade-offs

**Pro:** turns the cost-bomb escape hatch into a bounded higher-
fidelity digest. Combines naturally with tighter `retrieve_raw`
tool-description guidance (PR #300, merged) — the model is
DISCOURAGED from calling it casually, but when it does, the cost
is bounded.

**Pro:** operators who care about cost can enable LLM-fallback and
get the safety net without worrying that `retrieve_raw` misuse
undoes all their savings.

**Con:** adds a subagent round-trip to `retrieve_raw` latency (~1-3s
on Flash-tier). But `retrieve_raw` is already the escape hatch;
operators expecting it to be cheap already broke their mental model.

**Con:** the model can't get truly raw bytes when it needs them
(cross-tool piping, exact-quote extraction). Mitigation: a second
tool (`retrieve_raw_verbatim`) or an argument (`raw_mode=true`)
that skips the fallback. Not v1 — add if operator demand
materializes.

### Sequencing

Follow-up PR, not blocking. Ships after the `pkg/digest` pruner-in-
string fix (root cause of most retrieve_raw calls) and the wrap's
LLM subagent path (Phase 3, already in #290) both bed in.

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
