# Gemini Tier-1 follow-up plan (#2, #3, #4)

PR #3 (`feat/gemini-customtools-default`, item #1) shipped. This plan covers the three remaining substantive Tier-1 items from `TODO.md` (`## Gemini tool-calling optimization` section), split across two PRs.

## Goal

Close the Gemini batching gap that the customtools variant alone didn't solve on open-ended search. Probe baseline today: customtools on search = 27 turns, mean batch 1.00. Target after this work: mean batch > 1.5 on search, ≥ 3.0 on multiread, and the model picks `grep` / `read_file` over `bash` for code investigation.

## Branching strategy

Two PRs off `main`, **not stacked**. Each lands independently of the other and of unmerged PR #3. Order: A first (small, may inform B's description style), then B.

| PR | Branch | Items | Effort |
|---|---|---|---|
| **A** | `feat/gemini-parallelism-mandate` | #2 + #4 | ~1 hour |
| **B** | `feat/read-many-files-tool` | #3 | ~half-day |

## PR A — parallelism mandate + tool description rewrites

### Item #2: parallelism mandate in the default agent instruction

**Files (verify before editing):**
- `agent/agent.go` — locate the default system instruction. `grep -n "default.*instruction\|defaultInstruction\|baseInstruction" agent/*.go` first; current shape may be a constant, an `Option` default, or assembled in `New()`.
- `agent/agent_test.go` — add a test asserting the mandate substring is present in the assembled instruction.

**Text to add** (verbatim from gemini-cli `packages/core/src/prompts/snippets.ts`, lightly adapted to fit our voice):

> Tools execute in parallel by default. Execute multiple independent tool calls in parallel when feasible — searching, reading files, independent shell commands, or editing different files. When investigating code, if you need to read multiple files or grep multiple directories, issue all the tool calls in a single response; do not execute them one by one.

### Item #4: tool description rewrites

**Files:**
- `tools/builtins.go` — modify the description strings on the `grep`, `read_file`, and `bash` `spec` entries.

**Changes:**

| Tool | Edit |
|---|---|
| `grep` | Append: *"PREFERRED over `bash grep` — honors the permission gate, output truncation, and per-tool caps."* |
| `read_file` | Append: *"PREFERRED over `bash cat` — honors output truncation and the permission gate."* |
| `bash` | Prepend: *"For code investigation (reading files, searching, listing directories), prefer the structured `read_file`, `grep`, `list_dir` tools. Use this tool for actions those tools cannot perform."* |

### Verification

1. `go test ./agent/ ./tools/`
2. All 5 runnable presubmits: `dev/ci/presubmits/{build,test-unit,vet,verify-go-format,verify-mod-tidy}`
3. Real-LLM check via probe:
   - `dev/parallel-probe --task=search` (customtools, default) — confirm mean batch exceeds 1.00
   - `dev/parallel-probe --provider=anthropic-vertex --task=search` — confirm Claude's mean batch holds or rises from 1.76 baseline
4. Existing `dev/smoke/02-vertex-basic.sh` still passes against the new default instruction

### Commit shape

Two commits, both on `feat/gemini-parallelism-mandate`:

1. `feat: parallelism mandate in default agent instruction`
2. `feat: tool descriptions demote bash for code investigation`

Split because they're conceptually distinct and individually revertable.

### CHANGELOG (under `[Unreleased] / Changed`)

> - **Default agent instruction now mandates parallel tool calls** for independent operations, lifted from `google-gemini/gemini-cli`'s prompt patterns. Helps Claude marginally (~1.76 → ~1.85 mean batch in probe runs) and helps Gemini-customtools meaningfully on search.
> - **`grep` and `read_file` tool descriptions explicitly disparage `bash` equivalents.** Probe data: customtools variant picked `bash` 15/27 times on search even with structured tools available. Tool descriptions matter independently of the model variant.

### Docs to update alongside (per memory: site docs alongside README/DESIGN)

- `docs/site/content/docs/` — search for any tool description quotes or instruction text. Likely none specific to grep/read_file descriptions, but the providers / configuration docs may quote the default instruction.
- `docs/DESIGN.md` — section on default instruction if any.

---

## PR B — `read_many_files` tool

### Item #3: new batch tool

**Files (new):**
- `tools/read_many_files.go` — implementation
- `tools/read_many_files_test.go` — tests

**Files (modified):**
- `tools/builtins.go` — extend `BuiltinTools` struct with `ReadManyFiles bool`, add to `Default()`, `BuiltinToolNames()`, `Disable()`, and append a `spec` in `Build()`.
- `tools/builtins_test.go` — extend `TestBuild_DefaultProducesNTools` count, add the name to `wantNames`, extend `TestBuiltinTools_Disable_KnownNames` cases.

