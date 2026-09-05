---
name: gke-workload-troubleshooting
description: Diagnose a GKE workload failure you were delegated — image pull failures, crash loops, OOMKills, scheduling failures, mount errors, connectivity timeouts. Read-only; produces a root cause, the evidence for it, and a proposed manifest patch.
---

# GKE workload troubleshooting

This is a **method**, not a mission. It tells you *how* to diagnose the workload
you were asked about. It never tells you *what* to look at or *where* — that came
with your goal, and your goal outranks everything in this file.

## Before anything else: you already have your context

Do **not** open by acquiring context. You were delegated a specific investigation
and the parameters came with it — cluster, project, location, namespace, workload,
and usually the triggering event itself.

- **Never ask the operator.** This runs unattended. Nobody will answer, and a
  question ends your turn with nothing delivered.
- **Never go looking for your coordinates** — not in a config file, not in your
  process environment, not via the knowledge base. If the goal named them, use
  them verbatim.
- **Never enumerate clusters.** Listing clusters to work out which one you mean is
  always wrong: you were told which one. Straying to a cluster you were not asked
  about is the single most expensive mistake available to you.
- **If something you genuinely need is missing from the goal**, say so in your
  report and diagnose as far as the rest of it allows. A bounded partial answer
  beats a hunt.

If the goal named a workload, that workload is your scope. Related failures in
other namespaces are separate incidents — note them in a sentence, do not chase
them.

## Budget: 8 tool calls, or five minutes

Track it as you work. If you reach the budget without a root cause, stop and write
up what you established and what you ruled out. A documented dead end is a real
result; an unbounded hunt costs the parent its remaining session budget and still
returns nothing.

## Tools

Everything here is read-only, through the `gke` MCP. There is **no shell** — no
`kubectl`, no `gcloud`, no `git`. Do not write diagnostic steps as shell commands
and do not describe running them.

Every call that takes a `parent` uses the fully-qualified path from your goal:

```
projects/<project>/locations/<location>/clusters/<cluster>
```

Never a wildcard (`projects/-/locations/-`) — it returns 403 and costs a turn.

Reads you will actually use:

- `gke_get_k8s_resource` — `{parent, namespace, resourceType, name?, outputFormat?}`
  for pods, deployments, replicasets, services, endpoints, networkpolicies.
  Request `outputFormat: "YAML"` only when you need the full spec; the default
  table is much cheaper and usually enough.
- `gke_list_k8s_events` — namespace events, for scheduling / mount / image-pull
  signatures.

**Never invent a tool name.** If a step below implies a capability you do not see
in your registered tools, report that step as unavailable rather than improvising.

---

## Step 1 — classify from what you were already given

The triggering event usually carries the answer's shape in its `reason` and
`message`. Read them first and pick the branch — do not run a generic sweep.

| Signal in the event | Branch |
|---|---|
| `BackOff` / `ErrImagePull` / `ImagePullBackOff` | **Image pull** → Step 2 |
| `CrashLoopBackOff` / `Error` / non-zero exit | **Crash loop** → Step 3 |
| `OOMKilled` / exit code 137 | **Memory** → Step 3 |
| `FailedScheduling` / phase `Pending` | **Scheduling** → Step 4 |
| `FailedMount` / phase `ContainerCreating` | **Mount** → Step 4 |
| Connection timeouts in logs | **Connectivity** → Step 5 |
| Anything else | Start at Step 2's state read, then follow what you find |

**The enrichment may say the pod is gone.** That is common and expected — the pod
named in the event is frequently already replaced by the time you look. It does
not mean the incident is stale. Go to the **controller** (deployment →
replicaset → current pods) and diagnose from live state.

## Step 2 — establish current state

One read of the workload's pods, one of its controller:

- `gke_get_k8s_resource {parent, namespace, resourceType: "pod"}`
- `gke_get_k8s_resource {parent, namespace, resourceType: "deployment", name: <workload>}`

