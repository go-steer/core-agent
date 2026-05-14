# core-agent: design notes

This document captures the architectural choices behind core-agent — the *why*, not the *what*. For "what is this and how do I use it" read the [README](../README.md) and the [docs site](https://go-steer.github.io/core-agent/). Read this when you're staring at a design decision that isn't obvious and want to know what trade-off motivated it.

It's intentionally opinionated and intentionally honest about what's unresolved.

---

## Goals and non-goals

### Goals

- **Be the substrate for Go agents, not an agent itself.** Provide the wiring — model providers, MCP, skills, instructions, permissions, telemetry — so consuming projects only have to write their domain-specific tools and product logic.
- **Match cogo's conventions.** A future maintainer who knows cogo should recognize the package layout, the dev tooling, the AGENTS.md / `.agents/` convention, and the milestone discipline. The two projects are intended to coexist; copy-pasting fixes between them should be a one-line port.
- **Stay narrow.** Resist adding features that belong in the consumer (tools, UI, slash commands beyond `/exit`). Each addition is a maintenance liability for everyone who imports the library.
- **Make extension obvious.** The `Provider` interface, the `Option` pattern on `agent.New`, the `tools.GateToolset` wrapper — these are the shapes you want a consumer to be able to extend without reading half the codebase first.
- **Provide first-class Claude support.** ADK Go ships only Gemini and Apigee; we add Anthropic + Vertex Anthropic as the substantive new code.

### Non-goals

- **Not a finished agent.** No bash / file / grep tools shipped. No TUI. No CLA / no rich slash-command framework. Those belong in the consumer.
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
