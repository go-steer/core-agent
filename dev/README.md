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
