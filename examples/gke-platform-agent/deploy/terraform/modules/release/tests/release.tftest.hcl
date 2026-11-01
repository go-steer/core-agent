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

# Originally derived from go-steer/mast:examples/deploy/terraform/modules/release/tests/release.tftest.hcl.

# The cluster half: Namespace, core-agent-gcp-env ConfigMap, and per-namespace
# gated-apply RBAC objects.

mock_provider "kubernetes" {}

variables {
  project_id = "example-project"
}

run "the_required_value_reaches_the_config_and_namespace" {
  command = apply

  assert {
    condition     = yamldecode(output.kustomize_config_yaml).gcp.projectID == "example-project"
    error_message = "gcp.projectID did not reach kustomize_config_yaml"
  }

  assert {
    condition     = kubernetes_config_map_v1.gcp_env.data["GOOGLE_CLOUD_PROJECT"] == "example-project"
    error_message = "GOOGLE_CLOUD_PROJECT did not reach core-agent-gcp-env"
  }

  assert {
    condition     = kubernetes_namespace_v1.agent.metadata[0].name == "gke-platform-agent"
    error_message = "installed into ${kubernetes_namespace_v1.agent.metadata[0].name}, not the namespace the WIF principal's subject names"
  }
}

run "an_empty_project_id_is_refused_at_plan_time" {
  command = plan

  variables {
    project_id = ""
  }

  expect_failures = [var.project_id]
}

# The default install diagnoses everything and can change nothing. That is
# the shipped posture and it should not be possible to acquire write access
# by leaving a variable unset.
run "a_default_install_can_change_nothing" {
  command = apply

  assert {
    condition     = length(yamldecode(output.values_yaml).remediationNamespaces) == 0
    error_message = "the default install named remediable namespaces: ${join(", ", yamldecode(output.values_yaml).remediationNamespaces)}"
  }

  assert {
    condition     = length(kubernetes_role_v1.remediation) == 0 && length(kubernetes_role_binding_v1.remediation) == 0
    error_message = "the default install created remediation RBAC objects"
  }

  assert {
    condition     = output.overlay == "example"
    error_message = "expected read-only overlay 'example', got ${output.overlay}"
  }
}

run "named_namespaces_create_gated_apply_roles_and_bindings" {
  command = apply

  variables {
    remediation_namespaces = ["team-a", "team-b"]
  }

  assert {
    condition     = jsonencode(yamldecode(output.values_yaml).remediationNamespaces) == jsonencode(["team-a", "team-b"])
    error_message = "remediationNamespaces arrived as ${jsonencode(yamldecode(output.values_yaml).remediationNamespaces)}"
  }

  assert {
    condition     = length(kubernetes_role_v1.remediation) == 2 && length(kubernetes_role_binding_v1.remediation) == 2
    error_message = "expected one Role and RoleBinding per remediation namespace"
  }

  assert {
    condition     = kubernetes_role_v1.remediation["team-a"].metadata[0].namespace == "team-a" && kubernetes_role_v1.remediation["team-b"].metadata[0].namespace == "team-b" && kubernetes_role_binding_v1.remediation["team-a"].metadata[0].namespace == "team-a" && kubernetes_role_binding_v1.remediation["team-b"].metadata[0].namespace == "team-b"
    error_message = "remediation Role/RoleBinding was not created in the target remediation namespace"
  }

  assert {
    condition     = kubernetes_role_v1.remediation["team-a"].metadata[0].name == "gated-apply-gke-platform-agent" && kubernetes_role_binding_v1.remediation["team-a"].metadata[0].name == "gated-apply-gke-platform-agent" && kubernetes_role_binding_v1.remediation["team-a"].role_ref[0].kind == "Role"
    error_message = "remediation Role/RoleBinding metadata.name or role_ref.kind drifted from deploy/components/gated-apply/"
  }

  assert {
    condition     = toset(kubernetes_role_v1.remediation["team-a"].rule[0].api_groups) == toset(["apps"]) && toset(kubernetes_role_v1.remediation["team-a"].rule[0].resources) == toset(["deployments"]) && toset(kubernetes_role_v1.remediation["team-a"].rule[0].verbs) == toset(["patch"])
    error_message = "remediation Role rule drifted from deploy/components/gated-apply/role.yaml"
  }

  assert {
    condition     = kubernetes_role_binding_v1.remediation["team-a"].subject[0].kind == "User" && kubernetes_role_binding_v1.remediation["team-a"].subject[0].name == "serviceAccount:example-project.svc.id.goog[gke-platform-agent/core-agent-daemon]"
    error_message = "remediation RoleBinding subject drifted from deploy/components/gated-apply/rolebinding.yaml: ${kubernetes_role_binding_v1.remediation["team-a"].subject[0].name}"
  }

  assert {
    condition     = output.overlay == "gated-apply"
    error_message = "expected overlay 'gated-apply' when remediation_namespaces is set, got ${output.overlay}"
  }
}

# The module names the Secrets and creates neither. A Terraform-authored
# token is written to the state file in plaintext, and a state bucket is
# usually readable by more people than a Kubernetes Secret is.
run "the_bearer_secrets_are_named_and_not_created" {
  command = apply

  assert {
    condition     = output.required_secrets == tolist(["core-agent-users", "lookout-watch-token"])
    error_message = "the Secrets an operator must create by default are not what this module tells them to create: ${jsonencode(output.required_secrets)}"
  }

  assert {
    condition     = yamldecode(output.values_yaml).auth.usersSecret == "core-agent-users"
    error_message = "the config was pointed at a different Secret than the one the output names"
  }
}

run "disabling_the_watcher_drops_its_secret_from_the_list" {
  command = apply

  variables {
    watcher_enabled = false
  }

  assert {
    condition     = output.required_secrets == tolist(["core-agent-users"])
    error_message = "an operator who disables the watcher should only need core-agent-users: ${jsonencode(output.required_secrets)}"
  }
}

run "extra_values_are_preserved_in_order" {
  command = apply

  variables {
    extra_values = [
      "logLevel: debug\n",
    ]
  }

  assert {
    condition     = length(yamldecode(output.values_yaml).extraValues) == 1 && yamldecode(output.values_yaml).extraValues[0] == "logLevel: debug\n"
    error_message = "extra_values did not reach the rendered configuration document"
  }
}

run "an_unpinned_image_tag_is_allowed" {
  command = apply

  assert {
    condition     = var.image_tag == null
    error_message = "image_tag has a default override"
  }
}

run "a_pinned_image_tag_reaches_the_config" {
  command = apply

  variables {
    image_tag = "2.9.0"
  }

  assert {
    condition     = yamldecode(output.values_yaml).image.tag == "2.9.0"
    error_message = "the image_tag pin did not reach the rendered configuration"
  }
}
