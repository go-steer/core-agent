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

# Shared environment for the gke-platform-agent rig. `source` this from
# every other script in this directory; nothing here executes anything
# against a cluster.
#
# Everything is overridable from the environment, so the normal way to
# point the rig at your cluster is an env file you keep outside the repo:
#
#   cat > ~/.gke-platform-agent.env <<'EOF'
#   export PROJECT_ID=acme-platform-1234
#   export CLUSTER_NAME=prod-us-central1
#   export KUBE_CONTEXT=gke_acme-platform-1234_us-central1_prod-us-central1
#   export REGION=us-central1
#   EOF
#   source ~/.gke-platform-agent.env
#   ./scripts/set-up-demo.sh
#
# Editing the defaults below works too. What you must not do is leave
# them — `require_coordinates` refuses to proceed on a placeholder, on
# purpose: a rig that silently deployed into whatever cluster kubectl
# happened to be pointing at is worse than one that stops.

# ── Cluster / project ────────────────────────────────────────────────
# PROJECT_ID falls back to your active gcloud project; the rest have no
# safe fallback and must be supplied.
export PROJECT_ID="${PROJECT_ID:-$(gcloud config get-value project 2>/dev/null)}"
export CLUSTER_NAME="${CLUSTER_NAME:-your-cluster}"
export KUBE_CONTEXT="${KUBE_CONTEXT:-$(kubectl config current-context 2>/dev/null)}"
export REGION="${REGION:-us-central1}"           # GKE + Artifact Registry region

# The Vertex endpoint, which is NOT the cluster's region. Folding the two
# together sends `gke` calls to the wrong place and model calls to a
# region that may not serve the model — and it fails asymmetrically,
# because the daemon boots either way. "global" serves every model this
# recipe can run; a regional endpoint may not.
export VERTEX_LOCATION="${VERTEX_LOCATION:-global}"

export DEMO_NS="${DEMO_NS:-gke-platform-agent}"  # namespace the daemon runs in
export TARGET_NS="${TARGET_NS:-online-boutique}" # namespace we break to raise an incident

# Placeholders that must be replaced before anything is applied. Called
# by every script that touches the cluster.
require_coordinates() {
    local bad=0
    for var in PROJECT_ID CLUSTER_NAME KUBE_CONTEXT REGION; do
        local val="${!var}"
        case "${val}" in
            ""|your-*|my-*|CHANGE*)
                echo "✗ ${var} is unset or still a placeholder (got '${val}')" >&2
                bad=1
                ;;
        esac
    done
    if (( bad )); then
        echo "  Set them in the environment or edit scripts/prereqs.sh." >&2
        return 1
    fi

    return 0
}

