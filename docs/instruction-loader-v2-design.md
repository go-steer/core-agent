# Instruction loader v2: composition + multi-file

Design doc for extending `pkg/instruction` from single-file
AGENTS.md to a composable multi-file loader, scoped to maximize
ecosystem migration on-ramp while keeping the v1 surface tight.

**Status:** proposed (2026-06-01). Awaiting approval before
implementation. v2.3 candidate.

## Motivation

Today every consumer of core-agent has exactly one project-scope
instruction file (AGENTS.md, with CLAUDE.md / GEMINI.md as
first-match-wins fallbacks) and one user-scope file (`~/.agents/AGENTS.md`).
The result is a single concatenated `Instruction` string handed to the
agent at construction time.

This is fine for monolithic agents. It scales poorly along three axes
that consumers — particularly teams moving from Cursor, Antigravity, or
Scion's multi-agent configs — already hit:

1. **Large instruction sets become unmaintainable in one file.**
   A real autonomous-coding-agent system prompt grows past 200 lines
   (principles + workflows + tool guidance + per-domain notes). One
   file = merge conflicts on every change.
2. **Sharing across projects requires copy-paste.** A team running
   four similar projects with shared base principles + per-project
   overrides has no composition primitive; they either duplicate
   principles into each project's AGENTS.md or maintain an external
   sync script.
3. **Migration friction (real cases verified).** Cursor's
   `.cursor/rules/*.mdc` is a multi-file directory. Antigravity uses
   `@include` directives between AGENTS.md files. Scion uses per-agent-
   type instruction files (deferred — see non-goals). Operators
   moving onto core-agent today have to flatten everything into one
   AGENTS.md by hand.

   (Hermes Agent does *not* use a manifest+includes pattern — earlier
   drafts of this doc claimed otherwise; see the corrected migration
   recipe below. Hermes uses convention-named files at the project
   root, most of which are out-of-scope for the instruction loader
   anyway.)

## Goals

- **Compose instructions from multiple files** without losing the
  single-file working baseline.
- **Make migration trivial** — a Cursor `rules/` directory, an
  Antigravity AGENTS.md hierarchy, or a multi-file vendored config
  tree should drop into a `.agents/` directory with at most
  renaming, no flattening.
- **Stay backwards-compatible.** Existing AGENTS.md files (no
  includes, no `AGENTS.d/`) load identically to today.
- **Keep the v1 surface tight.** Resist becoming a templating engine
  or a rules engine. Two primitives, well-tested.

## Non-goals (v1)

- **Per-role instruction overlays** (different prompts for the
  orchestrator vs. each subagent role). Defer until a concrete
  consumer surfaces — likely shape is `.agents/AGENTS.d/subagent/<role>.md`
  but the design space is wider than that and not worth committing now.
- **Frontmatter-driven scoping / conditional loading.** Adds a rules-
  engine footgun. Skills already cover the "trigger on intent" pattern;
  instructions should stay declarative.
- **Remote / URL includes.** Out of scope on security grounds.
- **Templating** (variable substitution, conditionals in the markdown).
  Encourages spec-creep; users that genuinely need it can preprocess
  outside core-agent.
- **`@include`-style symbol imports** (pulling a section out of another
  file). Out of scope — files include whole files only.

## Proposed design

Two composable primitives, both backwards-compatible. Either, neither,
or both can be used in any scope.

### Primitive 1: `@include <relative-path>` directive

Within any instruction file (AGENTS.md, files under `AGENTS.d/`, or
any transitively-included file), a line whose entire content matches
`@include <path>` (with optional surrounding whitespace) is replaced
in-place by the content of the referenced file.

```markdown
# Agent instructions

You are a GKE on-call orchestrator.

@include base/principles.md
@include workflows/triage.md
@include workflows/incident-response.md

## Project-specific overrides

Default cluster: prod-us-central1.
```

**Path resolution:**

- Relative to the including file's directory. So
  `AGENTS.md` including `workflows/triage.md` resolves to
  `<dir-of-AGENTS.md>/workflows/triage.md`.
- `../` is permitted *up to* the scope root (project root for
  project-scope files; user-agent dir for user-scope files). An
  `@include` that escapes the scope root is an error.
