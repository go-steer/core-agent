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

# Build the self-development soak image (#1213, prerequisite P3), and
# push it only when asked to in so many words.
#
#   build-image.sh --ref <release tag>
#   build-image.sh --ref <40-char sha> --push --registry us-docker.pkg.dev/<project>/<repo>
#
# --ref is required and has no default (design decision 11): the soak
# measures one fixed upstream build, and an upgrade is a deliberate,
# logged soak event. It must be a go-steer/core-agent release tag or a
# full commit SHA; the Dockerfile refuses a branch, and so does this
# script, before docker is ever started.
#
# Pushing takes BOTH --push and --registry. --registry alone is refused
# rather than read as "push", and --push alone is refused rather than
# pushed to some default, so no combination of a typo and a forgotten
# flag sends an image anywhere.
#
# The build context is an empty temporary directory. The Dockerfile
# clones the ref from upstream itself; an empty context makes it
# impossible for the local checkout (or a mirror's main) to leak in.
set -euo pipefail

SELF_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
DOCKERFILE="${SELF_DIR}/Dockerfile"
IMAGE_NAME="core-agent-selfdev-soak"

usage() {
  cat <<'EOF'
Usage: dev/uat/selfdev-soak/build-image.sh --ref <tag|sha> [options]

  --ref REF          required: a go-steer/core-agent release tag
                     (vX.Y.Z or vX.Y.Z-pre) or a full 40-character SHA
  --tag NAME         local image name (default core-agent-selfdev-soak:<ref>)
  --platform P       passed to docker build (e.g. linux/amd64)
  --push             push the built image; requires --registry
  --registry R       registry/repository prefix to push to; requires --push
  -h, --help         this text
EOF
}

die() {
  echo "build-image.sh: $*" >&2
  exit 2
}

REF=""
TAG=""
PLATFORM=""
PUSH=0
REGISTRY=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --ref)       [[ $# -ge 2 ]] || die "--ref needs a value"; REF="$2"; shift 2 ;;
    --ref=*)     REF="${1#--ref=}"; shift ;;
    --tag)       [[ $# -ge 2 ]] || die "--tag needs a value"; TAG="$2"; shift 2 ;;
    --tag=*)     TAG="${1#--tag=}"; shift ;;
    --platform)  [[ $# -ge 2 ]] || die "--platform needs a value"; PLATFORM="$2"; shift 2 ;;
    --platform=*) PLATFORM="${1#--platform=}"; shift ;;
    --push)      PUSH=1; shift ;;
    --registry)  [[ $# -ge 2 ]] || die "--registry needs a value"; REGISTRY="$2"; shift 2 ;;
    --registry=*) REGISTRY="${1#--registry=}"; shift ;;
    -h|--help)   usage; exit 0 ;;
    *)           usage >&2; die "unknown argument: $1" ;;
  esac
done

if [[ -z "${REF}" ]]; then
  usage >&2
  die "--ref is required: a go-steer/core-agent release tag or full commit SHA (decision 11, no default)"
fi
if ! [[ "${REF}" =~ ^[0-9a-f]{40}$ || "${REF}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
  die "--ref ${REF} is neither a release tag (vX.Y.Z[-pre]) nor a full 40-character commit SHA; branches float and are refused"
fi
if [[ ${PUSH} -eq 1 && -z "${REGISTRY}" ]]; then
  die "--push needs --registry: there is no default registry to push to"
fi
if [[ ${PUSH} -eq 0 && -n "${REGISTRY}" ]]; then
  die "--registry given without --push: refusing to guess whether you meant to push (add --push, or drop --registry)"
fi

command -v docker >/dev/null 2>&1 || die "docker is not on PATH"

# Resolve the ref to a commit now, and hand it to the build: it becomes
# the image's org.opencontainers.image.revision label, and the
# Dockerfile refuses to build if its checkout is a different commit.
# go-steer/core-agent has no tag protection, so a tag is a name, not a
# proof; the commit is what gets recorded. An annotated tag lists its
# peeled commit as <tag>^{}, which wins over the tag object's own id.
UPSTREAM="https://github.com/go-steer/core-agent.git"
if [[ "${REF}" =~ ^[0-9a-f]{40}$ ]]; then
  COMMIT="${REF}"
else
  command -v git >/dev/null 2>&1 || die "git is not on PATH (needed to resolve ${REF})"
  LS="$(git ls-remote "${UPSTREAM}" "refs/tags/${REF}" "refs/tags/${REF}^{}")" \
    || die "git ls-remote ${UPSTREAM} failed"
  COMMIT="$(awk -v peeled="refs/tags/${REF}^{}" '$2 == peeled {print $1}' <<<"${LS}")"
  if [[ -z "${COMMIT}" ]]; then
    COMMIT="$(awk -v tag="refs/tags/${REF}" '$2 == tag {print $1}' <<<"${LS}")"
  fi
  [[ "${COMMIT}" =~ ^[0-9a-f]{40}$ ]] || die "tag ${REF} does not exist on ${UPSTREAM}"
fi

if [[ -z "${TAG}" ]]; then
  TAG="${IMAGE_NAME}:${REF}"
fi

CONTEXT="$(mktemp -d)"
# shellcheck disable=SC2064  # expand now, not at trap time
trap "rm -rf '${CONTEXT}'" EXIT

BUILD_ARGS=(build --file "${DOCKERFILE}" --build-arg "CORE_AGENT_REF=${REF}" --build-arg "CORE_AGENT_COMMIT=${COMMIT}" --tag "${TAG}")
if [[ -n "${PLATFORM}" ]]; then
  BUILD_ARGS+=(--platform "${PLATFORM}")
fi
BUILD_ARGS+=("${CONTEXT}")

echo "build-image.sh: building ${TAG} from go-steer/core-agent@${REF} (commit ${COMMIT})"
docker "${BUILD_ARGS[@]}"
# The binary reports the ref it was built from. Skipped for a foreign
# --platform, which this host may not be able to execute.
if [[ -z "${PLATFORM}" ]]; then
  docker run --rm "${TAG}" --version
fi

if [[ ${PUSH} -eq 1 ]]; then
  REMOTE="${REGISTRY%/}/${IMAGE_NAME}:${REF}"
  echo "build-image.sh: pushing ${REMOTE}"
  docker tag "${TAG}" "${REMOTE}"
  docker push "${REMOTE}"
  # The :<ref> tag is mutable (a rebuild overwrites it), so deploy by the
  # digest the registry just recorded.
  echo "build-image.sh: pushed. Deploy by digest, and record it with commit ${COMMIT}:"
  docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "${REMOTE}"
fi
