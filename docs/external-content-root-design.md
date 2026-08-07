# External content-root

**Status:** proposed (2026-08-07). Target: **v2.9**. Tracking issue: [#600](https://github.com/go-steer/core-agent/issues/600). Realizes the "external content-root (operator-declared trusted roots)" follow-on named in `docs/hermes-replacement-design.md` (W5).

## Motivation

The kube-platform-agent recipe ([#594](https://github.com/go-steer/core-agent/issues/594),
shipped as [#598](https://github.com/go-steer/core-agent/pull/598)) proves
core-agent can run the kube-agents Platform Agent — but only by **vendoring a
snapshot** of the upstream tree into `examples/kube-platform-agent/upstream/` and
**copying** the 18 skills into `.agents/skills/`. Two framework limits force that:

1. **`@include` and symlinks are scope-confined.** `ensureWithinScope`
   (`pkg/instruction/load.go:648-667`) rejects any include target whose
   canonicalized path escapes the including file's *scope root* — a deliberate
   exfil-vector defense (`load.go:643-647`: "an operator could be tricked into
   pointing an `@include` at a crafted symlink that pulls `/etc/passwd` into the
   system prompt"). So an `AGENTS.md` cannot reach a sibling repo.
2. **Config location is glued to the content root.** `-c <file>` sets
   `agentsDir = filepath.Dir(cfgPath)` (`cmd/core-agent/main.go:2033-2035`) and
   `projectRoot = filepath.Dir(agentsDir)` (`main.go:682-685`); all three loaders
   (`instruction.LoadForSession`, `skills.LoadAll`, `mcp.Build`) key off those.

To run a **genuinely unmodified** `kube-agents/agents/platform` checkout — adding
nothing to their tree, no vendored copy, no rename to `AGENTS.d/` — core-agent
needs to load instructions and skills from an operator-declared root *outside*
the project root. This is a general capability (any team pointing core-agent at a
content repo they don't own), not recipe-specific.

## Goals

- An additive config surface — `content_roots: [...]` in `.agents/config.json`
  and/or a `--agents-content-dir` flag — naming one or more external directories.
- Each declared root is an **explicitly-trusted scope**: `@include` and skills
  resolve *within* that root (self-confined to itself), bypassing the
  sibling-repo ban **only** for operator-declared paths.
- Skills from declared roots compose into the existing multi-source skill overlay.
- Zero change to the external tree; zero change required of existing recipes
  (empty `content_roots` = today's behavior exactly).

## Non-goals (v2.9)

- Relaxing scope confinement for anything *not* explicitly declared — undeclared
  `../` escapes and out-of-scope symlinks stay rejected.
- Remote/URL content roots — local paths only (URL includes remain out of scope
  on security grounds, per `docs/instruction-loader-v2-design.md`).
- Auto-translating an external MCP `config.yaml` — the recipe still writes its
  own `mcp.json` (translated once); see out of scope.
- Writing into the external tree.

## Conceptual model

A **content root** is an operator-declared directory that the loader treats as an
additional **trusted scope**, exactly like the existing home-agents root
(`pkg/instruction/load.go:198-210`) — loaded via `loadScope` with *itself* as the
`scopeRoot`, so its own `@include`s are confined to it but it does not need to sit
under `projectRoot`. The trust boundary moves from "one project root" to "the
project root plus the roots the operator explicitly named," and nothing else
changes: an undeclared path is still an escape.

**Ordering** means different things in the two loaders, which sequence scopes
oppositely — the doc is explicit about both:

- **Instructions are concatenated**, not overridden; the shared `visited` set
  (`load.go:190`) only dedups an *identical* file, keeping its first occurrence.
  The existing order is user → home-agents → project → caller
  (`load.go:192-218`). The content-roots block is inserted just before the
  project block, so external content is concatenated ahead of the project's own
  `AGENTS.md` (concatenation order, not an override).
- **Skills and MCP are first-declarer-wins with project first**
  (`skills/load.go:148-166`, `mcp/config.go:232`). There, content roots are
  inserted right *after* the project source, giving name-collision precedence
  project > content_roots (listed order) > home-agents > user.

## Detailed design

### Config surface (`pkg/config/config.go`)

A new top-level field on `Config` (`config.go:39-57`), snake_case tag like its
neighbors, `[]string` per the `PathScopeConfig.Allow` precedent (`config.go:211`):

```go
// ContentRoots are operator-declared external directories trusted as
// additional instruction/skill scopes. Paths are resolved relative to the
// agents dir. Empty = only the project root is trusted (default).
ContentRoots []string `json:"content_roots,omitempty"`
```

`--agents-content-dir` (repeatable) is the equivalent CLI flag, merged with the
config value. `Validate` (`config.go:894`) checks each entry is non-empty and
relative-or-absolute path-shaped (no environmental existence check — `Validate`
stays environment-free per `config.go:890-893`).

### `pkg/instruction` — a trusted-root scope walk

`instruction.Load` already has a functional-option pattern
(`type Option func(*loadOptions)`, `load.go:106`; `loadOptions{interp,
homeAgentsRoot}`, `load.go:108-122`). Add:

```go
func WithContentRoots(dirs []string) Option  // sets loadOptions.contentRoots
```

In `LoadForSession` (`load.go:177`), add a scope-walk block **between** the
home-agents block (`load.go:198-210`) and the project block (`load.go:212-216`),
one iteration per declared root, calling `loadScope(root, scope, ...)` with the
root as its own `scopeRoot`. Because `ensureWithinScope` (`load.go:648`) is passed
that root as the scope, the external tree's `@include`s are confined to the
external tree — the confinement invariant holds *within* each trusted island; it
is only the *cross-root* ban that the operator opt-in relaxes. The shared
`visited` set (`load.go:190`) already prevents include cycles across scopes.

**Overlay dirs.** Today only `AGENTS.d/` is scanned as an overlay
(`agentsDirName`, `load.go:63`; scan at `load.go:316-351`). An external tree may
keep overlays under a different dir (e.g. kube-agents' `governance/`). Whether to
add a configurable overlay-dir list is an open question — the kube-platform-agent
recipe reads governance **on demand** (not as always-on overlays), so B″ does not
strictly need it. Proposed: ship trusted roots first; add overlay-dir declaration
only if a consumer needs always-on external overlays.

### `pkg/skills` — one more source

Skills already compose N sources through `overlayFS` (`pkg/skills/overlay.go:36`)
in `LoadAll` (`load.go:140-168`): the `sources` slice is built by `append`ing
`openSkillsDir(dir)` for project, home-agents, and user (`load.go:149-157`), then
right-folded into an overlay chain (`load.go:164-166`), where the *first* source
wins a name collision. Adding external roots is a matter of `append`ing
`openSkillsDir(root)` for each declared root **right after the project source**
(so precedence is project > content_roots > home-agents > user) and threading them
through a `skills.WithContentRoots([]string)` option (mirroring
`WithHomeAgentsSkillsDir`, `load.go:98`). This is why skills are *easier* than
instructions here — no confinement relaxation is needed; skills are read from a
directory FS, not `@include`d.

### MCP

External MCP servers are **not** auto-loaded. The external tree's `config.yaml`
(Hermes format) is translated **once** into the recipe's own `mcp.json` (as
[#598](https://github.com/go-steer/core-agent/pull/598) already did for `gke` /
`developer_knowledge`). This keeps the external tree untouched and avoids parsing
a foreign MCP schema. Documented, not implemented here.

### `cmd/core-agent/main.go` — resolve and thread

Resolve `cfg.ContentRoots` (+ `--agents-content-dir`) relative to `agentsDir`
once, near where `projectRoot` is computed (`main.go:682-685`), then pass the
resolved list to the two loaders at their existing call sites:

- `instruction.LoadForSession(projectRoot, coreHome, ..., WithContentRoots(roots))`
  (`main.go:691-693`)
- `skills.LoadAll(ctx, agentsDir, coreHome, gate, ..., skills.WithContentRoots(roots))`
  (`main.go:891-892`)

The config-reload paths (`main.go:1163/1178/1239/1244`) get the same argument.

## Per-substrate impact

- **`pkg/config`** — `Config.ContentRoots` field + `Validate` case + round-trip
  test.
- **`pkg/instruction`** — `WithContentRoots` option, `loadOptions.contentRoots`,
  one scope-walk block in `LoadForSession`. Tests: an external root's `@include`
  resolves; a non-declared sibling still errors; an out-of-scope symlink *inside*
  a declared root still errors (confinement holds within the island).
- **`pkg/skills`** — `WithContentRoots` option + `append` into `sources`. Test:
  a skill in an external root is discovered at the right precedence.
- **`cmd/core-agent`** — flag parsing, path resolution, thread through loader call
  sites (incl. reload).

## Config surface — full example

```jsonc
{
  "version": 1,
  "model": { "provider": "vertex", "name": "gemini-3.5-flash" },
  // Point at a real, UNMODIFIED kube-agents checkout — nothing added to it.
  "content_roots": ["../kube-agents/agents/platform"],
  // MCP is still translated locally (the external tree is untouched):
  // .agents/mcp.json holds gke + developer_knowledge.
  "permissions": { "mode": "yolo", "require_plan_artifact": true },
  "tools": { "disable": ["bash"] }
}
```

Equivalent flag form: `core-agent -c .agents/config.json --agents-content-dir ../kube-agents/agents/platform`.

## Implementation phases

- **Phase 1 (PR ε.1 of #600)** — `Config.ContentRoots` + `Validate` + config
  round-trip test. No loader change.
- **Phase 2 (PR ε.2)** — `pkg/instruction` `WithContentRoots` + scope walk;
  confinement tests (declared resolves; undeclared and in-island escapes reject).
- **Phase 3 (PR ε.3)** — `pkg/skills` `WithContentRoots`; `cmd/core-agent`
  wiring + flag; end-to-end loader test against a fixture external tree.
- **PR B″** — the recipe gains a documented mode pointing `content_roots` at a
  real `kube-agents/agents/platform` checkout, replacing the copied skills and
  proving "run kube-agents with core-agent, unmodified."

## Open questions

### 1. Trusted-scope relaxation vs a separate loader path

Proposed: reuse `loadScope` with the root as its own `scopeRoot` (minimal, and
`ensureWithinScope` still guards *within* each root). Alternative: a distinct
"external" code path with its own (looser) rules — rejected as more surface area
and a second confinement implementation to audit. Confirm the reuse holds for the
symlink-canonicalization edge cases (`load.go:655-664`).

### 2. Overlay-dir declaration

Should content roots be able to declare extra overlay dirs (e.g. `governance/`)
that scan like `AGENTS.d/`? B″ does not need it (governance is on-demand). Defer
unless a consumer needs always-on external overlays.

### 3. Precedence when a name collides across roots

For skills/MCP: project > content_roots (listed order) > home-agents > user,
first-declarer-wins — matching `mcp.LoadAll` (`config.go:232`) and the skills
overlay fold (`load.go:164-166`). For instructions there is no override (all
scopes concatenate; `visited` dedups identical files only), so the choice is
purely concatenation order — content roots just before the project block.
Confirm both are the least-surprising defaults.

## Security considerations

The exfil-vector ban (`load.go:643-647`) exists because an operator could be
*tricked* into including untrusted content. `content_roots` does not weaken that:
it is an **explicit, in-config, operator-authored** declaration of trust for
named directories — the same "operator opt-in, validated" model as the
multi-session caller-overlay dir (`validateCallerIdentity` /
`ErrInvalidCallerIdentity`, `load.go:244-259`). Confinement is **not** globally
relaxed: undeclared `../` escapes, out-of-scope symlinks, and absolute/URL
includes all remain errors, and a symlink *inside* a declared root that points
*outside* it is still rejected by `ensureWithinScope` (the root is that scope's
`scopeRoot`). The blast radius is exactly the set of directories the operator
typed into their own config.

## Out of scope (deferred)

- Remote/URL content roots.
- Auto-translating a foreign MCP `config.yaml`.
- Configurable overlay dirs (open question 2) unless a consumer needs it.
- Writing back into the external tree.

## Dependencies and related work

- Consumer: kube-platform-agent recipe ([#594](https://github.com/go-steer/core-agent/issues/594)
  / [#598](https://github.com/go-steer/core-agent/pull/598)); the live variant is
  PR B″.
- Sibling enabler: declarative subagents ([#599](https://github.com/go-steer/core-agent/issues/599),
  `docs/declarative-subagents-design.md`).
- Relaxes the limitation documented in `docs/instruction-loader-v2-design.md`
  (§"can't @include across the scope root"); supersedes the sibling-repo symlink
  sketch in `docs/kube-agents-platform-fit.md`.
- Epic: [#589](https://github.com/go-steer/core-agent/issues/589),
  `docs/hermes-replacement-design.md` (W5).
