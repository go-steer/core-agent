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

# Launch a busybox debug pod that mounts the recipe CONTENT image exactly
# the way the daemon does — read-only at ${CONTENT_MOUNT} with a writable
# `plans` emptyDir nested inside — so you can verify the mount mechanics
# WITHOUT execing into the daemon (it's distroless, no shell).
#
# Modes (first arg):
#   check   (default) run assertions non-interactively; exit non-zero on
#           failure; the pod is deleted afterward.
#   shell   sleep, then drop you into an interactive `sh` with the mounts;
#           the pod is deleted when you exit.
#
# The pod copies the daemon's securityContext (fsGroup/runAsUser 65532) so
# the writable-plans check is faithful: the plans emptyDir is group-owned
# 65532 exactly as under the daemon. FLAVOR=copy mounts the -copy image.
set -euo pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
source "${SCRIPT_DIR}/prereqs.sh"
require_coordinates || exit 1


MODE="${1:-check}"
POD="content-debug"
FLAVOR="${FLAVOR:-image}"   # image | copy
if [[ "${FLAVOR}" == "copy" ]]; then
    CONTENT_REF="${CONTENT_IMAGE}:${CONTENT_TAG}-copy"
else
    CONTENT_REF="${CONTENT_IMAGE}:${CONTENT_TAG}"
fi
K="kubectl --context ${KUBE_CONTEXT} -n ${DEMO_NS}"

# The assertions run inside the pod. Kept POSIX-sh (busybox ash).
# CONTENT_MOUNT is interpolated by the OUTER shell (note the unquoted
# heredoc delimiter) so the pod checks the same path the daemon mounts.
read -r -d '' CHECK_SCRIPT <<EOF || true
set -u
root=${CONTENT_MOUNT}
fail=0
say() { printf '%s\n' "\$*"; }
say "== content sanity =="
if [ -f "\$root/.agents/config.hub.json" ]; then say "  config.hub.json: OK"; else say "  config.hub.json: MISSING"; fail=1; fi
# The anthropic variant is RENDERED at build time, so its absence means
# an image built before the flavor switch existed — which would leave
# MODEL_FLAVOR=anthropic pointing -c at a file that isn't there, and the
# daemon would crash-loop on a missing config rather than say so here.
if [ -f "\$root/.agents/${AGENT_CONFIG_BASENAME}" ]; then say "  ${AGENT_CONFIG_BASENAME} (flavor=${MODEL_FLAVOR}): OK"; else say "  ${AGENT_CONFIG_BASENAME}: MISSING — rebuild the content image with build-content-image.sh"; fail=1; fi
if [ -f "\$root/AGENTS.md" ]; then say "  parent AGENTS.md: OK"; else say "  parent AGENTS.md: MISSING"; fail=1; fi
# AGENTS.d/ is deliberately absent — its one file was a stub for
# fleet-governance SOPs this runtime cannot execute. Assert it stays gone,
# so a stale content image (or a resurrected COPY layer) is visible here
# rather than as a persona advertising playbooks that don't apply.
if [ -e "\$root/AGENTS.d" ]; then say "  AGENTS.d/: UNEXPECTED — stale content image, rebuild with build-content-image.sh"; fail=1; else say "  AGENTS.d/: absent (expected)"; fi

# This recipe is self-contained: its persona is written for core-agent
# rather than ported from another runtime, so it must NOT carry a vendored
# upstream/ workspace. If upstream/ shows up here, the wrong
# content.Dockerfile (or the wrong build context) was used, and the persona
# under test is not the one this recipe exists to test.
if [ -d "\$root/upstream" ]; then say "  upstream/: PRESENT — wrong content image, this recipe is self-contained"; fail=1; else say "  no vendored upstream/: OK"; fi

