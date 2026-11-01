# Terraform: `gke-platform-agent` IAM + RBAC on GKE

One `terraform apply` for the two halves of a `gke-platform-agent`
deployment whose coordinates have to agree — the Google Cloud IAM the
daemon's Workload Identity Federation principal needs (`modules/wif`),
and the cluster-side Namespace, `core-agent-gcp-env` ConfigMap, and
per-namespace `gated-apply` RBAC (`modules/release`) — with `project_id`
and `namespace` passed from one place.

```bash
cp terraform.tfvars.example terraform.tfvars   # edit project_id
terraform init
terraform apply
```

It does **not** create the GKE cluster. Point your kubeconfig at an
existing cluster with Workload Identity Federation enabled first; the
`kubernetes` provider uses whatever context
`kubectl config current-context` reports.

## Why this exists alongside `scripts/grant-iam.sh`

Because of one thing an imperative `gcloud` script structurally cannot
do: `grant-iam.sh` is idempotent forwards and not backwards. If a
project-level `roles/container.admin` binding is ever granted on the
daemon's Workload Identity principal, re-running a script in read-only or
namespaced mode leaves the wide binding standing until someone removes it
by hand with `gcloud`. While it stands, `deploy/components/gated-apply/`'s
per-namespace write Roles bound nothing on the GKE MCP path: GKE
authorizes a call if **either** IAM or RBAC allows it, so a project-level
`roles/container.admin` makes the namespace boundary decorative.

Here, flipping `write_scope` back to `"namespaced"` — or setting
`remediation_namespaces` — destroys the `roles/container.admin` binding.
`modules/wif/tests/wif.tftest.hcl` and `tests/root.tftest.hcl` prove it
with before-and-after state transitions.

## What it creates

| Module | Resources |
|---|---|
| `modules/wif` | Three base APIs (`container`, `aiplatform`, `iamcredentials`), three additive `google_project_iam_member` bindings (`roles/aiplatform.user`, `roles/mcp.toolUser`, and `roles/container.viewer` or `roles/container.admin`) on the daemon's Workload Identity Federation principal, and `roles/iam.serviceAccountUser` on the node service account. |
| `modules/release` | `kubernetes_namespace_v1.agent` (`gke-platform-agent`), `kubernetes_config_map_v1.gcp_env` (`core-agent-gcp-env`), and per-namespace `kubernetes_role_v1.remediation` + `kubernetes_role_binding_v1.remediation` matching `deploy/components/gated-apply/` (`patch` on `apps/deployments` bound to the daemon's Workload Identity `User`). |

The two are separate modules because they are often separate people —
`modules/wif` needs project-level IAM admin, `modules/release` needs
cluster credentials — and either can be called on its own. What the root
buys is that `project_id` and `namespace` reach both from a single place:
the namespace is inside the WIF principal's subject **and** inside the
RBAC `User` subject the RoleBinding names.

## What it deliberately does not create

**The bearer Secrets (`core-agent-users` and `lookout-watch-token`).** A
token Terraform authors is written to the state file in plaintext, and a
state bucket is usually readable by more people than a Kubernetes Secret
is. `terraform output next_steps` points to `scripts/gen-tokens.sh` and
[`../base/20-secrets-placeholder.md`](../base/20-secrets-placeholder.md).
`TestTerraformCreatesNoSecret` in `../../terraform_test.go` enforces that
no secret-generating resource is added here.

**Anything authoritative.** The role bindings are
`google_project_iam_member`, never `google_project_iam_binding` or
`_policy`. An authoritative resource would delete bindings this module
never created on the first apply. `TestTerraformWifIsAdditivePerMember`
enforces that across every `.tf` file in this tree.

**The project's API enablement, on the way out.** `disable_on_destroy`
and `disable_dependent_services` are both `false`, so destroying this
module cannot disable `container.googleapis.com` in a project running
other workloads on GKE.

## Tests

```bash
dev/ci/presubmits/verify-terraform
go test -v ./examples/gke-platform-agent -run TestTerraform
```

`verify-terraform` runs `terraform fmt -check`, `terraform validate`, and
`terraform test` (against mocked `google` and `kubernetes` providers — no
credentials, no network after `init`, no cluster) across the root and
both child modules. `terraform_test.go` checks the cross-artifact
invariants against `scripts/grant-iam.sh`,
`deploy/base/10-serviceaccount-daemon.yaml`, and
`deploy/components/gated-apply/`.
