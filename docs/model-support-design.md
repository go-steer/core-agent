# Model support beyond Gemini and Claude

**Status:** decided 2026-10-08; adopted 2026-10-10 (core-models phase L3',
`pkg/models/profiles`). The implementation lives in another repo. This note
records what the decision means for core-agent, so a contributor who goes looking
for OpenAI or vLLM support in `pkg/models` finds out where it went.

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
- **Adoption is core-models phase L3', and it has landed.** It was planned for
  after the Responses dialect (L2) and the self-hosted KV-metrics sampler (L3);
  it went first, on core-models v0.5.0 with the Chat Completions dialect alone,
  because that already covers the Vertex AI partner models and every
  self-hosted server tested. L2 and L3 are still pending in core-models and
  arrive here as version bumps. What landed:
  - `pkg/models/profiles` registers one constructor for every profile
    (`models.RegisterProfiles`); `models.Resolve` routes any provider name that
    isn't one of core-agent's own six to it.
  - `.agents/config.json` gains a top-level `providers` list in core-models'
    profile schema, checked for shape at load. core-models' built-in profiles
    (`vertex-maas`, `vllm`, `sglang`, `ollama`, `openai-compatible`) are
    selectable without one.
  - `--provider` and `model.provider` take any profile name. A profile can't
    take one of core-agent's six names.
  - Usage arrives in genai's fields plus the legacy cache-write sidecar key, so
    `pkg/usage` reads it unchanged.
  - A model no catalog prices is priced from the profile's declared `rates`, a
    new lowest-precedence `pricing` layer. An explicit cost ceiling on a
    profile model with no price is refused at startup.
  - The existing `gemini`, `vertex`, `anthropic` and `anthropic-vertex` paths
    are unchanged, and so is auto-detection.
- **The Gemini and Anthropic adapters move (L4/L5)** to core-models v0.6.0
  (`dialect/anthropic`, `dialect/gemini`, `dialect/gemini/vertexcache`).
  core-agent's newer behaviour came along with them: prompt caching and its
  TTLs, the 1-hour cache-write share, the bare-400 classification, and the
  cache-minimum check. `pkg/models/anthropic` and `pkg/models/gemini` become
  thin facades with the same exported API, mapping `config.Config` onto the
  library; `internal/vertexcache` is gone. Decisions the facades make:
  - **One Gemini retry layer.** `pkg/models.RetryPolicy` keeps 429/503 and the
    bare 400, its process-wide budget and the `RetryError` a transcript
    shows (#1206); the library's HTTP-layer retry is turned off for Gemini.
    Claude keeps the library's HTTP-layer retry, which replaces the SDK's own.
  - **One context vocabulary.** `models.AsSideCall`, `WithPriorSuccess`,
    `WithoutPromptCache` and `WithoutBuiltins` are core-models' `callctx`
    marks, so the library sees exactly what core-agent sets.
  - **The cache-write sidecar is a translation.** `models.Adapt` copies the
    library's `usage.Detail` cache writes into `cache_creation_input_tokens`
    and `cache_creation_1h_input_tokens`, so `pkg/usage` and `usage.Rebuild`
    over old event logs are unchanged.
  - The Gemini grounding projection (session events) stays here.
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

A provider fix is a core-models release and a version bump here. What stays
here is the facade: config mapping, defaults, the Gemini retry policy, and the
grounding projection.
