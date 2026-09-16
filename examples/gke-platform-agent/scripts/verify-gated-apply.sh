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

# Verify the gated-apply authorization boundary against the REAL cluster,
# using the daemon's REAL identity. Design: docs/gated-apply-design.md.
#
# Modes (first arg):
#   granted  (default) the gated-apply component IS applied. Asserts the
#            daemon's identity is authorized to patch Deployments in
#            TARGET_NS and nothing else.
#
# WHAT THIS DOES NOT PROVE. It talks to the API server directly, so it
# validates the RBAC leg only. Whether the GKE MCP endpoint will actually
# issue a `patch_k8s_resource` on the agent's behalf is a separate fact —
# the endpoint's tool surface, its readOnlyHint and the mounted allowlist
# all sit in front of this grant. A green run here is still consistent
# with an agent that cannot patch, and the drill scenario is what closes
# that gap.
#   denied             the component is NOT applied (or has been torn
#            down). Asserts the daemon cannot patch anywhere. Run this
#            BEFORE applying to establish the baseline, and after
#            teardown to prove the grant is really gone.
#
# WHY THIS SCRIPT HAS TO EXIST. A RoleBinding whose subject string is
# wrong does not fail — it simply never matches. There is no error, no
# event, no log line; the agent's patch just keeps returning 403 exactly
# as if the component had never been applied. And the obvious check does
# not work here: `kubectl auth can-i --as=<subject>` exercises only the
# RBAC authorizer, while GKE delivers IAM through a webhook keyed to the
# AUTHENTICATED identity, so it disagrees with reality in both directions
# (observed: it answered "no" to a read the real token is granted). The
# only valid probe is a real token, which means a pod running as the
# daemon's ServiceAccount.
#
# WHY IT IS SAFE TO RUN AGAINST A LIVE CLUSTER. Every request targets a
# Deployment name that DOES NOT EXIST. Authorization is evaluated before
# existence, so an authorized request returns 404 and a denied one returns
# 403 — the two are distinguishable, and neither creates, changes or
# deletes anything. The `delete` probe in particular cannot delete
# anything even if the boundary were wide open.
#
# That argument depends on the name really being absent, so the GET runs
# FIRST and anything other than a 404 aborts the probe before it writes.
# Otherwise the one case the safety claim has to cover — a Deployment that
# does exist — is discovered only after it has been patched.
#
# The pod runs as core-agent-daemon in DEMO_NS and mints the daemon's
# Google token from the metadata server, which is exactly the credential
# the GKE MCP endpoint presents on the agent's behalf. It cannot exec into
# the daemon itself — that image is distroless and has no shell.
set -euo pipefail

SCRIPT_DIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
source "${SCRIPT_DIR}/prereqs.sh"
require_coordinates || exit 1
# This one CREATES a probe Pod in DEMO_NS running as the daemon's
# ServiceAccount, and it is the worst of the set to get wrong. It derives the
# subject it reports from the committed RoleBinding but runs the pod in
# DEMO_NS, so under an override it would print one identity and measure
# another — and if the overridden namespace happens to have a
# `core-agent-daemon` ServiceAccount, the probe mints THAT principal's token
# and returns a confident answer about the wrong one. The PROBE_SA assertion
# below compares a name, not a namespace, and cannot catch it.
require_demo_ns_matches_base || exit 1

MODE="${1:-granted}"
case "${MODE}" in
    granted|denied) ;;
    *) echo "✗ unknown mode '${MODE}' (want: granted | denied)" >&2; exit 2 ;;
esac

POD="gated-apply-verify"
# The probe target. It must not exist, and the opening GET establishes that
# for the ONE object the grant is expected to reach —
# ${TARGET_NS}/deployments/${TARGET}, where a 200 aborts the run before any
# write. The other three targets (a ConfigMap in ${TARGET_NS}, Deployments in
# default and ${DEMO_NS}) cannot be preconditioned the same way: the token is
# expected to be denied there, so a GET returns 403 and establishes nothing.
# Their safety rests on this name instead, which is why it is deliberately
# absurd rather than merely unlikely. Give it a plausible name and the
# unexpected-success case stops being a loud assertion failure and becomes a
# mutation.
TARGET="gated-apply-probe-does-not-exist"
PROBE_IMAGE="${PROBE_IMAGE:-curlimages/curl:8.11.1}"

APISERVER=$(kubectl --context "${KUBE_CONTEXT}" config view --minify \
    -o jsonpath='{.clusters[0].cluster.server}')
if [[ -z "${APISERVER}" ]]; then
    echo "✗ could not resolve the API server address for context ${KUBE_CONTEXT}" >&2
    exit 1
fi

K="kubectl --context ${KUBE_CONTEXT} -n ${DEMO_NS}"

