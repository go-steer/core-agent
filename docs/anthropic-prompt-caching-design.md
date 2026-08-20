# Anthropic prompt caching

Status: shipped (v2.9). Issue [#714](https://github.com/go-steer/core-agent/issues/714).
Prerequisite: [#263](https://github.com/go-steer/core-agent/issues/263) (cache-write accounting), shipped first so the meter could tell the truth about what this feature costs.

## The problem

The ADK runner is stateless per turn: it replays the entire conversation on every model call. Turn N+1's prompt is turn N's prompt plus one exchange. Without caching, every token of the transcript is re-billed at the full input rate on every turn, so a session's cumulative input cost grows with the square of its length.

Anthropic's answer is prompt caching, and before this change core-agent used almost none of it:

- `anthropic.WithCacheSystem(true)` existed but was **library-only** — no config field, no CLI flag, and nothing in `cmd/core-agent` ever called it. A daemon launched with `--provider anthropic` cached nothing explicitly.
- Even if it had been set, it marked only the last **system** block. The system prefix is a few thousand tokens and does not grow; the transcript is the part that gets expensive.

Note that a first-party Anthropic endpoint also caches *automatically*, which is why `/usage` showed non-zero `input_tokens_cached` before this change. Automatic caching is opportunistic and unavailable on Vertex; it is not a substitute for asking.

## How the API works

Three facts drive the whole design.

1. **The cache is a byte-exact prefix match.** The request renders as tools → system → messages. A `cache_control` marker on a block means "the prefix ending at this block is worth storing". A later request that carries the identical prefix reads it back. Any change to an earlier byte invalidates everything after it.
2. **At most 4 markers per request.** A fifth is a `400`, not a silent drop.
3. **A marker searches backward at most 20 content blocks** for an entry to read. Beyond that window the read misses even though the entry exists.

Pricing: reads ≈ 0.1× base input, writes 1.25× (5-minute TTL) or 2× (1-hour TTL). Break-even is two requests at 5m.

## Design

### Where the markers go

| Marker | Placed on | Pays off |
|---|---|---|
| System | The last system block | Across turns *and* across sessions — it covers the tool schemas and the persona, identical for every session on the same build |
| Rolling history | The last cacheable content block, then every 16 blocks walking backward | Across turns — each request writes an entry ending at "now", and the next request reads it |

Budget: 4 total, system first, history takes the rest.

**Why walk backward.** Marking from the front would pin the cached prefix at a fixed early position and re-bill the growing tail forever — the exact failure mode #714 reported against the Vertex context cache, whose cached prefix stayed frozen at 15,192 tokens while input grew to 58K. Walking backward makes the marker positions move with the conversation.

**Why intermediate markers.** One agentic step can append more than 20 content blocks — a fan-out of parallel tool calls plus their results. When it does, a lone marker at the end can no longer see the previous turn's entry within its 20-block lookback: the read misses and the message history is re-written at 1.25×, worse than not caching at all. A 16-block spacing keeps each marker inside the next one's window.

**How big a turn survives.** With the system marker taking one slot, history markers sit at end-offsets 0, 16 and 32, so the deepest lookback reaches 52 blocks back. A turn that appends **53 or more** blocks — roughly a 26-wide parallel tool fan-out — leaves every prior entry out of reach and re-writes the message history. Two things bound the damage: the system marker sits at a fixed position and still reads, so tools+persona keep their discount; and the miss is self-healing, since the next turn reads the entry this one just wrote. Only an agent that fans out that wide *every* turn pays it repeatedly. `TestMarkHistoryBreakpoints_ToleratesA52BlockTurn` pins the boundary.

**What the markers can't help.** A parallel subagent fan-out forfeits the shared-prefix read: an entry only becomes readable once the first response starts streaming, so N siblings dispatched concurrently each pay the write. That is a property of the API, not of this placement.

**Why extra markers are safe.** Cache writes are billed for the tokens beyond the longest entry that already exists, so N markers over one turn's new blocks cost the same as one marker at the end. The extra markers buy chain continuity, not extra cost.

**Blocks that can't be marked.** `thinking` and `redacted_thinking` have no `cache_control` field. The walk skips them as marker sites but still counts them toward the stride, since the API's lookback counts every content block regardless of type.

### Where it's turned off

| Layer | Surface | Default |
|---|---|---|
| Config | `model.anthropic.prompt_cache.enabled` | `true` (nil = on) |
| CLI | `--no-prompt-cache` | off (caching on) |
| Per request | `models.WithoutPromptCache(ctx)` | not set |

Entry lifetime is a separate axis, added by #770: `model.anthropic.prompt_cache.ttl` / `--prompt-cache-ttl`, `5m` (default) or `1h`. The kill switch outranks it in the off direction.

Default-on, because the agentic loop issues its second request seconds after the first and clears the two-request break-even immediately. The shape that loses — a single request whose prefix never recurs — is the rare one, and it is the one an operator can disable.

One shape does pay a bounded tax under the default: a turn that makes exactly *one* model call (a plain conversational reply, no tools) more than five minutes after the previous one. Its entry has expired, so it writes at 1.25× and reads nothing — about $0.12 on a 100K-token transcript at Opus rates. An attach session with a human at the keyboard hits this on every long pause. #770 closed the TTL side of that gap: `--prompt-cache-ttl=1h` keeps the entry alive across the pause for a 0.75× premium on the writes that remain, which pays for itself the first time the longer-lived entry is read. `--no-prompt-cache` remains the answer when the prefix never recurs at all.

`--no-prompt-cache` is a **new** flag rather than a broadening of `--no-context-cache`. The latter is genuinely Vertex-specific: it controls a `CachedContent` resource with a TTL, a create/refresh lifecycle, and a delete at shutdown. Anthropic prompt caching has none of that. Overloading one flag onto both would leave it misdescribing whichever provider the operator is actually running.

The context-level opt-out exists because one `model.LLM` serves both the agentic loop and core-agent's internal one-shot calls. Four call shapes set it:

| Caller | Why it can't read an entry back |
|---|---|
| Compaction summarizer, checkpointer | Own system instruction, no tools — the prefix diverges from the loop's at the first block, and the window it summarizes has just moved |
| `/btw` side question | No system instruction and no tools at all, and the appended question makes the tail unique to the call |
| Tight-budget subtasks (`RunSubtask` with ≤2 turns) | The session ID is invocation-unique (#717), so the prefix never recurs; and with two turns the terminal turn writes the wrapper's whole payload — a large `tool_result` — for a read that never comes. The agentic wrappers (`agentic_read_file`, `agentic_grep`, `agentic_fetch_url`) and the MCP LLM-digest fallback all run this shape. Longer subtasks replay their prefix enough times to pay the write back and keep caching |

An entry written for any of them is a pure 25% surcharge on a call that already re-sends the whole conversation. They set the hint; the adapter reads it per request. The hint lives in `pkg/models` rather than on the Anthropic provider so `pkg/agent` doesn't have to import a backend, and it can only turn caching *off* — a provider disabled by config or CLI stays disabled.

### Vertex

Explicit prompt caching is available on Claude-via-Vertex exactly as it is first-party: the breakpoints ride the ordinary Messages request, and there is no resource to create. So `anthropic-vertex` gets the same defaults and the same gates. (Only *automatic* caching is first-party-only.)

### Subagents

A subagent that declares its own `model` resolves its own provider through `models.Resolve`, which never sees CLI flags — so `--no-prompt-cache` is threaded to `resolveSubagentProvider` explicitly. Overwriting `cfg.Model` with the subagent's block also drops the parent's `prompt_cache` setting, since that block hangs off `model.anthropic`; the parent's value is inherited unless the subagent states its own.

## What was deliberately left out

**The 1-hour TTL — since shipped as [#770](https://github.com/go-steer/core-agent/issues/770).** It was held back here because `pricing.Rates.CacheCreationInputPerMTok` was a single scalar holding the 5-minute rate (1.25×) and the 1-hour TTL bills 2×, so exposing the knob would have understated every cached turn by 37.5% — reproducing the exact defect #263 had just fixed. Both halves of that objection turned out to be answerable rather than permanent: LiteLLM does publish `cache_creation_input_token_cost_above_1hr`, and the response *does* say which TTL produced the writes (`usage.cache_creation.ephemeral_1h_input_tokens` / `ephemeral_5m_input_tokens`), so there is a right answer to bill rather than a guess about what the request asked for. `Rates` now carries a second write rate and `TurnUsage` a 1-hour subset of the write bucket. See "Entry lifetime" above.

**Minimum-prefix awareness.** Anthropic won't cache a prefix below a model-dependent minimum (512 tokens on Opus 5, 1024 on Opus 4.8 and Sonnet 5, 2048 on Opus 4.7, 4096 on Opus 4.6 and Haiku 4.5). Below it the marker is simply ignored: no error, no cost. Machinery to predict the threshold would add a model-version table to maintain in exchange for nothing measurable. Worth knowing that the minimum is not monotonic across generations, and that the highest of them lands on `claude-haiku-4-5` — core-agent's default small-model tier — so a short subtask prompt genuinely won't cache there.

**Cache-aware pricing on the subtask-savings sidecar.** `tool_savings_observer` re-derives a subtask's cost from token counts with `usage.Pricing.CostUSD`, which bills every input token as uncached, instead of carrying the cache-aware `SubtaskResult.CostUSD` the subtask already computed. Suppressing the cache on tight-budget subtasks makes that inert today (their cache buckets are zero), but it would misprice any future cache-reading subtask by up to 25%. Tracked in [#771](https://github.com/go-steer/core-agent/issues/771) rather than widened into this PR.

## Prefix stability

A prefix audit of the request path found the assembled prompt byte-reproducible turn over turn: the instruction layers are frozen at construction, the identity block is static, built-in tool order is a source-order slice (no map iteration anywhere on the ordering path), MCP servers are sorted by name, message history is append-only, and the Anthropic serializer is deterministic (the SDK's JSON encoder sorts map entries, and the tool-call ID synthesizer is a counter, not a random).

Four things do move, and operators should know which:

1. **MCP `tools/list` is re-issued every turn.** Membership, order, and schema text all come from the server. Tool schemas render *before* the system block, so a server that reorders its tools, reconnects with a different set, or edits a description invalidates every marker in the request. This is the one live invalidator in the tool block.
2. **Compaction and checkpointing rewrite the head of the history** by design (the boundary event's role is rewritten and everything before it is sliced off). One full message-cache miss per compaction, expected. Tools and system survive.
3. **A `pause_turn` continuation grows the request after it was marked.** A long server-side tool run (`WithWebSearch`) ends its request with `stop_reason: pause_turn`, and the adapter replays the paused assistant turn verbatim and re-issues. Those replayed blocks land after every existing marker, so the markers are cleared and re-placed on the grown request (`reapplyCacheBreakpoints`) — re-applying without clearing would stack a fifth marker and earn a 400.
4. **`spawn_agent`'s declaration is rendered per request** from the live subagent roster. Deterministic today — both the catalog and the grantable-tool list are sorted, and the roster is installed once at boot — but it is the one declaration computed per request rather than at build time.

Two latent invalidators are worth knowing about because they are one line away rather than live:

- ADK's skill toolset appends an `<available_skills>` block to the system instruction from a fresh, unsorted directory read on every request. It never fires here only because core-agent's toolset wrappers (`gate`, `serialize`, `instrument`) don't implement ADK's `RequestProcessor`, so ADK skips the call. Adding a forwarding `ProcessRequest` to any wrapper would make the system prompt disk-dependent and per-request.
- ADK interpolates `{ident}` / `{app:ident}` / `{user:ident}` placeholders in the system instruction from live session state on every request. Nothing in-tree writes session state, so it's a no-op today — but an operator `AGENTS.md` containing a `{word}` would become a state-dependent, per-request-variable system prompt.

## Code map

| Concern | Location |
|---|---|
| Breakpoint placement | `pkg/models/anthropic/cache.go` |
| Policy type + config gate | `pkg/models/anthropic/anthropic.go` (`CacheOptions`, `cacheOptionsFromConfig`) |
| Request assembly | `pkg/models/anthropic/convert.go` (`buildParams`) |
| Per-request opt-out | `pkg/models/promptcache.go`; set in `pkg/agent/compactor.go`, `pkg/agent/btw.go`, `pkg/agent/subtask.go` |
| Re-marking a grown request | `pkg/models/anthropic/cache.go` (`reapplyCacheBreakpoints`), called from `llm.go`'s pause_turn loop |
| CLI kill switch + TTL | `cmd/core-agent/main.go` (`--no-prompt-cache`, `--prompt-cache-ttl`), `pkg/compose/prompt_cache.go` |
| Subagent plumbing | `cmd/core-agent/subagents.go` (`resolveSubagentProvider`, `inheritPromptCache`) |
| Config schema | `pkg/config/config.go` (`PromptCacheConfig`) |
| Cost accounting | `pkg/pricing/pricing.go`, `pkg/usage` (see #263) |
