# M3 follow-ups — plan

## Recommendation summary

Three small, independent follow-ups that finish loose ends from M3 (autonomy + durable sessions + subagents). Together about a week of focused work; each item is shippable on its own.

1. **Glob / grep built-in tools** — execute the existing [`docs/tools-plan.md`](./tools-plan.md). The design has been locked in for months; the work is implementation + tests + docs. Most-leveraged single change because every tool-using agent benefits.
2. **`eventlog.WithSessionTree(parentID)` query option** — closes the audit-query gap from Phase 4. Today, finding "every event under run X" needs two queries (parent session + branch-prefix across sessions). One option, one SQL `LIKE` clause, one helper function on the Stream.
3. **Plan-doc refresh** — `docs/autonomous-plan.md`, `docs/eventlog-plan.md`, `docs/subagents-plan.md` all describe planning state, not shipped state. Add a short "Status" header at the top of each pointing to the canonical record (README M3 entry + `eventlog-decisions.md`). Preserves history, eliminates the "wait, did this ship?" confusion for new readers.

This is **a plan, not an implementation** — write the code when these items get prioritized. The point is to lock in design + scope so the work itself is mechanical.

## Context

After M3 shipped (autonomous driver + durable sessions + crash-resume + subagents), three small loose ends remain. None blocks anything; all three are straightforward enough that batching them into one milestone is unnecessary. They're listed here together because they share a "finish what M3 started" theme rather than an architectural connection.

The three items are independent. Order matters only insofar as #3 (plan-doc refresh) gets cheaper if done last — once #1 and #2 ship, the three plan docs gain another piece of "what shipped" each, so the refresh covers more ground.

---

## Item 1 — Glob / grep built-in tools

### Reference plan

The full design is in [`docs/tools-plan.md`](./tools-plan.md) (369 lines). Re-read before starting; the headline decisions:

- **Two tools, not one.** `glob(path, pattern)` returns paths; `grep(path, pattern)` returns line hits with file + line + text.
- **Stdlib only.** `filepath.WalkDir` + `filepath.Match` for glob; `regexp` (RE2) + `bufio.Scanner` for grep. No `bmatcuk/doublestar` — drops `**` recursive-glob support but keeps the dep graph clean.
- **Default-on** in `tools.Default()` and `BuiltinToolNames`. Same risk class as `read_file`/`list_dir` (passive read-only).
- **Output truncation** via `Truncate` + `cfg.ToolOutput.PerTool["glob"|"grep"]`. Defaults: glob 32KB / 500 lines; grep 256KB / 5000 lines.
- **Hidden-dir skip set**: `.git`, `.svn`, `.hg`, `node_modules`, `vendor`. Matches ripgrep's defaults.
- **No symlink follow.** `WalkDir`'s default `lstat` semantics — no loops, no path-scope escape.

### What's potentially stale in `docs/tools-plan.md`

The plan was written before M3 landed. Worth re-checking these as part of the implementation pass:

