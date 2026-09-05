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

# Deploy the gke-platform-agent hub + watcher to your GKE cluster.
#
# Order of operations, all from the recipe directory:
#   1. set your coordinates (see scripts/prereqs.sh)
#   2. ./scripts/build-content-image.sh   (push the OCI content image)
#   3. ./scripts/gen-tokens.sh            (create Secrets)
#   4. ./scripts/set-up-demo.sh           (this script)
#
# Auto-detects the cluster's Kubernetes version and picks the delivery
# path: OCI image volume on GKE 1.35+, else the initContainer-copy
# fallback. Force one with OVERLAY=example|copy.
#
# Tracing is ON by default and probed the same way: if the cluster has
# GKE Managed OpenTelemetry enabled, the *-otel variant of the chosen
# delivery overlay is applied and spans go to Cloud Trace. Force with
# OTEL=1|0 (OTEL=1 on a cluster without the CRD fails loudly rather
# than deploying something that traces nowhere).
#
# There is no "refresh deploy/ from somewhere else" step. This recipe is
# self-contained, so deploy/ here IS the source of truth.
set -euo pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
source "${SCRIPT_DIR}/prereqs.sh"

# ── Preflight ────────────────────────────────────────────────────────
require_coordinates || exit 1
command -v kustomize >/dev/null || { echo "✗ kustomize not on PATH (or use 'kubectl kustomize')"; exit 1; }
# Deploying alongside another watcher is fine; only ./scripts/break-workload.sh
# actually races. Surface it here so it's known before the incident step.
warn_foreign_watchers || true

# ── Pick the delivery path from the cluster's k8s version ────────────
# Image volumes: beta from k8s 1.33, enabled on GKE 1.35+. Below that,
# fall back to the initContainer-copy overlay.
SERVER_MINOR=$(kubectl --context "${KUBE_CONTEXT}" version -o json 2>/dev/null \
    | jq -r '.serverVersion.minor' | tr -cd '0-9')
OVERLAY="${OVERLAY:-auto}"
if [[ "${OVERLAY}" == "auto" ]]; then
    if [[ -n "${SERVER_MINOR}" && "${SERVER_MINOR}" -ge 33 ]]; then
        OVERLAY="example"
    else
        OVERLAY="copy"
        echo "⚠ cluster is k8s 1.${SERVER_MINOR:-?} (below the image-volume floor) — using initContainer-copy fallback."
    fi
fi
if [[ "${OVERLAY}" == "example" ]]; then
    OVERLAY_DIR="${DEMO_OVERLAY_DIR}"
    OTEL_OVERLAY_DIR="${DEMO_OVERLAY_OTEL_DIR}"
    CONTENT_REF="${CONTENT_IMAGE}:${CONTENT_TAG}"
    echo "→ delivery: OCI image volume (overlays/example)  [k8s 1.${SERVER_MINOR}]"
else
    OVERLAY_DIR="${DEMO_OVERLAY_COPY_DIR}"
    OTEL_OVERLAY_DIR="${DEMO_OVERLAY_COPY_OTEL_DIR}"
    CONTENT_REF="${CONTENT_IMAGE}:${CONTENT_TAG}-copy"
    echo "→ delivery: initContainer copy (overlays/initcontainer-copy)"
fi

# ── Decide the tracing axis ──────────────────────────────────────────
# Orthogonal to delivery: the *-otel overlays compose the delivery
# overlay above and add components/otel-gke. The Instrumentation CR in
# that component needs a CRD that only exists once the cluster has been
# updated with --managed-otel-scope, so probe for it rather than finding
# out from `kubectl apply`'s "no matches for kind".
#
# TWO DIRECTORIES FROM HERE ON, and the distinction is load-bearing:
#   OVERLAY_DIR  — the DELIVERY overlay. Every mutation below (the
#                  per-cluster sed patches, `kustomize edit set image`)
#                  targets this, because that is where the patch files
#                  and the image-volume fieldSpec live.
#   APPLY_DIR    — what actually gets built and applied. The *-otel
#                  composer when tracing is on, otherwise the same
#                  directory as OVERLAY_DIR.
OTEL="${OTEL:-auto}"
# Capture first, then match. `kubectl api-resources` exits non-zero when
# ANY APIService in the cluster is unavailable — a flaky metrics-server
# is enough — and under `set -o pipefail` that failure would mask a
# successful grep and silently downgrade a tracing-capable cluster to
# untraced. Look at what it printed instead of at how it exited.
otel_crd_present() {
    local resources
    resources=$(kubectl --context "${KUBE_CONTEXT}" api-resources \
        --api-group=telemetry.googleapis.com 2>/dev/null || true)
    grep -q "^instrumentations[[:space:]]" <<<"${resources}"
}