- Absolute paths are rejected.
- URL paths are rejected (string starts with `http://` / `https://`).
- Glob patterns are rejected at v1. If you want fan-in, use
  `AGENTS.d/`.

**Cycle + depth handling:**

- A file may not include itself transitively. The loader tracks the
  set of canonical absolute paths visited; revisiting a file errors
  with `instruction: include cycle at <path>` listing the chain.
- Maximum nesting depth is 8. Exceeding errors with
  `instruction: include depth exceeded (>8) at <path>`.

**Missing include behavior:**

- Missing includes are a **load error** (not silently skipped), so
  operator typos surface immediately. AGENTS.md itself is allowed
  to be absent (this matches today's behavior — memory is optional);
  files *referenced* via `@include` must exist.

**Frontmatter (YAML between `---` lines at the start of a file):**

- v1 **strips and discards** leading YAML frontmatter so the model
  doesn't see the metadata noise, but does not parse it. This keeps
  the door open for future per-file directives without committing to
  any v1 grammar.

**Source tracking:**

- Each included file becomes a separate entry in `Loaded.Sources`
  (so `/memory` shows every loaded file, not just the entry-point).
  Each entry's `Path` is the canonical absolute path; `Bytes` is the
  raw size; `Truncated` is preserved.

### Primitive 2: `.agents/AGENTS.d/*.md` directory

Linux conf.d-style fan-in. If `<scope-root>/AGENTS.d/` exists and
contains `*.md` files, they are loaded in lexical (byte-wise) order
of filename and concatenated after the scope's primary file
(AGENTS.md / CLAUDE.md / GEMINI.md).

```
.agents/
├── AGENTS.md             # main entry (optional — see below)
└── AGENTS.d/
    ├── 00-principles.md
    ├── 10-workflows.md
    └── 20-tooling.md
```

**Selection rules:**

- Only files matching `*.md` at the top level. Subdirectories are
  ignored (v1; not a recursive walk).
- Hidden files (name starts with `.`) are ignored — operators can
  stage WIP edits as `.foo.md` without polluting the loaded set.
- The directory's name is exactly `AGENTS.d` — no `CLAUDE.d` or
  `GEMINI.d` variants. The fallback chain only applies to the main
  entry, not to the directory.

**Interaction with the fallback chain:**

- The primary file is resolved per the existing first-match-wins
  chain (`AGENTS.md` → `CLAUDE.md` → `GEMINI.md`). The `AGENTS.d/`
  directory is loaded regardless of which primary won.
- If no primary file exists but `AGENTS.d/` is non-empty, the
  directory's content alone is the loaded instruction.
- If both are absent, the scope contributes nothing (today's behavior
  when neither exists).

**Interaction with `@include`:**

- Files under `AGENTS.d/` may use `@include` themselves. Path
  resolution is relative to that file (so `AGENTS.d/foo.md`
  including `../base/principles.md` is fine).

**Ordering:**

- Loaded order is: `<primary-file>` first, then `AGENTS.d/*.md` in
  lexical order. The 2-digit prefix convention (`00-`, `10-`, etc.)
  is operator-managed; the loader doesn't enforce it.

### Cross-scope ordering

Today's scope precedence is **user → project** (user-global loads
first, project content appended). v2 preserves this: each scope is
resolved fully (primary file + AGENTS.d) and the two scopes are
concatenated with a clean newline boundary.

So the full assembly order, with everything wired up, is:

1. `~/.agents/AGENTS.md` (or first-match-wins fallback) at user scope
2. `~/.agents/AGENTS.d/*.md` at user scope
3. `<project-root>/.agents/AGENTS.md` (or fallback) at project scope
4. `<project-root>/.agents/AGENTS.d/*.md` at project scope

Each `@include` within a file is processed inline during that file's
read, so a file's resolved content replaces the `@include` line where
it appears in the chain above.

## Common recipes

### Recipe: explicit ordering without renaming files

The `AGENTS.d/` directory uses lexical filename order, which is the
common Linux conf.d convention (`00-foo.md`, `10-bar.md`, ...). Teams
bringing in an existing multi-file layout often don't want to rename.
The `@include` primitive covers this without a separate config knob:
write your AGENTS.md as a pure manifest of includes.

Existing layout (no renames needed):
```
.agents/
├── AGENTS.md          # manifest only — see below
├── persona.md
├── principles.md
└── workflows.md
```

