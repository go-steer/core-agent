# ImagePullBackOff

Kubelet couldn't pull the container image and is backing off. Same fix
matrix as `ErrImagePull` (they're two states of the same underlying
problem — kubelet transitions ErrImagePull → ImagePullBackOff after a
few failed attempts).

## Budget

- Max turns: 6
- Max wall time: 5 min

## Diagnose

1. Get the pod events (they carry the real error from the container runtime):
   `kubectl -n {namespace} describe pod {name}` and look at the Events section.
   Look for lines like `Failed to pull image "...": rpc error: code = NotFound` or `... code = Unauthenticated` or `... x509: certificate signed by unknown authority`.

2. Extract the image reference the pod tried to pull:
   `kubectl -n {namespace} get pod {name} -o jsonpath='{.spec.containers[*].image}'`

3. Classify the failure:
   - **"not found" / "manifest unknown"** → image or tag doesn't exist in the registry.
   - **"unauthorized" / "authentication required"** → registry pull-secret missing or invalid.
   - **"x509: certificate signed by unknown authority"** → private registry with untrusted CA; node needs the CA cert.
   - **"connection refused" / "dial tcp: no such host"** → network path to the registry blocked (firewall, DNS, VPC endpoint).
   - **"toomanyrequests"** → Docker Hub rate limit (or similar registry-side throttle).

## Common fixes

| Symptom | Fix | Verify (interval → check) |
|---|---|---|
| Wrong tag (typo, or `:latest` moved) | `kubectl set image deployment/<name> {container}=<correct-image>:<tag>` | 90s → new pod pulls successfully; `Ready` |
| Missing pull secret (private registry) | Create Secret: `kubectl create secret docker-registry <name> --docker-server=... --docker-username=... --docker-password=... --docker-email=...`. Then patch ServiceAccount or Deployment `imagePullSecrets`. | 90s → pull succeeds |
| Wrong pull-secret registry hostname | Verify the Secret's `.dockerconfigjson` has an entry keyed by the exact registry host from the image reference. Recreate the Secret with the right key. | 90s → pull succeeds |
| Docker Hub rate limit (`toomanyrequests`) | Mirror the image to your registry (Artifact Registry / ECR / GHCR) or authenticate to Docker Hub (rate limits are per-IP unauth, per-account auth). | 3m → pull succeeds |
| GKE + Artifact Registry, WI misconfigured | Verify the pod's KSA has the `roles/artifactregistry.reader` IAM role bound to its principal (WI-for-GKE direct binding) or its impersonated GSA. `gcloud projects get-iam-policy <project>` | 90s → pull succeeds |
| Air-gapped cluster; image not mirrored | Push the image to the internal registry and update the manifest. | Coordinate with platform team; may be > budget. |

## When to escalate

- Cluster is air-gapped and mirroring isn't set up (needs infra work).
- Secret / IAM change requires a role you don't have (RBAC scoping too narrow).
- Registry is down (all pulls failing cluster-wide) — this is a fleet-wide incident, not a per-pod triage.
