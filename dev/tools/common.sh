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
#   repo_root        — absolute path to the git working tree root
#   ensure_tool      — go install <pkg>@<ver> if the binary isn't on PATH
#   run_step         — run a command + print a "▸ name" header (for ci aggregator)

set -euo pipefail

repo_root() {
  git -C "$(dirname "${BASH_SOURCE[0]}")" rev-parse --show-toplevel
}

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

# extras_modules
#
# Prints the relative paths of the optional extras/ Go modules, one
# per line. Each is a separate Go module with its own go.mod so its
# heavy dep tree (e.g. Scion) stays out of consumers of the core
# library. Presubmits walk these modules when they are buildable in
# the current environment (see extras_buildable).
extras_modules() {
  cat <<'EOF'
extras/scion-remote-agent
EOF
}

# extras_buildable <module-relpath>
#
# Returns 0 (true) when the given extras module can be built in the
# current environment, 1 (false) otherwise. The check inspects the
# module's go.mod for "replace ... => <path>" directives and confirms
# each filesystem target exists. This lets CI skip extras whose
# source-of-truth dep (e.g. Scion) isn't checked out, while local
# devs with the dep on disk get the full build/test cycle.
extras_buildable() {
  local mod="$1"
  local gomod="$(repo_root)/$mod/go.mod"
  [[ -f "$gomod" ]] || return 1
  # Parse `replace <name> => <path>` lines and verify each path exists.
  local line target
  while IFS= read -r line; do
    target="$(echo "$line" | sed -E 's/^replace[[:space:]]+[^[:space:]]+[[:space:]]+=>[[:space:]]+([^[:space:]]+).*$/\1/')"
    # Skip module-version replaces (target starts with a version-like
    # token, e.g. v1.2.3) — those resolve via the module proxy, not
    # the filesystem.
    case "$target" in
      v[0-9]*) continue ;;
    esac
    # Absolute paths must exist; relative paths are interpreted from
    # the module dir.
    if [[ "$target" == /* ]]; then
      [[ -d "$target" ]] || return 1
    else
      [[ -d "$(repo_root)/$mod/$target" ]] || return 1
    fi
  done < <(grep -E '^replace[[:space:]]' "$gomod" || true)
}