# DEMO_NS is not a coordinate you can move the deployment with, and this is
# the only place that says so. deploy/base hardcodes the daemon's namespace
# (00-namespace.yaml, 10-serviceaccount-daemon.yaml, and the watcher's
# suffixed cluster-scoped names); DEMO_NS is only ever a `kubectl -n`
# argument, and nothing substitutes it into base. Override it and nothing
# moves.
#
# WHO MUST CALL THIS, and why it is not folded into require_coordinates.
# The damage is not uniform. Scripts that CREATE, GRANT or DELETE anything
# named from DEMO_NS desynchronise it from where the daemon actually runs:
#
#   gen-tokens.sh        creates the Secrets somewhere the daemon will not
#                        look
#   grant-iam.sh         binds Workload Identity on a namespace with no
#                        workloads — no error, the daemon simply never gets
#                        a token
#   set-up-demo.sh       applies base into the hardcoded namespace, then
#                        talks to another one, and writes the gated-apply
#                        subject
#   teardown.sh          deletes a namespace that was never created and
#                        leaves the real one standing
#   debug-pod.sh         creates a Pod as `default`, a ServiceAccount that
#                        exists in EVERY namespace, so the create succeeds
#                        wherever it is pointed
#   verify-gated-apply.sh creates a probe Pod as the daemon's ServiceAccount
#                        and reports on an identity it read from a file, so
#                        under a mismatch it measures one principal and
#                        names another
#
# The last two read as diagnostics and were classified read-only in the
# first draft of this split. They are not: "does it create a Pod" is the
# question, not "is its purpose to look at something".
#
# Scripts that genuinely only read — attach.sh (port-forward),
# break-workload.sh (works entirely in TARGET_NS), build-content-image.sh
# (never mentions DEMO_NS) — fail loudly and harmlessly against an empty
# namespace. So does dev/uat/gke-drill, which sources this file and runs
# its dryrun against a fake kubectl with deliberately distinct coordinates
# so that a hardcoded name anywhere in the drill is caught; refusing there
# would make DEMO_NS the one coordinate it cannot vary.
#
# The parse is strict on purpose. This is `sed`, not a YAML parser, so the
# only safe thing it can do is refuse anything it was not written for:
# `head -1` on a file that grew a second document, or a second `  name:`
# under some other key, would silently pick a namespace nobody meant and
# then compare DEMO_NS against it. So: exactly one `kind:` line, and it
# must be Namespace; exactly one two-space `name:` line. If this manifest
# is ever restructured, this refuses rather than guesses, and the caller
# stops before it applies anything. TestDemoNSGuardParsesTheNamespace
# runs this function against mutated copies of the manifest to prove it.
require_demo_ns_matches_base() {
    local ns_manifest="${DEMO_DEPLOY_DIR}/base/00-namespace.yaml"
    if [[ ! -f "${ns_manifest}" ]]; then
        echo "✗ no deployment namespace manifest at ${ns_manifest}" >&2
        return 1
    fi

    local kinds names base_ns
    kinds=$(grep -c '^kind:' "${ns_manifest}")
    names=$(grep -cE '^  name:[[:space:]]' "${ns_manifest}")
    if [[ "${kinds}" != 1 || "${names}" != 1 ]] \
        || ! grep -q '^kind: Namespace$' "${ns_manifest}"; then
        echo "✗ ${ns_manifest} is not the single Namespace object this expects" >&2
        echo "  (found ${kinds} 'kind:' and ${names} 'name:' lines). It is read" >&2
        echo "  with sed, not a YAML parser, so it refuses rather than guess" >&2
        echo "  which namespace the daemon is deployed into." >&2
        return 1
    fi

    # Trailing whitespace is stripped rather than carried into the compare.
    # A name line ending in one space fails closed — which is safe — but it
    # bricks every mutating script while showing the operator two strings
    # that are visually identical: "DEMO_NS is 'x', but this deploy tree
    # hardcodes 'x '". An unreadable diagnostic on a fail-closed path is
    # still a defect, because the operator's next move is to doubt the
    # check rather than the file.
    base_ns=$(sed -nE 's/^  name:[[:space:]]+(.*[^[:space:]])[[:space:]]*$/\1/p' "${ns_manifest}")
    if [[ -z "${base_ns}" ]]; then
        echo "✗ could not read the deployment namespace from ${ns_manifest}" >&2
        return 1
    fi
    # The sed reads a bare scalar and only a bare scalar. `name: "x"` and
    # `name: x # a comment` are both valid YAML that kustomize and kubectl
    # accept, and both would come back through that expression with the quotes
    # or the comment still attached — failing closed, but reporting
    # `hardcodes '"x"'`, which is the same visually-confusing diagnostic the
    # trailing-whitespace strip above exists to prevent. So the value is
    # checked against the shape of an RFC 1123 label and the refusal says
    # what it found, rather than comparing a mangled string and blaming
    # DEMO_NS for the mismatch. This is a PARSE check, not an admission
    # check — it asks "did the sed read a name, or some YAML around a
    # name", and the API server owns everything else about the value,
    # length included.
    if [[ ! "${base_ns}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]]; then
        echo "✗ ${ns_manifest} has 'name: ${base_ns}', which is not a bare" >&2
        echo "  namespace name. This file is read with sed, not a YAML parser," >&2
        echo "  so a quoted value or a trailing '# comment' is carried through" >&2
        echo "  verbatim instead of being understood. Write the name unquoted" >&2
        echo "  and uncommented." >&2
        return 1
    fi
    if [[ "${DEMO_NS}" != "${base_ns}" ]]; then
        echo "✗ DEMO_NS is '${DEMO_NS}', but this deploy tree hardcodes '${base_ns}'." >&2
        echo "  DEMO_NS does not move the deployment — it is only a 'kubectl -n'" >&2
        echo "  argument — so overriding it desynchronises the Secrets, the IAM" >&2
        echo "  bindings, teardown and the gated-apply subject from where the" >&2
        echo "  daemon actually runs. Unset DEMO_NS, or rename the namespace in" >&2
        echo "  ${DEMO_DEPLOY_DIR}/base and update this default to match." >&2
        return 1
    fi
    return 0
}

# Does a rendered manifest (on stdin) actually compose the gated-apply
# component? Exit 0 if yes.
#
# NOT `grep -q gated-apply`. That was the first spelling and it is wrong
# on the DEFAULT below-floor path: overlays/initcontainer-copy delivers
# the content by running `cp -a … /gated-apply …` in an initContainer,
# because every flavor of the content image carries that directory
# whether or not any overlay selects it. So the literal string appears in
# the rendered Deployment of a deployment that composes no component at
# all, and both initcontainer-copy overlays matched.
#
# Nothing was mis-granted by that — kubectl applies the objects the
# overlay names, and a read-only overlay names none of them. What it did
# was rewrite two tracked files under deploy/components/gated-apply and
# print "→ gated-apply: … in …", which is a claim about authorization
# that was not true, on the path an operator is least able to check.
#
# Match on an OBJECT the component contributes instead, per document, the
# way set-up-demo.sh's rendered_daemon_ns does. A directory path in a
# command argument cannot look like a Role.
renders_gated_apply() {
    awk '
        function emit() { if (kind == "Role" && name ~ /^gated-apply-/) found = 1 }
        /^---[[:space:]]*$/   { emit(); kind=""; name=""; next }
        /^kind:[[:space:]]/   { kind = $2; next }
        /^  name:[[:space:]]/ { if (name == "") name = $2; next }
        END { emit(); if (found) exit 0; exit 1 }
    '
}

# ── Content source ───────────────────────────────────────────────────
# This recipe is SELF-CONTAINED — no content_roots, no vendored
# upstream/, no @include — so the content image is built straight from
# the recipe directory. There is no "refresh the content from somewhere
# else" step, and that is the thesis rather than a convenience; see
# ../README.md.
SCRIPT_DIR="${SCRIPT_DIR:-$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )}"
export RECIPE_ROOT="$( cd -- "${SCRIPT_DIR}/.." &> /dev/null && pwd )"

# ── Model flavor: Gemini or Anthropic, both via Vertex ───────────────
#   gemini     (default) parent gemini-3.7-flash, specialist gemini-3.7-flash
#   anthropic            parent claude-opus-5,    specialist claude-sonnet-5
#
# Switching costs NO IAM change. core-agent's "anthropic-vertex" provider
# authenticates with Application Default Credentials through
# google.FindDefaultCredentials (pkg/models/anthropic/vertex.go) and calls
# aiplatform.googleapis.com — the same host, the same token, the same
# roles/aiplatform.user grant the Gemini path already uses. Verified from
# the daemon KSA itself rather than from a laptop: a pod on
# `core-agent-daemon` holding only the Workload Identity direct binding
# got HTTP 200 from claude-opus-5, claude-sonnet-5 and claude-haiku-4-5.
#
# The parent/specialist SPLIT is kept in both flavors, so one run
# exercises two models and the specialist — where the long diagnostic
# reasoning happens — can be the cheaper one.
#
# VERTEX_LOCATION is load-bearing for the anthropic flavor: Claude on
# Vertex is served by the global endpoint, and a regional one 404s.
export MODEL_FLAVOR="${MODEL_FLAVOR:-gemini}"

export ANTHROPIC_PARENT_MODEL="${ANTHROPIC_PARENT_MODEL:-claude-opus-5}"
export ANTHROPIC_CLUSTER_MODEL="${ANTHROPIC_CLUSTER_MODEL:-claude-sonnet-5}"

# The cost ceilings are re-scaled for the anthropic flavor, and MUST be:
# leaving the Gemini numbers in place would trip the guardrail
# mid-incident, and a tripped ceiling reads as an agent failure to
# anyone who was not watching the ledger. claude-opus-5 is $5/$25 per
# MTok against gemini-3.7-flash's $0.75/$3.75 — ~6.7x on both rates.
#
# These are ABSOLUTE numbers sized against opus-5's own rates and
# validated in a live run; they are not derived from the Gemini pair, so
# 2.0/20.0 being 4x of the config's 0.5/5.0 while the rate ratio is 6.7x
# is not a discrepancy to "fix". Raise them if a real incident trips the
# ceiling.
#
# Caching is not a differentiator between the flavors. Vertex context
# caching still installs only on *gemini.Provider
# (pkg/compose/context_cache.go), but #772 turned Anthropic PROMPT
# caching on by default for the whole family including anthropic-vertex —
# cache_control breakpoints ride the ordinary Messages request, with no
# separate cache resource — so cached input on opus-5 bills at $0.50/MTok
# rather than the full $5.00.
export ANTHROPIC_MAX_TURN_COST_USD="${ANTHROPIC_MAX_TURN_COST_USD:-2.0}"
export ANTHROPIC_MAX_SESSION_COST_USD="${ANTHROPIC_MAX_SESSION_COST_USD:-20.0}"

# The only config committed in this repo is the Gemini one. The anthropic
# variant is DERIVED from it at image-build time by build-content-image.sh
# (four values move: the parent model, the specialist model, and the
# provider on each), so both flavors ship in one image and switching is a
# redeploy rather than a rebuild. Deriving beats committing a second copy:
# two hand-maintained configs drift, and the drift is invisible until the
# flavor you don't normally run is the one in front of a customer.
case "${MODEL_FLAVOR}" in
    gemini)    export AGENT_CONFIG_BASENAME="config.hub.json" ;;
    anthropic) export AGENT_CONFIG_BASENAME="config.hub.anthropic.json" ;;
    *)
        echo "✗ MODEL_FLAVOR must be 'gemini' or 'anthropic' (got '${MODEL_FLAVOR}')" >&2
        return 1 2>/dev/null || exit 1
        ;;
