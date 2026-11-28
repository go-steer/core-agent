# Code map: a generated package table, doc gates and enforced import rules

**Status:** proposed, 2026-11-20. Nothing here is built. Tracking issue: to be
filed. Numbers were measured at `e3090556` and each one is reproducible with
the command or method named next to it.

## TL;DR

A reader of this repository cannot trust its pointers. The prose that explains
each package mostly exists. The paths that lead to it are broken, the two
hand-kept layout maps have drifted, and the import rules are written down in
six places with no check behind any of them.

The recommendation, in the order it should land:

1. **Gate what is already written** so it cannot drift again: every package
   has a package comment, every `docs/*.md` path cited in a Go comment
   resolves, and every top-level `pkg/` directory appears in the `AGENTS.md`
   layout block.
2. **Generate one package table** from `go list` and the existing package
   comments, commit it at `docs/PACKAGES.md`, and link it from
   [`DESIGN.md`](./DESIGN.md). No model writes any of it.
3. **Turn the six written import rules into a test**, and freeze today's 30
   cross-boundary edges in an allowlist so a new one needs a stated reason.
4. **Have a human sign off the parts a graph cannot produce**: a refreshed
   layout section in `DESIGN.md`, a verdict on each frozen edge, and six flow
   walkthroughs.

First-run cost is small: 55 dead paths to repoint (45 of them mechanically),
4 to 6 package comments to write or reattach, 2 misleading package comments to
rewrite, and one `AGENTS.md` edit.

**What this does not fix:** `pkg/agent` is 20,085 non-test lines in 47 files. A
map tells a reader where to look. It does not shorten what they find there.

