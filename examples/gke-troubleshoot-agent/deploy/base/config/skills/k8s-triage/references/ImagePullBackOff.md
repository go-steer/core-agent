# ImagePullBackOff

Kubelet couldn't pull the container image and is backing off. Same
matrix as `ErrImagePull` (they're two states of the same underlying
problem — kubelet transitions ErrImagePull → ImagePullBackOff after a
few failed attempts).

## Budget

- Max turns: 6
- Max wall time: 5 min

## Diagnose (read-only)

1. Get the pod events — they carry the real error from the container runtime:
   `gke_list_k8s_events` scoped to `{namespace}` and `{name}` (or
   `gke_describe_k8s_resource` on the pod).
   Look for lines like `Failed to pull image "...": rpc error: code = NotFound`, `... code = Unauthenticated`, or `... x509: certificate signed by unknown authority`.

2. Extract the image reference the pod tried to pull:
   `gke_get_k8s_resource` (kind `Pod`) → `spec.containers[].image`.

3. Classify the failure:
   - **"not found" / "manifest unknown"** → image or tag doesn't exist in the registry.
   - **"unauthorized" / "authentication required"** → registry pull-secret missing or invalid.
   - **"x509: certificate signed by unknown authority"** → private registry with untrusted CA; node needs the CA cert.
   - **"connection refused" / "dial tcp: no such host"** → network path to the registry blocked (firewall, DNS, VPC endpoint).
   - **"toomanyrequests"** → Docker Hub rate limit (or similar registry-side throttle).

## Convergence check

Registry throttles and transient network faults clear on their own —
kubelet keeps retrying with backoff. Watch before you page anyone:

```
wait_and_verify(
  tool:            "gke_get_k8s_resource",
  args_json:       "{\"kind\": \"Pod\", \"namespace\": \"{namespace}\", \"name\": \"{name}\"}",
  expect_not_contains: "ImagePullBackOff",
  interval_seconds: 15,
  timeout_seconds: 120
)
```

For a `toomanyrequests` classification, use the full 3m budget — Docker
Hub windows are minutes, not seconds.

When the failing pod belongs to a Deployment that is mid-rollout (a bad
tag pushed by a deploy is the common case), watch the rollout instead —
it tells you whether the *workload* recovered, not just this one pod,
which is what the incident summary should claim:

```
wait_and_verify(
  tool:            "gke_get_k8s_rollout_status",
  args_json:       "{\"kind\": \"Deployment\", \"namespace\": \"{namespace}\", \"name\": \"{context.controller_ref}\"}",
  expect_contains: "successfully rolled out",
  interval_seconds: 15,
  timeout_seconds: 120
)
```

## Remediation proposals

| Evidence | Proposed change | Verify (interval → check) |
|---|---|---|
| Wrong tag (typo, or `:latest` moved) | Set `<controller>`'s image for `{container}` to the correct `<image>:<tag>`. Name the tag you believe is correct and how you know | 90s → new pod pulls; `Ready` |
| Missing pull secret (private registry) | Create a `kubernetes.io/dockerconfigjson` Secret for `<registry-host>` and add it to `imagePullSecrets` on the ServiceAccount or the pod spec | 90s → pull succeeds |
| Wrong pull-secret registry hostname | The Secret's `.dockerconfigjson` must be keyed by the exact registry host in the image reference. Recreate it with the right key | 90s → pull succeeds |
| Docker Hub rate limit (`toomanyrequests`) | Mirror the image into the project's Artifact Registry and repoint the workload, or authenticate to Docker Hub (limits are per-IP unauthenticated, per-account authenticated) | 3m → pull succeeds |
| GKE + Artifact Registry, Workload Identity misconfigured | Grant `roles/artifactregistry.reader` to the pod's KSA principal (WI direct binding) or its impersonated GSA. You can see the KSA on the pod spec; you cannot read IAM from here — say so and name the binding you want | 90s → pull succeeds |
| Air-gapped cluster; image not mirrored | Push the image to the internal registry and update the manifest. Platform-team work; likely beyond this incident's budget | Coordinate with platform team |

## When to escalate

- Cluster is air-gapped and mirroring isn't set up (needs infra work).
- The change is an IAM or Secret grant (you can neither read nor make it).
- Registry is down (all pulls failing cluster-wide) — this is a fleet-wide incident, not a per-pod triage.
