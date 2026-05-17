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

# Copy the demo's template directories into Scion's user-templates
# dir so `scion create research-orchestrator <task>` resolves.
#
# Default destination: ~/.scion/templates. Override with TEMPLATES_DIR.
set -euo pipefail

TEMPLATES_DIR="${TEMPLATES_DIR:-$HOME/.scion/templates}"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
src_dir="$script_dir/templates"

if [[ ! -d "$src_dir" ]]; then
  echo "register-templates.sh: source templates dir not found at $src_dir" >&2
  exit 1
fi

mkdir -p "$TEMPLATES_DIR"

for name in research-orchestrator research-investigator; do
  src="$src_dir/$name"
  dst="$TEMPLATES_DIR/$name"
  if [[ ! -d "$src" ]]; then
    echo "register-templates.sh: missing $src" >&2
    exit 1
  fi
  # Replace any prior copy to keep this re-runnable during iteration.
  rm -rf "$dst"
  cp -R "$src" "$dst"
  echo "▸ registered $name → $dst"
done

echo
echo "register-templates.sh: done. Confirm with:"
echo "  scion templates list | grep research-"