- The plan references "8 tools" after adding glob+grep. That count is still right (the M3 work didn't add to `tools.Default()`).
- The plan calls out that subagents weren't in `tools.Default()`. Still correct — `agent.WithSubagents` is the only path; subagents aren't a built-in.
- Per-tool `PerTool` config in `cfg.ToolOutput` — confirm the plan's defaults still match what's in `config/config.go` today.

### Files (per the existing plan)

- New: `tools/glob.go`, `tools/grep.go`, `tools/glob_test.go`, `tools/grep_test.go`
- Modified: `tools/builtins.go` (extend struct + Build switch), `tools/builtins_test.go`, `config/config.go` (default per-tool caps), `docs/DESIGN.md`, `README.md` (Features bullet enumeration), `docs/site/content/docs/library-api.md`, `docs/site/content/docs/configuration.md`

### Tests

Per `docs/tools-plan.md`:
- happy path (one tool per file, returns expected matches)
- gate denial (path outside scope → no result)
- truncation (output > cap → truncated with marker)
- hidden-file skip (`.git/` not walked)
- symlink non-follow (cycle doesn't hang)

### Estimated effort: 1-2 days.

---

## Item 2 — `eventlog.WithSessionTree(parentID)` QueryOption

### Why

Phase 4 shipped subagents in derived session rows (`<parent>:sub:<branch>`) — needed to dodge ADK's stale-session check. The trade-off, called out in `docs/eventlog-decisions.md`: audit queries that scope to `ForSession("parent-session")` no longer return subagent events. Workaround today is two queries:

```go
// Query 1: parent's events.
for entry := range stream.Since(0, ForSession(app, user, "task-1")) { ... }
// Query 2: subagent events across all sessions.
for entry := range stream.Since(0, WithBranchPrefix("research")) { ... }
```

This is correct but awkward — a real consumer auditing a run wants one query that returns the whole tree. `WithSessionTree(parentID)` closes the gap.

### Design

Add one `QueryOption`. Implementation: `LIKE` predicate on the overlay's `session_id` column matching exact parent + the derived `parent:sub:%` pattern.

```go
// WithSessionTree restricts results to the parent session ID and any
// derived sub-session IDs. A subagent's session is named
// "<parent>:sub:<branch>" by convention; this option's underlying SQL
// matches both the parent and any descendant sessions in one query.
//
// Mutually composable with the other QueryOptions; ForSession is
// redundant when WithSessionTree is set (the latter implies the
// session triple already).
func WithSessionTree(appName, userID, parentSessionID string) QueryOption
```

Inside `queryOpts`:
- `treeAppName`, `treeUserID`, `treeParentID string`
- mutually exclusive with `appName` / `userID` / `sessionID` set by `ForSession` (last-wins; document this)

In the SQL builder (`eventlog/sql.go` `queryRows`):
- if treeParentID set: add `WHERE app_name = ? AND user_id = ? AND (session_id = ? OR session_id LIKE ?)` with `parentID + ":sub:%"` as the like pattern
- order/limit unchanged

### Files

- `eventlog/eventlog.go` — new `WithSessionTree(app, user, parent string) QueryOption`; extend `queryOpts` with `treeAppName / treeUserID / treeParentID` fields
- `eventlog/sql.go` — extend `queryRows` to apply the predicate; document precedence vs. `ForSession`
- `eventlog/eventlog_test.go` — new tests (see below)
- `examples/with-subagent/main.go` — replace the "two queries" pattern with a single `WithSessionTree` call to demonstrate the cleaner shape
- `docs/site/content/docs/sessions.md` — add `WithSessionTree` to the filter table; update the Subagents cross-reference
- `docs/site/content/docs/library-api.md` — Subagents section: replace the two-query example with `WithSessionTree`
- `docs/eventlog-decisions.md` — Phase 4 record: note the gap is now closed

### Tests

- `TestWithSessionTree_ReturnsParentAndSubagent` — seed parent events + subagent events at branch=research; query with `WithSessionTree(parent)`; assert all events returned in seq order
- `TestWithSessionTree_IgnoresUnrelatedSessions` — two separate parents in the same DB; tree query for one returns only its descendants
- `TestWithSessionTree_DepthAgnostic` — parent + child + grandchild (`p`, `p:sub:a`, `p:sub:a:sub:b`); single tree query returns all three
- `TestWithSessionTree_ComposeWithAuthor` — combined with `WithAuthor("research")`, returns intersection

### Estimated effort: 1 day (mostly tests + docs; the SQL change is ~10 lines).

---

## Item 3 — Plan-doc refresh

### Why

Three plan docs in `docs/` describe planning state from before the work shipped:

- `docs/autonomous-plan.md` — describes the autonomous-run driver as planned. RunAutonomous + ResumeAutonomous + LifecycleTool + ask_user have all shipped.
- `docs/eventlog-plan.md` — describes Phases 1-4 of the eventlog work as planned. All four shipped. `docs/eventlog-decisions.md` is the canonical "what shipped + why" record.
- `docs/subagents-plan.md` — got a "Status: superseded" header during Phase 4 but the body still describes the agenttool-wrapped design we explicitly didn't ship.

A new contributor reading the codebase + plan docs gets the wrong picture. Two failure modes: they think a planned feature shipped that didn't (e.g., default research-safe subagent tool subset), or they implement against a design that's been replaced (e.g., agenttool-wrapping for subagents).

### Approach: status headers, not rewrites

Preserve each plan doc as a historical artifact (the *why* and the considered alternatives still have value). Prepend a clear status block at the top of each pointing to the canonical record:

```markdown
# <existing title>

## Status (YYYY-MM-DD): shipped — see <canonical doc>

This plan documented the design intent before <feature> shipped. The
actual implementation diverged in several places (see <decisions doc>
for the discovery-during-implementation record). The rest of this doc
is preserved as historical context — design alternatives, deferred
items, and rationale that stay useful even as the surface evolves.

**Canonical references:**
- README's M3 milestone entry
- docs/eventlog-decisions.md <relevant phase>
- <any other authoritative doc>

---

<rest of original plan unchanged>
```

### Per-doc instructions

**`docs/autonomous-plan.md`:** add status header. Reference: README M3 milestone entry, `docs/site/content/docs/autonomous.md` for the user-facing surface. Note any items in the original plan that didn't ship (none significant — autonomous shipped largely as designed).

**`docs/eventlog-plan.md`:** add status header noting Phases 1-4 all shipped. Reference: `docs/eventlog-decisions.md` for the implementation record. Acknowledge the Phase 4 pivot (derived session IDs vs. shared) in the header so readers don't get confused by the original plan's promise.

**`docs/subagents-plan.md`:** the existing status header points at `eventlog-plan.md#phase-4`. Strengthen by also pointing at `docs/eventlog-decisions.md` Phase 4 record (which has the full architectural pivot story). Optionally trim or strike the obviously-stale parts of the body (the `agenttool` wrapping section).

### Files

- `docs/autonomous-plan.md` — prepend status header
- `docs/eventlog-plan.md` — prepend status header
- `docs/subagents-plan.md` — strengthen existing status header

### Tests

None — pure docs change.

### Verification

- `git diff` shows only header additions
- Read each doc top-to-bottom to confirm a new contributor lands in the right place

### Estimated effort: half a day.

---

## What this plan deliberately does NOT do

- **Subagent cost rollup into the parent's `usage.Tracker`.** Listed as item 2 in the "next 5" suggestions; deferred because it's medium effort (2-3 days) and would benefit from being its own focused milestone. Worth a separate plan when prioritized.
- **Postgres integration tests.** Listed as item 3 in "next 5"; defer for the same reason — separate plan covering the test harness, CI matrix, and any GORM-dialect surprises.
- **Acceptance-m3.md.** The README's M3 milestone entry covers what shipped; an acceptance plan is mostly process discipline and can wait until M4 starts.
- **Real-LLM smoke runs.** Process change rather than a code change; deserves its own attention.

## Verification (when implementing)

```bash
cd /home/user/projects/core-agent
go test ./...
go vet ./...
go build ./...
for s in dev/ci/presubmits/*; do bash "$s"; done

# Item 1 smoke (with --provider=echo so no creds needed):
core-agent --provider=echo -p "use the glob tool to find every .go file"

# Item 2 smoke (drives the with-subagent example, which now uses
# WithSessionTree for the audit query):
go run ./examples/with-subagent

# Item 3 has no runtime verification — git diff + manual read.
```

## When the deferred items become active

- **Subagent cost rollup** — when a real consumer reports invoice-vs-tracker mismatch, or when we're about to bill someone for token usage that includes subagent calls.
- **Postgres integration tests** — when a consumer concretely deploys against Postgres, or when we want to make the multi-driver claim more credible than "library callers can swap it in."
- **Acceptance-m3.md / m4 acceptance plan** — at the start of M4 (so the discipline of "what does done look like" is set up before the work, not after).
