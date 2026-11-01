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

# Originally derived from go-steer/mast:examples/deploy/terraform/modules/wif/variables.tf.

variable "project_id" {
  description = "GCP project holding the GKE cluster and the Vertex AI quota."
  type        = string

  validation {
    condition     = length(var.project_id) > 0
    error_message = "project_id is required: it is substituted into the workload-identity pool name, and a placeholder there binds a principal that does not exist."
  }
}

variable "namespace" {
  description = <<-EOT
    Kubernetes namespace the daemon runs in. This is part of the identity,
    not just a location: it appears inside the WIF principal's subject, so
    it must be the namespace the deployment is actually installed into. The
    root module passes one value to both halves for exactly this reason.
  EOT
  type        = string
  default     = "gke-platform-agent"
}

variable "write_scope" {
  description = <<-EOT
    Where core-agent's authority to CHANGE a cluster comes from.

    "namespaced" (default) binds roles/container.viewer and leaves writes to
    the Kubernetes RBAC the release module (or deploy/components/gated-apply)
    renders — the boundary an operator can see and audit per namespace.

    "cluster-admin" binds roles/container.admin instead. GKE allows an API
    call if EITHER IAM or RBAC allows it, and the daemon reaches the cluster
    through the GKE MCP server as this principal, so while that binding
    stands the namespaced write Roles subtract nothing. Use it to unblock an
    incident, then come back.
  EOT
  type        = string
  default     = "namespaced"

  validation {
    condition     = contains(["namespaced", "cluster-admin"], var.write_scope)
    error_message = "write_scope must be \"namespaced\" or \"cluster-admin\"."
  }
}

variable "node_service_account" {
  description = <<-EOT
    The GKE node service account the daemon must be allowed to impersonate
    (required by the GKE MCP path). Empty derives the Compute Engine default
    for this project, which is what a cluster created without --service-account
    runs as.
  EOT
  type        = string
  default     = ""
}

variable "enable_apis" {
  description = <<-EOT
    Enable the three base APIs this deployment needs. Set false in an
    organization where service enablement is somebody else's pipeline, or
    where the caller has no roles/serviceusage.serviceUsageAdmin — the
    bindings below do not depend on this module having turned them on.
  EOT
  type        = bool
  default     = true
}
