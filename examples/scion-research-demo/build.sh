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

# Build the scion-research-demo container image.
#
# Wires the Scion source as a BuildKit named context so the
# orchestrator module's local `replace ... => /path/to/scion` line
# resolves inside the build container. Requires Docker BuildKit
# (default in Docker 23+).
#
# Configuration via env vars:
#   SCION_SRC_DIR  — path to your Scion checkout (default ~/projects/scion)
#   BASE_IMAGE     — Scion's base container providing sciontool + tmux
#                    + the scion user (default scion-base:latest)
#   IMAGE_TAG      — tag for the built image (default scion-research-demo:latest)
set -euo pipefail

SCION_SRC_DIR="${SCION_SRC_DIR:-$HOME/projects/scion}"
BASE_IMAGE="${BASE_IMAGE:-scion-base:latest}"
IMAGE_TAG="${IMAGE_TAG:-scion-research-demo:latest}"

if [[ ! -d "$SCION_SRC_DIR" ]]; then
  echo "build.sh: SCION_SRC_DIR=$SCION_SRC_DIR does not exist" >&2
  echo "build.sh: set SCION_SRC_DIR to your Scion checkout" >&2
  exit 1
fi

# Resolve repo root so this script works regardless of where it's
# invoked from.
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(git -C "$script_dir" rev-parse --show-toplevel)"

DOCKER_BUILDKIT=1 docker build \
  --build-arg "BASE_IMAGE=$BASE_IMAGE" \
  --build-context "scion-src=$SCION_SRC_DIR" \
  -t "$IMAGE_TAG" \
  -f "$script_dir/Dockerfile" \
  "$repo_root"

echo
echo "build.sh: built $IMAGE_TAG"
echo "  scion source: $SCION_SRC_DIR"
echo "  base image:   $BASE_IMAGE"