### Tool schema

```go
type readManyFilesArgs struct {
    Paths   []string `json:"paths,omitempty" jsonschema_description:"Explicit list of file paths to read."`
    Pattern string   `json:"pattern,omitempty" jsonschema_description:"Optional basename glob; walked from Path."`
    Path    string   `json:"path,omitempty" jsonschema_description:"Root for Pattern walk; defaults to '.'."`
}

type readManyFilesResult struct {
    Files []readManyFile `json:"files"`
}

type readManyFile struct {
    Path      string `json:"path"`
    Content   string `json:"content"`
    Truncated bool   `json:"truncated"`
    Skipped   string `json:"skipped,omitempty"` // gate denial or read error
}
```

Validation: require at least one of `paths` or `pattern`; reject if both are empty.

### Behavior

- Honors permission gate per path (silent skip with `skipped: "path scope"`)
- Skips hidden / vendored dirs in pattern mode, same as `glob` / `grep`
- Per-file truncation via `capsFor(cfg, "read_many_files", defaultBytes, defaultLines)` — default 64KB / 1000 lines per file
- Total result truncation via the standard `Truncate` engine over the JSON-serialized result
- New entry in `DefaultConfig().ToolOutput.PerTool` mapping for `"read_many_files"`: `{MaxBytes: 256 * 1024, MaxLines: 5000}` (whole-response cap)

### Description (gemini-cli-flavored)

> Read multiple files in a single call. Pass `paths` (explicit list) and/or `pattern` (basename glob, walked from `path` root, defaults to '.'). PREFERRED over multiple parallel `read_file` calls when you already know the set of files you need. Useful when investigating a feature spread across several files, comparing implementations, or pulling context for an edit.

### Verification

1. Unit tests cover: explicit paths only, pattern only, both combined, gate denial, per-file truncation, hidden-dir skip, missing-file handling, both-empty error
2. All 5 runnable presubmits
3. Real-LLM check: extend `dev/parallel-probe` with a `--task=multiread-explicit` variant that asks the model to read a specific list of files and confirm it uses `read_many_files` in one call (vs N parallel `read_file` calls).

### Commit shape

One commit: `feat: read_many_files batch tool`

### CHANGELOG (under `[Unreleased] / Added`)

> - **`read_many_files` built-in tool** — reads multiple files in a single call via explicit paths and/or glob pattern. Honors the permission gate per path and per-file truncation via `capsFor`. Matches `gemini-cli`'s pattern; Gemini models prefer one tool call taking a list over N parallel calls for known-set reads.

### Docs to update alongside

- `docs/site/content/docs/configuration.md` — tool list, per-tool truncation table
- `docs/site/content/docs/tools.md` (if it exists, otherwise create) — describe the new tool
- `docs/DESIGN.md` — mention if the doc enumerates the tool surface

---

## Execution checklist (per PR)

1. `git switch main && git pull` (assume PR #3 may merge in between)
2. `git switch -c <branch> main`
3. Edits per the file list above
4. `go test ./...`
5. `dev/ci/presubmits/{build,test-unit,vet,verify-go-format,verify-mod-tidy}` — all five
6. (Optional) Real-LLM probe verification
7. Commit(s) with the shapes above
8. CHANGELOG + site docs updated alongside
9. `git push -u origin <branch>`
10. `gh pr create --base main --title "..." --body "..."`

## Open questions (resolve at start of each PR)

- **PR A:** where exactly does the default instruction live? Quick grep in `agent/` to confirm before scoping the edit; the doc above guesses but doesn't promise.
- **PR A:** does adding ~80 chars to every system prompt have any token-cost implication worth flagging in the CHANGELOG? Probably not, but mention if surfacing.
- **PR B:** should `read_many_files` accept absolute paths only, or also relative paths resolved against cwd? Existing `read_file` accepts both — match it.
- **PR B:** when both `paths` and `pattern` are supplied, do we union the results or error? Lean union (more forgiving for the model).

## What's deliberately not in this plan

- Item #5 (`GEMINI_SYSTEM_MD` override) — lower priority per TODO.md; ship after the migration if at all.
- `view_file_outline` — explicitly deprioritized per TODO.md.
- Open-ended search fix — separate investigation per TODO.md (likely needs workflow-shaped subagents, not better primitives).
- Cogo mirror work — tracked separately in `../cogo/docs/gemini-tooling-plan.md`.
