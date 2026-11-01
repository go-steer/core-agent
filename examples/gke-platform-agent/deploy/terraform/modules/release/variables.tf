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

# Originally derived from go-steer/mast:examples/deploy/terraform/modules/release/variables.tf.

variable "project_id" {
  description = "GCP project. Substituted into the RBAC subject the GKE MCP path arrives as, and into core-agent-gcp-env."
  type        = string

  validation {
    condition     = length(var.project_id) > 0
    error_message = "project_id is required: a placeholder in the RBAC subject renders a binding the API server accepts and that binds nobody."
  }
}

variable "namespace" {
  description = "Namespace to install into. Must be the namespace the WIF principal's subject names."
  type        = string
  default     = "gke-platform-agent"
}

variable "location" {
  description = "Vertex AI location."
  type        = string
  default     = "global"
}

variable "cluster_location" {
  description = "GKE cluster location (region or zone) passed as GKE_LOCATION in core-agent-gcp-env."
  type        = string
  default     = "us-central1"
}

variable "model" {
  description = "Model the daemon runs its specialists on."
  type        = string
  default     = "gemini-2.5-flash"
}

variable "image_tag" {
  description = "Daemon image tag. Null or empty takes the overlay's pinned tag."
  type        = string
  default     = null
}

variable "remediation_namespaces" {
  description = <<-EOT
    The namespaces core-agent may CHANGE. Empty — the default — installs an
    agent that diagnoses the whole cluster and can write nowhere.

    Each entry becomes a namespaced Role and RoleBinding matching
    deploy/components/gated-apply/. Note that this is the half of the boundary
    the cluster enforces; it is only real while the IAM half is
    write_scope = "namespaced".
  EOT
  type        = list(string)
  default     = []
}

variable "cluster_name" {
  description = "Cluster name shown in triage context. Name it something you will recognise in a transcript."
  type        = string
  default     = "local"
}

variable "node_selector" {
  description = <<-EOT
    Node selector for the daemon pod.

    GKE Standard needs {"iam.gke.io/gke-metadata-server-enabled" = "true"} so
    the pod lands on a node that can serve Workload Identity Federation.
    Autopilot REJECTS that selector — it runs the metadata server
    everywhere — so the empty default is the Autopilot-safe one.
  EOT
  type        = map(string)
  default     = {}
}

variable "users_secret" {
  description = <<-EOT
    Name of the Secret holding the users.json bearer table.

    This module names it and does not create it: a Terraform-authored token —
    random_password, or a value passed in as a variable — is written to the
    state file in plaintext, and a state bucket is usually readable by more
    people than a Kubernetes Secret is. Create it with kubectl, out of band.
  EOT
  type        = string
  default     = "core-agent-users"
}

variable "watcher_enabled" {
  description = "Expect the lookout-watch companion Deployment (on by default in deploy/base)."
  type        = bool
  default     = true
}

variable "watcher_token_secret" {
  description = "Name of the Secret holding the watcher's copy of the bearer. Must carry the SAME token as the sa:lookout-watch entry in users_secret."
  type        = string
  default     = "lookout-watch-token"
}

variable "otel_enabled" {
  description = "Select the GKE Managed OpenTelemetry overlay variant (*-otel)."
  type        = bool
  default     = false
}

variable "extra_values" {
  description = <<-EOT
    Raw YAML documents merged over everything above, last wins — the escape
    hatch for kustomize/deployment settings this module does not name.
  EOT
  type        = list(string)
  default     = []
}