# Printed by both the soft-fallback and the hard-fail path, so an
# operator never has to go find the docs. The IAM grant is the one that
# gets forgotten: the CRD can be present and the spans still rejected.
# Needs a GKE control plane at 1.34.1-gke.2178000 or later.
print_otel_enable_commands() {
    echo "    gcloud services enable cloudtrace.googleapis.com telemetry.googleapis.com \\"
    echo "      --project=${PROJECT_ID}"
    echo "    gcloud container clusters update ${CLUSTER_NAME} --location=${REGION} \\"
    echo "      --managed-otel-scope=COLLECTION_AND_INSTRUMENTATION_COMPONENTS \\"
    echo "      --project=${PROJECT_ID}"
    echo "    gcloud projects add-iam-policy-binding ${PROJECT_ID} \\"
    echo "      --role=roles/cloudtrace.user \\"
    echo "      --member=principal://iam.googleapis.com/projects/${PROJECT_NUMBER:-<PROJECT_NUMBER>}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/${DEMO_NS}/sa/core-agent-daemon"
}

if [[ "${OTEL}" == "auto" ]]; then
    if otel_crd_present; then
        OTEL=1
    else
        OTEL=0
        echo "⚠ GKE Managed OpenTelemetry is not enabled on this cluster — deploying WITHOUT tracing."
        echo "  Turn it on (then re-run this script) with:"
        print_otel_enable_commands
        echo "  Set OTEL=0 to silence this."
    fi
elif [[ "${OTEL}" == "1" ]] && ! otel_crd_present; then
    # Explicitly requested and unavailable: fail rather than silently
    # downgrade. An operator who asked for traces and got a clean deploy
    # with none is worse off than one who got an error naming the fix.
    echo "✗ OTEL=1 but the Instrumentation CRD (telemetry.googleapis.com) is not served by this cluster."
    echo "  Enable managed OTel with:"
    print_otel_enable_commands
    echo "  ...or re-run with OTEL=0 to deploy without tracing."
    exit 1
fi

if [[ "${OTEL}" == "1" ]]; then
    APPLY_DIR="${OTEL_OVERLAY_DIR}"
    echo "→ tracing: ON (${APPLY_DIR##*/}) — spans to Cloud Trace via GKE Managed OTel"
else
    APPLY_DIR="${OVERLAY_DIR}"
    echo "→ tracing: off (${APPLY_DIR##*/})"
fi

# ── Patch per-cluster values into the chosen overlay ─────────────────
# All four coordinates the daemon needs, rewritten in the overlay's
# configMapGenerator (which merges over the base's placeholders).
#
# Match on the KEY, never on the placeholder value. Keying on the value
# is how this silently broke once already: the overlays' placeholders
# were renamed, every `s|VAR=old-value|VAR=new-value|` quietly matched
# nothing, and the deploy came up carrying the literal string
# "your-project-id" — a daemon that boots, passes every probe, and 403s
# on its first model call.
#
# The `[[:space:]]*-[[:space:]]*` anchor keeps this to the YAML list
# items and off the surrounding comment prose, which names all four.
patch_literal() {
    local key="$1" val="$2" file="$3"
    sed -i -E "s|^([[:space:]]*-[[:space:]]*)${key}=.*$|\1${key}=${val}|" "${file}"
    grep -qE "^[[:space:]]*-[[:space:]]*${key}=${val}$" "${file}" || {
        echo "✗ could not set ${key} in ${file}"
        echo "  Expected a '- ${key}=<value>' literal under configMapGenerator."
        exit 1
    }
}
patch_literal GOOGLE_CLOUD_PROJECT  "${PROJECT_ID}"       "${OVERLAY_DIR}/kustomization.yaml"
patch_literal GOOGLE_CLOUD_LOCATION "${VERTEX_LOCATION}"  "${OVERLAY_DIR}/kustomization.yaml"
patch_literal GKE_CLUSTER           "${CLUSTER_NAME}"     "${OVERLAY_DIR}/kustomization.yaml"
patch_literal GKE_LOCATION          "${REGION}"           "${OVERLAY_DIR}/kustomization.yaml"

