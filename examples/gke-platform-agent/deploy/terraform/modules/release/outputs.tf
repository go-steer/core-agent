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

# Originally derived from go-steer/mast:examples/deploy/terraform/modules/release/outputs.tf.

output "kustomize_config_yaml" {
  description = "The kustomize/deployment configuration document this module renders."
  value       = local.values
}

output "values_yaml" {
  description = "Alias for kustomize_config_yaml for test parity with the mast release module."
  value       = local.values
}

output "namespace" {
  description = "Namespace the release resources were created in, read off the Namespace resource rather than echoed back from the variable."
  value       = kubernetes_namespace_v1.agent.metadata[0].name
}

output "overlay" {
  description = "The deploy/overlays/<name> directory that matches this configuration."
  value       = local.overlay
}

output "rbac_subject" {
  description = "The RBAC User bound by each per-namespace remediation RoleBinding."
  value       = local.rbac_subject
}

output "required_secrets" {
  description = <<-EOT
    The Secrets this module names and does not create. Both must exist in the
    install namespace before the daemon and watcher pods become ready
    (see deploy/base/20-secrets-placeholder.md).
  EOT
  value = compact([
    var.users_secret,
    var.watcher_enabled ? var.watcher_token_secret : "",
  ])
}
