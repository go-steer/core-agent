# Pricing: extensible, current, and honest

Design doc for replacing core-agent's single hardcoded pricing table
(`usage/pricing.go`'s 9-entry `builtinPricing` map covering only
Gemini 3.x) with a layered lookup that supports per-project overrides,
a community-maintained external source, and explicit "rate unknown"
signaling when nothing resolves. Stops shipping silent `$0` lies for
the increasingly-common case of fine-tuned or new model variants.

## Why this doc exists

Today's pricing has three problems UAT surfaced:

1. **Coverage gap.** `gemini-3.5-flash`, the entire Anthropic family,
   the OpenAI family, fine-tuned variants — none have entries. Cost
   shows `$0.0000` for sessions on those models. Honest fix earlier
   surfaces `$—` instead, but the underlying gap is the real problem.
2. **No update path.** A new model lands, operators wait for a
   core-agent release. Rates change, same story. Hand-curated tables
   age badly when the curation lives on a release cadence.
3. **Override is single-model-only.** `cfg.Model.Pricing` matches
   only `cfg.Model.Name` (case-insensitive). After a `/model` switch
   the override doesn't apply. Operators who route across 3-4 models
   in one session can't set rates for all of them.

## What this changes

Three layers, each independently shippable, designed so the lookup
chain has one obvious answer and silent zeros become impossible.

### Layer A: file-based pricing (small, no network)

Lookup chain becomes:

1. `cfg.Model.Pricing` — single-model override (today's behavior;
   kept for backwards compat). Promote to a *map* keyed by model
   name so all routed models can be priced.
2. `.agents/pricing.json` — project-local additions (internal
   variants, team-specific routing).
3. `~/.core-agent/pricing.json` — user-global cache + manual
   overrides.
4. `usage/pricing.go`'s built-in table — the curated fallback (still
   in the binary so air-gapped installs and zero-config use cases
   "just work" for common Gemini models).
5. Longest-prefix match across the merged set — handles
   `gemini-3.1-pro-preview-customtools`-style suffixes.
6. `$—` (rate unknown) — never returns `$0` for a real session.

File format (one shape across all three locations):

```json
{
  "version": 1,
  "models": {
    "gemini-3.5-flash": { "input_per_m_tok": 0.075, "output_per_m_tok": 0.30 },
    "claude-opus-4-7":  { "input_per_m_tok": 15.0,  "output_per_m_tok": 75.0 },
    "gpt-5":            { "input_per_m_tok": 2.50,  "output_per_m_tok": 10.0 }
  }
}
```

Scope: ~80 LoC across `usage/`, `config/`, and a new
`internal/pricing/` package that holds the loader + merge logic.

### Layer B: external-source refresh (network, opt-in)

Daily fetch from a canonical community source into
`~/.core-agent/pricing.json`'s "external" section (kept separate
from manual overrides). Lookup at runtime sees the merge of all
sources; refresh only rewrites the external slice.

Cadence: once-per-day on startup. Skip when:
- Last fetch < 24h ago (stamped in the cache file)
- `pricing.refresh: false` in config
- `--no-pricing-refresh` CLI flag (for CI / air-gapped pods)
- Network unreachable (fail silent; operator sees the existing
  cache or `$—`, never a startup error)

HTTP: `If-Modified-Since` (or `ETag`) so we don't re-download
unchanged data. ~80 LoC across `internal/pricing/refresh.go` + the
startup wire-up in `cmd/core-agent/main.go`.

### Layer C: force refresh (trivial, on top of B)