# Read the identity out of the committed RoleBinding rather than composing
# one from DEMO_NS. DEMO_NS is a `kubectl -n` argument, not the daemon's
# namespace; they are equal only by default, and this script is on the
# read-only side of require_demo_ns_matches_base, so nothing here forces
# them to be. Composing one would put a subject this script INVENTED in front
# of an operator at the exact moment the probe has just failed and they are
# looking for something to paste into the manifest.
GATED_APPLY_SUBJECT=$(sed -nE 's|^[[:space:]]+name:[[:space:]]*(serviceAccount:.*)$|\1|p' \
    "${DEMO_DEPLOY_DIR}/components/gated-apply/rolebinding.yaml")
if [[ -z "${GATED_APPLY_SUBJECT}" ]]; then
    echo "✗ no 'name: serviceAccount:...' subject in deploy/components/gated-apply/rolebinding.yaml" >&2
    exit 1
fi

echo "→ verifying gated-apply boundary (mode=${MODE})"
echo "  identity : ${GATED_APPLY_SUBJECT}"
echo "  target   : ${TARGET_NS} (grant) vs default + ${DEMO_NS} (must stay denied)"

# Assertions run inside the pod (POSIX sh). The outer heredoc is
# UNQUOTED so APISERVER/TARGET_NS/DEMO_NS/TARGET interpolate here; inner
# shell variables are escaped so they evaluate in the pod.
#
# The leading sleep is not superstition: `kubectl run -i` can miss the
# first lines of output if the container writes before the attach
# settles, which silently truncated two earlier hand-runs of this probe.
# This script reads `kubectl logs` after completion instead, so the sleep
# is belt-and-braces, but it costs 2s and removes a whole class of
# flake report.
read -r -d '' PROBE_SCRIPT <<EOF || true
set -u
sleep 2
tok=\$(curl -s -H "Metadata-Flavor: Google" \
    "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token" \
    | sed -e 's/.*"access_token":"//' -e 's/".*//')
if [ -z "\$tok" ]; then echo "RESULT token-mint FAILED"; exit 1; fi
api="${APISERVER}/apis/apps/v1/namespaces"
core="${APISERVER}/api/v1/namespaces"
patch='{"spec":{"replicas":1}}'
cmpatch='{"data":{"gated-apply-probe":"x"}}'

code() {
    # \$1 verb, \$2 url, \$3 label, \$4 body
    if [ "\$1" = "GET" ]; then
        c=\$(curl -sk -o /dev/null -w '%{http_code}' \
            -H "Authorization: Bearer \$tok" "\$2")
    else
        c=\$(curl -sk -o /dev/null -w '%{http_code}' -X "\$1" \
            -H "Authorization: Bearer \$tok" \
            -H "Content-Type: application/strategic-merge-patch+json" \
            -d "\$4" "\$2")
    fi
    echo "RESULT \$3 \$c"
}

# The existence control runs FIRST, and it is a precondition rather than an
# assertion. "Nothing was mutated" rests entirely on ${TARGET} not existing;
# run the PATCHes first and a Deployment that does exist has already been
# scaled to one replica by the time the GET reports it.
#
# ONLY 404 continues. 403 is not absence — it is "you cannot see", which is
# a different fact and not the one this precondition needs. It is also a
# shape this very component can produce: the Role grants patch and not get,
# so a credential that can write without being able to read is exactly what
# lives here once the IAM read grant is missing or scoped away. Accept 403
# and the probe patches an object whose existence it never established.
code GET "\$api/${TARGET_NS}/deployments/${TARGET}" get-target
if [ "\$c" != 404 ]; then
    echo "RESULT abort-not-absent \$c"
    exit 0
fi

code PATCH  "\$api/${TARGET_NS}/deployments/${TARGET}" patch-target  "\$patch"
code DELETE "\$api/${TARGET_NS}/deployments/${TARGET}" delete-target "\$patch"
code PATCH  "\$api/default/deployments/${TARGET}"      patch-default "\$patch"
code PATCH  "\$api/${DEMO_NS}/deployments/${TARGET}"   patch-own     "\$patch"

# The RESOURCE axis. Without this the probe tests only verbs and
# namespaces, and a Role widened to apiGroups:["*"] resources:["*"]
# verbs:["patch"] passes every other assertion unchanged — while being
# able to patch Secrets, ServiceAccounts and RoleBindings in TARGET_NS.
# A ConfigMap is the cheapest witness: core group, same namespace, same
# verb, and outside the Role's resource list.
code PATCH "\$core/${TARGET_NS}/configmaps/${TARGET}" patch-configmap "\$cmpatch"
EOF

${K} delete pod "${POD}" --ignore-not-found --wait=true >/dev/null 2>&1 || true

