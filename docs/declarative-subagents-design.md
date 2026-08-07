# Declarative subagents

**Status:** proposed (2026-08-07). Target: **v2.9**. Tracking issue: [#599](https://github.com/go-steer/core-agent/issues/599). Supersedes the singular `subagent` config sketch in `docs/subagents-plan.md` (2026-05-15, itself superseded); complements the runtime-dynamic path in `docs/background-subagents-design.md`.

## Motivation

core-agent has two ways to give a parent agent a subagent, and neither is
config-driven:

1. **Code-static** — `agent.WithSubagents([]*Agent{...})` (`pkg/agent/agent.go:534`).
   You construct each `*agent.Agent` in Go (its own model, instruction, tools)
   and hand the slice to the option. This is what `examples/with-subagent/`
   does: each `*Agent` carries its own `adkmodel.LLM`, so a subagent runs on a
   **separate model** from its parent by construction (the example wires two
   scripted-mock providers; distinct *real* providers are supported the same way
   but the example does not exercise them).
2. **Runtime-dynamic** — the `spawn_agent` tool (`docs/background-subagents-design.md`),
   where the parent model *decides at runtime* to spawn a background/remote
   subagent.

There is no way to declare a fixed roster of subagents in `.agents/config.json`.
The concrete consumer is the kube-platform-agent recipe ([#594](https://github.com/go-steer/core-agent/issues/594),
shipped as [#598](https://github.com/go-steer/core-agent/pull/598)): Hermes ran
`agents/{platform,cluster,chat}` as separate profiles, and the natural core-agent
mapping is *the Platform Agent delegates to a read-only `cluster` subagent on its
own model and a GKE-read-only MCP scope* — a config-only relationship, in-process,
no Go build. That mapping is impossible today.

This is a general large→small delegation capability (a cheap parent triaging to a
specialist, or an expensive parent fanning read-only work to a scoped worker), not
a recipe-only feature.

## Goals

- Declare a fixed set of named subagents in `.agents/config.json` — no Go code.
- Each subagent gets its own **instructions** (supporting `@include`), an optional
  **model** (empty = inherit the parent's), and an optional **tool allowlist**.
- **Optional per-subagent MCP + skills scope** — a subagent can see a *different*
  MCP/skill surface than its parent (e.g. the `cluster` subagent gets
  GKE-read-only; the parent gets read-write). This is the design's main new
  primitive.
- Reuse the shipped subagent substrate (`NewSubagentTool`, branch isolation,
  depth cap) unchanged — this is a *wiring* feature, not a new runtime.

## Non-goals (v2.9)

- Replacing or changing `spawn_agent` (runtime-dynamic subagents stay as-is).
- Remote/peer subagents — that is W6 ([#595](https://github.com/go-steer/core-agent/issues/595))
  `call_peer`; this doc is in-process only.
- Recursive declarative nesting beyond the existing `MaxDepth` cap.
- Auto-translating an external MCP `config.yaml` into per-subagent scopes — the
  operator writes the subagent's `mcp.json` (or inline refs; see open questions).

## Conceptual model — three ways to get a subagent

| Mechanism | Defined in | Chosen by | Lifetime |
|---|---|---|---|
| `agent.WithSubagents` | Go code | the embedder | process |
| `spawn_agent` (background/remote) | runtime | the parent **model** | per-spawn |
| **`subagents[]` (this doc)** | `.agents/config.json` | the **operator** | process |

All three land on the same substrate: each subagent becomes a `tool.Tool`
(`NewSubagentTool`, `pkg/agent/subagent.go:120`) the parent invokes *by name*,
runs on its own baked-in LLM through a per-invocation runner against a
branch-injecting view of the parent session, with a depth cap. Declarative
subagents add **only** the config→`*Agent` construction glue in
`cmd/core-agent/main.go`; nothing in `pkg/agent` changes.

## Detailed design

### Config surface (`pkg/config/config.go`)

A new top-level slice on `Config` (`config.go:39-57`), slotting after `Tools`
(line 46), mirroring the existing sub-struct + snake_case-tag style:

```go
// Subagents declares in-process subagents the parent may call by name.
Subagents []SubagentSpec `json:"subagents,omitempty"`
```

```go
type SubagentSpec struct {
	Name         string       `json:"name"`                   // required, unique, tool-name-safe
	Description  string       `json:"description,omitempty"`  // shown to the parent model
	Instructions string       `json:"instructions,omitempty"` // inline or "@include <path>"
	Model        *ModelConfig `json:"model,omitempty"`        // nil = inherit parent
	MaxDepth     int          `json:"max_depth,omitempty"`    // 0 = NewSubagentTool default (2)
	Tools        []string     `json:"tools,omitempty"`        // optional allowlist; empty = inherit
	Scope        string       `json:"scope,omitempty"`        // dir for this subagent's mcp.json + skills/
}
```

`Model` reuses `ModelConfig` (`config.go:235-250`) verbatim, so a subagent
supports every provider the parent does. Distinct per-subagent models work by
construction — each subagent is a separate `agent.New` with its own
`adkmodel.LLM` (the two-provider pattern in `examples/with-subagent/`). `Tools`
follows the `ToolsConfig.Disable` precedent (`config.go:508`) but as an
allowlist.

### Validation (`Config.Validate`, `config.go:894`)

Append structural checks in the existing style (per-index `subagents[%d]`
messages, enum-switch for the provider like `config.go:901-906`):

- `Name` non-empty, unique across the slice, and matches the tool-name charset
  (the parent calls it as a function name).
- `MaxDepth >= 0`.
- If `Model` is set, re-validate its `Provider` against the provider constants
  (`config.go:829-836`) and require `Name`.
- `Tools` entries are known tool names.
- `Scope`, if set, is a relative path (resolved against `agentsDir` at load time,
  not here — `Validate` stays environment-free per `config.go:890-893`).

### Runtime wiring (`cmd/core-agent/main.go`)

The one new piece of glue. Today the parent's options are assembled at
`main.go:1124-1142` and passed to `buildAttachedAgent` →
`agent.New(m, agentOpts...)` (`cmd/core-agent/attached_agent.go:44`). We add a
`buildDeclaredSubagents(cfg, agentsDir, ...)` step before that assembly:

For each `SubagentSpec`:
1. Resolve its model — `cfg.Subagents[i].Model` if set, else reuse the parent
   `provider`/`cfg.Model` — into an `adkmodel.LLM` (the same provider-construction
   path the parent uses).
2. Resolve its instruction — inline `Instructions`, or an `@include` chain loaded
   through `pkg/instruction` (scope-confined exactly like the parent's).
3. Resolve its tool surface (see next section).
4. `agent.New(subLLM, agent.WithName(spec.Name), agent.WithDescription(...),
   agent.WithInstruction(...), agent.WithTools(...), agent.WithToolsets(...))`.
5. Collect the `*Agent` values and append `agent.WithSubagents(subs)` to the
   parent `opts` slice at `main.go:1124-1142`.

`agent.New` already resolves each subagent `*Agent` into a `NewSubagentTool` at
the end of parent construction (`agent.go:713-728`), capturing the parent's
session triple — so no change in `pkg/agent`.

### Per-subagent MCP + skills scope (the new primitive)

`mcp.Build` (`pkg/mcp/lifecycle.go:146`) and `skills.LoadAll`
(`pkg/skills/load.go:140`) are each keyed to a set of `agentsDir` scopes and
today feed **one** merged surface into the single parent agent. To give a
subagent a *different* surface without touching those signatures, the **proposed**
mechanism is a per-subagent **scope dir**:

- `spec.Scope` (default `.agents/subagents/<name>/`) may contain its own
  `mcp.json` and `skills/`.
- `buildDeclaredSubagents` calls `mcp.Build(ctx, <scopeDir>, "", ...)` and
  `skills.LoadAll(ctx, <scopeDir>, "", gate, ...)` against **that dir only**,
  passing the results as that subagent's own `WithTools`/`WithToolsets` at its
  `agent.New` — never the parent's, never another subagent's.
- A subagent with no `Scope` and no `Tools` gets **no tools of its own** unless
  the helper explicitly hands it some. `WithSubagents`/`NewSubagentTool` do **not**
  copy the parent's tools down (a subagent runs with exactly what its own
  `agent.New` received — see `examples/with-subagent/`, whose subagent has none).
  So "inherit the parent's surface" is an explicit threading step:
  `buildDeclaredSubagents` passes the parent's resolved `builtinTools`/toolsets
  (`main.go:1125-1126`) into the subagent's `agent.New` when no narrower scope or
  allowlist is declared. This is a deliberate default choice (open question 2),
  not automatic behavior.

This reuses the existing per-`agentsDir` keying (`mcp.LoadAll`'s multi-scope
merge, `config.go:232`; skills' `openSkillsDir` composition, `load.go:149-157`)
rather than inventing per-agent registries in `pkg/mcp`/`pkg/skills`. It does add
real cmd-side lifecycle wiring: each `mcp.Build` call returns its own
`[]*Server` that must be `CloseAll`'d and metrics-registered (mirroring
`main.go:883-889`), and `send`/`gate`/`makeMCPElicitor()`/`digestOpts` must be
threaded per subagent. The alternative — inline named MCP/skill references on the
spec — is lighter for simple cases but needs a global registry to resolve names;
see open questions.

## Per-substrate impact

- **`pkg/config`** — new `SubagentSpec` type + `Config.Subagents` field +
  `Validate` cases. Round-trip and validation tests.
- **`pkg/agent`** — **none**. `WithSubagents` / `NewSubagentTool` /
  `SubagentOptions` (`subagent.go:35-85`) are sufficient as-is.
- **`pkg/mcp`, `pkg/skills`** — **none** to signatures; we call the existing
  `Build`/`LoadAll` against a per-subagent scope dir.
- **`cmd/core-agent`** — the real weight lives here: a new
  `buildDeclaredSubagents` helper that constructs each subagent's LLM,
  instruction, and (scoped) tool surface, manages a per-subagent `mcp.Build`
  lifecycle (`CloseAll` + metrics, per `main.go:883-889`), and appends one
  `agent.WithSubagents(...)` at the options-assembly site (`main.go:1124-1142`).

## Config surface — full example

```jsonc
{
  "version": 1,
  "model": { "provider": "vertex", "name": "gemini-3.5-flash" },
  "subagents": [
    {
      "name": "cluster",
      "description": "Read-only investigation of a single GKE cluster.",
      "instructions": "@include upstream/cluster/SOUL.md",
      "model": { "provider": "vertex", "name": "gemini-3.5-flash" },
      "max_depth": 2,
      "scope": ".agents/subagents/cluster"   // its own mcp.json (gke read-only) + skills/
    }
  ]
}
```

## Implementation phases

- **Phase 1 (PR γ.1 of #599)** — `SubagentSpec` + `Config.Subagents` +
  `Validate`; config round-trip tests. No wiring yet.
- **Phase 2 (PR γ.2)** — `buildDeclaredSubagents` wiring for name / description /
  instructions / model + `WithSubagents`; a scripted/echo-provider test asserting
  the named tool is registered, invoked by name, and runs on its own model.
- **Phase 3 (PR γ.3)** — per-subagent MCP + skills scope; a test asserting the
  subagent sees only its own MCP/skills and the parent does not see the
  subagent's.
- **PR B′** — the kube-platform-agent recipe adopts a `cluster` subagent
  (persona `@include`d from a vendored `upstream/cluster/SOUL.md`, own model,
  GKE-read-only MCP scope). Config-only, in-process.

Each PR carries `-race` tests and an adversarial-review section.

## Open questions

### 1. Per-subagent tool surface: scope dir vs inline named refs

Scope dir (proposed) reuses the existing `agentsDir` keying and keeps a
subagent's config self-contained on disk, but adds a directory convention.
Inline refs (`"mcp": ["gke"]`, `"skills": ["fleet-audit"]`) read cleaner in the
config but require a name→server/skill registry the loaders don't expose today.
Settle before Phase 3.

### 2. Default tool surface + allowlist semantics

Two coupled decisions. **(a) Default when no `Scope`/`Tools` is declared:** since
`WithSubagents` does not copy the parent's tools down (see "Per-subagent MCP +
skills scope"), the helper must choose — inherit the parent's full surface
(convenient, but a config-declared subagent silently gets everything) or start
empty (safe, but every subagent must declare tools). Proposed default: inherit
the parent's surface, since a declarative subagent is a trusted, operator-authored
delegate. **(b) Allowlist semantics:** is `spec.Tools` applied *after* the
parent's permission gate, or a separate gate? Proposed: the allowlist filters the
subagent's registry; mutation still flows through the same `permissions.Gate` (and
`require_plan_artifact`) as the parent, so a scoped subagent cannot escalate.

### 3. Model inheritance ergonomics

`Model: nil` inherits the parent. Should we also allow a bare
`"model": "gemini-3.5-flash-lite"` string shorthand (name-only, provider
inherited)? Deferred unless a consumer needs it.

## Security considerations

A declarative subagent is strictly *less* privileged than the parent by
construction: it runs through the same `permissions.Gate`, the same
`require_plan_artifact` gate, the same depth cap (`NewSubagentTool` default 2),
and — with a scope dir or allowlist — a *narrower* tool surface. Instructions are
loaded through `pkg/instruction` with the same scope confinement as the parent,
so `@include` cannot escape the project root. There is no new escalation path.

## Out of scope (deferred)

- Remote/peer subagents (W6 `call_peer`, [#595](https://github.com/go-steer/core-agent/issues/595)).
- Auto-translating external MCP `config.yaml` into subagent scopes.
- Dynamic reconfiguration of the subagent roster at runtime (that is
  `spawn_agent`'s domain).

## Dependencies and related work

- Consumer: kube-platform-agent recipe ([#594](https://github.com/go-steer/core-agent/issues/594)
  / [#598](https://github.com/go-steer/core-agent/pull/598)); adoption is PR B′.
- Sibling enabler: external content-root ([#600](https://github.com/go-steer/core-agent/issues/600),
  `docs/external-content-root-design.md`) — a subagent whose persona lives in an
  external checkout composes with that capability.
- Substrate: `docs/background-subagents-design.md` (runtime `spawn_agent`),
  `docs/subagents-plan.md` (superseded singular sketch).
- Epic: [#589](https://github.com/go-steer/core-agent/issues/589),
  `docs/hermes-replacement-design.md` (W5).