# Watcher flags that must agree with values set elsewhere:
#
#   --cluster-name  identifies the source cluster on every inject, and
#                   must name the SAME cluster as GKE_CLUSTER above, or
#                   the agent investigates one cluster and reports the
#                   name of another.
#   --owner         is the identity each incident session belongs to. It
#                   must be a key in the users.json table gen-tokens.sh
#                   writes from ADMIN_IDENTITY — otherwise every session
#                   is owned by someone who cannot attach to it, and the
#                   operator sees an empty picker on a hub that is busy.
#
# Both default to the same values these variables default to, which is
# exactly why they need patching: the defaults agree, so a divergence
# only appears once someone overrides one of them, and it appears as
# behavior rather than as an error.
patch_watcher_flag() {
    local flag="$1" val="$2" file="${OVERLAY_DIR}/patch-watcher-args.yaml"
    sed -i -E "s|--${flag}=[^\"]*|--${flag}=${val}|" "${file}"
    grep -q -- "--${flag}=${val}\"" "${file}" || {
        echo "✗ could not set --${flag} in ${file}"
        exit 1
    }
}
patch_watcher_flag cluster-name "${CLUSTER_NAME}"
patch_watcher_flag owner        "${ADMIN_IDENTITY}"

# Model flavor: point the daemon's -c at the matching config file.
AGENT_CONFIG_PATH="${CONTENT_MOUNT}/.agents/${AGENT_CONFIG_BASENAME}"
sed -i -E "s|^  value: .*/\.agents/config\..*$|  value: ${AGENT_CONFIG_PATH}|" \
    "${OVERLAY_DIR}/patch-agent-config.yaml"
echo "→ model flavor: ${MODEL_FLAVOR}  (-c ${AGENT_CONFIG_BASENAME})"
if [[ "${MODEL_FLAVOR}" == "anthropic" ]]; then
    echo "    parent=${ANTHROPIC_PARENT_MODEL}  cluster=${ANTHROPIC_CLUSTER_MODEL}"
    echo "    ceilings: turn=\$${ANTHROPIC_MAX_TURN_COST_USD} session=\$${ANTHROPIC_MAX_SESSION_COST_USD}"
    # Claude 5 is served only by the global Vertex endpoint in this
    # project; a regional VERTEX_LOCATION yields a 404 at first call,
    # long after this script has reported success.
    [[ "${VERTEX_LOCATION}" == "global" ]] || {
        echo "✗ MODEL_FLAVOR=anthropic needs VERTEX_LOCATION=global (got '${VERTEX_LOCATION}')."
        echo "  us-east5 serves only claude-3-opus / claude-sonnet-4-5 — no Claude 5."
        exit 1
    }
fi

# Pin the CONTENT image, and only the content image.
#
# The daemon and watcher pins live in the overlay's `images:` block and
# this script deliberately does not touch them. They are published
# artifacts: which release deploys is a property of the recipe, it should
# be readable in the tree by anyone reviewing it, and it is checked there
# — `recipecheck`'s deploy-pin gate reads the manifests and fails a pin
# that is floating or below the recipe config's floor. A second copy of
# the same pin in this script would be a second source of truth for one
# fact, which is a thing that drifts rather than a convenience. To move
# the daemon version, edit `deploy/overlays/*/kustomization.yaml`.
#
# The content image is the opposite case: there is no published copy, YOU
# build and push it, and its reference is only knowable at deploy time
# from PROJECT_ID / REGION / AR_REPO / CONTENT_TAG. The custom fieldSpec
# in kustomizeconfig/images.yaml is what lets `images:` reach the
# image-*volume* reference as well as ordinary container images.
( cd "${OVERLAY_DIR}" \
  && kustomize edit set image "ghcr.io/go-steer/gke-platform-agent-content=${CONTENT_REF}" )

# Render APPLY_DIR, not OVERLAY_DIR: on the tracing path they differ,
# and the pins are only proven correct if we read them off the manifest
# that actually gets applied. The *-otel overlays declare no images: of
# their own precisely so these two agree.
echo "→ rendered image refs:"
kustomize build "${APPLY_DIR}" | grep -E "image:|reference:" | sed 's/^/    /'