esac

# ── Content image (Artifact Registry in your project) ────────────────
# The content ships as an OCI image volume, so it must live in a registry
# the GKE node SA can pull. Artifact Registry in the same project is the
# zero-config choice; build-content-image.sh creates the repo if missing
# and pushes both flavors.
export AR_REPO="${AR_REPO:-core-agent-recipes}"
export CONTENT_IMAGE="${CONTENT_IMAGE:-${REGION}-docker.pkg.dev/${PROJECT_ID}/${AR_REPO}/gke-platform-agent-content}"

# Bump on every content change.
#
# A PUSHED TAG IS SPENT. Re-pushing a live tag does not redeploy: the
# image volume and the initContainer both use imagePullPolicy:
# IfNotPresent, so a node holding the cached layer keeps serving the old
# content and the rollout becomes a coin flip decided by which node the
# pod lands on. Take the NEXT number instead.
#
# The registry is the only oracle for what is spent. This comment is not:
#   gcloud artifacts docker tags list "${CONTENT_IMAGE}" --project="${PROJECT_ID}"
export CONTENT_TAG="${CONTENT_TAG:-v4}"

# ── Published images: NOT set here ───────────────────────────────────
# The daemon and watcher pins live in the overlays' `images:` blocks
# (deploy/overlays/*/kustomization.yaml), and set-up-demo.sh does not
# touch them.
#
# They belong in the tree rather than in this file because which release
# deploys is a property of the recipe, not of your environment: it is the
# thing a reviewer needs to see, and `recipecheck`'s deploy-pin gate
# reads those manifests and fails a pin that floats or sits below the
# recipe config's declared floor of 2.9.0-dev.1. A shell variable is
# invisible to both. Only the CONTENT image is set from here, because
# only that one is built by you and has no published copy.
#
# Two things worth knowing if you go and edit those blocks. The GHCR tag
# for this repo's own images has NO leading `v` ("2.9.0"); lookout's
# does ("v0.23.0"). And an OLDER daemon does not fail on a newer recipe —
# pkg/config has no DisallowUnknownFields, so it boots clean, drops the
# blocks it does not know, and runs a persona instructing the model to
# call tools it never registered.
#
# The lookout pin has a floor of its own: v0.22.0, which is where
# /readyz, the ingressclasses/storageclasses grants and the watcher
# Deployment's `strategy: Recreate` came from. An older image under this
# base 404s the readiness probe forever and never goes Ready. That pin is
# tracked automatically — internal/imagepin walks `examples/`, so the
# weekly lookout-pin-check job (#787) finds this recipe without being
# told about it and opens a bump PR when upstream moves.

