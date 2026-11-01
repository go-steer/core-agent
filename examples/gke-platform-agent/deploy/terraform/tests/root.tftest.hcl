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

# Originally derived from go-steer/mast:examples/deploy/terraform/tests/root.tftest.hcl.

# The root module owns the values that have to agree across the two halves.
# These runs are about that seam — what each half does with a value it
# receives is tested inside that half.

mock_provider "google" {
  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }
}

mock_provider "kubernetes" {}

variables {
  project_id = "example-project"
}

# The namespace is inside the WIF principal's subject AND inside the RBAC
# subject the release binds. Two independent copies that drift produce a
# daemon that is Forbidden on every call while each half, read alone, looks
# right. So this run derives both ends from what was actually built and
# compares them.
run "one_namespace_reaches_the_identity_and_the_install" {
  command = apply

  variables {
    namespace = "sre-tools"
  }

  assert {
    condition     = output.install_namespace == "sre-tools"
    error_message = "the release went into ${output.install_namespace}"
  }

  assert {
    condition     = strcontains(output.wif_principal, "/subject/ns/${output.install_namespace}/sa/core-agent-daemon")
    error_message = "the IAM principal names a different namespace than the install: ${output.wif_principal}"
  }

  assert {
    condition     = strcontains(output.rbac_subject, "[${output.install_namespace}/core-agent-daemon]")
    error_message = "the RBAC subject names a different namespace than the install: ${output.rbac_subject}"
  }

  assert {
    condition     = output.release_rbac_subject == output.rbac_subject
    error_message = "module.release and module.wif disagree on rbac_subject: ${output.release_rbac_subject} vs ${output.rbac_subject}"
  }
}

# The daemon's ServiceAccount name is fixed by deploy/base/10-serviceaccount-daemon.yaml.
# Both halves hardcode it; this asserts they hardcoded the same thing.
run "both_halves_name_the_same_service_account" {
  command = apply

  assert {
    condition     = strcontains(output.wif_principal, "/sa/core-agent-daemon") && strcontains(output.rbac_subject, "/core-agent-daemon]") && output.release_rbac_subject == output.rbac_subject
    error_message = "the two identity strings disagree about the daemon's ServiceAccount: ${output.wif_principal} vs ${output.rbac_subject} vs ${output.release_rbac_subject}"
  }
}

# Autopilot runs the metadata server on every node and REJECTS this
# selector, so the default has to be the Autopilot-safe one: a wrong default
# here fails admission rather than doing nothing.
run "autopilot_is_the_default_and_gets_no_node_selector" {
  command = apply

  assert {
    condition     = length(yamldecode(output.chart_values).daemon.nodeSelector) == 0
    error_message = "the default install pinned the daemon onto selected nodes, which Autopilot refuses: ${jsonencode(yamldecode(output.chart_values).daemon.nodeSelector)}"
  }
}

run "gke_standard_pins_the_daemon_onto_metadata_server_nodes" {
  command = apply

  variables {
    gke_standard = true
  }

  assert {
    condition     = yamldecode(output.chart_values).daemon.nodeSelector["iam.gke.io/gke-metadata-server-enabled"] == "true"
    error_message = "GKE Standard did not get the metadata-server selector, so the daemon can land on a node where Workload Identity Federation is not served: ${jsonencode(yamldecode(output.chart_values).daemon.nodeSelector)}"
  }
}

# write_scope is the IAM half of the boundary and remediation_namespaces is
# the cluster half. GKE allows a call if IAM OR RBAC allows it, so a
# cluster-admin IAM binding makes the namespaced Roles decorative.
run "the_shipped_default_can_change_nothing_on_either_side" {
  command = apply

  assert {
    condition     = output.container_role == "roles/container.viewer"
    error_message = "the default IAM grant is ${output.container_role}, which authorizes writes regardless of what RBAC says"
  }

  assert {
    condition     = length(yamldecode(output.chart_values).remediationNamespaces) == 0
    error_message = "the default install holds a write grant in ${jsonencode(yamldecode(output.chart_values).remediationNamespaces)}"
  }
}

run "asking_for_cluster_admin_says_so_in_the_plan" {
  command = apply

  variables {
    write_scope = "cluster-admin"
  }

  assert {
    condition     = output.container_role == "roles/container.admin"
    error_message = "cluster-admin resolved to ${output.container_role}"
  }
}

run "setting_remediation_namespaces_narrows_iam_to_viewer" {
  command = apply

  variables {
    write_scope            = "cluster-admin"
    remediation_namespaces = ["team-a"]
  }

  assert {
    condition     = output.container_role == "roles/container.viewer"
    error_message = "remediation_namespaces must narrow IAM to roles/container.viewer so per-namespace RBAC is the active boundary, got ${output.container_role}"
  }

  assert {
    condition     = output.kustomize_overlay == "gated-apply"
    error_message = "expected kustomize_overlay 'gated-apply', got ${output.kustomize_overlay}"
  }
}

# The bearer Secrets are the one thing this configuration deliberately does
# not create, so the operator has to be told — by name, in the output, not
# in a README they have already closed.
run "the_apply_ends_by_naming_what_it_did_not_do" {
  command = apply

  assert {
    condition     = output.required_secrets == tolist(["core-agent-users", "lookout-watch-token"])
    error_message = "expected both bearer Secrets to be named as the manual step, got ${jsonencode(output.required_secrets)}"
  }

  assert {
    condition     = alltrue([for s in output.required_secrets : strcontains(output.next_steps, s)])
    error_message = "a Secret this install requires is missing from next_steps, which is where an operator reads it"
  }

  assert {
    condition     = strcontains(output.next_steps, "core-agent can change NOTHING right now")
    error_message = "a default install must say out loud that it holds no write grant; next_steps said: ${output.next_steps}"
  }
}
