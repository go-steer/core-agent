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
# The self-development soak's image (#1213, docs/selfdev-soak-design.md,
# prerequisite P3). Soak-only: the release image at the repo root is
# distroless and stays that way. See README.md beside this file.
#
# The agent in this image does development work, so it needs what a
# developer needs: a POSIX shell and coreutils for the bash tool, git,
# the Go toolchain the repo pins, and CA certificates. It does NOT get
# gh: the dispatcher pushes and opens PRs (design decision 7), so no
# GitHub credential or client belongs in this pod.
#
# The core-agent binary is built from a PINNED upstream ref (decision
# 11): a go-steer/core-agent release tag or a full commit SHA, passed as
# CORE_AGENT_REF. There is no default, and a branch name is refused,
# because a floating ref changes the agent being measured on every
# merge. The source is cloned from upstream inside the build, never
# copied from the build context, so neither the mirror's main nor a
# local checkout can end up in the image. Build with build-image.sh:
#
#   dev/uat/selfdev-soak/build-image.sh --ref <release tag or full SHA>
#
# Also deliberately absent: a `# syntax=` line. That directive pulls a
# frontend image by floating tag, which is the one unpinned image this
# file would otherwise depend on; the builder's built-in frontend
# handles everything below.

# The Go toolchain is the one dev/tools/verify-go-toolchain resolves
# from go.mod (it reads this ARG), and the base image below must carry
# exactly that version. The FROM lines name the version in their tag
# AND pin the multi-arch index by digest, so the digest decides what is
# pulled; the RUN check in the builder stage fails the build if the
# digest's Go is not GO_VERSION. When go.mod's toolchain moves, update
# all three together and refresh the digest with:
#
#   docker buildx imagetools inspect golang:<version>-bookworm
#
# dev/uat/selfdev-soak's Go test fails while they disagree.
ARG GO_VERSION=1.26.6

# ---- Builder stage: core-agent at the pinned ref ----
FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS builder

ARG GO_VERSION
# No default, on purpose: see the header. The RUN below is the guard.
ARG CORE_AGENT_REF
# The commit CORE_AGENT_REF resolved to when build-image.sh looked it up
# (git ls-remote). Also required: it becomes the image's
# org.opencontainers.image.revision label, and the build fails if the
# checkout is not that commit. go-steer/core-agent has no tag
# protection, so a tag that moved between lookup and build is caught
# here instead of producing an image whose label names another commit.
ARG CORE_AGENT_COMMIT

# The base image sets GOTOOLCHAIN=local, so the go command never
# downloads a different toolchain; a ref whose go.mod needs a newer Go
# than this image fails here, loudly, rather than building with
# something unpinned.
ENV CGO_ENABLED=0

# bash, for [[ =~ ]]: its ^ and $ anchor the whole string, where a
# line-oriented `grep -x` would accept a multi-line value whose one line
# looks like a SHA.
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]

