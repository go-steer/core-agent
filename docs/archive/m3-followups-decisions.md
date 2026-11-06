# M3 follow-ups — implementation decisions

Running record of choices made while executing [`docs/m3-followups-plan.md`](./m3-followups-plan.md). Each item gets a section with what landed, decisions made (with reasoning), and what was deliberately left out.

## Item 1 — Glob / grep built-in tools

Status: shipped on `main`.

### What landed

- **`tools/glob.go`** — `globArgs{Path, Pattern}` → `globResult{Paths, Truncated}`. Walks `path` (default `.`) with `filepath.WalkDir`; matches each file basename against `Pattern` via `filepath.Match`. Hidden-dir skip set (`.git`, `.svn`, `.hg`, `node_modules`, `vendor`); no symlink follow; gate-checked at the walk root and per matched path; output capped via `cfg.ToolOutput.PerTool["glob"]` (default 32KB / 500 lines).
- **`tools/grep.go`** — `grepArgs{Path, Pattern}` → `grepResult{Matches, Truncated}` where each match has `{Path, Line, Text}`. Walks `path` (default `.`) recursively when it's a directory; single-file mode when it's a regular file. Same skip set + symlink + gate semantics. Pattern is RE2 (`regexp.Compile`). Default cap 256KB / 5000 lines.
- **`tools/glob_test.go`** (8 tests) and **`tools/grep_test.go`** (8 tests) — happy path, gate denial (with a separate `scopedGate` helper for `ModeAllow`), hidden-dir skip, truncation, sort order, regex anchors, single-file mode, invalid pattern rejection.
- **`tools/builtins.go`** — `BuiltinTools` extended with `Glob bool`, `Grep bool`; both default-on in `Default()`; `Disable("glob")`/`Disable("grep")` work; canonical `BuiltinToolNames` updated.
- **`tools/builtins_test.go`** — `TestBuild_DefaultProducesSixTools` renamed to `TestBuild_DefaultProducesEightTools`; cases map extended; the unknown-name test switched to `"not_a_real_tool"` (was `"grep"`, now a real tool).
- **`config/config.go`** — `tool_output.per_tool` defaults gain `glob` (32KB / 500) and `grep` (256KB / 5000). The pre-existing `grep` entry of 16KB / 200 was bumped up to match the plan's defaults.
- **Docs**: `README.md` Features bullet enumeration, `docs/DESIGN.md` updated (deferred line replaced with shipped description; goals bullet enumeration extended), `docs/site/content/docs/library-api.md` "Built-in tools" section, `docs/site/content/docs/configuration.md` defaults block + tools.disable valid-names list.

### Decisions made (with reasoning)

**Stdlib-only, no `bmatcuk/doublestar`.**
Per the plan. The cost is no `**` recursive-glob shorthand. Workaround: pass an explicit walk root (`glob(path: "src", pattern: "*.go")`) — works for every realistic case. Adding the dep would have been one more transitive import for a syntactic convenience.

**`scopedGate` test helper, not just `permissiveGate`.**
The first version of the gate-denial tests used `permissiveGate` (yolo mode). Yolo bypasses path-scope checks entirely, so the assertion never fired. Added a separate `scopedGate(t, root)` that uses `ModeAllow` with a path scope — out-of-scope reads return a clear error rather than going through a (non-existent) prompt path. Both helpers stay; happy-path tests keep using `permissiveGate` for brevity.

**`TestGlob_DefaultsToCurrentDir` is non-parallel.**
Uses `t.Chdir` which mutates process-global state. The `testing` package refuses to allow `t.Chdir` on a parallel test. One non-parallel test in an otherwise-parallel file is fine.

**JSON-byte-cap loop drops trailing entries.**
`globResult` and `grepResult` are JSON-encoded then byte-capped. The capping is approximate — drop the last match, re-encode, check, repeat — because surgical truncation of JSON would require parsing and re-emitting. Predictable, correct, slightly wasteful for very large results. Acceptable trade-off.

