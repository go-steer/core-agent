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

# Render the self-development soak rig (#1213) and refuse to print it
# while anything in it is still a placeholder.
#
#   render.sh [--inputs FILE] [--fqdn-egress] | kubectl apply -f -   # the rig
#   render.sh [--inputs FILE] --a7-job | kubectl apply -f -          # the A7 run
#
# The kustomization in base/ reads two files that are not in the tree:
# the operator's inputs.env (inputs.env.example lists the keys) and
# dev/uat/selfdev-soak/config.soak.json. This script copies the deploy
# tree to a temporary directory, adds both, runs `kustomize build`, and
# prints the result only when:
#
#   - every key inputs.env.example lists is set, once, with no
#     surrounding whitespace, and no value still says REPLACE;
#   - SOAK_IMAGE is <repository>@sha256:<64 hex> (a digest, never a tag),
#     SOAK_APP_ID is a number, SOAK_SLACK_CHANNEL is a bare Slack ID and
#     SOAK_COMMIT_EMAIL has an @;
#   - the rendered output contains no REPLACE anywhere, and every image:
#     in it is a digest reference.
#
# Anything else exits non-zero with nothing on stdout, so a pipe into
# kubectl applies nothing. It never talks to a cluster.
#
# --inputs defaults to ~/.gke-drill/selfdev-soak/inputs.env.
# --fqdn-egress renders fqdn-egress/ (host-named egress; README.md).
# --a7-job renders a7-job/: ONLY the dispatcher's --once Job, applied as
# the runbook's last step.
set -euo pipefail

SELF_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
SOAK_DIR="$(dirname "${SELF_DIR}")"
INPUTS="${HOME}/.gke-drill/selfdev-soak/inputs.env"
VARIANT=""

die() {
  echo "render.sh: $*" >&2
  exit 2
}

usage() {
  cat <<'EOF'
Usage: dev/uat/selfdev-soak/deploy/render.sh [--inputs FILE] [--fqdn-egress | --a7-job]

  --inputs FILE    the operator's values (default ~/.gke-drill/selfdev-soak/inputs.env);
                   start from deploy/inputs.env.example
  --fqdn-egress    render the rig with host-named egress (fqdn-egress/)
  --a7-job         render only the A7 run, the dispatcher's --once Job (a7-job/)
  -h, --help       this text

Prints the rendered manifests on stdout, or nothing and a non-zero exit.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --inputs)      [[ $# -ge 2 ]] || die "--inputs needs a value"; INPUTS="$2"; shift 2 ;;
    --inputs=*)    INPUTS="${1#--inputs=}"; shift ;;
    --fqdn-egress|--a7-job)
      [[ -z "${VARIANT}" ]] || die "--fqdn-egress and --a7-job are separate renders; pick one"
      VARIANT="${1#--}"; shift ;;
    -h|--help)     usage; exit 0 ;;
    *)             usage >&2; die "unknown argument: $1" ;;
  esac
done

[[ -f "${INPUTS}" ]] || die "no inputs file at ${INPUTS}; copy deploy/inputs.env.example there and fill it in"

if command -v kustomize >/dev/null 2>&1; then
  KUSTOMIZE=(kustomize build)
elif command -v kubectl >/dev/null 2>&1; then
  KUSTOMIZE=(kubectl kustomize)
else
  die "neither kustomize nor kubectl is on PATH"
fi

# The keys the example declares are the keys that must be set.
mapfile -t REQUIRED < <(grep -E '^[A-Z_][A-Z0-9_]*=' "${SELF_DIR}/inputs.env.example" | cut -d= -f1)
[[ ${#REQUIRED[@]} -gt 0 ]] || die "inputs.env.example declares no keys"

declare -A VALUES=()
lineno=0
while IFS= read -r line || [[ -n "${line}" ]]; do
  lineno=$((lineno + 1))
  [[ -z "${line}" || "${line}" =~ ^[[:space:]]*# ]] && continue
  [[ "${line}" =~ ^([A-Z_][A-Z0-9_]*)=(.*)$ ]] || die "${INPUTS}:${lineno}: not KEY=VALUE"
  key="${BASH_REMATCH[1]}"
  value="${BASH_REMATCH[2]}"
  [[ -z "${VALUES[${key}]+set}" ]] || die "${INPUTS}:${lineno}: ${key} is set twice"
  VALUES["${key}"]="${value}"
done < "${INPUTS}"

for key in "${REQUIRED[@]}"; do
  [[ -n "${VALUES[${key}]+set}" ]] || die "${INPUTS} does not set ${key}"
  value="${VALUES[${key}]}"
  [[ -n "${value}" ]] || die "${key} is empty"
  [[ "${value}" != *REPLACE* ]] || die "${key} is still the placeholder; fill it in"
  [[ "${value}" =~ ^[^[:space:]](.*[^[:space:]])?$ ]] || die "${key} has leading or trailing whitespace"
done

DIGEST_RE='^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$'
[[ "${VALUES[SOAK_IMAGE]}" =~ ${DIGEST_RE} ]] \
  || die "SOAK_IMAGE must be <repository>@sha256:<64 hex>, the digest build-image.sh --push prints; a tag is mutable"
[[ "${VALUES[SOAK_APP_ID]}" =~ ^[0-9]+$ ]] || die "SOAK_APP_ID must be the writer App's numeric ID"
[[ "${VALUES[SOAK_SLACK_CHANNEL]}" =~ ^[A-Z0-9]+$ ]] || die "SOAK_SLACK_CHANNEL must be a Slack channel ID (e.g. C0123ABCD)"
[[ "${VALUES[SOAK_COMMIT_EMAIL]}" == *@* ]] || die "SOAK_COMMIT_EMAIL is not an email address"

WORK="$(mktemp -d)"
# shellcheck disable=SC2064  # expand now, not at trap time
trap "rm -rf '${WORK}'" EXIT

cp -R "${SELF_DIR}" "${WORK}/deploy"
cp "${SOAK_DIR}/config.soak.json" "${WORK}/deploy/base/config.soak.json"
cp "${INPUTS}" "${WORK}/deploy/base/inputs.env"
cp "${INPUTS}" "${WORK}/deploy/a7-job/inputs.env"

TARGET="${WORK}/deploy/${VARIANT:-base}"

OUT="${WORK}/rendered.yaml"
"${KUSTOMIZE[@]}" "${TARGET}" > "${OUT}" || die "kustomize failed"

if grep -n 'REPLACE' "${OUT}" >&2; then
  die "the rendered manifests still contain a placeholder (lines above)"
fi

images=0
while IFS= read -r ref; do
  images=$((images + 1))
  [[ "${ref}" =~ ${DIGEST_RE} ]] || die "rendered image ${ref} is not a digest reference"
done < <(sed -nE 's/^[[:space:]]*(- )?image:[[:space:]]*"?([^"[:space:]]+)"?[[:space:]]*$/\2/p' "${OUT}")
[[ ${images} -gt 0 ]] || die "the rendered manifests name no image at all"

cat "${OUT}"