# A tag is fetched as refs/tags/<ref>, so a branch can never answer for
# it. A SHA is fetched by object id, and GitHub serves any object in the
# repository's network that way, including unmerged pull-request heads,
# so a SHA must also be an ancestor of upstream main: decision 11 pins
# an upstream build, not just an immutable one.
RUN if [[ -z "${CORE_AGENT_REF:-}" ]]; then \
      echo "selfdev-soak: CORE_AGENT_REF is required: a go-steer/core-agent release tag (vX.Y.Z[-pre]) or a full 40-character commit SHA (design decision 11)" >&2; \
      exit 1; \
    fi; \
    have="$(go env GOVERSION)"; \
    if [[ "${have}" != "go${GO_VERSION}" ]]; then \
      echo "selfdev-soak: the base image carries ${have}, not go${GO_VERSION}; the FROM digest and GO_VERSION disagree" >&2; \
      exit 1; \
    fi; \
    git init -q /src; \
    git -C /src remote add origin https://github.com/go-steer/core-agent.git; \
    if [[ "${CORE_AGENT_REF}" =~ ^[0-9a-f]{40}$ ]]; then \
      git -C /src fetch -q --filter=blob:none origin "+refs/heads/main:refs/remotes/origin/main"; \
      if ! git -C /src merge-base --is-ancestor "${CORE_AGENT_REF}" refs/remotes/origin/main 2>/dev/null; then \
        echo "selfdev-soak: CORE_AGENT_REF=${CORE_AGENT_REF} is not a commit on go-steer/core-agent's main" >&2; \
        exit 1; \
      fi; \
      git -C /src -c advice.detachedHead=false checkout -q "${CORE_AGENT_REF}"; \
    elif [[ "${CORE_AGENT_REF}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then \
      git -C /src fetch -q --depth=1 origin "refs/tags/${CORE_AGENT_REF}"; \
      git -C /src -c advice.detachedHead=false checkout -q FETCH_HEAD; \
    else \
      echo "selfdev-soak: CORE_AGENT_REF=${CORE_AGENT_REF} is neither a release tag (vX.Y.Z[-pre]) nor a full commit SHA; branches float and are refused (design decision 11)" >&2; \
      exit 1; \
    fi; \
    mkdir -p /out; \
    head="$(git -C /src rev-parse HEAD)"; \
    if [[ "${head}" != "${CORE_AGENT_COMMIT:-}" ]]; then \
      echo "selfdev-soak: ${CORE_AGENT_REF} is ${head}, but CORE_AGENT_COMMIT=${CORE_AGENT_COMMIT:-<unset>}; build with build-image.sh, which resolves it (and a moved tag is caught here)" >&2; \
      exit 1; \
    fi; \
    printf '%s\n' "${head}" > /out/core-agent.commit; \
    printf '%s\n' "${CORE_AGENT_REF}" > /out/core-agent.ref

# The module path changes with a major version (/v2, /v3, ...), and an
# -X flag naming a package that does not exist is silently ignored, so
# read the path from the ref's own go.mod rather than writing it here.
# A tag build reports the tag as its version. A SHA build keeps the
# tree's own version string (e.g. v2.10.0-dev) and reports the SHA as
# its commit, because the version reaches attach peers (agent card,
# capabilities, user agent), which expect a semver there. Date is the
# commit's date, not the build's, so two builds of one ref stamp the
# same binary; `--version` labels it "built".
RUN cd /src; \
    mod="$(go list -m)"; \
    commit="$(cat /out/core-agent.commit)"; \
    date="$(git log -1 --format=%cI HEAD)"; \
    ldflags="-s -w -X ${mod}/internal/version.Commit=${commit} -X ${mod}/internal/version.Date=${date}"; \
    if [[ "${CORE_AGENT_REF}" == v* ]]; then \
      ldflags="${ldflags} -X ${mod}/internal/version.Version=${CORE_AGENT_REF}"; \
    fi; \
    go build -trimpath -ldflags "${ldflags}" -o /out/core-agent ./cmd/core-agent; \
    /out/core-agent --version

# The dispatcher (dev/uat/selfdev-soak/dispatcher) and the recipe come
# from the SAME pinned checkout, so one image digest pins the agent, the
# process that hands it work, and the instructions it runs under. The
# dispatcher pod runs this image with its command overridden
# (deploy/base/51-deployment-dispatcher.yaml); it needs git, which this
# image already has, and the same uid as the daemon (README.md).
#
# The recipe is the ref's repo-root AGENTS.md plus its .agents/ tree,
# copied to /out/recipe and mounted nowhere: the daemon's --agents-dir
# points into it, read-only, so an upgrade of the recipe is the same
# deliberate, logged event as an upgrade of the binary (decision 11),
# and no clone of the mirror (whose history once held the seed list,
# decision 20) has to exist for the daemon to boot. The empty plans
# directory is the mount point the deployment covers with a writable
# volume. A ref older than the dispatcher is refused here, not at
# deploy time.
RUN cd /src; \
    if [[ ! -d dev/uat/selfdev-soak/dispatcher || ! -f .agents/config.json || ! -f AGENTS.md ]]; then \
      echo "selfdev-soak: ${CORE_AGENT_REF} predates the soak dispatcher or the self-recipe; build a newer ref" >&2; \
      exit 1; \
    fi; \
    go build -trimpath -ldflags "-s -w" -o /out/selfdev-soak-dispatcher ./dev/uat/selfdev-soak/dispatcher; \
    mkdir -p /out/recipe; \
    cp -R AGENTS.md .agents /out/recipe/; \
    rm -rf /out/recipe/.agents/plans; \
    mkdir -p /out/recipe/.agents/plans

# ---- Runtime stage ----
# The same pinned image: it already carries git, a shell, coreutils,
# ca-certificates, python3, gcc (for -race) and the Go toolchain, so one
# digest pins everything the agent runs except jq. The test asserts the
# two FROM lines are identical.
FROM golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36

# The builder stage has already proved this is the commit it built.
ARG CORE_AGENT_COMMIT
LABEL org.opencontainers.image.source="https://github.com/go-steer/core-agent" \
      org.opencontainers.image.revision="${CORE_AGENT_COMMIT}" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.title="core-agent-selfdev-soak" \
      org.opencontainers.image.description="Soak-only image for the core-agent self-development soak (#1213); not a release artifact."

# A fixed non-root uid/gid, numeric so a pod's runAsNonRoot can verify
# it. Give the pod fsGroup: 10001 so the mounted volumes are writable.
#
# jq is the one tool the presubmit sweep needs that the base lacks
# (verify-gke-drill hard-fails without it). It comes from bookworm's
# archive at build time, so it is the one package this file does not
# pin by digest; nothing the agent is measured on depends on its patch
# level.
#
# The find at the end strips every setuid/setgid bit (su, passwd,
# mount, ...), so nothing in the image can raise the agent's privileges
# even where a pod forgets allowPrivilegeEscalation: false.
#
# safe.directory '*': git refuses a repository another uid owns, and so
# does `go build`'s VCS stamping. The workspace PVC is shared with the
# dispatcher's pod, and a clone restored or created by anything other
# than uid 10001 would stop every git call here. The check protects one
# user from another's repository on a shared host; this container has
# one user. It is not a substitute for running the dispatcher as uid
# 10001 (README.md): ownership is not writability.
ARG SOAK_UID=10001
RUN set -eu; \
    apt-get update; \
    apt-get install -y --no-install-recommends jq; \
    rm -rf /var/lib/apt/lists/*; \
    git config --system --add safe.directory '*'; \
    groupadd --gid "${SOAK_UID}" soak; \
    useradd --uid "${SOAK_UID}" --gid "${SOAK_UID}" --create-home --home-dir /home/soak --shell /bin/bash soak; \
    mkdir -p /workspace /cache/go/mod /cache/go/build /cache/go/path/bin /var/lib/core-agent /etc/core-agent-soak /usr/local/share/core-agent-soak; \
    chown -R "${SOAK_UID}:${SOAK_UID}" /workspace /cache /var/lib/core-agent /home/soak; \
    find / -xdev -perm /6000 -type f -exec chmod u-s,g-s {} +

COPY --from=builder /out/core-agent /usr/local/bin/core-agent
COPY --from=builder /out/core-agent.ref /out/core-agent.commit /usr/local/share/core-agent-soak/
COPY --from=builder /out/selfdev-soak-dispatcher /usr/local/bin/selfdev-soak-dispatcher
COPY --from=builder /out/recipe /usr/local/share/core-agent-soak/recipe

# Mount points, documented in README.md:
#   /workspace                          workspace PVC (the mirror's clone + worktrees)
#   /cache/go                           Go module + build cache PVC, and
#                                       GOPATH, so `go install`ed dev tools
#                                       (golangci-lint, govulncheck) survive
#                                       a pod restart. Its bin is LAST on
#                                       PATH: it is writable and persists
#                                       across issues, so first on PATH it
#                                       would let one issue's session shadow
#                                       git or go for every later one.
#   /var/lib/core-agent                 eventlog PVC (--session-db-path)
#   /etc/core-agent-soak/config.json    the soak config overlay (read-only)
#   /usr/local/share/core-agent-soak/recipe/.agents/plans
#                                       writable plans directory, shared
#                                       with the dispatcher's pod; the rest
#                                       of the recipe stays read-only
ENV HOME=/home/soak \
    GOPATH=/cache/go/path \
    PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/cache/go/path/bin \
    GOMODCACHE=/cache/go/mod \
    GOCACHE=/cache/go/build \
    GOTOOLCHAIN=local

WORKDIR /workspace
USER 10001:10001

# The config is pinned here rather than left to discovery. config.Find
# walks up from the cwd, and the cwd is a clone of the mirror whose
# committed recipe is permissions.mode "ask": an unpinned daemon would
# silently run the wrong posture. With -c in the entrypoint, a missing
# overlay mount is a startup error instead. Pass --agents-dir and the
# daemon flags as the container's args (see README.md).
ENTRYPOINT ["/usr/local/bin/core-agent", "-c", "/etc/core-agent-soak/config.json"]
