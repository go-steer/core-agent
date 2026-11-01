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

# Originally derived from go-steer/mast:examples/deploy/terraform/modules/wif/outputs.tf.

output "principal" {
  description = "The WIF principal every binding above names. Paste it into a gcloud policy query when something comes back Forbidden."
  value       = local.principal
}

output "ksa_name" {
  description = "The daemon's Kubernetes ServiceAccount. Fixed by deploy/base, repeated here because the principal embeds it."
  value       = local.ksa_name
}

output "rbac_subject" {
  description = <<-EOT
    The RBAC username the GKE API server gives this principal — a User, not
    a ServiceAccount. A binding that names only the ServiceAccount matches
    nothing on the MCP path, so every call is Forbidden. The gated-apply
    RoleBinding names this User; this output is what to grep for in
    `kubectl get rolebinding -o yaml` when checking that it did.
  EOT
  value       = "serviceAccount:${var.project_id}.svc.id.goog[${var.namespace}/${local.ksa_name}]"
}

output "bound_project_roles" {
  description = <<-EOT
    The project roles this module currently holds bound, read off the
    resources rather than recomputed from write_scope. It is what makes the
    narrowing checkable: after flipping write_scope back to "namespaced",
    roles/container.admin is absent from this list because the binding is
    gone, not because a variable changed.
  EOT
  value       = sort(keys(google_project_iam_member.daemon))
}

output "enabled_apis" {
  description = "APIs this module enabled — empty when enable_apis is false."
  value       = sort(keys(google_project_service.required))
}

output "container_role" {
  description = "Which container role write_scope resolved to, so the plan output says it without the reader translating."
  value       = local.container_role
}

output "node_service_account" {
  description = "The node service account the daemon may impersonate — derived when not supplied."
  value       = local.node_service_account
}