# Assert the -c patch landed on the argument we meant. patch-agent-config
# addresses args[1] by INDEX, so a reordering of the daemon's args in the
# base Deployment would silently rewrite the wrong flag — and the daemon
# would boot on the wrong model, or on a path that doesn't exist, with
# nothing in this script's output to say so.
RENDERED_CFG=$(kustomize build "${APPLY_DIR}" \
    | python3 -c '
import sys, yaml
for d in yaml.safe_load_all(sys.stdin):
    if not d or d.get("kind") != "Deployment": continue
    if d["metadata"]["name"] != "core-agent": continue
    args = d["spec"]["template"]["spec"]["containers"][0]["args"]
    print(args[args.index("-c") + 1] if "-c" in args else "")
')
if [[ "${RENDERED_CFG}" != "${AGENT_CONFIG_PATH}" ]]; then
    echo "✗ daemon -c resolved to '${RENDERED_CFG}', expected '${AGENT_CONFIG_PATH}'"
    echo "  patch-agent-config.yaml targets args[1] by index — check the base"
    echo "  Deployment's arg order in deploy/base/50-deployment-daemon.yaml."
    exit 1
fi
echo "→ daemon config: ${RENDERED_CFG}"

# Assert no placeholder survived into the manifest we are about to apply.
#
# This is the backstop for the whole patch-by-sed approach above. Every
# rewrite in this script is a regex against a checked-in file, and a
# regex that matches nothing is indistinguishable from a regex that
# matched — sed exits 0 either way. Each individual rewrite asserts its
# own result, but only this check covers the case nobody thought to
# rewrite at all: a placeholder added to the base or an overlay later,
# with no corresponding patch_literal line here.
#
# Placeholders are cheap to detect because they are spelled to be: no
# real deployment's coordinates contain "your-". Note that
# platform-oncall@example.com is NOT in this list — it is the default
# ADMIN_IDENTITY, and an identity is just a key in the bearer table, so
# the default is a working value rather than a placeholder.
#
# Comments are stripped first: the manifests explain their own
# placeholders, and the explanation is not the defect.
LEFTOVER=$(kustomize build "${APPLY_DIR}" \
    | sed -E 's/[[:space:]]*#.*$//' \
    | grep -nE 'your-project-id|your-cluster|your-repo' || true)
if [[ -n "${LEFTOVER}" ]]; then
    echo "✗ placeholders survived into the rendered manifest:"
    echo "${LEFTOVER}" | sed 's/^/    /'
    echo "  Something in ${APPLY_DIR} is not covered by a patch in this script."
    exit 1
fi

# ── Fresh session DB, then apply ─────────────────────────────────────
kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" scale deploy/core-agent --replicas=0 2>/dev/null || true
kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" delete pvc core-agent-session-db --ignore-not-found 2>/dev/null || true

kubectl --context "${KUBE_CONTEXT}" apply -k "${APPLY_DIR}"
kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" rollout status deploy/core-agent --timeout=180s
kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" rollout status deploy/lookout-watch --timeout=180s

# ── Verify the content mount actually resolved ───────────────────────
# `subagent` matters here specifically: the whole native-recipe delegation
# story rests on the cluster subagent loading from its own content root,
# and a silently-unloaded subagent looks identical to a healthy boot until
# the model tries to route to it.
echo "→ daemon boot lines (content root + skills + subagents + telemetry):"
kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" logs deploy/core-agent \
    | grep -Ei "attach listener|content root|skills|subagent|watchdog|context cache|otel|telemetry|trace" | sed 's/^/    /' || true

# On the tracing path, confirm the CR actually reached the Pods. The
# component's own two vars are guaranteed by the manifest we just
# applied; OTEL_EXPORTER_OTLP_ENDPOINT is the interesting one, because
# it is injected by GKE's webhook at Pod admission. Its absence means
# the CRD exists but managed OTel is not actually collecting — spans
# would go to the SDK's localhost:4318 default and vanish.
#
# THE FIRST-TIME-ENABLE RACE, and why this restarts rather than just
# warning. One `kubectl apply -k` submits the Instrumentation CR and the
# Deployment update together, and the step above scaled to 0 first — so
# the new Pods are admitted within a second or two of the CR being
# created. On a namespace that did not previously have the CR, GKE has
# usually not reconciled it yet, and the Pods come up with this recipe's
# two env vars and none of the injected ones. Observed exactly this on
# the 2026-08-19 deploy: the daemon logged
#   "telemetry: OTLP HTTP exporter (via ADK) → (default localhost:4318)"
# A restart fixes it permanently — by then the CR exists and the webhook
# fires at admission — so do it here instead of making every first-time
# operator read a warning and run it by hand. Second run onward the
# first check passes and this costs nothing.
otel_endpoint_injected() {
    local env_names
    env_names=$(kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" get pods \
        -l app.kubernetes.io/name=core-agent \
        -o jsonpath='{.items[0].spec.containers[?(@.name=="core-agent")].env[*].name}' 2>/dev/null || true)
    grep -qw "OTEL_EXPORTER_OTLP_ENDPOINT" <<<"${env_names}"
}

if [[ "${OTEL}" == "1" ]]; then
    echo "→ verifying managed-OTel injection reached the daemon Pod:"
    if otel_endpoint_injected; then
        echo "    OTEL_EXPORTER_OTLP_ENDPOINT: injected ✓"
    else
        echo "    OTEL_EXPORTER_OTLP_ENDPOINT absent — the CR was almost certainly"
        echo "    created in the same apply as the Pods. Restarting both Deployments"
        echo "    so the webhook can fire at admission..."
        kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" rollout restart \
            deployment/core-agent deployment/lookout-watch
        kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" rollout status \
            deploy/core-agent --timeout=180s
        kubectl --context "${KUBE_CONTEXT}" -n "${DEMO_NS}" rollout status \
            deploy/lookout-watch --timeout=180s
        if otel_endpoint_injected; then
            echo "    OTEL_EXPORTER_OTLP_ENDPOINT: injected after restart ✓"
        else
            # Restarting did not help, so this is not the race — the CR
            # exists but nothing is acting on it. Almost always a scope
            # that enables collection without instrumentation.
            echo "    ⚠ still absent after a restart. The Instrumentation CR exists but"
            echo "      GKE is not injecting. Check that the cluster's --managed-otel-scope"
            echo "      includes INSTRUMENTATION_COMPONENTS, not just COLLECTION:"
            echo "        gcloud container clusters describe ${CLUSTER_NAME} --location=${REGION} \\"
            echo "          --project=${PROJECT_ID} --format='value(managedOtelConfig)'"
            echo "      Spans are going to the SDK default (localhost:4318) and vanishing."
        fi
    fi

    # The IAM half, checked separately because it fails DIFFERENTLY: with
    # the binding missing, everything above still passes. The CR injects,
    # the SDK exports, neither binary logs anything unusual — and Cloud
    # Trace rejects the spans server-side. A healthy-looking deploy with
    # an empty trace list is the worst outcome this script can hand over,
    # so spend one read to rule it out.
    #
    # BOTH service accounts, because the tracing overlays instrument both
    # Deployments. The watcher is the one that gets forgotten: unlike the
    # daemon it holds no other GCP role, so there is no existing binding
    # to amend and nothing else breaks to tip you off. A half-bound
    # project yields traces that start at the daemon and lose the
    # watcher's inject span — a partial trace reads as a complete one.
    #
    # Best-effort: an operator without resourcemanager.projects.getIamPolicy
    # gets a skip, not a failure. Deploy has already succeeded by here.
    ksa_principal() {
        echo "principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/${DEMO_NS}/sa/$1"
    }
    trace_members=$(gcloud projects get-iam-policy "${PROJECT_ID}" \
        --flatten="bindings[].members" \
        --filter="bindings.role=roles/cloudtrace.user" \
        --format='value(bindings.members)' 2>/dev/null || true)
    if [[ -z "${trace_members}" || -z "${PROJECT_NUMBER}" ]]; then
        # Empty PROJECT_NUMBER would build a principal with an empty
        # project segment, which matches nothing — a guaranteed false
        # warning. Skip rather than cry wolf.
        echo "    ⓘ could not read the project IAM policy — skipping the roles/cloudtrace.user check."
    else
        for ksa in core-agent-daemon lookout-watch; do
            principal=$(ksa_principal "${ksa}")
            if grep -qxF "${principal}" <<<"${trace_members}"; then
                echo "    roles/cloudtrace.user on ${ksa}: granted ✓"
            else
                echo "    ⚠ ${ksa} is NOT bound to roles/cloudtrace.user — its spans are"
                echo "      exported but Cloud Trace will reject them. Grant it with:"
                echo "        gcloud projects add-iam-policy-binding ${PROJECT_ID} \\"
                echo "          --role=roles/cloudtrace.user \\"
                echo "          --member=${principal}"
                echo "      (no restart needed — the binding takes effect within a minute.)"
            fi
        done
    fi
fi

# On the image-volume path, explicitly verify the mount mechanics the
# daemon can't self-report (distroless, no shell): content present, and
# the writable plans emptyDir nested inside the read-only image volume.
# Debug pod, not `kubectl exec`.
if [[ "${OVERLAY}" == "example" ]]; then
    echo "→ verifying image-volume mount + nested plans via debug pod"
    "${SCRIPT_DIR}/debug-pod.sh" check || {
        echo "✗ content-mount verification FAILED — the daemon may be up but"
        echo "  serving a broken mount. Inspect with: ./scripts/debug-pod.sh shell"
        exit 1
    }
fi
echo
echo "✓ deployed. Attach as ${ADMIN_IDENTITY} with ./scripts/attach.sh, then"
echo "  break a workload with ./scripts/break-workload.sh to fire an incident."