**No `**` glob support.**
Documented in DESIGN.md and library-api.md. The model will discover this on its own when its first `**/*.go` returns no matches; the docs explain why and offer the workarounds (explicit walk root, or grep's recursive walk).

**Glob walks the whole tree even when matching only basenames.**
We could shortcut by checking `filepath.Match` against the path (skipping subdirectories that can't contain matches), but the basename semantic means we always need to descend. Acceptable for v1; revisit if walks become a bottleneck.

**No bash shortcut.**
Not wrapping `find` / `grep` external commands. Cross-platform (Windows works), no shell-quoting hazards, no PATH dependencies, deterministic output shape. The 200-line stdlib implementation is worth it.

### What did NOT land in Item 1

- **`**` recursive glob.** Stdlib-only constraint; pulling `bmatcuk/doublestar` was rejected.
- **Symlink-follow option.** Walking with `lstat` defaults; consumers who need follow-symlinks ask later.
- **`include_hidden: true` arg.** Hidden-dir skip is currently hard-coded. Add when a consumer asks.
- **Case-insensitive flag for grep.** RE2 has `(?i)` inline; consumers can pass that. No top-level flag added.

### Verification

```bash
go test ./tools/ ./config/...   # 16 new tests pass; existing tests still green
go vet ./...                    # clean
go build ./...                  # clean
for s in dev/ci/presubmits/*; do bash "$s"; done   # all green
```

---

## Item 2 — `eventlog.WithSessionTree`

Status: shipped on `main`.

### What landed

- **`eventlog.WithSessionTree(appName, userID, parentSessionID)`** QueryOption — returns events whose `session_id = parent OR session_id LIKE parent:sub:%` under the given (app, user) pair. One query for parent + every derived sub-session.
- **SQL pattern in `eventlog/sql.go`** — `WHERE app_name = ? AND user_id = ? AND (session_id = ? OR session_id LIKE ?)`. WithSessionTree wins over ForSession when both are set on the same query (last-wins; documented).
- **Four new tests in `eventlog/eventlog_test.go`**: `TestWithSessionTree_ReturnsParentAndSubagent`, `TestWithSessionTree_IgnoresUnrelatedSessions`, `TestWithSessionTree_DepthAgnostic` (parent + child + grandchild), `TestWithSessionTree_ComposesWithAuthor`.
- **`examples/with-subagent/main.go`** — replaced the two-query parent-then-branch-prefix audit pattern with a single `WithSessionTree` call. Output now shows the full tree in seq order under one header.
- **Docs**: `docs/site/content/docs/sessions.md` filter table gains a `WithSessionTree(...)` row; `docs/site/content/docs/library-api.md` Subagents section's "Audit log and isolation" subsection now leads with `WithSessionTree`; the "What's deferred" list drops the now-shipped item.

### Decisions made (with reasoning)

**Mutually-exclusive with `ForSession` via last-wins semantics.**
Both fill the same conceptual slot ("which session(s) am I querying?") but with different semantics. Rather than erroring on the conflict, the SQL builder picks one path: if `treeParentID` is non-empty, the tree query runs and `ForSession`'s fields are ignored. Documented in the godoc. Simpler than a runtime error; consumers who set both probably mean the tree.

**Naming `:sub:` as the literal separator in the LIKE pattern.**
The convention `<parent>:sub:<branch>` is established in `agent/subagent.go` (`deriveSubagentSessionID`). `WithSessionTree` hardcodes the same separator. If we ever change the naming convention, both places update together. Tested via the `DepthAgnostic` case which exercises `task:sub:a:sub:b`.

**No options for "tree of an arbitrary session-id pattern."**
A general "match by session-id LIKE pattern" would be more flexible (e.g. for non-subagent naming schemes) but invites SQL-injection-style misuse via wildcard characters. `WithSessionTree` keeps the intent narrow: parent + descendants under our specific naming convention.

### What did NOT land in Item 2

- **`WithSessionTree` on the `agent` package** as a parallel convenience accessor on `Agent`. The package boundary is fine — consumers reach through `agent.EventLog().Stream.Since(...)` already.
- **A `WithSessionTreePrefix(...)` variant** that takes just the parent session ID and matches without the `:sub:` separator. Would be more flexible but invites the misuse above.

### Verification

```bash
go test ./eventlog/...           # 4 new tests pass; existing tests still green
go run ./examples/with-subagent  # full tree returned in seq order under one header
```

---

## Item 3 — Plan-doc refresh

Status: shipped on `main`.

### What landed

- **`docs/autonomous-plan.md`** — prepended a "Status (2026-05-15): shipped" header pointing at README's M3 entry, the `autonomous.md` Hugo page, the `library-api.md` Autonomous + Crash-resume sections, and `eventlog-decisions.md` Phase 3.
- **`docs/eventlog-plan.md`** — prepended a "Status (2026-05-15): shipped" header noting all four phases shipped, calling out the Phase 4 deviation (derived session IDs), and pointing at the canonical references.
- **`docs/subagents-plan.md`** — already had a status header from Phase 4 work; left as-is. The existing header points at `eventlog-plan.md#phase-4` and `eventlog-decisions.md`.

### Decisions made (with reasoning)

**Status header, not body rewrite.**
Per the plan. Preserves history (design alternatives, considered-and-rejected paths, why decisions were made) while making it impossible for a new contributor to take the plan as a current API description. The header also explicitly notes Phase 4's deviation so readers don't get whiplash.

**Did not add a "what changed from the plan" diff.**
Tempted to write a section for each plan-doc enumerating the planned-vs-shipped delta. Decided against: the canonical references (README + decisions doc) already do this with the right fidelity. Adding a third place would just create drift.

**Did not delete the plan docs.**
Considered. The decisions doc could absorb the "why" content. Kept the plans because the "design alternatives considered and rejected" sections in each have value that doesn't fit naturally in a what-shipped record.

### What did NOT land in Item 3

- **Acceptance-m3.md.** The plan called this out as deferred; same logic still applies. The README's M3 entry covers what shipped; an acceptance plan is mostly process discipline and waits for M4 start.
- **Renaming any of the plan docs** (e.g., to `*-archive.md`). The status header makes the same point without a path change that would break any external links.

### Verification

```bash
git diff docs/autonomous-plan.md docs/eventlog-plan.md docs/subagents-plan.md
# Diffs: header prepended on the first two; subagents-plan unchanged.
```

---

## Out of scope (still deferred)

Per the plan, these were explicitly NOT in this batch and remain candidates for M4 or later:

- Subagent cost rollup into the parent's `usage.Tracker` — separate plan when prioritized
- Postgres integration tests + CI matrix entry — separate plan
- `docs/acceptance-m3.md` — wait until M4 starts
- Real-LLM smoke runs — process change, not a code change; deserves its own attention
