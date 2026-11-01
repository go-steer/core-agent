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

# Originally derived from go-steer/mast:examples/deploy/terraform/variables.tf.

variable "project_id" {
  description = "GCP project holding the GKE cluster and the Vertex AI quota. The only value with no default."
  type        = string
}

variable "namespace" {
  description = <<-EOT
    Namespace gke-platform-agent runs in. Passed to BOTH halves from here,
    which is the point of the root module: the namespace is part of the
    daemon's identity, and an IAM principal bound for one namespace against
    resources installed into another produces a deployment that is Forbidden
    everywhere with nothing obviously wrong in either half.
  EOT
  type        = string
  default     = "gke-platform-agent"
}

variable "write_scope" {
  description = "\"namespaced\" (default) or \"cluster-admin\". See modules/wif/variables.tf — flipping this back to namespaced REMOVES the admin binding, which an imperative gcloud script cannot do."
  type        = string
  default     = "namespaced"
}

variable "remediation_namespaces" {
  description = "Namespaces core-agent may CHANGE (via deploy/components/gated-apply). Empty installs an agent that diagnoses everything and writes nowhere."
  type        = list(string)
  default     = []
}

variable "image_tag" {
  description = "Daemon image tag override. Null takes the kustomize overlay's pinned tag."
  type        = string
  default     = null
}

variable "cluster_name" {
  description = "Cluster name shown in triage context and passed as GKE_CLUSTER."
  type        = string
  default     = "local"
}

variable "cluster_location" {
  description = "Cluster location (region or zone) passed as GKE_LOCATION."
  type        = string
  default     = "us-central1"
}

variable "gke_standard" {
  description = <<-EOT
    True for a GKE Standard cluster, which needs the daemon pinned onto
    metadata-server-enabled nodes for Workload Identity Federation. Leave
    false on Autopilot: it ships the metadata server everywhere and REJECTS
    that selector, so setting this on Autopilot fails admission rather than
    doing nothing.
  EOT
  type        = bool
  default     = false
}

variable "otel_enabled" {
  description = "Select the GKE Managed OpenTelemetry overlay variant (*-otel)."
  type        = bool
  default     = false
}

variable "enable_apis" {
  description = "Enable container/aiplatform/iamcredentials. False where service enablement is somebody else's pipeline."
  type        = bool
  default     = true
}

variable "node_service_account" {
  description = "Node service account the daemon may impersonate. Empty derives the Compute Engine default."
  type        = string
  default     = ""
}

variable "extra_values" {
  description = "Raw YAML documents merged over the configuration values this root sets, last wins."
  type        = list(string)
  default     = []
}
