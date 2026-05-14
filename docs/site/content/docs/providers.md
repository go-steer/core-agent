---
title: Providers
weight: 3
---

# Providers

`core-agent` ships four model backends, all behind the same `models.Provider` interface. Pick one explicitly via `model.provider` in `.agents/config.json` or with the `--provider` CLI flag, or let env-based auto-detection pick.

---

## Auto-detection

When `model.provider` is empty (the default), `core-agent` walks the environment in this order and picks the first match:

1. **Vertex Gemini** — fires when `GOOGLE_GENAI_USE_VERTEXAI=true` **and** `GOOGLE_CLOUD_PROJECT` is set
2. **Gemini API** — fires when `GOOGLE_API_KEY` **or** `GEMINI_API_KEY` is set
3. **Anthropic** — fires when `ANTHROPIC_API_KEY` is set

If none match, you get a clear error listing the env vars to set. **Anthropic-via-Vertex is not auto-detected** — it overlaps with Vertex Gemini in env vars, so you have to opt in explicitly with `--provider anthropic-vertex` or `model.provider: "anthropic-vertex"` in config.

---

## Gemini API

The simplest backend — talks directly to `generativelanguage.googleapis.com` with an API key.

| Provider name | `gemini` |
| Default model | `gemini-3.1-pro-preview` |
| Auth | API key |
| Env vars | `GEMINI_API_KEY` (preferred), `GOOGLE_API_KEY` (also accepted) |
| Config block | `model.api_key` (overrides env) |

### Config

```json
{
  "model": {
    "provider": "gemini",
    "name": "gemini-3.1-pro-preview"
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

To override:

```go
import "github.com/go-steer/core-agent/models/gemini"

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

The same options apply to `gemini.NewVertex(...)`. Other genai built-ins (`FileSearch`, `GoogleMaps`, `ComputerUse`, `EnterpriseWebSearch`, `GoogleSearchRetrieval`, `Retrieval`) aren't surfaced today — they require upstream setup and would yield API errors rather than working tools if flipped on without it.

---

## Vertex AI (Gemini)

Same Gemini models, but routed through Google Vertex AI with Application Default Credentials.

| Provider name | `vertex` |
| Default model | `gemini-3.1-pro-preview` |
| Auth | ADC (Application Default Credentials) |
| Env vars | `GOOGLE_GENAI_USE_VERTEXAI=true`, `GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION` |
| Config block | `model.vertex.{project,location}` |

### Config

```json
{
  "model": {
    "provider": "vertex",
    "name": "gemini-3.1-pro-preview",
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

---

## Anthropic (first-party)

Native ADK `model.LLM` adapter for Claude. ADK Go ships only Gemini and Apigee out of the box; this is one of `core-agent`'s two new pieces of code (the other is the same adapter pointed at Vertex AI — see below).

| Provider name | `anthropic` |
| Default model | `claude-opus-4-7` |
| Auth | API key |
| Env vars | `ANTHROPIC_API_KEY` |
| Config block | `model.anthropic.api_key` (overrides env) |

### Config

```json
{
  "model": {
    "provider": "anthropic",
    "name": "claude-opus-4-7"
  }
}
```

### CLI

```bash
ANTHROPIC_API_KEY=... core-agent --provider anthropic --model claude-opus-4-7 -p "ping"
```

### Adapter behavior

- **Streaming** is on by default. Partial text events arrive as `Partial: true` `LLMResponse`s; the final event has `TurnComplete: true` with the full content, usage metadata, and mapped `FinishReason`.
- **Tool round-trip** is supported: genai `FunctionCall` parts → Anthropic `ToolUseBlock`; genai `FunctionResponse` parts → Anthropic `ToolResultBlockParam`. IDs are preserved across the round-trip.
- **System prompt** from `genai.GenerateContentConfig.SystemInstruction` is extracted and lifted to Anthropic's top-level `System` field (Anthropic separates system from messages, unlike Gemini).
- **`MaxTokens`** defaults to 16,384 if not set on the request. Override with `Config.MaxOutputTokens`.
- **Stop reasons** map to genai `FinishReason` as: `end_turn`/`stop_sequence`/`tool_use` → `STOP`, `max_tokens` → `MAX_TOKENS`, `refusal` → `SAFETY`.
- **Prompt caching** is opt-in. Construct the provider with `anthropic.WithCacheSystem(true)` and the last system block carries an ephemeral `cache_control`. Off by default — only enable once you've confirmed the system prompt is stable across turns, otherwise you pay the cache write premium for nothing.

### Notes

- Get a key at [console.anthropic.com](https://console.anthropic.com).
- The current default model is `claude-opus-4-7`. Override per-call with `--model` or `cfg.Model.Name`.
- Pricing entries for Claude models are intentionally absent from `usage.PriceFor` today — `usage.Tracker.Append` will record zero cost for Claude turns. Override per-model via `cfg.Model.Pricing`.

---

## Anthropic via Vertex AI

Same adapter as `anthropic`, but the underlying client is constructed against Google Vertex AI. Use this when you want Claude but already have GCP infrastructure: ADC for auth, GCP billing, GCP IAM and compliance posture, no separate Anthropic API key to manage.

| Provider name | `anthropic-vertex` |
| Default model | `claude-opus-4-7` (Vertex sometimes wants a date-suffixed variant) |
| Auth | ADC + GCP project + region |
| Env vars | `ANTHROPIC_VERTEX_PROJECT_ID` (or `GOOGLE_CLOUD_PROJECT`), `CLOUD_ML_REGION` (or `GOOGLE_CLOUD_LOCATION`) |
| Config block | `model.anthropic.vertex.{project,location}` |

### Config

```json
{
  "model": {
    "provider": "anthropic-vertex",
    "name": "claude-opus-4-7",
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
  core-agent --provider anthropic-vertex --model claude-opus-4-7 -p "ping"
```

### Notes

- Region defaults to `us-east5` (the most common region for Anthropic on Vertex today). Override per-call with config or env.
- Vertex's Claude model IDs sometimes carry a `@version` suffix (e.g. `claude-opus-4-5@20251101`). The bare alias often works; if it doesn't, check the [Vertex Model Garden](https://console.cloud.google.com/vertex-ai/model-garden) for the current ID and pass it via `--model`.
- All adapter behavior (streaming, tool round-trip, system extraction, caching) is identical to first-party Anthropic — only the client construction differs. The conversion code (`models/anthropic/convert.go`, `stream.go`, `llm.go`) is shared.
- Auto-detection is intentionally off — opt in via `--provider anthropic-vertex` or `model.provider: "anthropic-vertex"`.

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
    "github.com/go-steer/core-agent/config"
    "github.com/go-steer/core-agent/models"
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

`models.Resolve(cfg)` will pick it up when `cfg.Model.Provider == "my-provider"`. See [Library API]({{< relref "library-api.md" >}}) for more.