# cluster/ is the \`cluster\` subagent's own content root. Its
# root:"../cluster" resolves to \$root/cluster; if the content image omits
# it (e.g. a content.Dockerfile without \`COPY cluster/\`), the daemon
# fails to boot. Assert the tree the subagent needs is actually mounted.
if [ -f "\$root/cluster/AGENTS.md" ] && [ -f "\$root/cluster/mcp.json" ]; then say "  cluster/ subagent root: OK"; else say "  cluster/ subagent root: MISSING — the cluster subagent would fail to load"; fail=1; fi
csk=\$(ls -1d "\$root"/cluster/skills/*/ 2>/dev/null | wc -l | tr -d ' ')
say "  cluster skills discovered: \${csk} (expect 6)"
[ "\${csk:-0}" -ge 1 ] || { say "  no cluster skills found"; fail=1; }

say "== nested writable plans =="
if [ -d "\$root/.agents/plans" ]; then
  say "  .agents/plans mount point: present"
else
  say "  .agents/plans mount point: MISSING — read-only image can't create it at mount time"; fail=1
fi
if touch "\$root/.agents/plans/.writetest" 2>/dev/null; then
  rm -f "\$root/.agents/plans/.writetest"; say "  plans writable: OK"
else
  say "  plans writable: NO — record_plan artifacts would fail, and plan_mode=required would block every mutation"; fail=1
fi

say "== read-only content root =="
if touch "\$root/.agents/.rotest" 2>/dev/null; then
  rm -f "\$root/.agents/.rotest"; say "  WARN: content root is writable (expected read-only)"
else
  say "  content root read-only: OK"
fi

if [ "\$fail" -eq 0 ]; then say "ALL CHECKS PASSED"; else say "CHECKS FAILED"; fi
exit "\$fail"
EOF

# Container command differs by mode; the pod spec is otherwise identical.
if [[ "${MODE}" == "shell" ]]; then
    CMD_JSON='["sh","-c","sleep 3600"]'
elif [[ "${MODE}" == "check" ]]; then
    CMD_JSON=$(printf '["sh","-c",%s]' "$(printf '%s' "${CHECK_SCRIPT}" | jq -Rs .)")
else
    echo "✗ unknown mode '${MODE}' (want: check | shell)"; exit 1
fi

echo "→ mounting ${CONTENT_REF} (flavor=${FLAVOR}) in a debug pod"
${K} delete pod "${POD}" --ignore-not-found --wait=true 2>/dev/null || true

# Full Pod spec — image volumes need spec.volumes[].image, not expressible
# via `kubectl run` flags. Mirrors deploy/base/50-deployment-daemon.yaml.
cat <<POD | ${K} apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${POD}
  labels: { app.kubernetes.io/name: content-debug }
spec:
  restartPolicy: Never
  securityContext:
    fsGroup: 65532
    runAsNonRoot: true
    runAsUser: 65532
  containers:
    - name: debug
      image: busybox:1.38.0
      command: ${CMD_JSON}
      volumeMounts:
        - { name: recipe-content, mountPath: ${CONTENT_MOUNT}, readOnly: true }
        - { name: plans, mountPath: ${CONTENT_MOUNT}/.agents/plans }
  volumes:
    - name: recipe-content
      image: { reference: ${CONTENT_REF}, pullPolicy: IfNotPresent }
    - name: plans
      emptyDir: { sizeLimit: 10Mi }
POD

trap '${K} delete pod "${POD}" --ignore-not-found --wait=false >/dev/null 2>&1 || true' EXIT

if [[ "${MODE}" == "shell" ]]; then
    ${K} wait --for=condition=Ready "pod/${POD}" --timeout=120s
    echo "→ dropping into the debug pod (exit to delete it)"
    ${K} exec -it "${POD}" -- sh
    exit 0
fi

# check mode: wait for terminal phase, stream logs, propagate exit code.
echo "→ running checks…"
for _ in $(seq 1 60); do
    phase=$(${K} get pod "${POD}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    [[ "${phase}" == "Succeeded" || "${phase}" == "Failed" ]] && break
    sleep 2
done
${K} logs "${POD}" || true
phase=$(${K} get pod "${POD}" -o jsonpath='{.status.phase}' 2>/dev/null || true)
if [[ "${phase}" == "Succeeded" ]]; then
    echo "✓ content mount verified (image-volume mechanics + nested plans)."
else
    echo "✗ verification pod ended in phase=${phase:-unknown} — see logs above."
    exit 1
fi
