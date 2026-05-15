# core-agent: design notes

This document captures the architectural choices behind core-agent — the *why*, not the *what*. For "what is this and how do I use it" read the [README](../README.md) and the [docs site](https://go-steer.github.io/core-agent/). Read this when you're staring at a design decision that isn't obvious and want to know what trade-off motivated it.

It's intentionally opinionated and intentionally honest about what's unresolved.

---

## Goals and non-goals

### Goals

- **Be the substrate for Go agents, not an agent itself.** Provide the wiring — model providers, MCP, skills, instructions, permissions, telemetry, and a baseline tool suite — so consuming projects only have to write their domain-specific tools and product logic.
- **Match cogo's conventions.** A future maintainer who knows cogo should recognize the package layout, the dev tooling, the AGENTS.md / `.agents/` convention, and the milestone discipline. The two projects are intended to coexist; copy-pasting fixes between them should be a one-line port.
- **Stay narrow but useful.** Resist adding features that genuinely belong in the consumer (TUI, slash commands beyond `/exit`, product-specific tools). Built-in tools (`read_file`, `write_file`, `edit_file`, `list_dir`, `bash`, `todo`) ship by default — they're the universal floor for any tool-using agent — but everything beyond that is a consumer concern. (See "Built-in tools" section below for the rationale on this line.)
- **Make extension obvious.** The `Provider` interface, the `Option` pattern on `agent.New`, the `tools.GateToolset` wrapper — these are the shapes you want a consumer to be able to extend without reading half the codebase first.
- **Provide first-class Claude support.** ADK Go ships only Gemini and Apigee; we add Anthropic + Vertex Anthropic as the substantive new code.

### Non-goals

- **Not a finished agent.** No TUI. No rich slash-command framework. No domain-specific tools beyond the generic file/shell/todo set. Those belong in the consumer.
- **Not a multi-language polyglot.** Go only. We don't try to mirror the Python ADK or the TypeScript ADK behavior.
- **Not a competitor to ADK.** We wrap ADK; we don't replace it. When ADK's `runner.Runner`, `llmagent.LlmAgent`, or `model.LLM` change shape, our adapter packages change too.
- **Not a benchmarking surface.** No micro-bench infrastructure, no eval harness. Consumers wire those up.
- **Not a replacement for cogo.** Cogo is a finished agent with a TUI; core-agent is the ground layer that future cogos could be built on top of.

---

## How this relates to cogo

Most of core-agent's package shapes are lifted nearly verbatim from cogo's `internal/` packages — `agent/`, `instruction/` (was `internal/memory/`), `config/`, `permissions/`, `mcp/`, `skills/`, `models/`, `telemetry/`, `usage/`, `session/`, `runner/headless.go` (was `internal/headless/`).

Why lift instead of share? Two reasons:

1. **Cogo's `internal/` is internal.** Go's `internal` rule prevents any other module from importing those packages. Cogo is a finished CLI binary, not a library; making its internals public would be a one-way door we don't want to walk through.
2. **The shapes are stable enough.** The packages we lifted have been through cogo's slice cycles and proved out in dogfood. Re-using them as the foundation for downstream projects is lower-risk than designing fresh.

The substantive new code in M1 was the Anthropic adapter — ADK Go has no Anthropic backend out of the box, and a Go agent that can't talk to Claude is a much smaller market. M2 added the Vertex backend for the same provider with no changes to the conversion code.

When fixing bugs, we should usually port between the two projects. The structure makes that mostly mechanical.

---

## Package layout + dependency rules

```
            agent ─────────────────────────────┐
              │                                │
              ├── instruction                  │
              ├── config                       │
              │                                ▼
              ├── permissions ◀──── tools ──▶ ADK toolset
              │                                ▲
              ├── mcp ─────────────────────────┤
              ├── skills ──────────────────────┘
              ├── models
              │     ├── gemini      ─▶ ADK / genai
              │     └── anthropic   ─▶ Anthropic SDK
              │                          (+ vertex/)
              ├── telemetry
              ├── usage
              ├── session
              └── runner ──────────────────────┐
                                               ▼
                                       cmd/core-agent
```

Two dependency rules worth pinning explicitly:

1. **`permissions/` does not import any ADK package.** It's pure policy logic. The bridge between `permissions.Gate` and ADK's `tool.Toolset` lives in a separate `tools/` package (one file, `gate.go`) so the gate stays usable in non-ADK contexts and so the policy code stays testable without ADK.
2. **`mcp/` and `skills/` import `tools/` for the gate wrapper** but do NOT depend on each other. They're independent capabilities; a consumer can use one without the other (or neither).

The `cmd/core-agent/main.go` is a *reference* composition. Production consumers will write their own binary that picks which packages to wire up.

---

## Multi-turn handling

We rely on ADK's `runner.Runner` + `session.InMemoryService()` to preserve conversation history across turns. The agent reuses the same `(userID, sessionID)` pair on every `Run()` call, and ADK appends the new events to the existing session.

Alternative considered: thread `Contents []*genai.Content` manually across calls (the way you'd do it without a session service). Rejected because:

- ADK's session service does this correctly already, with the right notion of "what's a turn" (handles tool-call cycles, partial events, etc.).
- Manual threading means we have to make our own decisions about how to compress / summarize history. That belongs in the consumer (or in a future M3+ work item), not in the base.
- The in-memory store is fine for v1 — REPL history dies with the process, but that's acceptable for a base library.

The marker for M3 (file-backed sessions) is in this rationale: when the time comes, plug in a `session.Service` implementation that persists to disk. The `agent.WithSession` option is already the seam.

---

## Provider interface

The `models.Provider` interface is the single extension point for adding a model backend:

```go
type Provider interface {
    Name() string
    Model(ctx context.Context, modelID string) (model.LLM, error)
}
```

Why this shape:

- **`Name()` returns the registered identity** — used by telemetry, transcripts, the resolver's error messages. We had a brief design choice between embedding `name` in the struct vs. returning a constant from each Provider type. Embedding won when M2 added `anthropic-vertex`: the same `Provider` struct now serves both the first-party and Vertex Anthropic backends, with `name` differing per construction. One `name` field, one `Name()` method.
- **`Model(ctx, modelID)` is per-call** rather than at Provider construction time. This lets one Provider serve many models without reconstruction (the Gemini provider is bound to one credential set but can return any Gemini model ID). The downside is that `Model()` may be called many times; implementations should be cheap there.
- **The registry pattern (`models.Register`/`models.Resolve`)** keeps the rest of the codebase free of provider-specific imports. `cmd/core-agent/main.go` blank-imports `models/anthropic` and `models/gemini` to trigger their `init()`s; everything else uses `models.Resolve(cfg)`.

What this *doesn't* express:

- Cost models, rate limits, caching policies — all live elsewhere (`usage/`, retry not yet implemented, opt-in `WithCacheSystem` per provider).
- Streaming preferences — controlled by `agent.RunConfig` and the LLM implementation, not the Provider.
- Multi-modal support — the `model.LLM` interface ADK exposes is genai-shaped, so any provider that wants images / audio has to convert to/from genai's Part types. The Anthropic adapter currently handles only text + tool round-trip; image support is a deferred item (see "Open questions" below).

---

## Anthropic adapter (the substantive new code)

This is the hardest piece in M1, and the only piece in M2. It's worth documenting in detail.

### The job

Implement ADK's `model.LLM` interface against Anthropic's API. ADK speaks genai-shaped requests/responses (because ADK was Gemini-first); Anthropic's API is its own shape. We sit in the middle and translate.

```go
// What ADK gives us:
type LLMRequest struct {
    Model    string
    Contents []*genai.Content              // Gemini-shaped messages
    Config   *genai.GenerateContentConfig  // system, tools, temp
    Tools    map[string]any                // unused (tools live on Config)
}

// What we have to produce:
iter.Seq2[*LLMResponse, error]
// where LLMResponse carries genai.Content + usage + finish reason
```

### Conversion decisions

#### System prompt extraction

Anthropic separates system from messages — it's a top-level `MessageNewParams.System []TextBlockParam` field. Gemini treats the system prompt as `genai.Content` with role `"system"` (or in `Config.SystemInstruction`, depending on caller).

Decision: **always read system from `Config.SystemInstruction`**, not from `Contents`. Lift it to the top-level Anthropic field.

Rationale: this matches what ADK's llmagent does — it puts the system instruction on `Config.SystemInstruction`, not in the messages. If a future caller decides to inline a `Content{Role: "system", ...}` in `Contents`, our converter would currently drop it (`mapRole` returns empty for unknown roles, which causes the message to be skipped). That's a deliberate trade-off — putting system content in two places would double-count it.

#### Role mapping

Genai uses `"user"` / `"model"`; Anthropic uses `"user"` / `"assistant"`. Empty role from genai is treated as user (matches genai's defaults).

#### Tool round-trip

Three places this gets tricky:

1. **Tool declarations** (request → Anthropic). Genai puts them on `Config.Tools` (`[]*genai.Tool` with `FunctionDeclarations`). The ADK's `req.Tools` `map[string]any` is unused — the existing Gemini provider ignores it too, so we follow suit.
2. **Assistant tool-use** (response → genai). When Anthropic returns `ToolUseBlock`, we convert it to `genai.Part{FunctionCall: ...}` with the tool's `ID`, `Name`, and unmarshalled JSON `Args`.
3. **User tool-result** (request → Anthropic). When genai sends back a `Part{FunctionResponse: ...}`, we convert it to Anthropic's `ToolResultBlockParam` keyed by the original `tool_use_id`. ID preservation is critical — Anthropic enforces matching IDs.

The schema projection for tool declarations was the fiddly part. Genai's `Schema` type has many fields; Anthropic wants a JSON-schema-shaped `map[string]any`. We JSON-roundtrip the genai schema and pull `properties` + `required` out. This is robust against future genai field additions but loses any genai-specific extensions (which we don't currently need).

#### Streaming

Default to streaming (`client.Messages.NewStreaming(...)`). For each event, accumulate via `Message.Accumulate()` and yield partial `LLMResponse` per text delta. After `MessageStopEvent`, yield the final `LLMResponse` with `TurnComplete: true`, full content, usage, and mapped FinishReason.

We deliberately ignore `InputJSONDelta` events (incremental tool args) — `Message.Accumulate()` builds the final `ToolUseBlock` for us; we surface the complete tool call only when the message is done. That matches what ADK's runner expects.

#### Stop-reason mapping

| Anthropic | genai |
|---|---|
| `end_turn`, `stop_sequence`, `tool_use` | `STOP` |
| `max_tokens` | `MAX_TOKENS` |
| `refusal` | `SAFETY` |
| `pause_turn`, anything else | `OTHER` |

`tool_use` → `STOP` is correct because the ADK runner uses `STOP` + the presence of a `FunctionCall` part to know it should dispatch tools. It's not a "max-tokens-style" interruption; it's the model deliberately ending a turn at a tool call.

#### MaxTokens default

Anthropic's API requires `MaxTokens`; there's no implicit default. If `Config.MaxOutputTokens` is set, use it; otherwise default to **16,384**. That's plenty for most turns and well under the streaming SDK's HTTP timeouts.

#### Prompt caching

Off by default. Construct the Provider with `WithCacheSystem(true)` to opt in — adds an ephemeral `cache_control` to the last system block, so repeated turns with the same system prompt get the prompt-caching discount.

Why opt-in: the cache write costs more than a normal request. If the system prompt changes between turns (which is the *default* for many use cases — agents that mutate their own instructions, agents that load fresh context per turn), caching loses money. Consumers who know their system prompt is stable should turn it on.

### Things the adapter explicitly doesn't do (yet)

- **Extended / adaptive thinking** — `claude-opus-4-7` defaults to no thinking. We don't expose `Thinking` config. Adding it is a future M3+ item, controlled by genai's `ThinkingConfig` field if/when ADK starts using it.
- **Server-side tools** (`web_search`, `code_execution`) — we don't surface them to genai callers because there's no genai equivalent.
- **Vision / inline data** — `Part.InlineData` is currently dropped during conversion. Adding image support means mapping to Anthropic's `ImageBlockParam` (base64 PNG/JPG/etc.).
- **Stop sequences, temperature, top_p** — Opus 4.7 rejects `temperature`/`top_p`/`top_k`, so we don't pass them. Stop sequences could be plumbed through `Config.StopSequences` in a future change.
- **Structured outputs** — `output_config.format` not exposed.
- **Citations** — not exposed.

These omissions are deliberate v1 simplifications, not architectural limitations. The adapter has the seams to add them.

### Mistakes we caught (and how)

Two failures that surfaced after the initial commit's CI run, both worth remembering:

1. **`gosec G101 false positive on `const EnvAPIKey = "ANTHROPIC_API_KEY"`.** It looks credential-like; it's not. Inline `// #nosec G101` is the canonical fix; a wholesale exclusion in `.golangci.yml` would be too broad.
2. **`int64 → int32` narrowing on token counts.** Anthropic's SDK gives token counts as int64; genai's metadata uses int32. Realistic token counts fit comfortably (~2B), but the linter doesn't know that. Inline `// #nosec G115` plus a short explanatory comment.

Don't migrate the project to "always run `gosec` on the whole tree from inside `dev/tools/lint-go --fix`" — the false-positive rate is high enough that we want each suppression to be a deliberate annotation, not a bulk silence.

---

## Vertex Anthropic (M2)

Same adapter, different client construction. The conversion code in `convert.go`, `stream.go`, and `llm.go` is provider-agnostic and unchanged between the two backends — that's the whole architectural bet of M2.

### Why a separate provider name (`anthropic-vertex` vs unified)

Considered making `anthropic` auto-detect Vertex from env vars. Rejected because:

- The signal is ambiguous. `GOOGLE_CLOUD_PROJECT` could mean Vertex Gemini *or* Vertex Anthropic. We need an explicit signal.
- Auth and billing differ. A user with both Anthropic API access and a GCP project might want to use one for some calls and the other for others. Two distinct provider names lets them pick per-config.
- Telemetry clarity. `provider="anthropic"` vs `provider="anthropic-vertex"` in transcripts and OTEL spans is more honest than a single name with an internal flag.

### Why explicit ADC (vs the SDK's `WithGoogleAuth`)

The Anthropic SDK's `vertex.WithGoogleAuth(ctx, region, projectID, scopes...)` panics if `google.FindDefaultCredentials` fails. We can't have a panic at startup when ADC isn't loadable — it makes "core-agent doesn't have GCP creds" indistinguishable from "core-agent crashed".

The fix is mechanical: we call `google.FindDefaultCredentials` ourselves, surface a clean error if it fails, and only then call `vertex.WithCredentials(ctx, region, project, creds)` which doesn't panic.

### Vertex model IDs

Vertex sometimes serves Claude under date-suffixed IDs like `claude-opus-4-5@20251101`. The bare alias often works but isn't guaranteed. We pass `req.Model` (or `cfg.Model.Name`) verbatim to the SDK, which puts it directly into the Vertex URL path. Users get to provide whatever Vertex accepts.

`DefaultModel` is `claude-opus-4-7` in both backends — good for first-party, may need an override for Vertex.

### Auto-detection deliberately off

Per the rationale above, `anthropic-vertex` is **not** part of `models.autoDetectProvider()`. Users opt in explicitly via `--provider anthropic-vertex` or `model.provider: "anthropic-vertex"` in config. M3 may revisit if a clean disambiguation env-var signal emerges.

---

## Gemini built-in tools

The Gemini Provider injects a small, opinionated set of Gemini's server-side built-in tools into every request, alongside any user-defined function declarations. The shape:

- **Default on** (no setup, universally useful): `GoogleSearch`, `URLContext`.
- **Default off** (useful but a real action surface): `CodeExecution`.
- **Not surfaced — needs upstream setup**: `FileSearch` (a `fileSearchStore` corpus), `GoogleMaps` (a Maps Platform API key), `ComputerUse` (a hosted desktop environment), `Retrieval` / `GoogleSearchRetrieval` (legacy; the latter overlaps `GoogleSearch` on modern models).
- **Not surfaced — backend-specific, no consumer yet**: `EnterpriseWebSearch` (Vertex-only, otherwise zero-setup).

### Why these three and not the others

The upstream-setup group is a footgun risk: flipping one on without provisioning the resource yields an opaque API error, not a working tool. The natural reading of "I set `WithFileSearch(true)` and it didn't work" is that the library is broken, not that the user forgot to provision a corpus. We surface these only when a consumer concretely needs them — and the toggle then has to take the resource handle as input, not just a bool.

`EnterpriseWebSearch` is the exception in that list — no corpus, no extra credentials beyond what `NewVertex` already requires. It stays off the surface because no consumer has asked, not because of a setup footgun.

`GoogleSearchRetrieval` is functionally redundant with `GoogleSearch` on modern models — keeping both as exposed toggles invites confusion.

The escape hatch for consumers who do need one of the skipped tools is straightforward: write a small wrapper around `gemini.Provider` that adds the right `*genai.Tool` entry. The conversion code is a half-dozen lines.

### Why CodeExecution defaults off

It's a real action surface. Other built-ins are passive — search returns text, URL context returns text. CodeExecution runs Python on Google's servers. That's:

- **A security consideration** for some deployment contexts. Even sandboxed code execution may be off-limits under certain compliance regimes.
- **A cost consideration** that scales differently from inference (per-second compute on top of per-token).

Both are decisions the consumer should make explicitly, not inherit silently from the default. So the rule of thumb: passive built-ins default on, active ones default off. CodeExecution is the only "active" one in this set; others can be added later under the same rule.

### Why a wrapper LLM rather than modifying the request directly

`models/gemini` builds the model via `adkgemini.NewModel(...)` and could in principle inject the built-in tools by mutating `Config.Tools` somewhere upstream. We chose to wrap with a thin `builtinsLLM` instead. Two reasons:

1. **Composability.** The wrapper is a `model.LLM` — same interface as the inner. If a future change adds a second wrapping concern (logging, retry, cost tracking), they stack cleanly: each is a wrapping layer, none of them know about each other.
2. **Locality.** The injection logic is entirely in `builtins.go`, not spread across the construction path. Future maintainers grep for "Config.Tools" and find one place that touches it.

The cost is one extra layer of indirection per call. For a streaming agent loop, that's noise.

---

## Mock providers and recording

`models/mock/` ships two `models.Provider` implementations and a recording wrapper, all credential-free:

- **`echo`** returns the user's last message as the model response. Zero config; for "does the binary boot?" sanity tests.
- **`scripted`** plays back a JSONL transcript turn-by-turn. For exercising the full agent loop (tool calls, prompt construction, the disable surface) without burning API quota.
- **`mock.NewRecorder(inner, w)`** wraps any `model.LLM` and appends each turn (request + response stream) to an `io.Writer` as JSONL. Enabled in the bundled CLI via `--record-to=path` or `cfg.mock.record`; works against `gemini`, `anthropic`, `echo`, or `scripted`.

The three pieces share one `RecordedTurn` JSON shape (`format.go`). Recording produces it; scripted consumes it.

### Strict vs lenient

By default, the scripted provider replays in **lenient** mode — it ignores incoming requests and yields the next recorded responses in order. That's the right default for "I want to drive the loop without an API key." Opt into **strict** mode (`cfg.mock.strict` or `--script-strict`) and each incoming request's `Contents` must JSON-equal the recorded request, surfacing prompt-construction regressions as test failures. Strict deliberately skips `Config` — tool declarations legitimately drift as the agent's tool registry evolves, and we don't want every tool addition to invalidate every recording.

### Caveat: tool environment isn't recorded

Replay reproduces the LLM side faithfully. Tool execution at replay time uses the **live environment** — actual `bash` against the actual filesystem. If the environment has changed since recording, the agent feeds different tool outputs back to the scripted LLM, which still returns the next canned response regardless. This is great for testing loop shape and prompt construction; less great for bit-exact session reproduction. Recording tool outputs alongside LLM turns would close the gap, but adds a much larger surface (which tools? all of them? what about side effects?) and is deferred until a concrete test scenario asks for it.

---

## Built-in tools

`tools/` ships six general-purpose tools — `read_file`, `write_file`, `edit_file`, `list_dir`, `bash`, `todo` — lifted from cogo's `internal/tools/`. The bundled CLI enables them all by default; library callers opt in via `tools.Build(cfg, gate, tools.Default())` (or pass a custom `BuiltinTools` instead of `Default()` for fine-grained control). `--no-builtin-tools` on the CLI disables the lot.

### Why we ship them (reversing M1's "narrow base" decision)

When we shipped M1, the rationale was "consumers add their own tools." That argument was weaker than it sounded. Three things forced a rethink:

1. **Inconsistency.** core-agent already ships a lot of opinionated machinery — the permission gate, MCP integration, skills loading, the Anthropic adapter, AGENTS.md loading. Drawing the line *just* before tools was inconsistent — it forced consumers to either copy a thousand lines of non-trivial code from cogo (with output capping, atomic writes, gate integration, the bash denylist) or write fresh.
2. **Universality.** Every coding agent, task-execution agent, or workspace-aware agent needs at minimum `read_file` + `write_file` + `bash`. The friction of "core-agent talks but can't act" is real and ships per-consumer.
3. **Cleaner downstream stories.** The Scion adapter (the next thing built on core-agent) would otherwise re-lift the same tools into `extras/scion-agent/internal/tools/`. Now it's a thin Scion-shaped wrapper around `tools.Build(cfg, gate, tools.Default())`.

### What's in scope vs. not

In scope (lifted from cogo):
- File ops: `read_file` (with offset/limit), `write_file` (atomic), `edit_file` (single-occurrence string replace), `list_dir` (sorted).
- Shell: `bash` (`/bin/sh -c` with timeout, `bash` denylist enforced via `permissions.Gate.CheckBash`).
- Plan tracking: `todo` (in-process store; `TodoStore.Items()` exposes a defensive copy for hosts that want to render plan progress).

Out of scope:
- **Web tools** (`web_fetch`, `web_search`) — Gemini's built-in `URLContext` and `GoogleSearch` cover this for Gemini-backed agents. For Anthropic-backed agents, Anthropic's `web_search` server-side tool is surfaced via `models/anthropic.WithWebSearch(true)` (off by default — per-search billing, treated as an active surface). `web_fetch` and other Anthropic server-side tools (code_execution, text_editor, memory, bash) aren't surfaced today; add them under the same `BuiltinTools` struct when a consumer needs one.
- **Glob / grep** — cogo doesn't have them; not needed for the immediate downstream consumers. Adding them is straightforward when one shows up.
- **Subagent tool** — deferred to M3.

### Why default-on at the CLI but explicit at the library

The bundled `cmd/core-agent` is the "out of the box" experience and needs to be useful. It calls `tools.Build(cfg, gate, tools.Default())` unconditionally (unless `--no-builtin-tools`). Granular per-tool disable is exposed two ways: `--disable-tools=bash,write_file` on the CLI, and `tools.disable: ["bash"]` in `.agents/config.json`. The two compose by union — anything disabled in either path is off — and both validate names against `tools.BuiltinToolNames()` so typos fail at startup rather than silently leaving a tool on.

The library (`agent.New`) does *not* auto-include tools. Two reasons:
- The agent layer doesn't have the gate or config dependency. Adding it would force every `agent.New` call to know about both, which would couple the agent to the permissions package even for consumers who don't want tools.
- Library callers know whether they want tools or not. One extra line — `agent.New(m, agent.WithTools(reg.Tools))` — is fine. The convenience matters more for the CLI, where users don't write code.

This differs from the Gemini built-ins pattern (which is default-on at the Provider layer) because the Provider already has its credentials; the agent doesn't have the gate.

### Why ungated tools aren't a thing

`tools.Build` rejects a nil gate. We don't ship "no-permission" tools because:
- The bash denylist needs the gate for the non-overridable `rm -rf /` refusal.
- File tools need the gate for the path-scope check (which is what makes "agent only operates inside the project root" actually true).

Skipping the gate would mean the security model falls apart silently. If a consumer genuinely wants ungated tools, they can construct them by calling `bashFunc`, `readFileFunc`, etc. with a permissive `permissions.New(permissions.Options{Mode: permissions.ModeYolo})` — but there's no shorter path.

### Why `Registry.Todo` is exposed

Cogo's pattern: the registry returns a `*TodoStore` alongside the tool list. This lets a host render plan progress (e.g. a `/todo` slash command) without round-tripping through the model. Cheap to keep; useful when a TUI is built on core-agent later.

---

## Adapters (`extras/`)

The library is the foundation; the bundled CLI is a reference. *Adapters* are the third layer: opt-in binaries that embed core-agent and translate it to a specific runtime's lifecycle contract. They live under `extras/` so the core packages stay free of runtime-specific concerns.

### Why a separate directory rather than a separate repo

A separate repo would be more honest about the dependency direction (the adapter depends on core-agent, not the other way around). We picked `extras/` for v1 because:

- **Tight feedback loop.** Adapters exercise core-agent's public API the way real consumers will. Keeping them in-tree means a breaking API change shows up immediately in the adapter's CI rather than weeks later when someone bumps a tag.
- **One-PR workflow.** A change that requires a small core-agent API tweak plus the adapter that needs it can land in one PR. With separate repos, you're juggling tag bumps and replace directives.
- **Discoverability.** A user reading the README sees "core-agent has an adapter for Scion" without going hunting.

The cost is the appearance of bloat — `go install ./...` builds the adapter too. We accept that. Adapters that grow large enough to deserve their own repo can be moved out without API change (the public surface stays in `agent/`, `tools/`, etc.).

### What goes in an adapter

An adapter owns:

- The **binary** (`main.go`) that wires core-agent's library together with runtime-specific glue.
- The **lifecycle plumbing** for that runtime — Scion's `agent-info.json` writer, k8s's pod-status emitter, A2A's message envelope, etc.
- The **packaging** (Dockerfile, manifest templates, k8s YAML) needed to deploy the binary.
- Its own **README** explaining build + deploy.

An adapter does *not* fold runtime-specific knowledge back into the core packages. If an adapter wants a hook that core-agent doesn't expose (e.g. ADK callbacks for control-flow interception), the adapter either inspects the existing `agent.Run()` event stream or that hook gets promoted to core-agent's public API only when a second adapter needs it. We avoid speculative API surface.

### First adapter: scion-agent

[`extras/scion-agent/`](../extras/scion-agent/) runs core-agent inside [Scion](https://github.com/GoogleCloudPlatform/scion)'s container runtime. Mirrors the Python `adk_scion_agent` example but built on core-agent.

Lifecycle hook strategy: the adapter's `streamTurn` ranges over `agent.Run()`'s event stream and emits transient activity (`thinking` / `executing` / `working`) on agent and tool boundaries. Sticky transitions (`ask_user`, `task_completed`, etc.) flow through a `sciontool_status` ADK tool the model invokes intentionally. No changes to core-agent's public API for this — if a future adapter needs *control-flow* callbacks (abort tool calls, substitute responses), we'll add `WithBeforeToolCallbacks` etc. on `agent.New` then.

---

## Permission gate

### Three modes (`ask` / `allow` / `yolo`)

Lifted from cogo unchanged. The mode set is deliberate:

- `ask` — interactive, prompts via `Prompter`. The default — you should not be running an agent with arbitrary tool access without explicit consent on each call.
- `allow` — non-interactive, allowlist-only. Anything not on the allowlist is rejected without prompting. The right mode for CI / batch / scripted runs.
- `yolo` — proceed unless the bash denylist or a deny-pattern fires. Local dev convenience.

We don't have a fourth mode like "deny everything". That would be `allow` mode with an empty allowlist, which already works.

### Why bash denylist is non-overridable

Even `yolo` mode can't run `rm -rf /` or `dd if=... of=/dev/sda`. The denylist is hard-coded in `permissions/denylist.go`. The reasoning:

- These are universally destructive actions with no legitimate non-test use case in an agent loop.
- Putting them behind a config knob means someone, somewhere, will turn the knob off and lose data. Better to make the refusal absolute and force the user to bypass it deliberately (i.e. by editing core-agent itself).
- The list is small and surgical. It refuses the exact patterns most likely to happen by accident.

The denylist intentionally *doesn't* try to be a complete bash sandbox. It's a safety net for the worst patterns, not a security boundary. Real isolation requires a sandbox (container, VM, etc.).

### Path scope

File tools may only touch the project root + user home + explicit allowlist. Out-of-scope access either prompts (in `ask` with a Prompter) or fails.

The reason path scope sits *next to* the policy gate rather than inside it: scope is a per-tool-class concept (file tools care, bash doesn't). Combining them would force every tool to know about path scope. Keeping them separate lets `CheckGeneric()` (used by MCP and skills) skip the scope check and lets `CheckFileRead` / `CheckFileWrite` apply it.

### Decision granularity

`Decision` has five values, not two. The expensive design decision was `DecisionAllowSessionTool` — "trust this tool entirely for the rest of the session". Without it, agents that read many files (one of the most common patterns) would re-prompt on every read. With it, the user can grant trust once and never see another modal for that tool.

The cost: `AllowSessionTool` short-circuits the path scope check too. That's deliberate — once you trust a tool, you trust it for any path. But it's a real escalation; document it in any UI that surfaces the choice.

### No Prompter in cmd/core-agent

The bundled CLI ships with `Prompter: nil`. That means `ask` mode in the REPL fails closed if any tool needs gating. Consumers building real interactive frontends supply their own Prompter (terminal, modal, Slack bot, whatever).

This is a deliberate v1 simplification — building a good terminal Prompter is non-trivial (cursor handling, multi-line, etc.) and would commit core-agent to a particular UI shape. Better to leave it to the consumer and use `mode: yolo` or pre-baked allowlists for the REPL out of the box.

---

## MCP integration

### Tool namespacing

Every MCP tool from server `<name>` becomes `<sanitized_name>_<tool>`. Why:

- **Collision avoidance**: an MCP filesystem server's `read_file` doesn't shadow a consumer-provided `read_file`.
- **Gemini compliance**: function names must match `[A-Za-z0-9_]{1,64}`. Server names with hyphens or dots get sanitized.

We use `_` not `.` because `.` would fail Gemini's regex.

### Env-var interpolation

`mcp.json` supports `${env:NAME}` placeholders in `env` values (stdio) and `headers` values (http). Resolved at server-start time from the parent process's env.

Why not full Go template syntax? Two reasons:

- The use case is narrow — secrets in headers and env values. Full templating invites abuse.
- It's the same syntax users see in `dotenv` files and most CI systems. Familiar.

Unset variables expand to empty strings. That's deliberate — matches shell behavior, fails closed for tokens.

### Per-server failure isolation

A stdio server whose binary doesn't exist, or an HTTP server that 404s, surfaces as `Status: error` on its `Server` record. The agent continues with whichever servers came up cleanly. We don't fail-fast on bad MCP config because we want one broken server not to lock the user out.

The host (the consumer's binary or `cmd/core-agent`) is responsible for surfacing per-server status to the user. The bundled CLI prints a one-line stderr warning per failure; richer hosts can render a `/mcp` view.

### No reload

Currently `mcp.json` is read once at startup. Picking up an edit requires a process restart. A `/reload` slash command in a host could call `Server.Close()` on every old server, then re-run `mcp.Build()` — the API supports it, we just don't expose a workflow.

### Stdio child shutdown

Stdio MCP children get SIGTERM, then SIGKILL after 3 seconds. That's the standard "polite then forceful" pattern. The 3-second window is somewhat arbitrary but matches what most tools do. Long enough for a graceful exit, short enough that a process leak doesn't hold the parent process hostage.

---

## Skills loading

The format we adopted is Anthropic's published `SKILL.md` format — YAML frontmatter (`name`, `description`) plus a markdown body. The body is the prompt content the agent reads when it invokes the skill.

Why this format and not invent our own:

- **It exists**, with a published spec.
- **It's portable**: a user with existing SKILL.md bundles drops them in unchanged.
- **The frontmatter is minimal** — just enough metadata to surface name + description to the agent.

ADK ships a `skilltoolset` package that does the loading. We pass `os.DirFS(skillsDir)` as the source and the rest is upstream code.

Lazy loading: bodies aren't read until a skill is invoked. Cold-start stays fast even with large skill libraries. Trade-off: a malformed body in a skill that's never invoked won't surface until it is.

Permission gating: skill invocations go through the gate under the `skill` namespace. Allowlist patterns look like `skill:my-skill`. Same shape as MCP gating.

---

## CLI shape (`cmd/core-agent`)

The bundled CLI is a *reference* implementation. Two modes:

- **`-p PROMPT`**: one-shot. Stream partial text to stdout, tool-call summaries to stderr, exit. The shape every agent CLI of this era has.
- **No `-p`**: REPL. Read a line, send through `agent.Run()`, stream back. Same session ID across the loop so ADK preserves history. Built-in commands: `/exit`, `/quit`, EOF.

Why no Bubble Tea / TUI: belongs in the consumer. The base library should be runnable headless; a TUI is a presentation choice.

Why no slash-command framework: same reasoning. `/exit` is the minimum viable. Anything richer (e.g. `/model`, `/permissions`) requires interactive UI shapes that are consumer-specific.

---

## Telemetry, usage, sessions

Three small packages, each lifted from cogo unchanged.

- **`telemetry/`** — OTEL setup. Off by default (no spans). Console mode for local debug, OTLP for production. The one gotcha worth knowing: ADK's `telemetry.New(...)` returns providers but does NOT install them as OTEL globals. We call `providers.SetGlobalOtelProviders()` explicitly. Without that, ADK's instrumentation runs against the noop tracer and you wonder why nothing's being emitted.
- **`usage/`** — per-turn token + cost tracker. Pricing comes from a built-in table for the Gemini family; Anthropic models return zero pricing today (deferred — consumers override per-model via `cfg.Model.Pricing`).
- **`session/`** — transcript persistence. One-shot writes a JSON file under `.agents/sessions/`; REPL doesn't (because in-memory ADK sessions don't survive process exit anyway).

---

## What's deliberately out of scope (and why)

Each of these was considered and rejected for v1. The rationale matters because future M3+ work will keep relitigating these.

**Subagents** — a `WithSubagents([]*Agent)` option that registers each subagent as a synthetic tool. Marker is in `agent/agent.go`. Reason for deferral: cogo doesn't have it yet, and the right shape depends on whether you want subagent-as-tool (Anthropic-style) or ADK's native sub-agent transfer. Pick the shape based on the first concrete consumer that needs it.

**Bubble Tea TUI** — would commit core-agent to a particular UI library and pull in ~30 transitive deps. Belongs in the consumer.

**File-backed session service** — `session.InMemoryService()` works for v1. For long-lived agents the right answer is a `session.Service` implementation that persists to BoltDB or SQLite. Drop it in via the (currently hard-coded) session service field on `runner.Config` — that field gets a `WithSessionService` option when this lands.

**Slash-command framework** — anything richer than `/exit` requires interactive UI semantics that vary by host. Best to let consumers add their own.

**Anthropic feature coverage** — extended thinking, structured outputs, server-side tools, vision, prompt caching beyond the simple `WithCacheSystem` toggle. All require small, well-bounded additions to the Anthropic adapter. None block v1 use.

**Auto-detection for `anthropic-vertex`** — env-var overlap with Vertex Gemini makes this risky. Possible solution: fire only when `ANTHROPIC_VERTEX_PROJECT_ID` is set without `GOOGLE_API_KEY`. Worth doing once the env semantics are designed deliberately, not as a follow-on.

**Built-in tools** — bash, read_file, write_file, edit_file, grep, list_dir. Cogo has these in `internal/tools/`. We deliberately don't ship them. Reasoning: they're consumer policy as much as code (output capping per tool, security model, etc.) and we want each consumer to make those choices explicitly. The seam (`agent.WithTools`) is sufficient.

**CLI for everything cogo's CLI does** — `init`, `migrate`, `models`, `mcp`, `skills`, etc. Cogo's `cmd/cogo` has dozens of subcommands. Our `cmd/core-agent` has one (REPL with optional `-p`). Same reasoning: a base library shouldn't dictate CLI ergonomics.

---

## Open architectural questions

These aren't blockers for any specific milestone but they're worth thinking about before they ossify:

### Should the `Provider` interface know about pricing?

Today `usage/pricing.go` has a hard-coded table keyed by model ID. Provider-specific pricing logic (e.g. Vertex's region-dependent rates, Anthropic's prompt caching discount) doesn't fit cleanly. Two paths:

- Extend `Provider` with `Pricing(modelID) Pricing` — providers express their own rates.
- Keep pricing centralized and let consumers override per-model.

We picked option 2 for v1. If the hard-coded table grows past ~10 models or if Vertex region-pricing matters, revisit.

### How should `Prompter` be wired?

Today it's an interface with one method (`AskApproval`). For richer interactions (e.g. "elicit a value with a schema", "show a multi-choice picker"), the interface would need to grow. Should it be one fat interface, several small ones, or replaced with a callback per kind of prompt?

The MCP elicitation handler is *already* a separate seam (`ElicitorFn`). We may end up with three or four orthogonal "prompter" hooks. Worth thinking about consolidating before they multiply.

### Should the agent loop itself be replaceable?

`agent.Agent.Run()` returns the ADK runner's iterator directly. That's fine when ADK is the runner, but if a consumer wants to wrap it (e.g. for retry, for compaction, for telemetry middleware), they have to wrap the iterator manually.

A `Middleware` pattern on `agent.New` would let consumers inject hooks ("before tool call", "after partial event"). Adds complexity; not yet worth it.

### When does the schema bump (`config.SchemaVersion = 1`)?

Any breaking change to `.agents/config.json` requires bumping `SchemaVersion`, which then forces every consumer's config to be migrated. We should be conservative — accumulate breaking changes into a single bump rather than dribbling them out.

The current schema feels well-shaped enough that we shouldn't need to bump for a while. If we add more provider-specific blocks (e.g. for Bedrock), they fit under `Model.Anthropic.Bedrock` or `Model.Bedrock` without breaking anything.

---

## How to update this document

When making a substantive architectural change:

1. Write the change in code with the rationale captured in commit message + (where appropriate) inline package comments.
2. Update this document's relevant section so future-you doesn't have to reconstruct the rationale from git history.
3. If a new section is warranted, add it in the order below and don't reshuffle existing sections — internal links to specific sections may exist elsewhere.

Honest design docs are unmaintained design docs that get burned and rewritten. Keep this one short and honest.