# Installed before the run, not after: a `kubectl run` that creates the pod
# and then fails would otherwise leave it behind with no handler.
cleanup() { ${K} delete pod "${POD}" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT

# shellcheck disable=SC2086
${K} run "${POD}" \
    --image="${PROBE_IMAGE}" \
    --restart=Never \
    --overrides="{\"spec\":{\"serviceAccountName\":\"core-agent-daemon\"}}" \
    --command -- sh -c "${PROBE_SCRIPT}" >/dev/null

# The --overrides above is the entire reason this probe means anything: it
# is what makes the pod authenticate as the daemon. Dropped or malformed,
# the pod silently runs as `default` and every row below becomes a denial
# that looks like a boundary holding. Assert it rather than trust it.
PROBE_SA=$(${K} get pod "${POD}" -o jsonpath='{.spec.serviceAccountName}')
if [[ "${PROBE_SA}" != "core-agent-daemon" ]]; then
    echo "✗ probe pod is running as '${PROBE_SA}', not core-agent-daemon" >&2
    echo "  Its results would describe the wrong identity. Check --overrides." >&2
    exit 1
fi

if ! ${K} wait --for=jsonpath='{.status.phase}'=Succeeded "pod/${POD}" --timeout=120s >/dev/null 2>&1; then
    echo "✗ probe pod did not complete; logs follow" >&2
    ${K} logs "${POD}" >&2 || true
    ${K} describe pod "${POD}" >&2 || true
    exit 1
fi

OUT=$(${K} logs "${POD}")

# Expected codes. 404 means AUTHORIZED (the object does not exist); 403
# means DENIED. Only the first row differs between the two modes — which
# is the point: the grant must be exactly one verb in exactly one
# namespace, so everything else is identical either way.
if [[ "${MODE}" == "granted" ]]; then
    want_patch_target=404
else
    want_patch_target=403
fi

fail=0
check() {
    local label="$1" want="$2" desc="$3"
    local got
    got=$(printf '%s\n' "${OUT}" | sed -n "s/^RESULT ${label} //p")
    if [[ -z "${got}" ]]; then
        echo "  ✗ ${label}: no result in probe output — ${desc}"
        fail=1
        return
    fi
    if [[ "${got}" == "${want}" ]]; then
        echo "  ✓ ${label}: ${got} — ${desc}"
    else
        echo "  ✗ ${label}: got ${got}, want ${want} — ${desc}"
        fail=1
    fi
}

ABORTED=$(printf '%s\n' "${OUT}" | sed -n 's/^RESULT abort-not-absent //p')
if [[ -n "${ABORTED}" ]]; then
    echo "✗ the opening GET for ${TARGET_NS}/${TARGET} answered ${ABORTED}, not 404," >&2
    echo "  so the probe could not establish that the target is absent and stopped" >&2
    echo "  rather than patch it." >&2
    if [[ "${ABORTED}" == "403" ]]; then
        echo "  403 means the token cannot read Deployments in ${TARGET_NS} — check the" >&2
        echo "  IAM read grant (roles/container.viewer). It does NOT mean 'absent'." >&2
    else
        echo "  Delete whatever is at that name, or set a different probe name." >&2
    fi
    exit 1
fi

echo "== boundary =="
check patch-target    "${want_patch_target}" "PATCH in ${TARGET_NS} ($([[ ${MODE} == granted ]] && echo 'the grant' || echo 'must be denied before the component is applied'))"
check delete-target   403 "DELETE in ${TARGET_NS} — the Role grants patch, and only patch"
check patch-default   403 "PATCH in default — namespace scope holds"
check patch-own       403 "PATCH in ${DEMO_NS} — it cannot patch its own namespace either"
check patch-configmap 403 "PATCH a ConfigMap in ${TARGET_NS} — the Role's resource list holds, not just its verb"
check get-target      404 "GET in ${TARGET_NS} — the IAM read grant, unchanged by any of this"

if (( fail )); then
    echo
    echo "✗ gated-apply boundary does NOT match mode=${MODE}" >&2
    if [[ "${MODE}" == "granted" ]]; then
        echo "  If patch-target is 403, the RoleBinding subject almost certainly does" >&2
        echo "  not match. A wrong subject is silent. The committed file currently says:" >&2
        echo "    ${GATED_APPLY_SUBJECT}" >&2
        echo "  Check that the project id in it is yours (set-up-demo.sh substitutes it)" >&2
        echo "  and that the bracket is the namespace the daemon actually runs in — which" >&2
        echo "  is what deploy/base sets, not DEMO_NS. Compare against:" >&2
        echo "    kubectl -n ${DEMO_NS} get sa core-agent-daemon -o name" >&2
    fi
    echo "  raw probe output:" >&2
    printf '%s\n' "${OUT}" >&2
    exit 1
fi

echo
echo "✓ gated-apply boundary matches mode=${MODE}"
echo "  Nothing was mutated: the opening GET confirmed no Deployment named"
echo "  ${TARGET} exists in ${TARGET_NS}, and the three requests that could"
echo "  have written anywhere else were all refused (403)."
