#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Shared helpers for dev/tools/* scripts.
#
# Source this from each tool with:
#   . "$(dirname "$0")/common.sh"
#
# Provides:
#   repo_root          — absolute path to the git working tree root
#   resolve_toolchain  — the Go version go.mod pins us to (e.g. 1.26.6)
#   ensure_tool        — go install <pkg>@<ver> if the binary isn't on PATH
#   run_step           — run a command + print a "▸ name" header (for ci aggregator)
#
# Sourcing this also exports GOTOOLCHAIN, pinning every go command these
# scripts run to the toolchain go.mod names. See pin_toolchain below.

set -euo pipefail

repo_root() {
  git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel
}

# resolve_toolchain — print the Go toolchain version go.mod pins us to,
# without the `go` prefix (1.26.6).
#
# go.mod carries two versions and they mean different things:
#
#   go 1.26.4          — the language compatibility floor.
#   toolchain go1.26.6 — the toolchain we actually build with.
#
# Stdlib CVEs are fixed by toolchain patch releases, so the second line
# is the one that decides whether a shipped binary is vulnerable. Read
# the first one by mistake and CI stays green while the published image
# ships a stdlib that govulncheck already knows about (#736). This is
# the one place that distinction is encoded; everything else calls here.
#
# Falls back to the `go` directive, which is what go.mod looks like when
# the two would be identical (`go mod tidy` drops a toolchain line that
# adds nothing).
resolve_toolchain() {
  local gomod version
  gomod="$(repo_root)/go.mod"
  version=$(grep -oE '^toolchain go[0-9]+\.[0-9]+(\.[0-9]+)?$' "$gomod" \
            | sed -n '1p' | sed 's/^toolchain go//')
  if [[ -z "$version" ]]; then
    version=$(grep -oE '^go [0-9]+\.[0-9]+(\.[0-9]+)?$' "$gomod" \
              | sed -n '1p' | awk '{print $2}')
  fi
  if [[ -z "$version" ]]; then
    echo "resolve_toolchain: no toolchain or go directive in ${gomod}" >&2
    return 1
  fi
  echo "$version"
}

# pin_toolchain — export GOTOOLCHAIN=go<pinned> unless the caller has
# already chosen one.
#
# GOTOOLCHAIN=auto (the default) treats go.mod's toolchain directive as
# a *minimum*: a machine with a newer patch release keeps using it. That
# is the wrong direction for the checks these scripts run — source-mode
# govulncheck reports against the stdlib of the toolchain running it, so
# a developer or CI runner one patch ahead of what we ship scans a
# stdlib nobody will ever download and reports green either way. Naming
# the version exactly makes every local run and every CI job agree with
# the release artifacts.
#
# A caller who sets GOTOOLCHAIN explicitly (`local` in a hermetic or
# offline builder, most likely) is left alone — that's a deliberate
# choice, and silently overriding it would trade one surprise for
# another.
pin_toolchain() {
  if [[ -n "${GOTOOLCHAIN:-}" ]]; then
    return 0
  fi
  local version
  version=$(resolve_toolchain) || return 1
  export GOTOOLCHAIN="go${version}"
}

pin_toolchain

# ensure_tool <bin-name> <go-install-target>
#
# Checks for <bin-name> on PATH; if missing, installs the pinned version
# via `go install`. Honors GOBIN, falls back to $(go env GOPATH)/bin.
# After install, prepends GOBIN to PATH for the calling shell.
ensure_tool() {
  local name="$1"
  local target="$2"
  if command -v "$name" >/dev/null 2>&1; then
    return 0
  fi
  local gobin
  gobin="${GOBIN:-$(go env GOPATH)/bin}"
  # Already installed at GOBIN but not on PATH? Just expose it.
  if [[ -x "$gobin/$name" ]]; then
    export PATH="$gobin:$PATH"
    return 0
  fi
  echo "▸ $name not found — installing $target into $gobin" >&2
  GOBIN="$gobin" go install "$target"
  export PATH="$gobin:$PATH"
  if ! command -v "$name" >/dev/null 2>&1; then
    echo "ensure_tool: $name still missing after install" >&2
    return 1
  fi
}

# run_step <label> <command...>
#
# Runs the command and prints a tidy header. Used by the ci aggregator
# so each check has a visible boundary in the output. Exit code is
# propagated.
run_step() {
  local label="$1"; shift
  printf '\n\033[1m▸ %s\033[0m\n' "$label"
  "$@"
}
