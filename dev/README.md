# dev/

Build- and test-tooling. Same scripts power both local development and
GitHub Actions CI, so a green local run is the same green run as remote.

## Quickstart

```bash
# Run every CI check locally (fast-fail order).
dev/tools/ci

# Run all checks even after a failure (collect every problem at once).
dev/tools/ci --keep-going

# Auto-fix formatting (gofmt + goimports).
dev/tools/fix-go-format
```

Missing tools (`golangci-lint`, `goimports`, `govulncheck`) auto-install
into `$GOBIN` (or `$(go env GOPATH)/bin`) on first use. No setup needed
beyond a Go toolchain.

## Layout

```
dev/
├── tools/                 # entry points users run locally
│   ├── ci                 # aggregator — runs every check below
│   ├── vet                # go vet ./...
│   ├── build              # go build ./...
│   ├── test-unit          # go test -race -coverprofile
│   ├── lint-go            # golangci-lint (auto-installs v2.12.1)
│   ├── verify-go-format   # gofmt -s + goimports check (read-only)
│   ├── fix-go-format      # gofmt -s -w + goimports -w (auto-fix)
│   ├── verify-mod-tidy    # `go mod tidy` clean check
│   ├── verify-vuln        # govulncheck ./...
│   ├── verify-go-toolchain # every named Go version agrees with go.mod
│   ├── verify-coretui-guards # capability guards vs the pinned core-tui
│   ├── examples-smoke     # the credential-free examples still run
│   ├── add-license-headers # bulk-applier for SPDX + copyright headers
│   ├── common.sh          # shared bash helpers (ensure_tool, run_step)
│   └── .golangci.yml      # linter config
└── ci/
    └── presubmits/        # thin delegators called by .github/workflows/ci.yml
        ├── vet            # → dev/tools/vet
        ├── build          # → dev/tools/build
        ├── test-unit      # → dev/tools/test-unit
        ├── lint-go        # → dev/tools/lint-go
        ├── verify-go-format
        ├── verify-mod-tidy
        ├── verify-go-toolchain
        ├── verify-coretui-guards
        ├── examples-smoke
        └── verify-vuln
```

## Adding a check

1. Drop a new script under `dev/tools/<name>` (executable, `set -euo pipefail`,
   sources `common.sh`).
2. Add it to the `STEPS` array in `dev/tools/ci`.
3. Add a one-line delegator under `dev/ci/presubmits/<name>` that
   `exec`s the tool script.
4. Reference the presubmit from `.github/workflows/ci.yml`.

That's it — the delegator pattern means the GitHub workflow never has
to know what the check actually does.

A check with real logic puts that logic in a Go program under `dev/`
rather than in the bash — `dev/lookout-pin-check` and
`dev/coretui-guard-check` are the two — and the `dev/tools/` script
becomes a `go run` wrapper over it. (`lookout-pin-check` predates the
convention and is still invoked straight from its own scheduled
workflow; `verify-coretui-guards` is the wrapped shape.) Go programs
there are part of the main module, so `go build ./...`, `go vet` and
`go test` cover them like anything else.

`examples-smoke` is the one exception to "the Go program lives under
`dev/`": its manifest is `examples/internal/smokeset`, and Go's
internal rule refuses an import of it from `dev/`, so the runner sits
at `examples/internal/smokerun` instead. The wrapper is in the usual
place and behaves like every other check.

## The examples smoke gate

`dev/tools/examples-smoke` (tool: `examples/internal/smokerun`) builds
and runs every example program marked runnable and fails on any that
does not exit 0. No arguments and no credentials: every runnable
example defaults to the scripted mock provider and opts into a real one
behind a flag the runner never passes, and provider keys are dropped
from the child environment so the answer doesn't depend on whose
machine it runs on. The whole set takes about five seconds.

