---
name: gke-workload-security
description: Audit the security posture of a GKE cluster and its workloads — Workload Identity, network policy, node hardening, Pod Security Standards, secret handling. Read-only; produces findings, the evidence for them, and proposed hardening the parent applies.
---

# GKE workload security

This is a **method**, not a mission. It tells you *how* to audit the posture of
the cluster and workload you were asked about. It never tells you *what* to audit
or *where* — that came with your goal, and your goal outranks everything in this
file.

Do not open by acquiring context, enumerating clusters, or asking the operator.
You were given the project, location, cluster, and usually the namespace; use
them verbatim.

## Reads — there is no shell

Everything here is read-only, through the `gke` MCP. There is **no** `kubectl`
and **no** `gcloud`: you cannot run a command, an audit script, or an `apply`.
Where this skill says "propose", it means *write it into your report* for the
platform agent to act on.

Every call that takes a `parent` uses the fully-qualified path from your goal —
never a wildcard.

| What you need | Read |
|---|---|
| Cluster posture (identity, addons, node hardening, private config) | `gke_get_cluster` |
| Node-pool hardening (Shielded VM, sandbox, service account) | `gke_get_node_pool`, `gke_list_node_pools` |
| Namespaces, service accounts, network policies, pods | `gke_get_k8s_resource`, `gke_describe_k8s_resource` |
| Recent denials / admission rejections | `gke_list_k8s_events` |

**Never invent a tool name.** If a check below needs a capability you do not see
in your registered tools, report that check as unavailable rather than
improvising a substitute.

## Step 1 — cluster posture, in one read

`gke_get_cluster` answers most of the audit at once. Read these fields and record
what each one actually says, not what you expect it to say:

- `workloadIdentityConfig.workloadPool` — absent means workloads authenticate as
  the node service account. This is the highest-value finding on most clusters.
- `networkPolicy.enabled` / `networkConfig.datapathProvider` — Dataplane V2
  (`ADVANCED_DATAPATH`) enforces network policy natively; on that datapath the
  legacy addon is neither present nor needed, so do not report its absence as a
  gap.
- `shieldedNodes.enabled`, `binaryAuthorization.evaluationMode`,
  `privateClusterConfig`, `masterAuthorizedNetworksConfig` — node integrity,
  image provenance, and control-plane exposure.
- `releaseChannel` — an unenrolled cluster does not get security patches
  automatically.

Per node pool, `config.sandboxConfig` (gVisor), `config.serviceAccount` (the
`default` compute SA is over-privileged), and `config.shieldedInstanceConfig`.

## Step 2 — workload posture

Scope this to the namespace you were given.

- **Workload Identity binding** — read the pod's `spec.serviceAccountName`, then
  that ServiceAccount. A KSA that is meant to reach Google APIs carries
  `iam.gke.io/gcp-service-account`. Missing annotation plus API calls in the logs
  is the signature of a workload falling back to node credentials.
- **Network isolation** — list `networkpolicy` in the namespace. None at all
  means every pod is reachable from every other pod in the cluster.
- **Pod Security Standards** — read the namespace object's labels for
  `pod-security.kubernetes.io/enforce`. Unlabeled non-system namespaces run
  unrestricted.
- **Container hardening** — from the pod or deployment spec:
  `securityContext.runAsNonRoot`, `readOnlyRootFilesystem`,
  `allowPrivilegeEscalation`, dropped capabilities, and any `hostPath`,
  `hostNetwork`, or `privileged: true`.
- **Secret handling** — env vars sourced from `Secret` objects versus a
  `secrets-store.csi.k8s.io` volume.

## Step 3 — report and propose

You are **read-only by construction**. Do not claim anything is hardened, and do
not describe yourself as having applied, enabled, or created anything.
Remediation belongs to the platform agent.

Report each finding as: **what is not set**, **the field you read that
establishes it**, and **the concrete change**. Rank by exploitability in this
cluster, not by generic severity.

Workload-level fixes are manifests — hand back the YAML:

- **Default-deny isolation** — `assets/default-deny-netpol.yaml`, with the target
  namespace named in your report. Say plainly that it denies egress too, so DNS
  and any required egress need companion policies; proposing it without that
  caveat proposes an outage.
- **Workload Identity** — the KSA annotation
  (`iam.gke.io/gcp-service-account: <gsa>@<project>.iam.gserviceaccount.com`) plus
  the IAM binding of role `roles/iam.workloadIdentityUser` for the member
  `serviceAccount:<project>.svc.id.goog[<namespace>/<ksa>]`. The IAM half is a
  Google Cloud change, not a manifest — name it as such.
  `assets/workload-identity-pod.yaml` is the smoke-test pod to propose alongside
  it, not something you deploy.
- **Pod Security Standards** — the namespace labels
  `pod-security.kubernetes.io/enforce: restricted` and `enforce-version: latest`,
  pinned to a version if the namespace should not track cluster upgrades.
- **Secret Manager CSI** — a `SecretProviderClass` naming
  `projects/<project>/secrets/<secret>/versions/latest`, mounted read-only, in
  place of env-injected Secrets.
- **Network policy logging** (Dataplane V2 only) — a `NetworkLogging` resource
  named `default` with `allow` and `deny` logging enabled.

Cluster-level fixes are **API settings, not manifests**. Name the setting and the
value — the platform agent applies them through the cluster-update path or the
cluster's IaC:

| Finding | Setting to propose |
|---|---|
| No Workload Identity | `workloadIdentityConfig.workloadPool = <project>.svc.id.goog` |
| Policy unenforced (non-Dataplane-V2) | enable the `NetworkPolicy` addon |
| Nodes not shielded | `shieldedNodes.enabled = true` (node pools need recreation) |
| Untrusted images admitted | Binary Authorization, `evaluationMode` per policy |
| Untrusted workloads unisolated | GKE Sandbox (gVisor) on a dedicated node pool |

Say which changes are disruptive — node-pool recreation, datapath changes, and
default-deny all cause downtime if applied carelessly. A proposal that hides its
blast radius is a worse deliverable than no proposal.

Do not end with a bare status line like "audited the cluster." If a read failed,
say which check is therefore unanswered rather than presenting an assumption as a
finding.