# ── Identities (must match .agents/config.hub.json) ──────────────────
# The watcher POSTs as WATCHER_IDENTITY while asserting ADMIN_IDENTITY as
# the caller (its --owner flag), so operators own — and can attach to —
# the incident sessions it opens.
export ADMIN_IDENTITY="${ADMIN_IDENTITY:-platform-oncall@example.com}"
export WATCHER_IDENTITY="${WATCHER_IDENTITY:-sa:lookout-watch}"

# ── Layout ───────────────────────────────────────────────────────────
export DEMO_DIR="${RECIPE_ROOT}"
export DEMO_DEPLOY_DIR="${DEMO_DIR}/deploy"

# FOUR read-only overlays, two orthogonal axes. Content DELIVERY (image
# volume vs initContainer copy) is forced by the cluster's Kubernetes
# version; TRACING (on vs off) is forced by whether the cluster has GKE
# Managed OpenTelemetry enabled. set-up-demo.sh probes for both and picks.
#
# The two *-otel dirs are thin composers: they add the otel-gke component
# and nothing else. Crucially they carry NO `images:` block and no
# per-cluster patches — set-up-demo.sh writes all of that into the
# DELIVERY overlay, and it flows through the composition. So the pins
# above stay the single source of truth no matter which of the four is
# applied.
export DEMO_OVERLAY_DIR="${DEMO_DEPLOY_DIR}/overlays/example"                            # image volume (the default target)
export DEMO_OVERLAY_COPY_DIR="${DEMO_DEPLOY_DIR}/overlays/initcontainer-copy"            # fallback delivery
export DEMO_OVERLAY_OTEL_DIR="${DEMO_DEPLOY_DIR}/overlays/example-otel"                  # image volume + tracing
export DEMO_OVERLAY_COPY_OTEL_DIR="${DEMO_DEPLOY_DIR}/overlays/initcontainer-copy-otel"  # fallback + tracing

