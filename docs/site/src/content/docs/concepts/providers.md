---
title: Providers
---


`core-agent` ships four model backends, all behind the same `models.Provider` interface. Pick one explicitly via `model.provider` in `.agents/config.json` or with the `--provider` CLI flag, or let env-based auto-detection pick.

---

## Auto-detection

When `model.provider` is empty (the default), `core-agent` walks the environment in this order and picks the first match:

1. **Vertex Gemini** — fires when `GOOGLE_GENAI_USE_VERTEXAI=true` **and** `GOOGLE_CLOUD_PROJECT` is set
2. **Gemini API** — fires when `GOOGLE_API_KEY` **or** `GEMINI_API_KEY` is set
3. **Anthropic** — fires when `ANTHROPIC_API_KEY` is set

If none match, you get a clear error listing the env vars to set. **Anthropic-via-Vertex is not auto-detected** — it overlaps with Vertex Gemini in env vars, so you have to opt in explicitly with `--provider anthropic-vertex` or `model.provider: "anthropic-vertex"` in config.

---

## Configuring built-ins

Every provider here ships some **server-side built-in tools** — web search, URL fetching, sandboxed code execution. The model invokes them inside the provider's own infrastructure and the results come back folded into the response, so `core-agent` never sees a tool call for them. Nothing in the function-tool layer reaches them: not `tools.disable`, not a subagent's `tools` allowlist, and not `--no-builtin-tools`, which governs `core-agent`'s **own** suite (`read_file`, `bash`, …) and is an unrelated axis despite the shared word.

`model.builtin_tools` is the lever:

```json
{
  "model": {
    "provider": "vertex",
    "name": "gemini-3.7-flash",
    "builtin_tools": {
      "web_search": false,
      "url_context": false,
      "code_execution": false
    }
  }
}
```

The names are deliberately provider-neutral, because **the defaults are not symmetric**: Gemini ships web search and URL context on, Anthropic ships web search off. A deployment that builds one image for both flavors changes whether its agent can reach the public internet just by switching provider. Stating the posture in `builtin_tools` makes it hold across that switch.

| Key | Gemini | Anthropic |
|---|---|---|
| `web_search` | `google_search` (default on) | `web_search` (default off) |
| `url_context` | `url_context` (default on) | — |
| `code_execution` | `code_execution` (default off) | — |

Each field is a tri-state: omit it to keep the provider's default, and only an explicit `true` / `false` moves one. A key with no equivalent on the resolved provider is ignored — which only ever fails safe, since a tool the provider cannot send is one it cannot leave on.

**Check the startup summary.** The daemon's `model:` line names the effective set:

```
core-agent: model: gemini-3.7-flash provider=vertex project=p location=global builtin-tools=url_context
```

`builtin-tools=none` means the provider supports them and they are all off; the segment is absent entirely for backends with no server-side built-ins (`echo`, `scripted`). Worth reading rather than assuming — config decoding does not reject unknown fields, so a misspelled key is silently discarded and this line is the only place the discard shows up.

A [subagent](/agent-design/subagents-and-wrappers/) that declares its own `model` block inherits each `builtin_tools` field it does not set, per field. An operator who turned web search off for the project meant it for the whole process; declaring a model must not quietly hand it back. Subagents whose effective set differs from the parent's get their own boot line.

---

## Gemini API

The simplest backend — talks directly to `generativelanguage.googleapis.com` with an API key.

| Provider name | `gemini` |
| Default model | `gemini-3.7-flash` |
| Auth | API key |
| Env vars | `GEMINI_API_KEY` (preferred), `GOOGLE_API_KEY` (also accepted) |
| Config block | `model.api_key` (overrides env) |

### Config

```json
{
  "model": {
    "provider": "gemini",
    "name": "gemini-3.7-flash"
  }
}
```

### CLI

```bash
GEMINI_API_KEY=... core-agent -p "ping"
GEMINI_API_KEY=... core-agent --provider gemini -m gemini-3-flash-preview -p "ping"
```

### Notes