A `/pricing refresh` slash command that triggers the same fetch on
demand. The TUI surfaces the result inline ("Updated 247 models
from LiteLLM" or "Failed: connection refused"). ~10 LoC of
glue in `internal/tui/commands.go` + handler in `update.go`.

## Decisions to lock in

### Data source for layer B

| Source | Pros | Cons |
|---|---|---|
| **LiteLLM** [`model_prices_and_context_window.json`](https://github.com/BerriAI/litellm/blob/main/model_prices_and_context_window.json) | Community-maintained; covers ~hundreds of models from every major provider; structured JSON; stable URL; non-commercial use is unrestricted | Third-party dependency; LiteLLM's schema includes more than just prices (we'd ignore the rest); MIT-licensed file |
| **OpenRouter** [`/api/v1/models`](https://openrouter.ai/api/v1/models) | Official-ish; structured JSON; covers OpenAI/Anthropic/Google/Meta/etc. | Auth-aware (logged-in users see custom rates); requires API key for some endpoints; vendor-product-coupled |
| **Per-vendor scraping** | No dependency | Brittle; breaks on every vendor page redesign; one scraper per vendor; high maintenance |
| **Self-published** (`pricing.core-agent.dev/v1.json`) | We own the schema; we control updates | We take on curation burden; needs publishing infra |

**Recommendation: LiteLLM.** Pragmatic; the schema is stable; non-LLM
projects (langchain, etc.) already consume it; no auth required;
falls back to our built-in table when unreachable. We map their
field names (`input_cost_per_token` etc.) to our `Pricing` struct
in a one-function adapter — cheap to maintain.

### Refresh trigger

| Trigger | Pros | Cons |
|---|---|---|
| **Daily on startup** (recommended) | Self-maintaining; new models appear within 24h of operator's next session | Startup adds a ~200ms network call (mitigated by cache + 304) |
| **Weekly on startup** | Less network noise | Misses fast-moving model launches |
| **First startup of each session** | Always fresh | Each session pays the cost; annoying for short scripts |
| **Manual only** (no daily) | Zero startup network | Operators forget; rates rot |

**Recommendation: daily, opt-in.** Default to enabled (most operators
want it); easy to disable.

### Cache file format

One file, sectioned:
```json
{
  "version": 1,
  "external": {
    "fetched_at": "2026-05-24T15:32:11Z",
    "source": "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json",
    "etag": "W/\"abc123\"",
    "models": { /* fetched data, mapped to our shape */ }
  },
  "manual": {
    "models": { /* operator-curated, never auto-modified */ }
  }
}
```

Refresh rewrites only the `external` section. `manual` is operator-
edited and round-trips intact. Layer A's project-local
`.agents/pricing.json` uses just the flat `{ "version": 1, "models":
{...} }` shape — no `external`/`manual` split (project files are
always "manual").

### Where does built-in pricing live?

**Keep** `usage/pricing.go`'s map as the zero-config fallback. Air-
gapped pods + fresh installs + offline CI all need *some* pricing
without network or files. Stays small (5-10 popular models), bumped
on releases when major new models land. Layer B reduces — but
doesn't eliminate — the need to update it.

### Lookup precedence (final)

```
cfg.Model.Pricing[name]        (CLI / config override)
  → .agents/pricing.json       (project file)
  → ~/.core-agent/pricing.json (user file: manual + external merged)
  → usage/pricing.go           (built-in table)
  → longest-prefix match       (across the merge of all the above)
  → $— (unknown)
```

First exact match wins; only the prefix match falls back across the
merge. Operators can override LiteLLM's number for any model by
adding to the manual section.

## Settled

1. **Pull the whole LiteLLM file.** ETag-cached so re-fetches are
   304s; cold start transfers ~3 MB once. K8s docs should call out
   the storage hit + the `--no-pricing-refresh` opt-out.
2. **Stale-on-failure uses cache + stderr warning.** TUI stays
   usable when offline; operators see a one-line "pricing data is
   N days stale" note. Corrupt-cache same: warn + fall back to
   built-in.
3. **Layer C includes `/pricing set <model> <in> <out>`.** Writes
   to the manual section of `~/.core-agent/pricing.json`
   atomically. Operators don't have to leave the TUI to fix a
   rate.

## Out of scope

- Per-context pricing (e.g. anthropic's prompt-caching rates that
  differ from base rates). Tracker doesn't separate cached vs
  uncached tokens today; revisit when we add cache-aware token
  accounting.
- Per-tool pricing (function-calling, vision, etc.). Same reason —
  not in the tracker's data model.
- An `update_pricing` agent tool — operators don't want the model
  making network calls for accounting reasons.
- A pricing dashboard / web UI. CLI / TUI is the surface.

## Phased delivery

Three PRs, each independently shippable:

1. **Layer A + map-shaped cfg override.** `internal/pricing` package
   skeleton + loader + lookup chain + tests. Closes the
   "operators can't price multiple models" pain. No network.
2. **Layer B.** Add the LiteLLM fetcher + daily-refresh wire-up +
   `--no-pricing-refresh` flag + config knob. Network-aware.
3. **Layer C.** `/pricing refresh` slash + (optionally) `/pricing
   set` slash. Trivial on top of B.

Each PR carries its own tests + docs/site updates.
