# Model support beyond Gemini and Claude

**Status:** decided 2026-10-08. The implementation lives in another repo. This
note records what the decision means for core-agent, so a contributor who goes
looking for OpenAI or vLLM support in `pkg/models` finds out where it went.

## The decision

core-agent needs more providers than it has today:
- OpenAI first-party;
- the Vertex AI MaaS partner models (Grok, DeepSeek, Qwen, gpt-oss, Llama);
- self-hosted models served by vLLM, SGLang or Ollama.

mast needs the same set. The two repos' Gemini and Anthropic adapters started as
one copy, mast's ported from `b8dd225`, and have since drifted in both
directions:
- core-agent is ahead on rolling and 1-hour Anthropic prompt caching (#714,
  #770), Gemini transient retry (#1039, #1247) and `IsBelowCacheMinimum`
  (#1067).
- mast is ahead on a pointer-valued usage sidecar that tells "not reported"
  apart from zero, and on option-struct constructors with no registry.

Building the new providers here and porting them would add a third copy to
reconcile.

So provider support is built **once**, in
[`go-steer/core-models`](https://github.com/go-steer/core-models), and both repos
import it. The full design is
[core-models `docs/design.md`](https://github.com/go-steer/core-models/blob/main/docs/design.md).
The requirements it has to meet are in mast's
[`docs/model-support-design.md`](https://github.com/go-steer/mast/blob/main/docs/model-support-design.md):
R1–R8, including cost and usage parity with Gemini and Claude.

## What it means for core-agent

- **Do not add new providers to `pkg/models`.** The OpenAI-shaped dialects
  (`openai-chat` and `openai-responses`) and the profiles that ride on them are
  written in core-models.
- **core-agent consumes core-models through the `adkv1` shim.** The library's
  core is ADK-free and speaks genai. `core-models/adkv1` adapts it to
  `google.golang.org/adk` v1's `model.LLM`, and mast uses `adkv2`. When
  core-agent moves to ADK v2 the shim goes away; that move isn't scheduled and
  doesn't block this work.
- **Adoption is core-models phase L3'**, after the Chat Completions dialect,
  the Responses dialect and the self-hosted KV-metrics sampler land:
  - `models.Register` gains profile-backed providers.
  - `.agents/config.json` gains a profile section; the schema comes from
    core-models, and core-agent decides where it is read from.
  - `--provider` stops being a closed set.
  - The existing `gemini`, `vertex`, `anthropic` and `anthropic-vertex` paths
    are unchanged.
- **The Gemini and Anthropic adapters move later (L4/L5).** core-agent's newer
  behaviour comes along with them: prompt caching and its TTLs, the 1-hour
  cache-write share, Gemini retry, and the cache-minimum check. After that,
  `pkg/models` becomes a thin facade that maps `config.ModelConfig` onto
  profiles, and its subpackages become imports. The legacy cache-write sidecar
  keys (`cache_creation_input_tokens`, `cache_creation_1h_input_tokens`) keep
  being written until `pkg/usage` reads the library's record, so `usage.Rebuild`
  over old event logs keeps working.
- **Pricing moves last (L6).** `pkg/pricing`'s mechanics merge with mast's
  backend-keyed catalog in core-models. The file locations under
  `~/.core-agent`, the refresh policy and the config override stay here.
- **Product policy stays here:**
  - the auto-detect precedence;
  - `taskclass` defaults and `--task`;
  - compaction thresholds;
  - `pkg/usage` tracking and its metrics;
  - CLI flags;
  - the echo and scripted mocks.

## What does not change

Nothing changes in core-agent's code today. Provider fixes to
`pkg/models/{anthropic,gemini}` and `internal/vertexcache` keep landing here
until each package is extracted. After that a fix is a core-models release and a
version bump here.