Nothing below is decided except what existing repo rules already decide. Those
are under [Settled decisions](#settled-decisions-do-not-relitigate). Everything
else is under [Proposed decisions](#proposed-decisions-awaiting-the-maintainer)
and [Open questions](#open-questions-for-the-maintainer).

## Problem and evidence

The code is mostly written by agents and has to be readable by people. A
person opening the tree needs three things: what each package is for, how the
packages are allowed to depend on each other, and how one request moves
through them. Today each of the three is either missing or cannot be trusted.

| What a reader looks for | What they find | Measure |
|---|---|---|
| The design doc a comment cites | A path that does not exist | 55 of 63 distinct `docs/*.md` paths cited in non-test Go files are dead (192 of 218 occurrences) |
| What a package is for | No comment | 11 of 123 packages have none visible to `go list` |
| What the two biggest entry points are | A wrong description | `cmd/core-agent` and `pkg/agent` |
| The package layout | Two hand-kept maps, both out of date | `DESIGN.md` names 14 packages of 53; `AGENTS.md` omits 4 top-level `pkg/` directories |
| The dependency rules | Prose in six places | 0 of 6 rules have a check |

**Dead doc pointers.** `docs/README.md` moves a design doc to `docs/archive/`
when it ships. Nothing updates the Go comments that cite it. 45 of the 55 dead
paths (172 occurrences) are that case: the file exists under `docs/archive/`
with the same name. `docs/multi-session-design.md` alone is cited 21 times.
The other 10 paths (20 occurrences) need a look: a wrong-case
`docs/design.md`, three stale paths under `docs/site/`, and six docs that
exist nowhere under `docs/`, led by `docs/v0.3-plan.md` with 7 citations.
`AGENTS.md` has one dead path of its own,
`docs/compose-extraction-design.md`.

**Missing package comments.** `go list -f '{{.Doc}}' ./...` is empty for 11
packages. They are three different defects:

- 2 have a `// Package` comment separated from the `package` clause by a blank
  line, so Go drops it: `pkg/taskclass`, `pkg/tools/agentic`.
- 2 have no comment at all: `pkg/agent/autonomous`, `pkg/agent/background`.
  Both are `promised`.
- 7 contain only `_test.go` files, which `go list` does not read for a package
  comment: `internal/selfrecipe`, `dev/uat/selfdev-soak` and five directories
  under `examples/`. Five of the seven have a comment in the test file.

**Misleading package comments.** `cmd/core-agent` describes itself as "a thin
CLI wrapper" that "drops into a stdin REPL". It is 36 non-test files and
11,574 lines, imports 45 internal packages, and `AGENTS.md` says the TUI is
its default. `pkg/agent` says it "wraps the Google ADK runner with sensible
defaults". It is the 20,085-line hub that 29 packages import.

**`DESIGN.md` layout drift.** The diagram under "Package layout + dependency
rules" draws `agent` above ten packages. `pkg/agent` imports five of them
(`config`, `permissions`, `models`, `usage`, `tools`) and none of the other
five (`instruction`, `mcp`, `skills`, `telemetry`, `runner`). It still names
`session`, now a deprecated alias of `pkg/transcript`. The diagram may have
been meant as a concept sketch; the heading says dependency rules, so a reader
takes it as imports.

**`AGENTS.md` layout drift.** The layout block omits `approval`, `evals`,
`memory` and `statestore`. It is loaded into every agent's system prompt, so
every agent starts with a map that is missing four directories.

**Written import rules, none checked.** The six rules:

| Rule | Written in | Holds today |
|---|---|---|
| `permissions` imports no ADK package | `DESIGN.md` | Direct imports: yes. Transitively: **no**, 6 ADK packages arrive through `pkg/config` |
| `mcp` and `skills` do not import each other | `DESIGN.md` | Yes |
| `permissions` does not import `pkg/models` | `pkg/permissions/auto.go`, `archive/auto-mode-design.md` | Yes |
| `attach` does not import `usage` | `pkg/attach/metrics.go` | Yes |
| `pkg/memory` stays a leaf | `pkg/memory/memtools/memtools.go` | Yes, 0 internal imports |
| `pkg/digest` does not import `pkg/agent` | `pkg/digest/digest.go` | Yes |

The first row matters. `DESIGN.md` gives the reason for the rule as keeping
the gate "usable in non-ADK contexts". That reason needs the transitive
reading, and the transitive reading is already broken. Nobody was told.

**Cross-boundary edges nobody has classified.** 10 edges run from `pkg/` into
`internal/`, and 20 run from a `promised` package to an `unsupported` one
(dispositions from `dev/api-surface.txt`). All 30 are legal Go. Nothing
records which were intended.

**This has happened before.** #391, "Refresh stale identity docs: AGENTS.md,
DESIGN.md, north-star.md", closed on 2026-07-25. A one-off refresh held for
less than four months. The fix has to be a check.

## What exists today (facts this design builds on)

- **The graph is free.** `go list ./...` returns all 123 packages and 382
  internal import edges in under 2 seconds with no extra tooling. 37 packages
  are `main`. By tree: `pkg/` 53, `internal/` 26, `examples/` 25, `dev/` 17,
  `cmd/` 2.
- **`dev/api-surface.txt` already classifies `pkg/`**: 27 `promised`, 24
  `unsupported`, enforced by `TestEveryImportablePackageIsADecision`.
- **One import-boundary gate exists**, `dev/tools/verify-slim-deps`, for the
  slim embed only.
- **`internal/testutil` already hosts repo-wide structural tests** that read
  `dev/` data files (`surface_test.go`, `deprecation_test.go`,
  `complexity_test.go`).
- **`docs-lint` runs on every PR, docs-only ones included**
  (`.github/workflows/docs-lint.yml`). `ci.yml` skips `**/*.md`-only PRs.
- **`docs-lint` check 5 requires every `docs/*.md` to be indexed in
  `docs/README.md`.** A generated `docs/PACKAGES.md` has to be registered
  there.
- **`AGENTS.md` is 18,284 characters, about 4,600 tokens**, paid by every
  agent on every run. Its layout block is about 2,500 characters (630 tokens).

## Settled decisions (do not relitigate)

Each of these follows from a rule the repo already has. This design adds
nothing to them.

1. **`docs/DESIGN.md` is the architectural entry point.** Source:
   [`docs/README.md`](./README.md). The code map hangs off its layout section.
2. **`dev/api-surface.txt` is the classification of `pkg/` packages.** Source:
   [`compatibility-policy.md`](./compatibility-policy.md), settled decisions 1
   and 2. The table reads it and introduces no second classification.
3. **`unsupported` packages do not move under `internal/` before v4.0.**
   Source: `compatibility-policy.md`, out of scope. The `promised` →
   `unsupported` edges are therefore classified in place, not removed by
   relocation.
4. **A check is a script in `dev/tools/`, a one-line delegator in
   `dev/ci/presubmits/`, an entry in `dev/tools/ci`, and a CI step. Real logic
   goes in a Go program under `dev/`.** Source:
   [`dev/README.md`](../dev/README.md), "Adding a check";
   `TestEveryPresubmitHasACIStep` enforces the CI step.
5. **`AGENTS.md` holds rules and pointers and stays short.** Source: its own
   preamble. The package table does not go there.
6. **A shipped design doc moves to `docs/archive/`.** Source: `docs/README.md`,
   registry and status policy. Any gate on doc paths has to work with that
   move, and this doc will make the move itself.
7. **Internal docs need no `CHANGELOG.md` bullet; a change to a rule in
   `AGENTS.md` does.** Source: `AGENTS.md`, "Which doc changes are
   user-visible".
8. **Any PR that touches Go code goes through the adversarial review gate**,
   comment-only changes included. Source: `AGENTS.md`, "Adversarial review
   gate before every PR".

## Proposed decisions (awaiting the maintainer)

None of these is decided. Each has a recommended answer and the reason for it.

1. **Commit the table at package level; print the volatile columns on
   demand.** A committed file is what a reader and a reviewer can see in a
   diff. Package, kind, surface and synopsis change only when a package is
   added, removed or re-described. Fan-in, depth and churn change on most PRs
   and would make the file a merge-conflict source.
2. **The synopsis is the first sentence of the package comment, copied
   verbatim.** The comment is what `go doc` and pkg.go.dev already show. A
   second description in another file is a second thing to keep true.
3. **Gates fail by default; `--print` shows the data; `--write` regenerates.**
   This matches `verify-coretui-guards` and `examples-smoke`. It avoids a
   `--check` flag, which `dev/README.md` reserves for scheduled jobs that
   report `drift=true|false` and exit 0.
4. **Dead doc paths are fixed by repointing the comment to the real path.** A
   path that opens is worth more to a reader than a resolver that knows to
   look in the archive. The cost is recorded as Amendment 1.
5. **The doc-reference gate runs with `docs-lint`, not with the Go pipeline.**
   The PR that archives a doc is docs-only, and `ci.yml` skips it. A gate in
   the Go pipeline would go red on the next unrelated Go PR.
6. **The package-comment gate covers all 123 packages**, reading `_test.go`
   files when a package has nothing else. A directory a reader can open should
   say what it is.
7. **Import rules are a Go test over `go list` in `internal/testutil`, not
   `depguard`.** The `promised` → `unsupported` rule has to read
   `dev/api-surface.txt`, which `depguard` cannot. This is Amendment 3.
8. **Each import rule states whether it means direct or transitive imports.**
   The `permissions` / ADK rule is encoded as direct, which passes, and the
   transitive breach is filed as debt. Encoding it as transitive makes the
   first run red.
9. **The 30 cross-boundary edges are frozen in an allowlist that can only
   shrink without comment.** A new edge adds a line with a reason. This is the
   same shape as the `funlen` / `gocognit` ratchet.
10. **The `AGENTS.md` layout block stays hand-kept, with a test that every
    top-level `pkg/` directory is named.** About 630 tokens buys every agent a
    map in its prompt. The full table would add roughly 18,000 to 20,000
    characters (4,500 to 5,000 tokens) and more than double the file.
11. **Human-owned sections carry a sign-off line**: reviewer, date, and the
    commit they were checked against. An agent may draft them. Without the
    line they are drafts.

## What gets generated

### Package table {#CM-1}

One Go program under `dev/` (working name `dev/code-map`), wrapped by
`dev/tools/verify-code-map`, produces `docs/PACKAGES.md`.

| | Columns | Where |
|---|---|---|
| Committed | package path, kind (`library` / `main` / `test-only`), surface (`promised` / `unsupported` / blank outside `pkg/`), synopsis | `docs/PACKAGES.md`, linked from the `DESIGN.md` layout section, indexed in `docs/README.md` |
| Printed only | direct imports, fan-in, fan-out, depth, 30-day commit count, the import rules that apply and whether they hold | `dev/tools/verify-code-map --print [package]` |

The gate regenerates the table in memory and fails on any difference from the
committed file. A PR that adds a package or edits a package comment
regenerates the file with `--write`.

No model output is committed. The file carries a "generated, do not edit"
header naming the command.

## What CI enforces

| Gate | Fails today on | First-run cost |
|---|---|---|
| Package comment CM-2 | 11 packages | Reattach 2, write 2, decide 7 test-only (5 have text to promote), rewrite 2 misleading |
| Doc references CM-3 | 55 paths, 192 occurrences, plus 1 in `AGENTS.md` | Repoint 45 paths mechanically; examine 10 |
| Package table freshness CM-1 | New | None |
| `AGENTS.md` layout CM-4 | 4 directories | One edit |
| Import rules CM-5 | 0 of 6 as direct imports; 1 of 6 as transitive | Encode 6 rules |
| Cross-boundary ratchet CM-6 | New; 10 + 20 edges frozen | Generate the allowlist |

### Package comment gate {#CM-2}

Every package has a non-empty package comment attached to a `package` clause.
The gate cannot judge whether a comment is true. The two misleading comments
are rewritten by hand in the same PR, and a wrong comment stays a review
matter.

### Doc reference gate {#CM-3}

Every `docs/*.md` path in a non-test Go file and in `AGENTS.md` names a file
that exists. Scope is deliberately narrow: paths in Go comments and in the one
prompt-loaded file. Markdown-to-markdown links under `docs/` were not measured
and are not covered.

### `AGENTS.md` layout gate {#CM-4}

Every top-level directory under `pkg/` is named in the layout block. Today
that is 32 directories with 4 missing. Sub-packages are not required; the
block stays a summary.

### Import rule gate {#CM-5}

A table-driven test: package, forbidden import, direct or transitive, and the
doc or comment that states the rule. A failure prints the rule's source so the
reader of a red build learns why the rule exists.

### Cross-boundary edge ratchet {#CM-6}

A data file under `dev/` lists the 10 `pkg/` → `internal/` edges and the 20
`promised` → `unsupported` edges. The test fails on an edge that is not
listed, and on a listed edge that no longer exists, so the list cannot go
stale in either direction.

## What humans write and sign off

### `DESIGN.md` layout refresh {#CM-7}

Replace the diagram in "Package layout + dependency rules" with one that
matches the import graph at the level of the main packages, link
`docs/PACKAGES.md` for the rest, and collect the six import rules into that
section with a pointer to the test. The diagram is drawn by hand on purpose: a
generated picture of 382 edges explains nothing.

### Frozen edge classification {#CM-8}

Each of the 30 edges in the CM-6 allowlist gets a verdict on its line:
`intended`, with one sentence of why, or `debt`, with an issue number. This is
design work. The 10 `pkg/` → `internal/` edges come first; they are fewer and
six packages account for all of them (`pkg/compose` 3, `pkg/attach` 2,
`pkg/evals` 2, and one each from `pkg/agent`, `pkg/evals/judge` and
`pkg/tools/peer`).

### Flow walkthroughs {#CM-9}

The import graph shows nouns. It cannot show that `pkg/permissions` never
imports `pkg/approver`, which implements its `Approver` interface and is wired
in by `cmd/core-agent`. Six short walkthroughs carry that:

1. one turn, end to end;
2. a permission decision;
3. session persistence and resume;
4. remote attach;
5. subagent spawn;
6. startup wiring through `cmd/core-agent` and `pkg/compose`.

Each step names a symbol (`permissions.FromConfig`, not a file and line).
A check resolves every named symbol with `go doc`, so a rename breaks the
build instead of the document.

## Amendments to existing docs (accept or reject each)

These are the places where the recommendation touches something already
written. None is assumed.

1. **`docs/README.md`, registry policy.** The policy says to flip the status
   and `git mv` the doc. With CM-3 in place, the same PR also has to
   repoint every Go comment that cites it, which turns a docs-only PR into a
   Go PR with full CI and the adversarial review gate. Accepting CM-3 means
   adding that step to the policy text. The alternative is in open question 5.
2. **`docs/compatibility-policy.md`.** `promised` packages expose
   `unsupported` types in their API; `config.Config` has a field of type
   `hooks.Config`, and `pkg/hooks` is `unsupported`. The policy is silent on
   what the SemVer promise means for such a field. A policy line is needed
   either way. This design does not write it (open question 3).
3. **`dev/tools/.golangci.yml`, header comment.** It states an intent to add
   `depguard` "once the baseline is clean". Proposed decision 7 uses a Go test
   instead. Accepting it means editing that comment so the two do not
   disagree.
4. **`docs/DESIGN.md`, non-goals.** "No micro-bench infrastructure, no eval
   harness" contradicts `pkg/evals` and `pkg/evals/judge`, both `promised`.
   This is outside the layout section that CM-7 refreshes. Either widen
   CM-7 to cover it or file it separately (open question 8).

## Out of scope

- **A root `ARCHITECTURE.md`.** It would compete with `docs/DESIGN.md`, which
  settled decision 1 names as the entry point.
- **Committed LLM-written package summaries.** 112 of 123 packages already
  have a comment. A model-written copy is a second text to keep in sync, with
  nothing checking it against the first.
- **The package table inside `AGENTS.md`.** Roughly 4,500 to 5,000 tokens on
  every agent run, against settled decision 5.
- **A committed map of exported symbols.** `go doc` serves it on demand and it
  changes on most PRs.
- **`doc.go` in every package.** The existing comments live in ordinary files.
  Moving them is churn across more than a hundred packages with no gain for a
  reader.
- **Third-party graph tools.** `go list` is type-checked, already installed
  and takes under 2 seconds. A syntactic parser adds a dependency and removes
  precision.
- **A blanket `deadcode` gate.** The assessment counted 433 functions
  unreachable from any `main` without tests and 37 unreachable even with them
  (not re-run for this doc). Most of the first group is library API that no
  in-repo binary calls, so "delete what `deadcode` reports" would delete
  promised surface. The 37 are a separate triage.
- **Git activity in a committed file.** It is stale the next day. It stays in
  `--print`.
- **`CODEOWNERS`.** The repo has one maintainer.
- **A generated concurrency inventory.** The walkthroughs come first; revisit
  if they prove insufficient.
- **Splitting `pkg/agent` or `cmd/core-agent`.** This design makes the hubs
  findable. Making them smaller is a refactor with its own design doc.
- **Moving `unsupported` packages or removing cross-boundary edges.** CM-8
  classifies them. Acting on a `debt` verdict is separate work.
- **Checking markdown-to-markdown links.** Not measured; `docs-lint` says of
  itself that it is not a link checker.

## Phasing

Each row is one PR that can merge on its own. Rows 2 to 6 fix what they gate
in the same PR, because a gate cannot merge red. Any row that adds a
presubmit edits `.github/workflows/`.

| # | PR | Covers | Touches Go | Depends on |
|---|---|---|---|---|
| 1 | This design doc | | No | |
| 2 | Repoint the dead doc paths; add the doc-reference gate | CM-3 | Comments only | 1 |
| 3 | Reattach, write and rewrite package comments; add the gate | CM-2 | Comments, one Go program | 1 |
| 4 | Fix the `AGENTS.md` layout block and its dead path; add the test | CM-4 | One test | 1 |
| 5 | Generator, `docs/PACKAGES.md`, link from `DESIGN.md`, `docs/README.md` entry | CM-1 | One Go program | 3, for complete synopses |
| 6 | Import-rule test and the frozen-edge allowlist | CM-5, CM-6 | Tests, one data file | 1 |
| 7 | `DESIGN.md` layout refresh, signed off | CM-7 | No | 5, 6 |
| 8 | Edge verdicts, `pkg/` → `internal/` first | CM-8 | Data file | 6 |
| 9–14 | One walkthrough each, plus the symbol check in the first | CM-9 | One check | 5 |

Rows 2 to 4 are the cheapest and remove the most reader-facing damage. If the
work stops after row 4, the repo is still better off. Row 4 needs a
`#### Documentation` bullet only if it adds a rule line to `AGENTS.md`;
fixing the layout block alone does not.

## Risks

- **Gates add friction to every PR that adds a package.** One comment, one
  table regeneration, one `AGENTS.md` line for a new top-level directory.
- **A gate proves presence, not truth.** A package comment can be attached,
  non-empty and wrong. The table then publishes the wrong sentence.
- **Walkthroughs rot by meaning.** The symbol check catches a rename. It does
  not catch a step that still resolves and no longer happens.
- **No gate here checks that a design doc describes what the binary does.**
  #1593 is the current case: a shipped design names two runner plugins that
  no binary registers, and every symbol it cites resolves. Only a walkthrough
  traced from `main` by a person would have caught it.
- **Sign-off with one maintainer is one person's reading time.** Eight
  human-owned documents (the layout section, the edge list and six
  walkthroughs) are the real cost of this design, more than the tooling.

## Open questions for the maintainer

1. **Is the table committed, or only printed?** Recommended: committed at
   package level (proposed decision 1).
2. **Does the `AGENTS.md` layout block stay hand-kept with a test, or become a
   pointer to `docs/PACKAGES.md`?** Recommended: keep it (proposed decision
   10).
3. **Do `promised` packages that expose `unsupported` types need a policy
   line?** This amends `compatibility-policy.md` (Amendment 2). Only
   `config.Config.Hooks` was confirmed by reading; the extent is unmeasured.
4. **Does "`permissions` imports no ADK package" mean direct or transitive?**
   Direct holds. Transitive fails today through `pkg/config`. Recommended:
   encode direct now and file the transitive breach (proposed decision 8).
5. **Repoint comments on every archive move, or let the gate accept
   `docs/X.md` when `docs/archive/X.md` exists?** Repointing keeps paths
   openable and costs a Go PR per archived doc. The resolver costs nothing and
   leaves 45 paths that do not open. Recommended: repoint (proposed decision
   4, Amendment 1).
6. **Is the `DESIGN.md` diagram an import diagram or a concept sketch?** The
   answer decides whether CM-7 redraws it or relabels it.
7. **Do the 7 test-only packages need a package comment?** Recommended: yes,
   read from the test file (proposed decision 6). The alternative is to exempt
   them and list them in the table as `test-only` with no synopsis.
8. **Does CM-7 also correct the stale non-goal about eval harnesses**
   (Amendment 4), or is that a separate fix?
9. **Where do the walkthroughs live, and are these the right six?**
   Recommended: `docs/flows/<name>.md`, linked from `DESIGN.md`, which is
   already 57,708 characters.
10. **What counts as sign-off?** Recommended: a line in the document naming
    the reviewer, the date and the commit (proposed decision 11).
