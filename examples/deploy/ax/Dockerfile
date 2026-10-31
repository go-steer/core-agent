# syntax=docker/dockerfile:1.7
#
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
#
# core-agent packaged as an AX task image.
# AX: https://github.com/google/ax
# Agent Substrate: https://github.com/agent-substrate/substrate
#
# Build from the repo root:
#
#   docker build -f examples/deploy/ax/Dockerfile -t core-agent-ax .
#
# Three binaries end up in the image:
#   /usr/local/bin/ax-task-runner  AX's runner, PID 1 (AX always starts this path)
#   /usr/local/bin/ax-agent-entry  wrapper that runs one core-agent turn safely (see entrypoint/)
#   /usr/local/bin/core-agent
#
# The runner is built from AX source rather than copied from AX's default
# image, which is not publicly pullable (google/ax#424). That also keeps AX
# out of core-agent's go.mod.

# Tracks go.mod's toolchain directive; kept in sync by dev/tools/verify-go-toolchain.
ARG GO_VERSION=1.26.10
# AX's go.mod needs a newer toolchain than core-agent's, hence separate stages.
ARG AX_GO_VERSION=1.27.1

# ---- AX runner ----
FROM --platform=$BUILDPLATFORM golang:${AX_GO_VERSION}-alpine AS ax-runner
ARG TARGETOS
ARG TARGETARCH
# Pin to an AX commit; AX's API is v1alpha1 and changes without notice.
# Defaults to the fork ref (.ax-fork-ref) so serve mode's spec.http.port
# pass-through is present; override AX_REPO=https://github.com/google/ax.git
# and AX_REF=$(cat examples/deploy/ax/.ax-ref) for upstream one-shot only.
ARG AX_REPO=https://github.com/mastersingh24/ax.git
ARG AX_REF=e8bc98d87d9e3dcdee4aafa899bc802186d67dcb
RUN apk add --no-cache git
WORKDIR /src
RUN git init -q . \
 && git fetch -q --depth=1 "$AX_REPO" "$AX_REF" \
 && git checkout -q FETCH_HEAD \
 && test "$(git rev-parse HEAD)" = "$AX_REF"
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -o /out/ax-task-runner ./cmd/ax-task-runner

# ---- core-agent + wrapper ----
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=v0.0.0-dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
ENV CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH}
RUN go build \
    -ldflags "-s -w \
        -X github.com/go-steer/core-agent/v3/internal/version.Version=${VERSION} \
        -X github.com/go-steer/core-agent/v3/internal/version.Commit=${COMMIT} \
        -X github.com/go-steer/core-agent/v3/internal/version.Date=${BUILD_DATE}" \
    -trimpath \
    -o /out/core-agent \
    ./cmd/core-agent \
 && go build -trimpath -ldflags "-s -w" -o /out/ax-agent-entry ./examples/deploy/ax/entrypoint

# ---- Final stage ----
#
# Not distroless: AX's workspace setup shells out to git to clone a
# Workspace's repos. The runner also binds port 80 and writes its setup
# markers under /ax, so the image runs as root, as AX's default image does.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git curl \
 && rm -rf /var/lib/apt/lists/*

LABEL org.opencontainers.image.source="https://github.com/go-steer/core-agent" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.description="core-agent packaged as an AX task image."

COPY --from=ax-runner /out/ax-task-runner /usr/local/bin/ax-task-runner
COPY --from=builder /out/core-agent /out/ax-agent-entry /usr/local/bin/
# The recipe task-serve.yaml runs.
COPY --from=builder /src/examples/gke-platform-agent /opt/core-agent/recipes/gke-platform-agent
RUN mkdir -p /opt/core-agent/recipes/gke-platform-agent/.agents/plans

# Matches what AX sets; useful for a plain `docker run`.
ENTRYPOINT ["/usr/local/bin/ax-task-runner"]
