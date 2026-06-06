# syntax=docker/dockerfile:1.7
#
# Multi-stage distroless build for core-agent / core-agent-slim /
# core-agent-tui. Same Dockerfile produces all three variants via
# build args:
#
#   VARIANT     — "core-agent" (default) or "core-agent-tui".
#   BUILD_TAGS  — "" (default; full build) or "no_tui" (slim variant,
#                 ~5MB smaller; omits the embedded bubble-tea TUI for
#                 headless-only deployments).
#
# The Go toolchain version is passed in from `go.mod` via GO_VERSION,
# so the build image automatically tracks the project's Go version
# without a hardcoded duplicate that can drift.
#
# Final stage is gcr.io/distroless/static-debian12:nonroot. We can use
# the "static" variant (not "base") because all Cgo-flavored
# dependencies have been swapped for pure-Go equivalents — notably
# glebarez/sqlite (pure-Go) instead of mattn/go-sqlite3 (cgo). That
# leaves the binary fully self-contained and lets us run as the
# distroless nonroot user (UID 65532) without any glibc or musl
# runtime to worry about.
#
# Future: a `-debug` variant on a minimal OS (Ubuntu-minimal or
# debian-slim) for operators who need a shell + basic tools (curl,
# kubectl-style debugging) inside the pod. Defer until a consumer
# asks — most debugging happens via `kubectl exec` into a sidecar
# or by inspecting the eventlog from outside.

# ---- Builder stage ----
# Alpine base for a smaller (~150MB vs ~900MB) builder image — faster
# CI cold-cache pulls. CGO_ENABLED=0 below means we don't care about
# the builder's libc (musl vs glibc); we only ship the binary.
ARG GO_VERSION=1.26.3
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

WORKDIR /src

# Cache module downloads in a separate layer so iterative source
# changes don't re-fetch dependencies.
COPY go.mod go.sum ./
RUN go mod download

# Bring in the rest of the source.
COPY . .

# Build-time inputs. Defaults are appropriate for `docker build`
# without explicit args (produces a dev-flavored binary); the
# release-images.yml GitHub Action overrides them all.
ARG VARIANT=core-agent
ARG BUILD_TAGS=""
ARG VERSION=v0.0.0-dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# Cross-compile target. Set by `docker buildx build --platform`
# when building multi-arch images. Without buildx these default to
# the host's GOOS/GOARCH.
ARG TARGETOS
ARG TARGETARCH

# CGO_ENABLED=0 is mandatory — we want a fully-static binary that
# drops into distroless/static without any glibc/musl runtime. The
# project's deps are pure-Go (verified: glebarez/sqlite,
# modernc.org/sqlite, no `import "C"` in our source).
ENV CGO_ENABLED=0 \
    GOOS=${TARGETOS} \
    GOARCH=${TARGETARCH}

# -s -w strips DWARF + symbol table to shrink the binary by ~30%.
# -trimpath strips the absolute paths in stack traces (which would
# otherwise leak the build host's filesystem layout).
# The -X flags overwrite the internal/version package's defaults so
# `--version` reports the real release identity.
RUN go build \
    -tags "${BUILD_TAGS}" \
    -ldflags "-s -w \
        -X github.com/go-steer/core-agent/internal/version.Version=${VERSION} \
        -X github.com/go-steer/core-agent/internal/version.Commit=${COMMIT} \
        -X github.com/go-steer/core-agent/internal/version.Date=${BUILD_DATE}" \
    -trimpath \
    -o /out/binary \
    ./cmd/${VARIANT}

# ---- Final stage ----
# distroless/static-debian12 carries only the bits needed to run a
# static Go binary (CA certs, /etc/passwd with the nonroot user,
# tzdata). No shell, no package manager, no userland — minimal
# attack surface.
#
# :nonroot tag pre-creates UID 65532 + GID 65532 (user "nonroot")
# and sets USER, so the binary runs unprivileged out of the box.
FROM gcr.io/distroless/static-debian12:nonroot

# OCI image labels for registry tooling + GHCR metadata pages.
LABEL org.opencontainers.image.source="https://github.com/go-steer/core-agent" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.description="core-agent — foundational Go library + CLI for long-running, multi-turn, multi-agent LLM applications."

COPY --from=builder /out/binary /usr/local/bin/binary

WORKDIR /workspace

# nonroot is already set by the base image's :nonroot tag, but we
# redeclare for clarity and to insulate against any future base-image
# default change.
USER nonroot:nonroot

ENTRYPOINT ["/usr/local/bin/binary"]
