---
name: k8s-triage
description: |
  Handle a Kubernetes event inject shaped like
  {"kind": "k8s-event", "reason": "<Reason>", "namespace": "...", ...}.
  Loads the reason-specific reference and drives the diagnose → fix
  → verify loop for any k8s failure mode. Falls back to a generic
  playbook (references/_fallback.md) for unknown reasons.
---

# k8s triage router

You have been invoked with a triage inject from the `k8s-event-watcher`
sidecar. The message body is a JSON payload with these fields:

```
{
  "kind": "k8s-event",
  "reason": "CrashLoopBackOff",       // the k8s Event.Reason
  "namespace": "...",
  "kind_of_object": "Pod",
  "name": "...",
  "container": "...",                  // may be empty
  "uid": "...",
  "message": "...",
  "count": 5,                          // sidecar's dedup count
  "first_seen": "...",
  "last_seen": "...",
  "cluster": "prod-us-central1",
  "context": { "controller_ref": "...", "node": "...", "labels": {...} }
}
```

Follow the four steps below **in order**. Do NOT skip to fixing before
diagnosing; do NOT apply a fix without composing a `record_plan` when
plan-first is enabled.

## Step 1 — load the reference

Call the `load_skill_resource` tool with:
- `skill_name`: `k8s-triage`
- `resource_path`: `references/{reason}.md`  (substitute the payload's `reason` verbatim; k8s reasons are CamelCase like `CrashLoopBackOff`)

If the call returns `ErrResourceNotFound`, retry with
`resource_path`: `references/_fallback.md`. That fallback covers unknown
or custom reasons with generic k8s troubleshooting guidance.

## Step 2 — follow the reference

Each reference has three sections in this order:

1. **Budget** — max turns and wall-time budget for this incident. Track
   it as you work. If you exceed budget without resolution, jump to
   Step 4 (Close).
2. **Diagnose** — a numbered list of checks. Run them all before
   proposing any fix. If a step points to another reference
   (e.g. "chain to `references/OOMKilled.md`"), load that file via
   `load_skill_resource` and continue from its Diagnose section.
3. **Common fixes** — a table of Symptom → Fix → Verify. Match the
   diagnosis to a row; if no row matches, escalate rather than
   guess.

## Step 3 — fix-and-verify

Before applying ANY mutating action:

1. If the session has `require_plan_artifact: true` (check the mode via
   `/mode` if unsure), call `record_plan` with:
   - What you observed
   - What fix you propose
   - The verify criterion (from the reference table's Verify column)
   - Rollback plan if verify fails
2. Apply the fix (via the GKE MCP: `apply_manifest`, `patch_resource`,
   `scale_deployment`, `rollout_undo`, etc.; or via `bash` +
   `kubectl` if the MCP tool for that action doesn't exist).
3. Sleep the verify interval named in the reference row.
4. Re-run the Diagnose section from Step 2. Note which checks now pass.
5. Decision:
   - **All Diagnose checks pass** → Step 4 (Close, resolved).
   - **Original checks pass but new events fired** → repeat Diagnose;
     may indicate a cascade; may need to chain to another reference.
   - **Still failing after 2 attempts** → revert the fix if possible
     (`rollout_undo`, restore prior ConfigMap revision, etc.), then
     jump to Step 4 (Close, unresolved).

## Step 4 — close the incident

Post a structured summary as your final message. Use this template
verbatim so downstream tooling (Slack MCP, ticket MCPs) can parse it:

```
INCIDENT SUMMARY
================
Status: RESOLVED | UNRESOLVED | ESCALATED
Incident: {namespace}/{name} ({uid})
Reason: {reason}
Cluster: {cluster}
Reference used: references/{reason}.md
Root cause: <one line>
Actions taken:
  1. <action>  → <outcome>
  2. <action>  → <outcome>
Final state: <one line — pod state, deployment status, or similar>
```

If Status is UNRESOLVED or ESCALATED, ALSO call the escalation MCP
(check `/mcp` for `slack`, `jira`, `pagerduty`, or `webhook`
integrations). Attach the summary above plus:
- Session URL — the human operator can attach a TUI to continue.
- What was tried (verbose — every command + response).
- What you'd try next if you had more budget / permissions.

If Status is RESOLVED, no escalation call is needed; the eventlog is
the audit trail.

## Meta

- **Never invent tool names.** If a reference names an MCP tool
  you don't see in `/mcp`, degrade gracefully to `bash` + `kubectl`.
- **Cluster scope.** The payload's `cluster` field is authoritative.
  If the current MCP context doesn't match, switch context via
  `gcloud container clusters get-credentials` before acting.
- **Don't chase symptoms across pods.** A `CrashLoopBackOff` in pod A
  and a `FailedMount` in pod B are two incidents. Focus on the
  incident triple in the payload.
