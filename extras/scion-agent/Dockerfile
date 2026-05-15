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

# scion-agent — core-agent built for the Scion runtime.
#
# The base image (BASE_IMAGE) provides:
#   - sciontool (PID 1 init + status reporting CLI)
#   - tmux (so `scion message` can deliver follow-up input via send-keys)
#   - the scion user / UID
#
# Build (from the repo root, not from this directory):
#   docker build \
#     --build-arg BASE_IMAGE=scion-base:latest \
#     -t scion-core-agent \
#     -f extras/scion-agent/Dockerfile .
#
# The ENTRYPOINT is inherited from scion-base (`sciontool init --`) so
# scion-agent runs supervised, with sciontool wrapping it in a tmux
# session and forwarding signals.

# ---------------------------------------------------------------------
# Builder — produces a static linux/amd64 binary so the runtime image
# doesn't need a Go toolchain.
# ---------------------------------------------------------------------
FROM golang:1.26-bookworm AS builder

WORKDIR /src

# Layer caching: copy go.mod + go.sum first.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the repo and build.
COPY . .
ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64
RUN go build -trimpath -ldflags="-s -w" -o /out/scion-agent ./extras/scion-agent

# ---------------------------------------------------------------------
# Runtime — scion-base + the binary.
# ---------------------------------------------------------------------
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

USER root

# Workspace is mounted by scion at runtime; just ensure the mount
# point exists with the right ownership.
RUN mkdir -p /workspace && chown scion:scion /workspace
WORKDIR /workspace

COPY --from=builder /out/scion-agent /usr/local/bin/scion-agent

# Default command — scion's harness wraps this with `--input <task>`
# when a task is delivered (see harness-configs/scion/config.yaml).
CMD ["scion-agent"]
