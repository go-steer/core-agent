# Option 1: lift public packages into `pkg/`

A focused write-up of the most conservative `pkg/` reorganization
option for `core-agent`. Other options (deeper reshape, breaking-
internal-restructure) are out of scope for this document.

## What moves

Every public, library-importable Go package currently at the
repo root moves to `pkg/`. The rest stays put.

```
core-agent/
├── pkg/                           ← NEW
│   ├── agent/         (was ./agent/)
│   ├── attach/        (was ./attach/)
│   ├── config/        (was ./config/)
│   ├── eventlog/      (was ./eventlog/)
│   ├── instruction/   (was ./instruction/)
│   ├── mcp/           (was ./mcp/)
│   ├── models/        (was ./models/)
│   ├── permissions/   (was ./permissions/)
│   ├── recording/     (was ./recording/)
│   ├── runner/        (was ./runner/)
│   ├── session/       (was ./session/)
│   ├── skills/        (was ./skills/)
│   ├── telemetry/     (was ./telemetry/)
│   ├── tools/         (was ./tools/)
│   └── usage/         (was ./usage/)
├── cmd/                           ← unchanged
│   ├── core-agent/
│   └── core-agent-tui/
├── internal/                      ← unchanged
├── examples/                      ← unchanged
├── extras/                        ← unchanged (scion-agent has its own go.mod)
├── dev/                           ← unchanged
├── docs/                          ← unchanged
└── SKILLS/                        ← unchanged
```

15 directories move. ~150 `.go` files relocate. The repo root
goes from 22 top-level directories to 8.

## What does NOT move

- **`cmd/`** — Go convention; binaries always live under `cmd/`.
- **`internal/`** — the language enforces its scope, not the
  filesystem layout. Keep it at root.
- **`examples/`** — buildable examples that demonstrate `pkg/...`
  usage. They keep their root position for discoverability (an
  `examples/` immediately under the repo root is the convention
  users expect from `gopkg.in/<x>/examples/...`).
- **`extras/scion-agent`** — has its own `go.mod`, lives in its
  own module. Not a sub-package; the `pkg/` move doesn't apply.
- **`dev/`** — CI presubmits, dev tooling, release scripts.
- **`docs/`** and **`SKILLS/`** — documentation and skill bundles,
  not Go code.

## Import path consequences

Every external import of a moved package changes prefix:

```
github.com/go-steer/core-agent/agent       ->  github.com/go-steer/core-agent/pkg/agent
github.com/go-steer/core-agent/config      ->  github.com/go-steer/core-agent/pkg/config
github.com/go-steer/core-agent/permissions ->  github.com/go-steer/core-agent/pkg/permissions
... (15 packages x N call sites)
```

Internal call sites (within `cmd/core-agent`, `cmd/core-agent-tui`,
`internal/...`, `examples/...`, `extras/scion-agent`, sibling
`pkg/...` packages) update in the same commit. External consumers
(downstream binaries, AX, cogo) update their go.mod or hit a
compile error.

Repo grep:
- 118 `.go` files reference `github.com/go-steer/core-agent/...`
- 130 files total (including markdown) reference the soon-to-be-
  `pkg/` packages

All 118 are in-repo and are updated in the move commit. The 130
includes ~12 docs files with code examples that also need
updating.

## Mechanics

The move is mechanical and `go fmt`-able:

1. `git mv <pkg>/ pkg/<pkg>/` for each of the 15 packages, in a
   single commit.
2. `gofmt -r 'github.com/go-steer/core-agent/<pkg> ->
   github.com/go-steer/core-agent/pkg/<pkg>' -w .` (or a
   handful of `sed -i` calls — `goimports` will sort).
3. `go mod tidy && go build ./... && go test ./...` — must be
   clean before the commit lands.
4. Update `dev/ci/presubmits/*` paths if any hardcode `./agent`,
   `./config`, etc. (most use `./...` and are unaffected).

The whole reshape is one PR — splitting it across multiple PRs
creates an awkward intermediate state where half the packages
have moved and imports are inconsistent. Single squash-merge is
cleaner.

## Breaking change posture

We **fold this into the v2.0 retag**, not a new v3.0.

The v2.0.0 tag was cut on 2026-05-29 but has not been announced
yet. The pre-announce window is the right time to make additional
breaking changes for free — consumers haven't bumped to v2.0 yet,
so moving the import paths now costs them zero. Once we announce,
the same change would cost a v3.0 cycle.

Concretely:

