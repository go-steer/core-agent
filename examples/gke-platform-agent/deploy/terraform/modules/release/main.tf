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

# Originally derived from go-steer/mast:examples/deploy/terraform/modules/release/main.tf.

# The cluster half of a gke-platform-agent install: the target namespace,
# the core-agent-gcp-env ConfigMap, and — when remediation_namespaces is
# non-empty — per-namespace Role and RoleBinding objects matching
# deploy/components/gated-apply/ (patch on apps/deployments for the daemon's
# Workload Identity RBAC User).

locals {
  ksa_name     = "core-agent-daemon"
  rbac_name    = "gated-apply-${var.namespace}"
  rbac_subject = "serviceAccount:${var.project_id}.svc.id.goog[${var.namespace}/${local.ksa_name}]"

  overlay = length(var.remediation_namespaces) > 0 ? (
    var.otel_enabled ? "gated-apply-otel" : "gated-apply"
    ) : (
    var.otel_enabled ? "example-otel" : "example"
  )

  values = yamlencode({
    gcp = {
      projectID       = var.project_id
      location        = var.location
      clusterLocation = var.cluster_location
    }
    namespace = var.namespace
    overlay   = local.overlay
    model     = var.model
    image = {
      tag = var.image_tag != null ? var.image_tag : ""
    }
    daemon = {
      nodeSelector = var.node_selector
    }
    auth = {
      usersSecret = var.users_secret
    }
    watcher = {
      enabled     = var.watcher_enabled
      clusterName = var.cluster_name
      tokenSecret = var.watcher_token_secret
    }
    remediationNamespaces = var.remediation_namespaces
    extraValues           = var.extra_values
  })
}

resource "kubernetes_namespace_v1" "agent" {
  metadata {
    name = var.namespace
    labels = {
      "app.kubernetes.io/name" = "gke-platform-agent"
    }
  }
}

resource "kubernetes_config_map_v1" "gcp_env" {
  metadata {
    name      = "core-agent-gcp-env"
    namespace = kubernetes_namespace_v1.agent.metadata[0].name
  }

  data = {
    GOOGLE_CLOUD_PROJECT  = var.project_id
    GOOGLE_CLOUD_LOCATION = var.location
    CLUSTER_NAME          = var.cluster_name
    GKE_CLUSTER           = var.cluster_name
    GKE_LOCATION          = var.cluster_location
  }
}

resource "kubernetes_role_v1" "remediation" {
  for_each = toset(var.remediation_namespaces)

  metadata {
    name      = local.rbac_name
    namespace = each.value
  }

  rule {
    api_groups = ["apps"]
    resources  = ["deployments"]
    verbs      = ["patch"]
  }
}

resource "kubernetes_role_binding_v1" "remediation" {
  for_each = toset(var.remediation_namespaces)

  metadata {
    name      = local.rbac_name
    namespace = each.value
  }

  role_ref {
    api_group = "rbac.authorization.k8s.io"
    kind      = "Role"
    name      = kubernetes_role_v1.remediation[each.key].metadata[0].name
  }

  subject {
    api_group = "rbac.authorization.k8s.io"
    kind      = "User"
    name      = local.rbac_subject
  }
}
