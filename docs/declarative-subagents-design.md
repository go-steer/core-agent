# Declarative subagents

**Status:** accepted; implementing (2026-08-07). Target: **v2.9**. Tracking issue: [#599](https://github.com/go-steer/core-agent/issues/599). Supersedes the singular `subagent` config sketch in `docs/subagents-plan.md` (2026-05-15, itself superseded); complements the runtime-dynamic path in `docs/background-subagents-design.md`.

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
- Auto-translating an external MCP `config.yaml` into subagent scopes — the
  operator writes the shared `mcp.json` and the subagent names servers from it.

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
subagents are almost entirely config→`*Agent` construction glue in
`cmd/core-agent/main.go`. The one `pkg/agent` addition is a small
`WithSubagentMaxDepth` option so a per-subagent `MaxDepth` from config reaches
`SubagentOptions.MaxDepth` instead of being validated and then silently
dropped (`WithSubagents` otherwise always applies the substrate default). No
substrate behavior changes.

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
	Tools        []string     `json:"tools,omitempty"`        // built-in allowlist; unset = inherit
	MCP          []string     `json:"mcp,omitempty"`          // server names from the shared mcp.json
	Skills       []string     `json:"skills,omitempty"`       // skill names from the shared skills/
}
```

The tool surface is **inline-referenced against the shared config** (open
question 1, resolved → inline refs; see below), not a per-subagent directory:
`MCP` names servers from the one `.agents/mcp.json`, `Skills` names skills from
the one `.agents/skills/`, and `Tools` is a built-in allowlist. This keeps a whole
recipe in a single `config.json` + `mcp.json` + `skills/` tree — one Kubernetes
ConfigMap, no nested `subagents/<name>/` tree to mount.

`Model` reuses `ModelConfig` (`config.go:235-250`) verbatim, so a subagent
supports every provider the parent does. Distinct per-subagent models work by
construction — each subagent is a separate `agent.New` with its own
`adkmodel.LLM` (the two-provider pattern in `examples/with-subagent/`). `Tools`
follows the `ToolsConfig.Disable` precedent (`config.go:508`) but as an
allowlist.

### Validation (`Config.Validate`, `config.go:894`)

Structural checks in the existing style (per-index `subagents[%d]` messages,
enum-switch for the provider like `config.go:901-906`), in a `validateSubagents`
helper:

- `Name` non-empty, unique across the slice, and `[A-Za-z0-9_-]{1,64}`
  (the parent calls it as a function name).
- `MaxDepth >= 0`.
- If `Model` is set, re-validate its `Provider` against the provider constants
  (`config.go:829-836`) and require `Name`.
- `Tools` / `MCP` / `Skills` entries are non-empty strings. Whether a referenced
  server or skill **exists** is a wiring-time check (it needs the loaded
  `mcp.json` / skills dir), not here — `Validate` stays environment-free per
  `config.go:890-893`.

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

### Per-subagent MCP + skills scope (inline refs)

`mcp.Build` (`pkg/mcp/lifecycle.go:146`) and `skills.LoadAll`
(`pkg/skills/load.go:140`) are each keyed to a set of `agentsDir` scopes and
today feed **one** merged surface into the single parent agent. The resolved
mechanism (open question 1) is **inline refs against that shared surface** — the
parent loads MCP/skills once, and each subagent selects a subset **by name**:

- `mcp.Build` already returns per-server toolsets (`[]tool.Toolset`,
  `lifecycle.go:146`), so filtering to `spec.MCP` is a name match over the
  parent's already-started servers — **no second `mcp.Build`, no per-subagent
  server lifecycle** (`CloseAll`/metrics stay single, at `main.go:883-889`).
  `.agents/mcp.json`'s server map *is* the registry the names resolve against;
  a read-only variant is just a second named server (`gke` + `gke-readonly`).
- Skills load once into a merged toolset (`skills.LoadAll`, `load.go:140`).
  Selecting `spec.Skills` needs a small **name filter** on that toolset — the one
  genuinely new bit of `pkg/skills` surface (a filtered view, not a second load).
- `Tools` filters the parent's built-in registry by name.
- `buildDeclaredSubagents` passes the filtered tools/toolsets as that subagent's
  own `WithTools`/`WithToolsets` at its `agent.New` — never leaking one subagent's
  scope to another.

**Default when nothing is declared (open question 2, resolved → inherit).** A
subagent with no `Tools`/`MCP`/`Skills` inherits the parent's full surface. Note
this is an **explicit threading step**, not `WithSubagents` behavior:
`WithSubagents`/`NewSubagentTool` do **not** copy the parent's tools down (a
subagent runs with exactly what its own `agent.New` received — see
`examples/with-subagent/`, whose subagent has none), so `buildDeclaredSubagents`
hands the parent's resolved `builtinTools`/toolsets (`main.go:1125-1126`) to a
no-scope subagent. An operator-authored declarative subagent is a trusted
delegate; it is still bound by the same `permissions.Gate` + `require_plan_artifact`
as the parent, so inheriting cannot escalate. Set an explicit empty list
(`"mcp": []`) to grant none of a dimension.

This keeps the recipe in one `config.json` + one `mcp.json` + one `skills/` tree
(a single ConfigMap) and avoids inventing per-agent registries in
`pkg/mcp`/`pkg/skills`. The rejected alternative — a per-subagent scope directory
(`.agents/subagents/<name>/mcp.json` + `skills/`) with its own `mcp.Build`
lifecycle — gives stronger on-disk isolation but adds a directory convention,
per-subagent `CloseAll`/metrics wiring, and a nested tree to mount; inline refs
won on declarative-deploy ergonomics.

## Per-substrate impact

- **`pkg/config`** — new `SubagentSpec` type + `Config.Subagents` field +
  `Validate` cases. Round-trip and validation tests.
- **`pkg/agent`** — one small addition: `WithSubagentMaxDepth(n int)`, so a
  per-subagent `MaxDepth` reaches `SubagentOptions.MaxDepth` at the parent's
  `WithSubagents` resolution rather than being validated and silently dropped.
  `WithSubagents` / `NewSubagentTool` / `SubagentOptions` (`subagent.go:35-85`)
  are otherwise sufficient as-is; no substrate behavior changes.
- **`pkg/mcp`** — **none** to signatures; the parent's single `mcp.Build`
  already returns per-server toolsets we filter by name.
- **`pkg/skills`** — one small addition: a name-filtered view of the merged
  skills toolset (no second `LoadAll`, no new keying).
- **`cmd/core-agent`** — the real weight lives here: a new
  `buildDeclaredSubagents` helper that constructs each subagent's LLM,
  instruction, and (inline-scoped) tool surface by filtering the parent's already
  loaded built-ins / MCP toolsets / skills by name, then appends one
  `agent.WithSubagents(...)` at the options-assembly site (`main.go:1124-1142`).
  No per-subagent `mcp.Build`/`CloseAll` — the single parent lifecycle
  (`main.go:883-889`) is unchanged.

## Config surface — full example

One `config.json` + one `mcp.json` + one `skills/` tree — a single ConfigMap.
The subagent selects its narrower surface inline, by name, from the shared config:

```jsonc
// .agents/config.json
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
      "tools": ["read_file", "grep"],       // built-in allowlist
      "mcp": ["gke-readonly"],              // a server named in the shared mcp.json
      "skills": ["gke-cluster-lifecycle"]  // a skill in the shared skills/
    }
  ]
}
```

```jsonc
// .agents/mcp.json — both endpoints live here once; parent refs "gke", the
// subagent refs "gke-readonly". Inline refs select distinct servers, not just
// subsets of one.
{
  "servers": {
    "gke":          { "transport": "http", "url": "https://container.googleapis.com/mcp" },
    "gke-readonly": { "transport": "http", "url": "https://container.googleapis.com/mcp/read-only" }
  }
}
```

A subagent that declares none of `tools`/`mcp`/`skills` inherits the parent's
full surface; an explicit `"mcp": []` grants none of that dimension.

## Implementation phases

- **Phase 1 (PR γ.1 of #599)** — `SubagentSpec` + `Config.Subagents` +
  `Validate`; config round-trip tests. No wiring yet.
- **Phase 2 (PR γ.2)** — `buildDeclaredSubagents` wiring for name / description /
  instructions / model / `max_depth` (via the new `WithSubagentMaxDepth`) +
  `WithSubagents`; a scripted/echo-provider test asserting the named tool is
  registered, invoked by name, inherits the parent model when unset, and runs on
  its own model when declared.
- **Phase 3 (PR γ.3)** — inline MCP + skills + tools filtering; a test asserting
  a subagent that names `gke-readonly` sees only that server's toolset (not the
  parent's `gke`), a subagent that names a skill subset sees only those, and a
  no-scope subagent inherits the parent's full surface.
- **PR B′** — the kube-platform-agent recipe adopts a `cluster` subagent
  (persona `@include`d from a vendored `upstream/cluster/SOUL.md`, own model,
  GKE-read-only MCP scope). Config-only, in-process.

Each PR carries `-race` tests and an adversarial-review section.

## Open questions

### 1. Per-subagent tool surface: scope dir vs inline named refs — RESOLVED (inline refs)

Resolved 2026-08-07 in favor of **inline named refs** (`"mcp": ["gke-readonly"]`,
`"skills": ["fleet-audit"]`, `"tools": ["read_file"]`). The decisive constraint is
the concrete consumer: kube-platform-agent deploys as a Kubernetes ConfigMap, so
the most declarative single-tree layout wins — one `config.json` + one `mcp.json`
+ one `skills/`, no nested `subagents/<name>/` directory to mount. The
name→server/skill "registry" the refs resolve against is not new machinery: it is
the `.agents/mcp.json` server map (already a named map) and the skills directory
(already name-addressable); the only added surface is a name filter over the
parent's already-loaded toolsets, not a second `mcp.Build`/`LoadAll`. The rejected
scope-dir alternative offered stronger on-disk isolation at the cost of a
directory convention and per-subagent MCP lifecycle wiring.

### 2. Default tool surface + allowlist semantics — RESOLVED (inherit parent)

Resolved 2026-08-07. **(a) Default when no `tools`/`mcp`/`skills` is declared:**
inherit the parent's full surface. A declarative subagent is a trusted,
operator-authored delegate (unlike a `spawn_agent` roster the *model* invents at
runtime), so convenience wins and an explicit empty list (`"mcp": []`) grants none
of a dimension. Because `WithSubagents` does not copy the parent's tools down,
this default is an explicit threading step in `buildDeclaredSubagents` (see
"Per-subagent MCP + skills scope (inline refs)"), not automatic behavior.
**(b) Allowlist semantics:** the allowlist filters the subagent's registry;
mutation still flows through the same `permissions.Gate` and
`require_plan_artifact` as the parent, so inheriting the full surface cannot
escalate privilege.

### 3. Model inheritance ergonomics

`Model: nil` inherits the parent. Should we also allow a bare
`"model": "gemini-3.5-flash-lite"` string shorthand (name-only, provider
inherited)? Deferred unless a consumer needs it.

## Security considerations

A declarative subagent is strictly *less* privileged than the parent by
construction: it runs through the same `permissions.Gate`, the same
`require_plan_artifact` gate, the same depth cap (`NewSubagentTool` default 2),
and — when it declares inline `tools`/`mcp`/`skills` — a *narrower* tool surface.
Instructions are
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