- Delete the local + remote `v2.0.0` tag.
- Land the `pkg/` reorg PR on `main` (alongside the docs relref
  fix that's already in flight as #72).
- Re-cut `v2.0.0` from the new HEAD.
- Recreate the GitHub Release page pointing at the new tag.
- Update the CHANGELOG `v2.0.0` section to add a "**Breaking:**
  all public packages moved under `pkg/`" line with the
  downstream-update snippet.

The CLI binaries are unaffected — `cmd/core-agent` and
`cmd/core-agent-tui` keep their install paths. Only library
consumers see the change, and we know all of them.

Downstream consumers (coordinate updates against the new v2.0.0):

- `AX` (distributed runtime that sits above `core-agent`) — see
  [[reference_ax_runtime]] in memory.
- `cogo` — the project `core-agent` was extracted from; depends
  on a subset of public packages.
- The site docs themselves (`docs/site/content/docs/...`) which
  show import strings in code samples (updated in the same PR).

## Why option 1 (this option)

The argument for the `pkg/` layout is **discoverability and
convention**:

- New users opening the repo immediately see what's a library
  (`pkg/`), what's a binary (`cmd/`), what's a sample
  (`examples/`), what's internal (`internal/`), and what's
  tooling/docs (`dev/`, `docs/`). The 22-directory root we have
  today buries that signal.
- It matches the layout of every other go-steer module and most
  of the Go ecosystem's larger projects (kubernetes, etcd, OTel,
  gRPC-go, prometheus, ko, …).
- `pkg/` is a stable boundary — the `pkg/X/` packages are the
  thing we promise to keep working; everything else is fair game
  to refactor without a release note.

Option 1 keeps the move strictly mechanical. Every file's
contents are byte-identical post-move except for import lines.
No package boundaries change, no public APIs move between
packages, no internal/public re-classification happens in the
same PR. That separation is the point: do the layout reshape
first, do the API-surface tightening as a separate v2.1+ effort
once the new layout has settled.

Specifically: the **v2.0 pre-announce window is the cheap moment**
to do this. We had to retag anyway to ship the docs relref fix
(PR #72) so the live site reflects the v2.0 layout — folding the
`pkg/` move into the same retag piggybacks on a release cycle we
were going to do anyway, instead of starting a v3.0 cycle later.

## Why NOT option 1 (the case against)

- **Pre-1.0 CLI ergonomics don't need this.** Most users invoke
  the binary; library callers are a smaller cohort. The
  discoverability win benefits them, not the bulk of users.
- **Even pre-announce, the retag isn't free.** The cogo and AX
  repos still need updates; the v2.0.0 tag has to be force-moved;
  the GitHub Release page has to be recreated. Cheaper than a
  v3.0 cycle, but not zero.
- **`pkg/` is a convention, not a rule.** The Go community is
  split — `pkg/` is common but not universal; stdlib doesn't use
  it. We can defend the flat root if asked.

## What this option does NOT decide

Out of scope for option 1, intentionally:

- Whether any currently-public package should be moved to
  `internal/`. (`recording/` and `instruction/` are candidates,
  but that decision should be its own discussion.)
- Whether `mcp/` and `tools/mcp/`-like subpackage relationships
  should be reshuffled.
- Whether the `models/` package (1 file) should be inlined into
  `agent/` or `config/`.
- Anything about the `extras/` module layout.

## Sequence (if approved)

Two PRs land back-to-back on `main`, then a v2.0.0 retag:

**PR A: docs relref fix** — already up as PR #72. Merge first;
unrelated to the reorg but on the same retag path.

**PR B: `pkg/` move** — one mechanical commit:

1. Branch `refactor/pkg-layout` off `main` after PR #72 merges.
2. `git mv` each of the 15 directories into `pkg/`.
3. Bulk-rewrite imports across the tree (one `gofmt -r` per
   moved package, or one targeted `sed`).
4. Update any `examples/*` go.mod files if any pin specific
   versions.
5. Update `docs/site/content/docs/library/api.md` and any other
   docs with import-path examples.
6. Run all presubmits clean.
7. Update the `v2.0.0` section of CHANGELOG to add a "**Breaking:**
   all public packages moved under `pkg/`" line with a downstream
   sed snippet — the v2.0.0 release notes haven't been announced
   yet, so this can land as part of the original entry rather than
   as a new heading.
8. Merge to `main`.

**Retag v2.0.0**:

1. `git tag -d v2.0.0` (local) and `git push origin :refs/tags/v2.0.0`
   (remote) — force-delete the stale tag.
2. `git tag v2.0.0 <new-HEAD-sha>` and `git push origin v2.0.0`.
3. Delete the GitHub Release page for v2.0.0 (cut on 2026-05-29)
   and recreate it pointing at the new tag, with the updated
   release notes.
4. Coordinate the AX and cogo go.mod updates against the new
   v2.0.0 tag — both repos pull at announce time, not before.

Estimated PR B size: 150-ish files touched (the 118 import-site
files + the 15 moved-directory file entries Git tracks + a
handful of docs). All changes are mechanical; review is "did
anything not move that should have."

## What to do if you read this and disagree

Two main objections worth raising:

- **Retagging is a smell.** A released tag is supposed to be
  immutable. Even pre-announce, this assumes nobody — no CI run,
  no curious early adopter, no go.sum cache — has pulled v2.0.0
  yet. If we want to play it strictly safe, ship the reorg as
  v3.0.0 later and live with the consumer churn.
- **Discoverability win doesn't pay for itself.** If the flat
  root genuinely doesn't bother anyone, keep the current layout
  and use the v2.0 announce window for shipping the library
  features (#94 memory backend, #88 hooks) instead of reshaping
  the disk layout.