- Both `GEMINI_API_KEY` and `GOOGLE_API_KEY` work; `GEMINI_API_KEY` is the name Gemini's own docs and tutorials use, `GOOGLE_API_KEY` is the umbrella name. Precedence: explicit config → `GOOGLE_API_KEY` → `GEMINI_API_KEY`.
- Get a key at [aistudio.google.com](https://aistudio.google.com).

### Built-in tools

The Gemini Provider injects a small set of Gemini's server-side built-in tools into every request, alongside any user-defined function declarations.

| Tool | Default | Notes |
|---|---|---|
| **GoogleSearch** | on | Public web search grounding. No setup. |
| **URLContext** | on | Fetch + ground on URLs the model decides to visit. No setup. |
| **CodeExecution** | off | Sandboxed Python on Google's servers. Useful for math, data analysis, file processing. Off by default — opt in once you've decided server-side code execution fits your security and cost posture. |

To override from config, use the provider-neutral [`model.builtin_tools`](#configuring-built-ins) block:

```json
{
  "model": {
    "provider": "vertex",
    "name": "gemini-3.7-flash",
    "builtin_tools": { "web_search": false }
  }
}
```

`web_search` is Gemini's `google_search`; `url_context` and `code_execution` keep their names. Each field is tri-state — omit it to keep the default above, and only an explicit `true` / `false` moves one.

To override from the library:

```go
import "github.com/go-steer/core-agent/v2/pkg/models/gemini"

// Turn one off:
provider, _ := gemini.NewAPIKey(key, gemini.WithURLContext(false))

// Turn CodeExecution on:
provider, _ := gemini.NewAPIKey(key, gemini.WithCodeExecution(true))

// Replace the whole set:
provider, _ := gemini.NewAPIKey(key, gemini.WithBuiltinTools(gemini.BuiltinTools{
    GoogleSearch: true,
    // URLContext + CodeExecution off
}))
```

The same options apply to `gemini.NewVertex(...)`. Other genai built-ins aren't surfaced today: `FileSearch`, `GoogleMaps`, `ComputerUse`, `Retrieval`, and `GoogleSearchRetrieval` all need upstream setup (a corpus, a Maps key, a hosted environment) and would yield API errors rather than working tools if flipped on without it. `EnterpriseWebSearch` is Vertex-only but otherwise zero-setup — it stays unsurfaced only because no consumer has asked.

### Server-side built-ins on Vertex AI

`GoogleSearch` and `URLContext` work on both the direct Gemini API and Vertex AI (since v1.0.1). Vertex's streaming search-grounding API emits a small number of heartbeat SSE frames (no `Candidates[]`, just `UsageMetadata` and a response ID); ADK's stream aggregator treats any empty-candidates chunk as a fatal `empty response` error, which previously killed Vertex grounded responses 30–60% of the time. The Gemini provider's `builtinsLLM` wrapper now drops those heartbeat-error chunks on Vertex only — the direct Gemini API path is untouched. Function-calling tools (`bash`, `read_file`, consumer-supplied tools) were already unaffected.

### Surfacing grounded search activity (v1.1.0+)

`GoogleSearch` runs entirely inside Google's infrastructure — there's no client-side request/result round-trip the way there is for `bash` or `read_file`. But the **evidence trail** (the queries the model issued, the URLs it grounded on) is in the response payload, and `core-agent` surfaces it in two places (since v1.1.0):

**In `runner.WriteEvents` output** — alongside the standard `→ tool(...)` / `← tool(...)` chat-style lines, you'll see:

```text
↪ google_search: query: "San Francisco news May 16 2026"
↪ google_search: sfgate.com — https://vertexaisearch.cloud.google.com/grounding-api-redirect/...
```

The `↪` sigil (magenta when colored) distinguishes "Google did this on the server" from your own `→` / `←` tool dispatch. Deduplicated within a turn so repeated metadata in the stream doesn't double-print. **Tradeoff to know:** the grounding metadata only lands on the model's aggregated response, so these lines appear *after* the model's text rather than interleaved with it.

**In the eventlog** — when `--session-db` is enabled, the same activity is projected as queryable rows authored `gemini/google_search`, branch-preserved alongside the original model event:

```sql
SELECT seq, author FROM agent_eventlog
WHERE author = 'gemini/google_search'
  AND app_name = 'core-agent' AND user_id = 'me' AND session_id = '<id>'
ORDER BY seq;
```

The `cmd/core-agent` CLI wires the projection automatically when `--session-db` is used with `--provider=gemini` / `vertex`. Library callers opt in explicitly:

```go
handle, _ := eventlog.Open(ctx, sqlite.Open("sessions.db"))
handle.Service = gemini.GroundingProjection(handle.Service)
a, _ := agent.New(m, agent.WithEventLog(handle))
```

The synthetic events leave `Content.Role` empty so ADK's content processor skips them when building the next turn's LLM context — they're audit + display only, never re-injected as conversation history.

**Known gap:** `URLContext` evidence isn't projected today. ADK's gemini converter drops `URLContextMetadata` before the wrapper can see it; surfacing it requires intercepting raw genai responses below ADK and is deferred until a consumer needs it.

### Gemini 3.0+ required when combining built-ins with function tools

When `GoogleSearch` / `URLContext` / `CodeExecution` are enabled (the default for the first two) **alongside** any function-calling tools — including `core-agent`'s default tool suite (`tools.Default()`) — you must use a **Gemini 3.0-or-later** model. Gemini 2.5 and older reject the combined request with `Built-in tools ({google_search}) and Function Calling cannot be combined in the same request`.

`core-agent` sets `Config.ToolConfig.IncludeServerSideToolInvocations = true` whenever it injects server-side built-ins, which is the flag Gemini 3+ requires to permit the combination. The library's default model `gemini-3.7-flash` satisfies this requirement out of the box, so consumers who don't override don't need to think about it.

If you must use a Gemini 2.5 model, two workarounds:

```bash
# CLI: drop the function-calling suite entirely, keep server-side built-ins.
core-agent --provider=gemini -m gemini-2.5-flash --no-builtin-tools -p "..."
```

```go
// Library: drop server-side built-ins, keep function calling.
provider, _ := gemini.NewAPIKey(key,
    gemini.WithGoogleSearch(false),
    gemini.WithURLContext(false),
)
m, _ := provider.Model(ctx, "gemini-2.5-flash")
a, _ := agent.New(m, agent.WithTools(myTools))
```

Same constraint applies to `gemini.NewVertex(...)` — it's a Gemini-API restriction, not provider-specific.

---

## Vertex AI (Gemini)

Same Gemini models, but routed through Google Vertex AI with Application Default Credentials.

| Provider name | `vertex` |
| Default model | `gemini-3.7-flash` |
| Auth | ADC (Application Default Credentials) |
| Env vars | `GOOGLE_GENAI_USE_VERTEXAI=true`, `GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION` |
| Config block | `model.vertex.{project,location}` |

### Config

```json
{
  "model": {
    "provider": "vertex",
    "name": "gemini-3.7-flash",
    "vertex": {
      "project": "my-gcp-project",
      "location": "us-central1"
    }
  }
}
```

### CLI

```bash
gcloud auth application-default login
GOOGLE_GENAI_USE_VERTEXAI=true \
  GOOGLE_CLOUD_PROJECT=my-gcp-project \
  GOOGLE_CLOUD_LOCATION=us-central1 \
  core-agent -p "ping"
```

### Notes

- ADC resolution follows the standard Google chain: `GOOGLE_APPLICATION_CREDENTIALS`, `gcloud auth application-default login`, then workload identity in production environments.
- Project/region in config takes precedence over env vars.

### Context caching

Vertex explicit context caching is **on by default** for the stable request prefix (system instruction + tools). On turn 1 the daemon captures the fully-assembled request and creates a `CachedContent` resource; every subsequent turn stamps that cache handle onto the request so the prefix bills at ~10% of the input rate. Typical GKE-triage session prefix is 4–8k tokens — savings compound across every turn.

| Knob | Where | Default |
|---|---|---|
| Kill switch | `--no-context-cache` CLI flag | off (caching ON) |
| Per-project enable | `model.vertex.context_cache.enabled` | `true` (nil = on) |
| Cache TTL | `model.vertex.context_cache.ttl` | `"6h"` |
| Refresh threshold | `model.vertex.context_cache.refresh` | `"30m"` |

Example config:

```json
{
  "model": {
    "provider": "vertex",
    "name": "gemini-3.5-flash",
    "vertex": {
      "project": "my-gcp-project",
      "location": "us-central1",
      "context_cache": {
        "enabled": true,
        "ttl": "6h",
        "refresh": "30m"
      }
    }
  }
}
```

Startup log line confirms the wiring:

```
core-agent: context cache: enabled (ttl=6h0m0s, model=gemini-3.5-flash)
```

Any Vertex `Caches.*` RPC failure degrades to running uncached — the session never fails because of a cache error. Failures are logged with a `core-agent-vertexcache:` prefix so operators can spot them.

A failed cache creation is retried on a bounded backoff (15s, 30s, 1m, 2m, 4m — six attempts over roughly eight minutes), driven by later turns rather than a background timer. This matters most on a freshly deployed daemon whose Workload Identity binding hasn't propagated yet: the first `Caches.Create` gets `403 PERMISSION_DENIED`, and without the retry the agent would pay full input price for the rest of its life over a permissions problem that resolved itself in the first two minutes. Look for:

```
core-agent-vertexcache: Caches.Create failed (attempt 1 of 6; retrying no sooner than 15s from now): ...
```

Once the budget is spent the manager gives up for good — a genuinely misconfigured project is not retried forever — and says so:

```
core-agent-vertexcache: Caches.Create failed 6 times (giving up; agent will run uncached for its lifetime): ...
```

---

## Anthropic (first-party)

Native ADK `model.LLM` adapter for Claude. ADK Go ships only Gemini and Apigee out of the box; this is one of `core-agent`'s two new pieces of code (the other is the same adapter pointed at Vertex AI — see below).

| Provider name | `anthropic` |
| Default model | `claude-opus-5` |
| Auth | API key |
| Env vars | `ANTHROPIC_API_KEY` |
| Config block | `model.anthropic.api_key` (overrides env) |

### Config

```json
{
  "model": {
    "provider": "anthropic",
    "name": "claude-opus-5"
  }
}
```

### CLI

```bash
ANTHROPIC_API_KEY=... core-agent --provider anthropic --model claude-opus-5 -p "ping"
```

### Adapter behavior

- **Streaming** is on by default. Partial text events arrive as `Partial: true` `LLMResponse`s; the final event has `TurnComplete: true` with the full content, usage metadata, and mapped `FinishReason`.
- **Tool round-trip** is supported: genai `FunctionCall` parts → Anthropic `ToolUseBlock`; genai `FunctionResponse` parts → Anthropic `ToolResultBlockParam`. IDs are preserved across the round-trip.
- **System prompt** from `genai.GenerateContentConfig.SystemInstruction` is extracted and lifted to Anthropic's top-level `System` field (Anthropic separates system from messages, unlike Gemini).
- **`MaxTokens`** defaults to 16,384 if not set on the request. Override with `Config.MaxOutputTokens`.
- **Stop reasons** map to genai `FinishReason` as: `end_turn`/`stop_sequence`/`tool_use` → `STOP`, `max_tokens` → `MAX_TOKENS`, `refusal` → `SAFETY`.
- **Prompt caching** is **on by default** on both `anthropic` and `anthropic-vertex` ([#714](https://github.com/go-steer/core-agent/issues/714)). See [Prompt caching](#prompt-caching) below.
- **Cache accounting** works regardless of the above, because Anthropic also caches *automatically* on the first-party and Claude Platform on AWS endpoints. All three input buckets are reported: total prompt as `PromptTokenCount`, cache reads as `CachedContentTokenCount`, and cache **writes** on `LLMResponse.CustomMetadata` under `cache_creation_input_tokens` — genai's usage struct has only two input fields, and writes bill at a premium (1.25× input) rather than a discount, so folding them into either existing bucket would misprice the turn. `pkg/usage` reads the sidecar and surfaces it as `input_tokens_cache_write` ([#263](https://github.com/go-steer/core-agent/issues/263)).

### Prompt caching

Anthropic prompt caching is **on by default** for both providers in the family. It is a different mechanism from [Vertex context caching](#context-caching) above — there is no cache resource to create, just `cache_control` markers on the ordinary Messages request — so the two share no config and no kill switch.

The cache is a byte-exact **prefix** match over the rendered request, in the order tools → system → messages. core-agent places markers in two places:

| Marker | Covers | Why |
|---|---|---|
| System | The tool schemas + the whole system prompt | Identical for every turn *and* every session on the same build, so it keeps paying off across restarts |
| Rolling history | The conversation up to the current end | The runner replays the whole transcript each turn, so turn N+1's prefix is turn N's entire prompt |

History markers are placed backward from the end, 16 content blocks apart, up to the API's limit of 4 markers per request (the system marker takes one of the 4). The spacing matters: a breakpoint only searches back 20 content blocks for an existing entry, and one agentic step can append more than that when the model fans out parallel tool calls. Without intermediate markers the chain to the previous turn's entry breaks, the read misses, and the message history is re-written at the write premium. The spacing tolerates a turn that appends up to 52 content blocks — roughly a 26-wide parallel tool fan-out; beyond that the message cache misses for one turn and re-warms on the next (the system marker keeps reading throughout).

| Knob | Where | Default |
|---|---|---|
| Kill switch | `--no-prompt-cache` CLI flag | off (caching ON) |
| Per-project enable | `model.anthropic.prompt_cache.enabled` | `true` (nil = on) |
| Entry lifetime | `model.anthropic.prompt_cache.ttl`, or `--prompt-cache-ttl` | `5m` |

```json
{
  "model": {
    "provider": "anthropic",
    "name": "claude-opus-5",
    "anthropic": {
      "prompt_cache": { "enabled": false }
    }
  }
}
```

A subagent that declares its own `model` resolves its own provider; it inherits the parent's `prompt_cache` setting unless its own model block sets one, and `--no-prompt-cache` applies to it too.

Startup line:

```
core-agent: prompt cache: enabled (5m ttl, system + rolling history breakpoints)
```

#### Entry lifetime

`ttl` is `5m` (the API default) or `1h`; anything else is a config error at load. `--prompt-cache-ttl` overrides the config for one run — the same checkout wants `5m` under a human at the keyboard and `1h` under a cron that wakes every 20 minutes — and applies to declarative subagents too. The kill switch outranks it: `--no-prompt-cache` with a TTL set still means no caching.

The choice is arithmetic, not preference. A 1-hour write bills at **2×** the input rate against the 5-minute one's 1.25×, and both read at ~10%. Paying the extra 0.75× is worth it only when an entry that would have expired at five minutes gets read at least once more inside the hour:

| Gap between turns | Cheaper |
|---|---|
| Under 5 minutes | `5m` — the entry survives either way, so the 1h premium buys nothing |
| 5 minutes to an hour, and the prefix recurs | `1h` — one avoided re-write more than pays the premium |
| Over an hour, or a prefix that never recurs | `5m`, or `--no-prompt-cache` |

Both rates are billed correctly: the response reports the write split per TTL, and `pkg/pricing` carries a separate 1-hour write rate per model ([#770](https://github.com/go-steer/core-agent/issues/770)). A turn that mixes them — possible when a longer-lived entry is refreshed alongside a fresh one — is priced bucket by bucket rather than at one blended rate.

Leaving the default in place keeps the request bytes identical to a pre-`ttl` build, so upgrading does not orphan the entries the previous build wrote.

**Economics.** Cache reads bill at ~10% of the input rate; cache **writes** bill at 125% of it. Break-even is two requests carrying the same prefix — which the agentic loop clears within seconds of turn 1.

The shape that loses is a call whose prefix never recurs, so core-agent opts those out automatically: the compaction summarizer, the checkpointer, the `/btw` side question, and any subtask running on a budget of two turns or fewer (the `agentic_*` wrappers and the MCP LLM-digest fallback). Their prefixes diverge from the loop's at the first block, so nothing can read what they would write.

One shape stays on and pays a bounded tax: a turn that makes exactly **one** model call after more than five minutes idle — a plain conversational reply in an attach session with a human at the keyboard. Its entry has expired, so it writes at 1.25× and reads nothing (~$0.12 on a 100K-token transcript at Opus rates). If that is most of your workload, `--prompt-cache-ttl=1h` turns those misses into reads for a 0.75× premium on the writes that remain; `--no-prompt-cache` is the answer when the prefix never recurs at all.

**From Go.** A library consumer gets the same defaults from `anthropic.New` / `anthropic.NewVertex`. Override with `anthropic.WithPromptCache(anthropic.CacheOptions{...})` at construction, or `Provider.SetPromptCache` before the first `Model()` call. Pass a zero `CacheOptions` to turn everything off. `models.WithoutPromptCache(ctx)` suppresses caching for one request. The older `WithCacheSystem` is deprecated; it keeps its original all-or-nothing meaning, so `WithCacheSystem(false)` still means no caching at all.

**What invalidates it.** Anything that changes an earlier byte: a compaction or checkpoint (which rewrites the head of the history by design), a change to the system prompt or the agent's name/description, and — the one that can bite mid-session — an MCP server whose `tools/list` response changes. Tool schemas render before the system block, so a server that reorders its tools, reconnects with a different set, or edits a description invalidates every marker in the request.

**TTL.** Markers use Anthropic's default 5-minute TTL. The 1-hour TTL is not exposed: it bills writes at 2× base input where the 5-minute one bills 1.25×, and the rate catalog carries a single write rate, so shipping it today would understate every cached turn by 37.5%. Tracked in [#770](https://github.com/go-steer/core-agent/issues/770).

### Built-in tools

The Anthropic provider can inject Claude's server-side built-in tools alongside any user-defined function declarations.

| Tool | Default | Notes |
|---|---|---|
| **WebSearch** | off | Server-side web search. Per-search billing on top of token cost. Off by default — opt in once you've decided the cost and external-call posture are acceptable. |

To enable from config, use the provider-neutral [`model.builtin_tools`](#configuring-built-ins) block:

```json
{
  "model": {
    "provider": "anthropic",
    "name": "claude-opus-5",
    "builtin_tools": { "web_search": true }
  }
}
```

`url_context` and `code_execution` have no Anthropic equivalent surfaced today and are ignored here.

To enable from the library:

```go
import "github.com/go-steer/core-agent/v2/pkg/models/anthropic"

provider, _ := anthropic.New(key, anthropic.WithWebSearch(true))

// Or replace the whole set:
provider, _ := anthropic.New(key, anthropic.WithBuiltinTools(anthropic.BuiltinTools{
    WebSearch: true,
}))
```

The same options apply to `anthropic.NewVertex(...)`. Other Anthropic server-side tools (`web_fetch`, `code_execution`, `text_editor`, `memory`, `bash`) aren't surfaced today — add them to `BuiltinTools` when a concrete consumer needs one.

### Notes

- Get a key at [console.anthropic.com](https://console.anthropic.com).
- The current default model is `claude-opus-5`. Override per-call with `--model` or `cfg.Model.Name`.
- Claude 5-generation models (`claude-sonnet-5`, `claude-opus-5`, `claude-fable-5`) are fully supported as of v2.8: thinking-default tool loops round-trip correctly (thinking blocks are replayed with signatures intact), builtin pricing ships for all three, and they appear in the TUI's `/model` picker.
- Builtin pricing ships for every chat/tool-calling Claude model in the LiteLLM catalog, including the cache read and cache write rates, so `usage.Tracker.Append` records real cost for Claude turns out of the box. Override per-model via `cfg.Model.Pricing`; a model with no builtin entry still resolves by longest-prefix match (`claude-opus-4-7-1m` picks up `claude-opus-4-7`'s rates) and only falls through to zero cost if nothing matches at all.

---

## Anthropic via Vertex AI

Same adapter as `anthropic`, but the underlying client is constructed against Google Vertex AI. Use this when you want Claude but already have GCP infrastructure: ADC for auth, GCP billing, GCP IAM and compliance posture, no separate Anthropic API key to manage.

| Provider name | `anthropic-vertex` |
| Default model | `claude-opus-5` (Vertex sometimes wants a date-suffixed variant) |
| Auth | ADC + GCP project + region |
| Env vars | `ANTHROPIC_VERTEX_PROJECT_ID` (or `GOOGLE_CLOUD_PROJECT`), `CLOUD_ML_REGION` (or `GOOGLE_CLOUD_LOCATION`) |
| Config block | `model.anthropic.vertex.{project,location}` |

### Config

```json
{
  "model": {
    "provider": "anthropic-vertex",
    "name": "claude-opus-5",
    "anthropic": {
      "vertex": {
        "project": "my-gcp-project",
        "location": "us-east5"
      }
    }
  }
}
```

### CLI

```bash
gcloud auth application-default login
ANTHROPIC_VERTEX_PROJECT_ID=my-gcp-project \
  CLOUD_ML_REGION=us-east5 \
  core-agent --provider anthropic-vertex --model claude-opus-5 -p "ping"
```

### Notes

- Region defaults to `us-east5` (the most common region for Anthropic on Vertex today). Override per-call with config or env.
- Vertex's Claude model IDs sometimes carry a `@version` suffix (e.g. `claude-opus-4-5@20251101`). The bare alias often works; if it doesn't, check the [Vertex Model Garden](https://console.cloud.google.com/vertex-ai/model-garden) for the current ID and pass it via `--model`.
- All adapter behavior (streaming, tool round-trip, system extraction, caching) is identical to first-party Anthropic — only the client construction differs. The conversion code (`models/anthropic/convert.go`, `stream.go`, `llm.go`) is shared.
- Auto-detection is intentionally off — opt in via `--provider anthropic-vertex` or `model.provider: "anthropic-vertex"`.

---

## Echo (mock)

Returns the user's last message verbatim as the model response. No credentials, no streaming, no tool calls. Useful for credential-free smoke tests of the binary.

| Provider name | `echo` |
| Default model | (ignored) |
| Auth | none |
| Env vars | none |
| Config block | none |

### CLI

```bash
core-agent --provider=echo -p "ping"
# model response: "ping"
```

Auto-detection is intentionally off — opt in via `--provider=echo` or `model.provider: "echo"`.

---

## Scripted (mock)

Replays a JSONL transcript turn-by-turn. Pair with `--record-to` against a real provider to capture the transcript first; then run with `--provider=scripted` to replay it offline.

| Provider name | `scripted` |
| Default model | (ignored) |
| Auth | none |
| Env vars | none |
| Config block | `mock.script` (required), `mock.strict` (optional) |

### Config

```json
{
  "model": { "provider": "scripted" },
  "mock":  { "script": "fixtures/session.jsonl", "strict": false }
}
```

### CLI

```bash
# Capture a real session:
GEMINI_API_KEY=... core-agent --record-to=/tmp/session.jsonl -p "summarize main.go"

# Replay it without credentials:
core-agent --provider=scripted --script=/tmp/session.jsonl -p "anything"

# Strict mode — fail on prompt drift:
core-agent --provider=scripted --script=/tmp/session.jsonl --script-strict -p "summarize main.go"
```

### Notes

- Lenient mode (default): yields the next recorded responses regardless of the incoming request. Good for "drive the loop without an API key."
- Strict mode: the incoming request's `Contents` must JSON-equal the recorded request. Catches regressions in prompt construction. `Config` is intentionally not compared — tool decls legitimately drift.
- Replay reproduces the LLM side faithfully, but tool execution at replay time uses the live environment. If files have changed, the agent feeds different outputs back to the scripted LLM, which still returns the next canned response. See [DESIGN.md → Mock providers and recording](https://github.com/go-steer/core-agent/blob/main/docs/DESIGN.md) for the full caveat.
- The script must contain at least one turn; an empty file is rejected at startup.
- One script, one cursor. `--provider=scripted` is a single-agent replay: everything that asks the provider for a model shares that cursor, so a fan-out of background subagents would divide one transcript between them. Embedding callers who need each agent to replay the same script from its own first turn should construct `mock.NewScriptedPerCall` directly — see [Embed → Recording LLM turns](/embed/api/#recording-llm-turns).

---

## Roadmap

Likely additions in future milestones, ordered by approximate effort:

- **Amazon Bedrock** as a third Anthropic backend — direct extension of the Vertex pattern; the SDK ships a `bedrock/` subpackage that mirrors `vertex/`.
- **Claude Platform on AWS** — Anthropic-operated, SigV4-authed via the SDK's `aws/` subpackage.
- **Anthropic feature coverage** — extended/adaptive thinking, structured outputs, server-side tools (`web_search`, `code_execution`), vision.

See the [project README's Milestones section](https://github.com/go-steer/core-agent#milestones) for what's currently planned.

---

## Adding your own provider

The `models.Provider` interface is the extension point:

```go
type Provider interface {
    Name() string
    Model(ctx context.Context, modelID string) (model.LLM, error)
}
```

Register your implementation in an `init()` and import the package for its side effect:

```go
package myprovider

import (
    "github.com/go-steer/core-agent/v2/pkg/config"
    "github.com/go-steer/core-agent/v2/pkg/models"
)

func init() {
    models.Register("my-provider", func(cfg *config.Config) (models.Provider, error) {
        return &Provider{...}, nil
    })
}
```

Then in your binary:

```go
import _ "your.org/myprovider"
```

`models.Resolve(cfg)` will pick it up when `cfg.Model.Provider == "my-provider"`. See [Library API](/embed/api/) for more.
