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

# Originally derived from go-steer/mast:examples/deploy/terraform/outputs.tf.

output "wif_principal" {
  description = "The IAM principal every binding names."
  value       = module.wif.principal
}

output "rbac_subject" {
  description = "The RBAC username the GKE API server gives that principal. Every daemon RoleBinding must name it; grep for it when a tool call comes back Forbidden."
  value       = module.wif.rbac_subject
}

output "release_rbac_subject" {
  description = "The RBAC User bound by module.release's per-namespace remediation RoleBindings; must equal rbac_subject."
  value       = module.release.rbac_subject
}

output "container_role" {
  description = "Which container role write_scope resolved to."
  value       = module.wif.container_role
}

output "install_namespace" {
  description = "Namespace the release installed into, read off the Kubernetes Namespace resource. It must be the namespace inside rbac_subject above; if those two ever disagree, every tool call will be Forbidden."
  value       = module.release.namespace
}

output "kustomize_overlay" {
  description = "The deploy/overlays/<name> directory that matches this configuration."
  value       = module.release.overlay
}

output "kustomize_config" {
  description = "The rendered configuration document for the kustomize overlay."
  value       = module.release.kustomize_config_yaml
}

output "chart_values" {
  description = "Alias for kustomize_config for test parity."
  value       = module.release.values_yaml
}

output "required_secrets" {
  description = "Secrets this configuration names and deliberately does not create. The daemon and watcher stay not-ready until they exist."
  value       = module.release.required_secrets
}

output "next_steps" {
  description = "What is left to do by hand, in order."
  value       = <<-EOT
    1. Create the bearer Secrets out of band (see deploy/base/20-secrets-placeholder.md):

         Required secrets in ${var.namespace}: ${join(", ", module.release.required_secrets)}
         (./scripts/gen-tokens.sh creates both from a temp dir outside the checkout)

       Not created here: a Terraform-authored token is written to the state
       file in plaintext, and a state bucket is usually readable by more
       people than a Kubernetes Secret is.

    2. Apply the matching kustomize overlay:

         kubectl apply -k deploy/overlays/${module.release.overlay}

    3. Check the boundary is the one you think it is:

         PROJECT_ID=${var.project_id} ./scripts/verify-gated-apply.sh ${length(var.remediation_namespaces) == 0 ? "denied" : "granted"}

    ${length(var.remediation_namespaces) == 0 ? "4. core-agent can change NOTHING right now — it diagnoses cluster-wide and holds\n       no write grant anywhere, which is the default. Set remediation_namespaces\n       when you are ready." : "4. core-agent may patch apps/deployments in: ${join(", ", var.remediation_namespaces)}"}
  EOT
}
