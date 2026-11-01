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

# Originally derived from go-steer/mast:examples/deploy/terraform/main.tf.

# One `terraform apply` for a gke-platform-agent install on GKE: the IAM
# bindings the daemon's Workload Identity Federation principal needs, and the
# cluster-side namespace, coordinate ConfigMap, and per-namespace gated-apply
# RBAC objects.
#
# The two halves are separate modules because they are often separate
# people — modules/wif needs project-level IAM admin, modules/release needs
# cluster credentials — and an operator who only wants one can call one.
# What the root buys is that project_id and namespace reach both from a
# single place. They are not two settings that happen to match: the
# namespace is inside the WIF principal's subject AND inside the RBAC
# subject the RoleBinding names, so two independent copies that drift
# produce a daemon that is Forbidden on every call with both halves looking
# correct in isolation.

locals {
  # Setting remediation_namespaces opts into per-namespace Kubernetes RBAC
  # (deploy/components/gated-apply/), which is only a real boundary when the
  # GCP IAM half is "namespaced" (roles/container.viewer). GKE authorizes a
  # call if EITHER IAM or RBAC allows it, so a lingering cluster-admin IAM
  # binding would make the namespaced Roles decorative.
  write_scope = length(var.remediation_namespaces) > 0 ? "namespaced" : var.write_scope
}

module "wif" {
  source = "./modules/wif"

  project_id           = var.project_id
  namespace            = var.namespace
  write_scope          = local.write_scope
  enable_apis          = var.enable_apis
  node_service_account = var.node_service_account
}

module "release" {
  source = "./modules/release"

  project_id             = var.project_id
  namespace              = var.namespace
  image_tag              = var.image_tag
  cluster_name           = var.cluster_name
  cluster_location       = var.cluster_location
  otel_enabled           = var.otel_enabled
  remediation_namespaces = var.remediation_namespaces
  extra_values           = var.extra_values

  node_selector = var.gke_standard ? {
    "iam.gke.io/gke-metadata-server-enabled" = "true"
  } : {}

  # The per-namespace RBAC is what authorizes a write once write_scope is
  # "namespaced", so the IAM bindings have to exist before the daemon starts
  # reaching for them. Terraform sees no dependency between an IAM member
  # and a Kubernetes resource, so it is stated.
  depends_on = [module.wif]
}