`AGENTS.md` contents:
```markdown
@include persona.md
@include principles.md
@include workflows.md
```

The model never sees the `@include` lines — they're replaced in
place by the referenced file content. The order in the manifest is
the order the content lands in the system prompt.

### Recipe: shared base + per-project overrides

User scope holds the shared base; project scope adds the
project-specific overlay. The two scopes concatenate
automatically (user first), so nothing extra is needed.

```
~/.agents/AGENTS.md                # team's standard principles
~/.agents/AGENTS.d/style-guide.md
~/.agents/AGENTS.d/tool-prefs.md

<project>/.agents/AGENTS.md         # project-specific behavior
<project>/.agents/AGENTS.d/runbook.md
```

To pull in a chunk that lives outside the project (e.g., a shared
runbook tree symlinked or vendored under the user-agents dir),
use `@include ../some-other-file.md` from a user-scope file. (You
can't `@include` across the scope root, so the file has to be
inside the user-agents dir already.)

### Recipe: factor a workflow out of a long AGENTS.md

Pull the chunk into its own file under `AGENTS.d/` (no rename
required if you don't care about ordering relative to other
`AGENTS.d/` files), or `@include` it from AGENTS.md if order
matters.

## Edge cases (the answers we commit to)

| Edge case | Behavior |
|---|---|
| Empty `AGENTS.d/` (directory exists, no .md files) | Silently no-op |
| `AGENTS.d/` exists but no AGENTS.md | Loaded content is just the directory's files |
| `@include` of an absolute path | Error: `instruction: include path must be relative` |
| `@include` of a path escaping scope root | Error: `instruction: include path escapes scope <root>` |
| `@include` of a missing file | Error: `instruction: include not found: <path>` |
| Symlink to a file inside scope | Allowed (canonical path tracked for cycle detection) |
| Symlink to a file outside scope | Error: same as escape rule |
| `@include` inside a fenced code block (` ``` ` ... ` ``` `) | NOT processed — `@include` is recognized only on its own line outside any code fence |
| Recursive include of self | Error: cycle |
| Depth > 8 | Error: depth exceeded |
| File with only YAML frontmatter, no body | Loaded as empty content (frontmatter stripped) |
| Non-UTF-8 file | Error: `instruction: invalid UTF-8 in <path>` |
| File over 64 KiB | Truncated with `Truncated=true` (mirrors current behavior) |

## Migration story

Concrete recipes for the three main inbound sources:

### From Hermes Agent

Hermes uses convention-named files at the project root (no manifest,
no `@include`). Of those, only some are instruction-loader concerns;
the rest are data-layer concerns covered by other features.

| Hermes file | Role | Migrate to |
|---|---|---|
| `AGENTS.md` | Workspace instructions | `.agents/AGENTS.md` — works 1:1, no changes |
| `SOUL.md` | Persona | Either concatenate into `AGENTS.md`, or drop as `.agents/AGENTS.d/00-persona.md` if you prefer the multi-file layout |
| `MEMORY.md` | Agent-maintained memory entries | **Not an instruction-loader concern** — see [`shared-memory-design.md`](shared-memory-design.md). Once the shared-memory layer ships, the FTS5-over-eventlog index replaces the static file. |
| `USER.md` | Per-user model entries | Same — shared-memory layer with a user-scope tag. Not an instruction-loader concern. |
| `~/.hermes/skills/` | Skill bundles | Already covered by `pkg/skills` (drop into `~/.core-agent/skills/`). |

In other words, the Hermes → core-agent migration is much smaller than
"multi-file vs. single-file" framing implies. The instruction-loader
v2 work helps the SOUL.md split (and helps native composability), but
the headline Hermes migration story belongs to the shared-memory
design, not here.

### From Scion's per-agent configs

Scion has multiple agent types with their own system prompts.
At v1 we don't support per-role overlays, so the migration is "pick
the orchestrator's prompt as your AGENTS.md; pass other roles'
prompts via `spawn_agent`'s `system_prompt` arg." When per-role
overlays land (deferred), the Scion-side mapping becomes 1:1.

### From Cursor's `.cursor/rules/*.mdc`

Rename `.cursor/rules/` → `.agents/AGENTS.d/`, strip the `.mdc` →
`.md` rename. Cursor frontmatter is YAML-shaped and gets silently
stripped per the frontmatter rule above.

