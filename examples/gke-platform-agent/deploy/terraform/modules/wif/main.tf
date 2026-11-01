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

# Originally derived from go-steer/mast:examples/deploy/terraform/modules/wif/main.tf.

# The IAM half of a gke-platform-agent install: three base APIs and four role
# bindings on the daemon's Workload Identity Federation principal.
#
# This matches scripts/grant-iam.sh's base set, and the module exists because
# of the one thing an imperative gcloud script structurally cannot do: the
# script is idempotent forwards and not backwards. Re-running it with a
# narrower scope leaves an earlier roles/container.admin binding in place
# until removed by hand with gcloud. While it stands, the per-namespace write
# Roles bound nothing on the GKE MCP path — GKE allows a call if IAM or RBAC
# allows it — so the narrowing an operator believes they performed has not
# happened.
#
# Here, flipping write_scope destroys the binding. That is the feature.

locals {
  # The RBAC subject the API server gives this principal is built from the
  # same three values (see the root module's rbac_subject output). The
  # daemon's ServiceAccount name is FIXED in deploy/base/10-serviceaccount-daemon.yaml.
  ksa_name = "core-agent-daemon"

  project_number = data.google_project.this.number

  principal = "principal://iam.googleapis.com/projects/${local.project_number}/locations/global/workloadIdentityPools/${var.project_id}.svc.id.goog/subject/ns/${var.namespace}/sa/${local.ksa_name}"

  container_role = var.write_scope == "cluster-admin" ? "roles/container.admin" : "roles/container.viewer"

  project_roles = [
    "roles/aiplatform.user",
    "roles/mcp.toolUser",
    local.container_role,
  ]

  node_service_account = coalesce(
    var.node_service_account,
    "${local.project_number}-compute@developer.gserviceaccount.com",
  )

  apis = var.enable_apis ? toset([
    "container.googleapis.com",      # GKE + the GKE MCP server
    "aiplatform.googleapis.com",     # Vertex AI / Gemini
    "iamcredentials.googleapis.com", # the WIF token-exchange path
  ]) : toset([])
}

data "google_project" "this" {
  project_id = var.project_id
}

resource "google_project_service" "required" {
  for_each = local.apis

  project = var.project_id
  service = each.value

  # Destroying a gke-platform-agent install must not turn off
  # container.googleapis.com in a project that is running other things on GKE.
  # Terraform's default here is the other way round, and it is the wrong
  # default for a module that is a guest in somebody else's project.
  disable_on_destroy         = false
  disable_dependent_services = false
}

# google_project_iam_member and deliberately NOT google_project_iam_binding
# or _policy. The _binding resource is authoritative for a role across every
# member, so adopting it here would delete whatever else in the operator's
# project holds roles/container.viewer; _policy would do that to the whole
# project. A module installed into somebody else's project has to be
# additive per (role, member), which also gives write_scope its narrowing:
# destroying one member removes one binding and touches nothing else.
resource "google_project_iam_member" "daemon" {
  for_each = toset(local.project_roles)

  project = var.project_id
  role    = each.value
  member  = local.principal
}

resource "google_service_account_iam_member" "node_sa_user" {
  service_account_id = "projects/${var.project_id}/serviceAccounts/${local.node_service_account}"
  role               = "roles/iam.serviceAccountUser"
  member             = local.principal
}
