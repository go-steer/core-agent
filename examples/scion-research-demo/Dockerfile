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

# scion-research-demo — one container image that ships BOTH the
# research-orchestrator binary (which wires BackgroundAgentManager +
# scionremote.Spawner) and the stock scion-agent binary (used by the
# investigator template). Both templates point at this same image;
# the harness-config picks the binary via `args:`.
#
# Build via examples/scion-research-demo/build.sh — that wrapper
# stages the Scion source as a BuildKit named context (so the
# orchestrator module's `replace ... => /path/to/scion` resolves
# during `go build` inside the container).
#
# The base image (BASE_IMAGE) is Scion's, providing sciontool +
# tmux + the scion user — see extras/scion-agent/Dockerfile for the
# parent recipe this mirrors.

# syntax=docker/dockerfile:1.7

# ---------------------------------------------------------------------
# Builder — compiles both binaries as static linux/amd64.
# ---------------------------------------------------------------------
FROM golang:1.26-bookworm AS builder

WORKDIR /src

# Repo + Scion source come in as two contexts; copying them side by
# side preserves the `replace ../../../scion` path the orchestrator
# module's go.mod uses inside the container.
COPY . /src/core-agent
COPY --from=scion-src . /src/scion

ENV CGO_ENABLED=0 GOOS=linux GOARCH=amd64

# Build the stock scion-agent (used by the investigator template).
WORKDIR /src/core-agent
RUN go build -trimpath -ldflags="-s -w" -o /out/scion-agent ./extras/scion-agent

# Build the orchestrator binary from inside its module. Override
# the dev-only filesystem path the module's `replace` line uses by
# substituting it for the staged source at /src/scion.
WORKDIR /src/core-agent/extras/scion-remote-agent
RUN go mod edit -replace github.com/GoogleCloudPlatform/scion=/src/scion \
 && GOWORK=off go build -trimpath -ldflags="-s -w" -o /out/research-orchestrator ./cmd/research-orchestrator

# ---------------------------------------------------------------------
# Runtime — scion-base + both binaries.
# ---------------------------------------------------------------------
ARG BASE_IMAGE
FROM ${BASE_IMAGE}

USER root

RUN mkdir -p /workspace && chown scion:scion /workspace
WORKDIR /workspace

COPY --from=builder /out/scion-agent /usr/local/bin/scion-agent
COPY --from=builder /out/research-orchestrator /usr/local/bin/research-orchestrator

# Default command is unset — the harness-config supplies the args
# explicitly (`research-orchestrator` for the orchestrator template,
# `scion-agent` for the investigator template).