Why it exists (#852): `go build ./...` proves the examples compile, and
nothing proved they still run. `examples/parallel-spawn` had been
exiting 1 for some time — every spawn refused after ad-hoc spawns
became opt-in — while its README advertised "Exits 0"; it was found by
hand during unrelated work (#476), not by a check.

The runnable set is a list, not a glob, because the examples split
three ways (runnable, needs a key or an argument or binds a listener,
covered by its own tests). The list lives in
`examples/internal/smokeset` with a test that requires every
`examples/*/main.go` to appear in one column or the other, so a new
example is either covered or excluded with a written reason — never
silently uncovered.

```bash
dev/tools/examples-smoke           # the gate
dev/tools/examples-smoke --print   # the disposition table
```

What it does **not** prove: that an example demonstrates what its
README claims. Only `parallel-spawn` and `compose-multi-session` assert
on their own results today; the rest exit non-zero only when an
operation errors. The runner also fails if a run leaves the worktree
dirty, since an example writing into the checkout is a defect the exit
code would not catch.

## The core-tui capability gate

`dev/tools/verify-coretui-guards` (tool: `dev/coretui-guard-check`)
enumerates the exported interfaces of `github.com/go-steer/core-tui/tui`
**at the version `go.mod` resolves to** and requires each of the repo's
two TUI hosts to account for every one of them. A capability is
accounted for when it is either:

- **guarded** — a `var _ coretui.X = (*T)(nil)` line in the host's guard
  file (`cmd/core-agent/coretui_guards.go` for the local `--tui` host,
  `internal/coretuiremote/guards.go` for attach mode); or
- **declined** — a `//coretui:declined X` directive in that file's
  trailing comment, immediately followed by prose naming `coretui.X` and
  saying why this host does not implement it.

Both halves of a decline are required. The directive is what the gate
counts; the bullet is the decision, and the gate refuses a directive
whose following prose names a different interface, so a rename cannot
leave one pointing at the other. The guard files are also the *only*
place either may appear — a guard next to its method is a hard error
that points back here.

Why it exists (#812): the guards make core-tui adding a **method** to an
interface we name a build failure. Nothing made core-tui adding a whole
new **interface** anything at all — core-tui feature-detects by type
assertion with no error and no log, so an unimplemented capability is
simply absent.

```bash
dev/tools/verify-coretui-guards           # the gate
dev/tools/verify-coretui-guards --print   # the interface x adapter matrix
```

Run `--print` when picking up a core-tui bump; it is the audit table,
and `UNACCOUNTED` in a column is what the gate fails on. What it does
**not** catch: a decline whose reason has quietly stopped being true
while the interface still exists (the [#811](https://github.com/go-steer/core-agent/pull/811)
case, where a capability became implementable). Proving that needs a
type-check of the host, not a comparison of two lists; the gate catches
the after-effect — a decline left in place once the guard lands — as a
contradiction.

## CI on PRs

Open a PR against `main` from a short-lived feature branch (e.g.
`feat/m3-subagents`, `fix/mcp-leak`). CI runs on the PR; merging is
gated on the four required status checks.

For this to actually gate merges, the repo's branch protection on
`main` must require these checks (settings → branches → main):

- `test`
- `lint`
- `go mod tidy is clean`
- `govulncheck`

Docs-only PRs (`**/*.md`) are handled by the companion `ci-docs.yml`
workflow, which emits the same four check names trivially-green so
branch protection is satisfied without running the full Go pipeline.

## Scheduled automation

Two weekly jobs keep generated or externally-owned values from drifting
silently. Both follow the same shape, and a third should copy it:

| Workflow | Schedule | Tool | Answers |
|----------|----------|------|---------|
| `pricing-regen.yml` | Mondays 09:07 UTC | `dev/regen-builtin-pricing` | have LiteLLM's rates or context windows moved? |
| `lookout-pin-check.yml` | Tuesdays 06:23 UTC | `dev/lookout-pin-check` | is the recipe's pin of `ghcr.io/go-steer/lookout` still upstream's current release? |

The shared conventions, each of which exists because of a specific
failure:

- **`--check` reports on stdout, not through the exit code.** Exactly
  `drift=true` or `drift=false`, and exit 0 either way. A non-zero exit
  always means the tool broke. Both are invoked via `go run`, which
  collapses every non-zero child status to 1 — so an exit-code
  convention would make a network hiccup indistinguishable from real
  drift, and the job would open a pull request on nothing.
- **Default mode writes the change.** The auto-PR carries a real diff,
  so it is reviewable and so path-filtered workflows (the recipe's kind
  e2e, for one) actually run on it.
- **Presubmits run on the rewritten tree before the PR opens.** A
  generator that produces something lint or tests reject should fail
  the workflow, not open a bad pull request.
- **The App token is minted unconditionally.** Most weeks there is no
  drift; gating the mint would leave the credentials unexercised until
  the one run that needs them.

Run either locally the same way CI does:

```
go run ./dev/regen-builtin-pricing --check
go run ./dev/lookout-pin-check --check
```

`dev/lookout-pin-check` additionally takes `--releases=<file.json>` to
replay a captured release list with no network at all, which is how its
own tests and any offline reproduction work, and `--resolved=<file.json>`
to write out what a live run resolved. The workflow uses the pair to
resolve upstream exactly once: the `--check` step records its answer and
the rewrite step replays it, so a release cut between the two steps
cannot leave the tree written to a tag the drift verdict never saw.

## License headers

Every source file carries the full Apache 2.0 header at the top,
attributed to Google LLC:

```
// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
```

(`#`-prefixed for shell, YAML, and Python.) The `goheader` linter
inside `dev/tools/lint-go` enforces this on every `.go` file — CI
fails if a new Go source is missing it. For shell, YAML, and Python
files, run `dev/tools/add-license-headers` after creating new ones;
the script is idempotent and normalizes any existing header (including
the older SPDX-shorthand variant) to the current canonical form.

## Pinned tool versions

| Tool          | Version    | Source                                                     |
|---------------|------------|------------------------------------------------------------|
| golangci-lint | v2.12.1    | `dev/tools/lint-go` (`GOLANGCI_LINT_VERSION` env var)      |
| goimports     | latest     | `dev/tools/fix-go-format`, `dev/tools/verify-go-format`    |
| govulncheck   | latest     | `dev/tools/verify-vuln`                                    |

Bump deliberately — new linter releases can introduce findings that
block CI. When you bump golangci-lint, run `dev/tools/lint-go` locally
first to fix anything new before pushing.
