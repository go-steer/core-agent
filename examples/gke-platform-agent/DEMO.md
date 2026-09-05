# Running `gke-platform-agent` on a live GKE cluster

This is the operator walkthrough: deploy the hub and its watcher, break a real
workload, watch the agent triage the resulting incident, tear it down. It is
also the rig the GKE drill
([#970](https://github.com/go-steer/core-agent/issues/970)) scores against.

Everything here needs a cluster. For a credential-free look at the recipe, see
[`README.md`](README.md) — the recipe runs locally against Vertex with no
Kubernetes at all, and `recipe_test.go` validates its shape in ordinary CI.

> **This costs money.** A run puts a Gemini-backed daemon on a cluster and lets
> it read that cluster on every incident. The config caps spend at $0.50/turn
> and $5.00/session, but the cluster, the Artifact Registry repo and the Cloud
> Trace spans are all billable and outlive the session. Run
> [`./scripts/teardown.sh`](#teardown) when you are done.

## What gets deployed

Two Deployments in one namespace (`gke-platform-agent` by default):

- **`core-agent`** — the hub. Loads the recipe content from an OCI image
  volume, listens on `:7777` with bearer auth against a `users.json` table, and
  serves one session per incident plus whatever sessions an operator opens.
- **`lookout-watch`** — [lookout](https://github.com/go-steer/lookout), watching
  the whole cluster and turning what it sees into incident *injects* against the
  hub. Each inject opens a session; the hub triages it.

The agent has no write path to the cluster. Its only Kubernetes access is the
read-only GKE MCP endpoint, and `bash`, `write_file`, `edit_file` and
`delete_file` are all disabled — so "propose-only" is a property of the toolset
rather than a request in the persona. See [the enforcement table in
`README.md`](README.md#constraints-are-enforced-not-requested).

## Prerequisites

- A GKE cluster you are willing to break a workload in, and `kubectl` context
  for it.
- A workload to break. The defaults assume [Online
  Boutique](https://github.com/GoogleCloudPlatform/microservices-demo)'s
  `emailservice` in namespace `online-boutique`; override with `WORKLOAD` and
  `TARGET_NS`.
- `gcloud`, `kubectl`, `kustomize`, `docker`, `jq`, `python3`, `openssl`.
- `core-agent-tui` on your `PATH` for the attach step:
  `go install github.com/go-steer/core-agent/v2/cmd/core-agent-tui@latest`.
- Workload Identity Federation enabled on the cluster, and the daemon KSA bound
  to `roles/aiplatform.user`. The agent authenticates to Vertex as
  `core-agent-daemon` in the deployment namespace.

## Set your coordinates

Every script sources [`scripts/prereqs.sh`](scripts/prereqs.sh), which reads
its values from the environment and falls back to placeholders. Keep them in a
file outside the repository:

```sh
cat > ~/.gke-platform-agent.env <<'EOF'
export PROJECT_ID=acme-platform-1234
export CLUSTER_NAME=prod-us-central1
export KUBE_CONTEXT=gke_acme-platform-1234_us-central1_prod-us-central1
export REGION=us-central1
EOF
source ~/.gke-platform-agent.env
```

Editing the defaults in `prereqs.sh` works too. What you cannot do is leave
them: every script calls `require_coordinates` and refuses to run against a
value that still looks like `your-cluster`, and `set-up-demo.sh` re-checks the
*rendered manifest* for surviving placeholders before it applies anything.

## The run

All commands are from the recipe directory.

```sh
./scripts/build-content-image.sh   # build + push the content image to Artifact Registry
./scripts/gen-tokens.sh            # bearer tokens -> users.json Secret + watcher Secret
./scripts/set-up-demo.sh           # deploy hub + watcher; verify the content mount
./scripts/attach.sh --new          # operator TUI, fresh session (for general prompts)
./scripts/break-workload.sh        # break emailservice -> incident inject -> new session
./scripts/attach.sh                # hub picker; open the incident session
./scripts/teardown.sh              # namespace + cluster-scoped RBAC + local token stash
```

Live bearer tokens are written to `${TMPDIR:-/tmp}/gke-platform-agent/`, never
into the checkout. `attach.sh` reads them from there and passes the token to
the TUI *by variable name*, so it never reaches `argv` or `ps`.

`set-up-demo.sh` probes the cluster for two independent things and picks one of
four overlays — see [Content delivery](#content-delivery-overlayexamplecopy) and
[Tracing](#tracing-otel10). On the image-volume path it then runs
`./scripts/debug-pod.sh check`, which mounts the content image exactly as the
daemon does and asserts the mechanics the daemon cannot self-report (it is
distroless — there is no shell to exec into): content present, the selected
config file there, no stray `upstream/`, the `cluster/` subagent root with its
six skills, the nested writable `plans` emptyDir, and a read-only content root.

### What to ask

Open a fresh session with `./scripts/attach.sh --new` and try, in order:

1. **"who are you?"** — a direct answer, not an incident report. This is the
   identity probe [the rewrite](README.md#why-a-second-gke-platform-recipe-exists)
   exists to pass.
2. **"what's wrong with `<workload>`?"** — a recorded plan first, then
   delegation to `cluster` with `wait: true`, then a report built on what came
   back.
3. **"apply that fix"** — a refusal that explains the proposal *is* the
   deliverable, without going looking for a repo to edit.

Then break something and attach to the session the watcher opens.

## The six break modes

`./scripts/break-workload.sh <mode>` breaks `emailservice` (override with
`WORKLOAD=`) a different way each time. The watcher runs `--sources=auto`
against the full vendored ClusterRole, so eleven sources resolve — but both of
the original modes land on `k8s-events` alone. The other three reach past it:

| Mode | Failure | Watcher sources | Skill exercised |
| --- | --- | --- | --- |
| `bad-secret` (default) | volume mount of a Secret that doesn't exist → `FailedMount` | k8s-events | workload-troubleshooting |
| `bad-image` | image tag that doesn't exist → `ImagePullBackOff` | k8s-events | workload-troubleshooting |
| `oom` | 8Mi memory limit → `OOMKilled` → `CrashLoopBackOff` | object-state (restart count climbing), k8s-events | workload-troubleshooting |
| `unschedulable` | 200-CPU request → `Pending` / `FailedScheduling` | capacity (pending-pod aging, `NotTriggerScaleUp`), k8s-events | workload-scaling |
| `bad-probe` | readiness probe on a closed port → stalled rollout | rollout (`rollout_stall`), object-state (`progress_deadline`), k8s-events | workload-troubleshooting, reliability |
| `restore` | undo any of the above | — | — |

Two sources these modes deliberately do **not** reach, so nobody writes a
seventh mode expecting them: `saturation` needs metrics-server and trends usage
against requests, so an OOMKill never touches it; and `degradation` by
construction never fires when a ready ratio reaches 0 — a pod that goes NotReady
and stays there is a single transition, which cannot reach its flip threshold.
Those cases belong to `object-state` and `k8s-events` on purpose.

Two details in the modes that look arbitrary and are not:

- **`unschedulable` requests 200 CPUs** because that exceeds the largest GKE
  machine type (`c3-standard-176`), so the cluster autoscaler answers
  `NotTriggerScaleUp` instead of provisioning. Lower it to something a node pool
  could fit and CA scales up for real, the pod schedules, and you are billed for
  the lesson.
- **`bad-probe` drops `progressDeadlineSeconds` to 60** so the deadline signal
  lands in about a minute rather than ten. That field is on the Deployment spec,
  not the pod template, so `rollout undo` does not revert it — `restore` resets
  it explicitly, and only if it still holds the 60 this script set.

**One at a time.** `restore` is `kubectl rollout undo`, which rewinds exactly
one revision, so breaking twice without restoring in between rolls you back to
the *earlier* breakage rather than to health. The script says so if the restore
leaves the workload unhealthy.

The pass condition is the same for every mode: **the parent's report must carry
the `cluster` subagent's actual evidence and proposed patch**, not a bare
"diagnosed the issue" status line. A content-free summary is the failure this
recipe exists to detect, and it is what G1/G3 of the drill rubric score.

### Sharing a cluster with another watcher

Every `lookout-watch` watches Events cluster-wide, and separate watchers share
no dedup window — so one broken workload fires into every hub on the cluster,
and whichever injects first owns the incident. Deploying alongside another
recipe is fine; *breaking a workload* while both are up is a race.

`warn_foreign_watchers` in `prereqs.sh` detects this and prints the scale-down
command. `set-up-demo.sh` warns; `break-workload.sh` prompts before racing
(`FORCE=1` to skip). Teardown is safe either way: every cluster-scoped name is
suffixed with the deployment's namespace, so two deployments tear down
independently.

## Content delivery (`OVERLAY=example|copy`)

The recipe content — `AGENTS.md`, `.agents/`, and the `cluster/` subagent root
— ships as an **OCI image volume**, not a ConfigMap. At ~1.3 MiB it is over the
ConfigMap limit, and an image is the artifact that already travels with a
registry.

Image volumes are beta from Kubernetes 1.33 and enabled on GKE 1.35+. Below
that, the `initcontainer-copy` overlay pulls the same content as an ordinary
image and an init container copies it into an `emptyDir`. `set-up-demo.sh`
reads the cluster's version and picks; `OVERLAY=example|copy` forces one.

`build-content-image.sh` pushes **two tags** for this reason — `:v1` built
`FROM scratch` for the image-volume path, and `:v1-copy` built `FROM
chainguard/busybox` (the copy path needs a `cp`). The content is byte-identical.

**A pushed tag is spent.** `imagePullPolicy: IfNotPresent` means re-pushing a
live tag reaches only nodes that have not cached the layer, which turns a
rollout into a coin flip. Bump `CONTENT_TAG` when the content changes.

## Tracing (`OTEL=1|0`)

**Tracing is on by default.** `set-up-demo.sh` asks the cluster whether it
serves the `telemetry.googleapis.com` `Instrumentation` CRD — the marker for
GKE Managed OpenTelemetry — and if it does, deploys the `-otel` overlay. Spans
go to Cloud Trace with no collector to run.

That is the second axis, orthogonal to content delivery, which is why there are
four overlays for two decisions:

|                               | tracing off          | tracing on (default)      |
| ----------------------------- | -------------------- | ------------------------- |
| **image volume** (K8s ≥ 1.33) | `example`            | `example-otel`            |
| **initContainer copy**        | `initcontainer-copy` | `initcontainer-copy-otel` |

- `OTEL=0` — deploy without tracing.
- `OTEL=1` — require tracing; **fails** if the CRD is absent, rather than
  deploying something that traces into the void.
- unset (`auto`) — probe, and on a cluster without managed OTel print the exact
  `gcloud` commands to enable it, then carry on untraced.

On the traced path the script checks the two things that fail *silently*: that
GKE's webhook actually injected `OTEL_EXPORTER_OTLP_ENDPOINT`, and that the
daemon KSA holds `roles/cloudtrace.user`. The second is the nastier one —
without it the deploy is healthy, the SDK exports, and Cloud Trace rejects the
spans server-side, so the only symptom is an empty trace list.

**Grant `roles/cloudtrace.user` to both KSAs**, not just the daemon.
`lookout-watch` holds no other GCP role, so it is the one that gets forgotten —
and the result looks fine: a trace with the turn, the tool calls and the
subagent delegation all present, missing only the inject span that started it.
Nothing errors. `set-up-demo.sh` checks both and prints the missing command.

One gotcha when you go looking: Cloud Trace's `+service_name:` filter does
**not** match the OTel `service.name` attribute and returns nothing. Filter on
`k8s.namespace.name:gke-platform-agent` instead.

Everything else — what the component patches, why it is env vars rather than
config, the GKE prereqs, and sampling — is in
[`deploy/components/otel/README.md`](deploy/components/otel/README.md).

## Gemini or Anthropic (`MODEL_FLAVOR`)

The same recipe runs on either model family, both through Vertex:

| `MODEL_FLAVOR`     | parent agent       | `cluster` specialist |
| ------------------ | ------------------ | -------------------- |
| `gemini` (default) | `gemini-3.7-flash` | `gemini-3.7-flash`   |
| `anthropic`        | `claude-opus-5`    | `claude-sonnet-5`    |

```sh
./scripts/build-content-image.sh                    # flavor-agnostic; run once
MODEL_FLAVOR=anthropic ./scripts/set-up-demo.sh     # redeploy only, no rebuild
./scripts/set-up-demo.sh                            # ...and back to gemini
```

Every script sources `prereqs.sh` itself, so a command prefix is enough — there
is nothing to export or re-source first. An unrecognized `MODEL_FLAVOR` exits 1
before anything is applied.

Only `.agents/config.hub.json` (the Gemini one) is committed. The Anthropic
config is **derived at image-build time**: `build-content-image.sh` rewrites
four values with `jq` — both models and both cost ceilings — and stages the
result alongside the original, so the image carries both and `MODEL_FLAVOR`
only decides which one the daemon's `-c` points at. Rendering rather than
checking in a second file is deliberate: everything outside those four values is
identical, and a hand-maintained copy would drift on the first edit that is not
about models. Override the derived values with `ANTHROPIC_PARENT_MODEL`,
`ANTHROPIC_CLUSTER_MODEL`, `ANTHROPIC_MAX_TURN_COST_USD`,
`ANTHROPIC_MAX_SESSION_COST_USD`.

Four things worth knowing before running the Anthropic flavor:

- **No IAM change is needed.** core-agent's `anthropic-vertex` provider
  authenticates with ADC through `google.FindDefaultCredentials` and calls
  `aiplatform.googleapis.com` — same host, same token, same
  `roles/aiplatform.user` as the Gemini path.
- **`VERTEX_LOCATION` must stay `global`.** Claude 5 is served by the global
  Vertex endpoint only; `us-east5`, which is core-agent's
  `anthropic.DefaultVertexRegion`, lists just `claude-3-opus` and
  `claude-sonnet-4-5`. `set-up-demo.sh` refuses the Anthropic flavor with a
  non-`global` location rather than let it 404 at the first model call.
- **Cost ceilings are absolute, sized for `claude-opus-5`** — $2.00/turn and
  $20.00/session against the Gemini flavor's $0.50 and $5.00. They are not a
  multiple of the Gemini numbers: opus-5 is $5.00/$25.00 per MTok against
  `gemini-3.7-flash`'s $0.75/$3.75, so the rate ratio is ~6.7× while the
  ceilings are 4×. Running the Anthropic flavor on the Gemini ceilings trips the
  guardrail mid-incident and reads as an agent failure.
- **The specialist's model is why this is not a `--model` flag.**
  `--provider`/`--model` would move the parent, but `resolveSubagentProvider`
  only falls back to the parent when a subagent does not pin its own model — and
  `cluster` deliberately pins its own. A flag-based switch would leave the
  specialist on Gemini while the parent moved to Claude.

## Teardown

```sh
./scripts/break-workload.sh restore   # put the workload back first
./scripts/teardown.sh                 # namespace, cluster-scoped RBAC, token stash
./scripts/teardown.sh --images        # ...and delete the content images
```

`teardown.sh` deletes the namespace (workloads, Secrets, PVC), the
cluster-scoped and `kube-system` watcher RBAC that a namespace delete would
otherwise orphan, and the local token stash. It leaves the project-level
WIF/IAM bindings alone: those are path-based (namespace + KSA name), so they
stay valid across a delete and recreate.

**Restore the workload before tearing down**, or the broken Deployment stays
broken with nothing left watching it. Note that `restore` exits 0 even if
`rollout undo` failed to produce a healthy workload — read its output rather
than its exit status.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| Daemon `CrashLoopBackOff`, no useful log | Content mount is wrong. Run `./scripts/debug-pod.sh check`. |
| Daemon boots, first model call 403s | `GOOGLE_CLOUD_PROJECT` is a placeholder, or the KSA lacks `roles/aiplatform.user`. |
| Daemon boots, first model call 404s | `GOOGLE_CLOUD_LOCATION` is a region. It is the *Vertex endpoint* and wants `global`; `GKE_LOCATION` is where the cluster lives. |
| Watcher logs `status 401: unauthorized: no valid credential` | Token rotation without a watcher restart. Re-run `./scripts/gen-tokens.sh`, which restarts both Deployments. |
| Watcher logs `asserted-caller header rejected` | Proxy identity mismatch — the watcher's token is valid but its identity is not the configured `proxy_identity`. |
| Incident fires, but the operator's session picker is empty | The watcher's `--owner` is not a key in the bearer table. Both come from `ADMIN_IDENTITY`; `set-up-demo.sh` patches `--owner` from it. |
| Nothing injects at all | Another recipe's watcher took the incident. Check `warn_foreign_watchers`. |
| `attach.sh` reports the port is in use | A stale `kubectl port-forward` that is alive but no longer serving. The script prints the `ss`/`pkill` commands. |
| Traces are empty but everything is healthy | Missing `roles/cloudtrace.user`, or you filtered Cloud Trace on `+service_name:`. |