### From Antigravity's AGENTS.md hierarchy

If they already use `@include`, content moves over with zero changes.
If they use a different include syntax, one-time `sed` rewrite.

## Implementation notes

Touches one package:

- `pkg/instruction/load.go`:
  - New helper `resolveIncludes(path, content, depth, visited)` that
    expands `@include` lines, recursing.
  - New helper `loadDirectory(scopeRoot, dirName)` that walks
    `AGENTS.d/`, applies the `.md` + non-hidden filter, sorts
    lexically, returns concatenated content.
  - `Load(projectRoot, userRoot)` rewired to use both helpers.
  - `Loaded.Sources` extended to include every loaded file (entry +
    transitively-included + AGENTS.d/*).

- `pkg/instruction/load_test.go`: parallel tests for each edge case
  in the table above plus the migration recipes.

- `docs/site/content/docs/reference/configuration.md`: new section
  "Multi-file instructions" with the AGENTS.d layout, `@include`
  grammar, and the migration recipes.

- `cmd/core-agent`'s `/memory` slash already lists `Loaded.Sources`;
  no change needed — it'll just show more files.

- `cmd/core-agent`'s `/reload` already re-walks `instruction.Load`
  via the `agent.WithAttachReloader` closure; no change needed —
  reload picks up new AGENTS.d/ files automatically.

Estimated scope: **~400 LoC** (200 implementation + 200 tests) +
**~50 LoC** docs. Single PR.

## Out of scope (recorded so we don't re-propose)

- **Per-role instruction overlays.** Deferred. Likely shape:
  `.agents/AGENTS.d/subagent/<role>.md` with `BackgroundAgentManager`
  reading the role file when present. Not committed.
- **Frontmatter-driven directives** (`# +scope: project`,
  `# +order: 50`, etc.). v1 strips frontmatter; v2 can add
  directives without breaking compat.
- **`@include` with globs.** Use `AGENTS.d/` instead.
- **Conditional includes** (load X only when tool Y is enabled).
  This is what skills are for.
- **Cross-scope inclusion** (a project file including a user file).
  Out — keeps the scope boundary clear.
- **External include sources** (URL, gist, git repo). Operator can
  vendor those into the project tree as they would any other
  dependency.

## Open questions

1. **Should `@include` be limited to AGENTS.md only, or work in
   AGENTS.d/ files too?** Current proposal: works in any loaded
   markdown file. Argument for restricting: forces a single
   "manifest" file as the include orchestrator, simpler mental
   model. Argument against: more rigid; AGENTS.d/ users can't
   factor common bits out. **Recommend allowing in both;
   simplicity comes from the depth cap + cycle detection.**

2. **Should the directory name be `AGENTS.d` exactly, or also
   `CLAUDE.d` / `GEMINI.d`?** Current proposal: `AGENTS.d` only.
   Argument for: the fallback chain is for the *primary* entry,
   not for fan-in. Argument against: parity with the chain.
   **Recommend AGENTS.d only;** the chain exists for legacy
   compat, the directory is a v2 addition with no legacy.

3. **Should the user-global scope also support `AGENTS.d/`?**
   Current proposal: yes, same shape at both scopes. Argument
   for asymmetry: user scope is "small base nudges, project scope
   is the bulk." Argument for symmetry: surprising to support at
   one but not the other; users who maintain extensive personal
   workflows benefit. **Recommend symmetric.**

4. **Should the loader emit a warning when a primary file
   contains an `@include` line that's inside a fenced code block?**
   Today the rule says "not processed in code blocks." But if an
   operator pastes `@include foo.md` into a code example and
   *expects* it to be processed, they'd be confused. **Recommend
   no warning; document the rule clearly and let example-pasting
   debug itself.**

5. **Cycle-detection scope.** If user-scope AGENTS.md includes
   `/home/user/some-file` and project-scope AGENTS.md *also* includes
   `/home/user/some-file`, is that a cycle? Current proposal: **no.**
   The visited-set is per-Load call but only tracks *this assembly's*
   path. Including the same file twice in independent chains is a
   warning at most; the resulting Instruction string just has the
   content twice. Operators wanting strict dedup can move the shared
   content to user-scope and not include it from project-scope.

## Acceptance criteria

A v1 implementation lands when:

- All edge cases in the table parse + behave as documented
- The Cursor + Antigravity migration recipes work end-to-end on a
  vendored test fixture (Hermes is mostly a 1:1 AGENTS.md rename;
  the Scion per-agent-type case is deferred per non-goals)
- `Loaded.Sources` correctly enumerates every loaded file across both
  scopes
- `/memory` and `/reload` work unchanged for the new file layouts
- An AGENTS.md with no `@include` and no `AGENTS.d/` sibling produces
  byte-identical output to today's loader (true regression test)
- The configuration reference doc shows the AGENTS.d layout and
  `@include` grammar with a worked migration example

## Composition with other features

- **Skills** (`SKILL.md` bundles): already loaded separately via
  `pkg/skills`. No change. Operators can have AGENTS.d/ alongside
  `skills/` — they serve different roles (instructions vs. tool-
  triggered workflows).
- **`/reload`** (server-side action): already re-walks
  `instruction.Load` via the `agent.WithAttachReloader` closure.
  Picks up new AGENTS.d/ files + edits to included files
  automatically.
- **Shared-memory layer** (`docs/shared-memory-design.md`):
  orthogonal. Memory is queryable + agent-mutable data; instructions
  are static, operator-authored configuration. Including memory
  query results into an `@include`-d file would be a separate
  feature (and probably belongs in the memory layer's `recall_memory`
  tool, not here).
- **`+ New session` picker** (task #4): if/when multi-session daemon
  lands, each session inherits the same instruction loader output.
  Per-session overrides could later be implemented as
  `.agents/AGENTS.d/session/<sid>.md`, but that's deferred.

## Why now

- v2.2 just shipped + we have a quiet window before v2.3 commits to
  its headline feature.
- The shared-memory design is a much larger lift (~600–800 LoC, two
  PRs); the instruction loader could land in parallel as a smaller
  second-billing item.
- Three concrete consumers will likely benefit immediately:
  - GKE PE team (the `examples/gke-parallel-triage/` recipe ships
    one AGENTS.md today; with this it can ship a clean
    multi-file layout)
  - Cursor / Antigravity migration evaluators (Hermes migration is
    smaller scope than originally framed — see the corrected
    migration table)
  - Cogo flip (Path C from `cogo-core-agent-integration.md`) —
    cogo's instructions are large enough that the multi-file
    layout would be a quality-of-life win during/after the flip

## Decision log

| Date | Decision | Rationale |
|---|---|---|
| 2026-06-01 | Two primitives: `@include` directive + `AGENTS.d/` directory | Each addresses a different composition style; both are common in the ecosystem; together they cover the migration matrix |
| 2026-06-01 | No frontmatter parsing at v1; strip silently | Keeps surface tight; forward-compatible for future directives |
| 2026-06-01 | Missing include = error, missing primary file = OK | Operator typos surface immediately; memory remains optional overall |
| 2026-06-01 | Cycle detection via canonical-path visited set, depth cap = 8 | Safer than depth-cap alone; matches what most preprocessors do |
| 2026-06-01 | Per-role overlays + frontmatter directives + templating: out of scope | Avoid spec-creep; ship a tight v1; revisit when consumers actually ask |
| 2026-06-02 | Corrected Hermes migration framing | Earlier drafts described Hermes as "persona/memory/tools as separate files referenced from a manifest" — that was extrapolation, not citation. Hermes actually uses convention-named files at the project root with no manifest; only SOUL.md falls under the instruction loader's scope. MEMORY.md / USER.md belong to the shared-memory design. Doc rewritten to reflect this and to elevate Cursor + Antigravity (both verified) as the leading migration cases. |
| 2026-06-02 | Considered + rejected a `config.instructions.files: [...]` third primitive | Would let operators declare explicit load order without renaming files, addressing a real rename-friction concern. Rejected because the `@include` primitive already covers this: write AGENTS.md as a pure manifest of `@include` lines. Same explicit-ordering effect, same no-rename, no new schema field, no third primitive for operators to learn. The "explicit ordering without renaming" recipe in Common Recipes is the documented path. Revisit if operators push back on the manifest pattern (likely complaint: AGENTS.md as a manifest feels indirect; config-first teams prefer schema fields over markdown directives). |