# Two MORE, for the apply-capable leg. This is a THIRD axis, and it is not
# like the other two: nothing about a cluster decides it, an operator
# does. So it is composed rather than enumerated — the wiring lives once
# in deploy/components/gated-apply and these two dirs are worked examples
# of composing it onto the image-volume path. Enumerating the axis would
# have made deploy/overlays a 2x2x2 to express one choice.
#
# Below the image-volume floor there is no shipped gated overlay: change
# the `resources:` entry in overlays/gated-apply to ../initcontainer-copy
# (the initContainer already copies /gated-apply out of the image for
# exactly this). Not shipped because it has no consumer, and an unshipped
# overlay is an untested one.
export DEMO_OVERLAY_GATED_DIR="${DEMO_DEPLOY_DIR}/overlays/gated-apply"            # image volume + apply leg
export DEMO_OVERLAY_GATED_OTEL_DIR="${DEMO_DEPLOY_DIR}/overlays/gated-apply-otel"  # image volume + apply leg + tracing

# Where the content image is mounted in the daemon pod. Kept in one place
# because debug-pod.sh must mount it at exactly the same path the daemon
# does, or its assertions prove nothing. Declared before the LEG switch
# below, which derives AGENTS_ROOT from it.
export CONTENT_MOUNT="/opt/gke-platform-agent"

# ── LEG: read-only (default) | d1 | d2 ───────────────────────────────
# Which POSTURE to deploy. `readonly` is the recipe as scenarios A/B/C
# run it. d1 and d2 both select the gated-apply content root and the
# `patch` grant that goes with it; they differ by one config field.
#
#   d1  permissions.mode "ask"    — the patch waits for a human. ATTENDED:
#                                   nothing answers a prompt in a pod, so
#                                   somebody has to be on the TUI.
#   d2  permissions.mode "allow"  — the patch is allowlisted, nothing
#                                   prompts, deny-by-default for anything
#                                   unlisted. This is the leg an
#                                   unattended soak can actually exercise.
#
# Run d1 before d2 on a given day: under d1 the worst case is a bad
# proposal, under d2 it is a bad patch on a live workload.
# See docs/gated-apply-design.md.
export LEG="${LEG:-readonly}"

