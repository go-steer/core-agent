# Governance SOPs — read on demand

The Platform Agent's fleet-governance playbooks are vendored under
`upstream/governance/`. They are **not** loaded into every turn — read the one
that matches the task with `read_file` before you start that work, and follow it.
(Under Hermes these ran as scheduled cron jobs; here they are operator- or
session-triggered until the cron increment lands.)

**Adapt the steps to this runtime.** Most SOPs were written for Hermes and name
tooling that is unavailable here — `kubectl`, `gcloud`, `export KUBECONFIG`, and
the dropped `platform_control` tools (`list_cc_pods`, `audit_log_searcher`, the
Config-Connector diagnostics). `bash` is disabled, so **do not** try to run those.
Instead: use the `gke` MCP tools for cluster/fleet state, `read_file`/`grep` for
files, plan every mutation with `record_plan`, and publish via a GitOps pull
request rather than an in-cluster apply. Where a SOP's only data source is a
disabled tool (notably `compliance_audit`, `security_patch_orchestrator`, and any
Config-Connector step — see the README's "Known gaps"), that check is degraded
here: report what you *can* observe via the `gke` MCP and flag the gap rather than
inventing a result.

| When the task is… | Read |
|---|---|
| First run in a new environment — discover projects, clusters, repos | `bootstrap-inventory-scan` → `upstream/governance/inventory.md` |
| Reconcile live fleet state against the source-of-truth blueprint | `upstream/governance/blueprint_sync_sop.md` |
| Security / RBAC posture audit | `upstream/governance/compliance_audit_sop.md` |
| Configuration drift across the fleet | `upstream/governance/fleet_consistency_drift_sop.md` |
| Fleet-wide cost / waste audit | `upstream/governance/fleet_wide_cost_analysis_sop.md` |
| Capacity planning / rebalancing across clusters | `upstream/governance/global_capacity_orchestrator_sop.md` |
| Deprecation / end-of-life tracking | `upstream/governance/lifecycle_deprecation_manager_sop.md` |
| Workload reliability / obtainability audit | `upstream/governance/obtainability_audit_sop.md` |
| Propagate a policy change across the fleet | `upstream/governance/policy_propagation_sop.md` |
| Upgrade / security-patch readiness | `upstream/governance/security_patch_orchestrator_sop.md` |
| Validate clusters against fleet standards | `upstream/governance/standardization_validator_sop.md` |

Each SOP names concrete `gke` MCP checks and a publish step. Remember the runtime
overlay: the publish/write path is a GitOps **pull request**, not an in-cluster
mutation, and every mutating step is gated behind `record_plan`.