If the pods are healthy and the failure is not reproducible in current state,
**stop and report that**. Rollout and progress-deadline signals in particular
often resolve on their own. Reporting "already recovered, here is what it was"
in two sentences is a correct and valuable outcome. Hunting a fault that is no
longer there is the most expensive thing you can do.

For an **image pull** branch, the diagnosis is usually complete here: read the
container image reference off the deployment (or replicaset) spec and compare it
to the failing image in the event. A tag that does not exist, a registry the node
cannot authenticate to, or a digest that was garbage-collected are the three
causes. You have no git history and no repo access, so do **not** try to determine
"the last known good tag" — propose the correction and let the parent's hand-off
establish the right value.

## Step 3 — crash loops and memory

Read the terminated state off the pod (`outputFormat: "YAML"` on the specific pod,
or the replicaset if pods churn too fast to catch).

- **Exit code 137 / OOMKilled** — compare `resources.limits.memory` against
  observed usage. Distinguish an **application leak** (unbounded growth, sawtooth
  restarts) from a **limit that is simply too low for legitimate demand**; the
  proposed fix differs, and saying which one you believe it is — and why — is the
  substance of the report.
- **Other non-zero exit codes** — the application itself is failing. Read the
  events for the pod and quote the terminating message. Look for language-level
  stack traces (`panic:`, `Traceback`, `NullPointerException`) and for
  read-only-filesystem write errors, which are fixed with an `emptyDir` mount.

## Step 4 — scheduling and mounts

`gke_list_k8s_events` on the namespace, then match:

- **`FailedScheduling`** — read the message. `Insufficient memory` / `Insufficient
  cpu` means the request does not fit; a taint message means missing tolerations
  (Spot nodes are the usual culprit); a node-affinity message means the selector
  does not match any node pool. On Autopilot, an invalid compute class or an
  accelerator selector that fails validation shows up here.
- **`FailedMount`** — the message names exactly what is absent: a `PersistentVolumeClaim`,
  a `Secret`, or a `ConfigMap`. Confirm the referenced object is genuinely missing
  before proposing it be created.

## Step 5 — connectivity

Only if logs or the event point at timeouts:

- `gke_get_k8s_resource {parent, namespace, resourceType: "endpoints", name: <target-service>}`
  — an empty endpoint list means the *target* service is the broken one, and that
  is a different incident; say so and stop.
- `gke_get_k8s_resource {parent, namespace, resourceType: "networkpolicy", outputFormat: "YAML"}`
  — if endpoints exist but traffic times out, check whether egress to the target
  is actually permitted.

---

## Step 6 — report

You are **read-only by construction**. Do not apply patches, do not open pull
requests, do not claim anything is fixed or healthy that you did not read.
Remediation belongs to the platform agent. Your job ends at a clear explanation
and a concrete proposed fix.

**`return_result` is how the work reaches the parent — so put it all in there.**
That call is the hand-off: what you pass to it is what the parent sees, and
calling it ends your run.

Structure it as:

- **Root cause** — one or two sentences, specific and causal. Not "there is an
  image problem" but "the deployment references
  `gcr.io/google-samples/does-not-exist:v0-demo-break`, a tag that does not exist
  in that repository, so every pod fails to pull and backs off."
- **Evidence** — the quoted event / spec / status excerpts, and which `gke_*`
  reads produced them.
- **Proposed patch** — the YAML, ready for the parent to hand off.

Do **not** end with a bare status line like "diagnosed the issue." A summary
without the analysis leaves the parent with nothing to act on, and it will redo
your work. If you could not determine the cause, say exactly that and report what
you ruled out — a documented dead end is a real result, a confident guess is not.

## When to stop early

Stop and report, rather than continuing, when any of these is true:

- Current state is healthy and the failure is not reproducible.
- The root cause lies in a different workload or namespace than the one you were
  given (say which, and that it is a separate incident).
- You have spent the budget.
- A read has failed twice for the same reason — report the failure as your
  finding; do not retry it a third time hoping for a different answer.
