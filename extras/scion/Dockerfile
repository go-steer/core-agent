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
# Layers core-agent onto scion-base. Same shape as
# harnesses/antigravity/Dockerfile in Scion — BASE_IMAGE build-arg
# on top, binary drop into /usr/local/bin, CMD to the runtime.
#
# Two build modes:
#
#   1. Multi-stage from source (default; what this file does). Build
#      from the core-agent repo root so the builder stage sees the
#      source:
#
#        docker build \
#          --build-arg BASE_IMAGE=scion-base:latest \
#          -t scion-core-agent \
#          -f extras/scion/Dockerfile .
#
#   2. Pre-built binary from GHCR. Skip the builder stage entirely by
#      copying from the published image (see the commented "prebuilt"
#      block below).
#
# When this bundle moves to Scion upstream, mode 2 is the natural
# steady-state (no core-agent source in Scion's tree) — switch by
# replacing the builder stage with the commented block.

# Global build args — declared here so `docker build --build-arg
# BASE_IMAGE=...` reaches the final stage and so `--target builder`
# doesn't trip on an unset BASE_IMAGE at parse time. Default assumes
# scion-base is available locally; override for a pinned tag or
# registry-qualified image.
ARG BASE_IMAGE=scion-base:latest
ARG GO_VERSION=1.26.3
ARG BUILDPLATFORM=linux/amd64

# ---- Builder stage ----
# Alpine-based Go builder — matches the repo-root Dockerfile so a
# consumer building this locally gets identical binary bits to the
# GHCR release. Cross-compiles for the requested TARGETARCH via
# buildx's platform matrix.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Cache module downloads in their own layer so iterative source
# changes don't re-fetch the dep graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=v0.0.0-dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# CGO_ENABLED=0 keeps the binary fully static — pure-Go deps
# (glebarez/sqlite, modernc.org/sqlite) mean nothing needs a libc.
ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

RUN go build \
    -ldflags "-s -w \
        -X github.com/go-steer/core-agent/internal/version.Version=${VERSION} \
        -X github.com/go-steer/core-agent/internal/version.Commit=${COMMIT} \
        -X github.com/go-steer/core-agent/internal/version.Date=${BUILD_DATE}" \
    -trimpath \
    -o /out/core-agent \
    ./cmd/core-agent

# ---- Alternative builder (uncomment when moving upstream to Scion) ----
# FROM ghcr.io/go-steer/core-agent:latest AS builder
# # The GHCR release image places the binary at /usr/local/bin/binary
# # (see the repo-root Dockerfile). Rename here for the final COPY.
# USER root
# RUN mkdir -p /out && cp /usr/local/bin/binary /out/core-agent
# ------------------------------------------------------------------------

# ---- Final stage ----
FROM ${BASE_IMAGE}

USER root

# Standard config layout: .agents/ holds the project config (loaded
# from cwd or discovered upward); .core-agent/ holds the durable
# session db when --session-db is enabled. Both owned by scion so
# the runtime can write them.
RUN mkdir -p /home/scion/.agents /home/scion/.core-agent \
  && chown -R scion:scion /home/scion/.agents /home/scion/.core-agent

COPY --from=builder /out/core-agent /usr/local/bin/core-agent

# Stay as root at end-of-image — matches the Antigravity harness and
# lets Scion's runtime pick the container user (typically `scion`)
# via `docker run --user`. Setting USER scion here would fight that
# and force the harness to override.
#
# Scion's harness ignores CMD anyway (it substitutes the whole
# invocation from `command.base` in config.yaml) — CMD is a sensible
# default for `docker run` outside Scion, drops into core-agent's
# TUI when stdin is a terminal.
CMD ["core-agent"]