# AGENTS_ROOT is the directory inside the content mount whose `.agents/`
# the daemon loads. Every path that depends on which root is selected is
# derived from this one variable — the `-c` value, and the writable plans
# mount that has to sit beside it, because record_plan derives plansDir =
# agentsDir + "/plans". debug-pod.sh reads it too, so its assertions run
# against the same root the daemon does.
case "${LEG}" in
    readonly)
        export AGENTS_ROOT="${CONTENT_MOUNT}"
        export LEG_CONFIG_BASENAME="${AGENT_CONFIG_BASENAME}"
        ;;
    d1|d2)
        export AGENTS_ROOT="${CONTENT_MOUNT}/gated-apply"
        export LEG_CONFIG_BASENAME="config.${LEG}.json"
        # The gated-apply root ships gemini configs only.
        # build-content-image.sh derives the *.anthropic variants for
        # .agents/config.hub.json and .agents/config.json and nothing
        # else, so MODEL_FLAVOR=anthropic here would point -c at a file
        # that is not in the image — a crash-loop several minutes after
        # this script reports success. Refuse instead.
        if [[ "${MODEL_FLAVOR}" != "gemini" ]]; then
            echo "✗ LEG=${LEG} requires MODEL_FLAVOR=gemini (got '${MODEL_FLAVOR}')." >&2
            echo "  The gated-apply content root ships no anthropic config variant:" >&2
            echo "  build-content-image.sh renders those for .agents/ only." >&2
            return 1 2>/dev/null || exit 1
        fi
        ;;
    *)
        echo "✗ LEG must be 'readonly', 'd1' or 'd2' (got '${LEG}')" >&2
        return 1 2>/dev/null || exit 1
        ;;
esac

# The single spelling of the daemon's -c argument. set-up-demo.sh writes
# this into a patch file and then asserts the RENDERED manifest carries
# it; debug-pod.sh asserts the file exists in the mount.
export AGENT_CONFIG_PATH="${AGENTS_ROOT}/.agents/${LEG_CONFIG_BASENAME}"

# Per-run state the rig produces: the operator's bearer token, and the
# users.json bearer table on its way into a Secret. Deliberately under
# TMPDIR and not in the checkout. Both are live credentials for the
# running daemon, and a .gitignore entry is a weaker guarantee than a
# path that was never inside the repository — one `git add -f`, one
# `cp -r` of the recipe directory, one archive of the worktree, and a
# gitignored secret has travelled anyway.
export RIG_STATE_DIR="${RIG_STATE_DIR:-${TMPDIR:-/tmp}/gke-platform-agent}"

# Convenience (best-effort; harmless if gcloud is not yet configured).
export PROJECT_NUMBER="$(gcloud projects describe "${PROJECT_ID}" --format='value(projectNumber)' 2>/dev/null)"

# ── Cross-deployment race guard (read-only) ──────────────────────────
# Every lookout-watch watches Events CLUSTER-WIDE, and separate watcher
# Deployments share no dedup window. So if another watcher is running —
# examples/gke-troubleshoot-agent's in `agent-triage`, say, which targets
# the same online-boutique — breaking a workload fires an incident into
# ITS daemon too, and the two race for the event. This prints any active
# watcher OUTSIDE ${DEMO_NS}, one namespace per line; empty means clear.
foreign_watchers() {
    kubectl --context "${KUBE_CONTEXT}" get deploy -A \
        -l app.kubernetes.io/name=lookout-watch \
        -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.status.availableReplicas}{"\n"}{end}' 2>/dev/null \
        | awk -v ns="${DEMO_NS}" '$1 != ns && $2+0 > 0 { print $1 }'
}

# Warn (to stderr) about any foreign watcher and print the exact scale-down
# command. Returns 0 if clear, 1 if a foreign watcher is active.
warn_foreign_watchers() {
    local found; found=$(foreign_watchers)
    [[ -z "${found}" ]] && return 0
    {
        echo "⚠ another lookout-watch is active and watches the SAME cluster-wide"
        echo "  events this deployment does — it will race for incidents. Quiesce it first:"
        while IFS= read -r fns; do
            [[ -n "${fns}" ]] && echo "      kubectl --context ${KUBE_CONTEXT} -n ${fns} scale deploy/lookout-watch --replicas=0"
        done <<< "${found}"
        echo "  (restore later with --replicas=1)"
    } >&2
    return 1
}
