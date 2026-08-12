---
name: k8s-triage
description: |
  Handle a Kubernetes event inject shaped like
  {"kind": "k8s-event", "reason": "<Reason>", "namespace": "...", ...}.
  Loads the reason-specific reference and drives the diagnose → verify
  → propose loop for any k8s failure mode. Falls back to a generic
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

Follow the four steps below **in order**. You have a read-only tool
surface: the loop is diagnose → verify → propose, and the proposal is
the deliverable a human or a pipeline applies.

## Step 0 — record the plan first (or every MCP call is denied)

This daemon runs with `require_plan_artifact: true`. Plan-first gating
covers **MCP tools too**, not just writes — so `gke_get_k8s_resource`
is denied until `record_plan` has been called once in the session.
AGENTS.md step 1 already has you doing this; if you somehow reach a
"plan required" error, call `record_plan` and continue. Do not treat it
as a permission problem to report.

## Step 1 — load the reference

Call the `load_skill_resource` tool with:
- `skill_name`: `k8s-triage`
- `resource_path`: `references/{reason}.md`  (substitute the payload's `reason` verbatim; k8s reasons are CamelCase like `CrashLoopBackOff`)

If the call returns `ErrResourceNotFound`, retry with
`resource_path`: `references/_fallback.md`. That fallback covers unknown
or custom reasons with generic k8s troubleshooting guidance.

## Step 2 — follow the reference

Each reference has four sections in this order:

1. **Budget** — max turns and wall-time budget for this incident. Track
   it as you work. If you exceed budget without a conclusion, jump to
   Step 4 (Close).
2. **Diagnose (read-only)** — a numbered list of checks, each naming the
   `gke` MCP tool that answers it. Run them all before concluding
   anything. If a step points to another reference (e.g. "chain to
   `references/OOMKilled.md`"), load that file via `load_skill_resource`
   and continue from its Diagnose section.
3. **Convergence check** — the `wait_and_verify` call that decides
   RESOLVED vs UNRESOLVED. See Step 3.
4. **Remediation proposals** — a table of Evidence → Proposed change →
   Verify. Match your diagnosis to a row; if no row matches, escalate
   rather than guess. The Fix column is something you *write down*, not
   something you apply.

## Step 3 — verify before you conclude

Some failures clear on their own: a registry blip ends, a node comes
back, a rollout that was already in flight finishes. You cannot tell
that apart from a hard failure without looking twice — so look twice,
with one tool call:

```
wait_and_verify(
  tool:             "<a read-only tool from the reference's Verify column>",
  args_json:        "{\"namespace\": \"...\", \"name\": \"...\"}",
  expect_contains:  "<the string the check looks for>",
  interval_seconds: 15,
  timeout_seconds:  <the row's interval, in seconds>
)
```

The reference row's Verify column reads `interval → check`, which is
exactly the two arguments above. The whole poll loop comes back as ONE
tool result — attempts, elapsed time, and the last observation — so
waiting three minutes costs one turn instead of one turn per look. Use
`expect_jq` when the check is structural rather than a substring (e.g.
`.status.phase == "Running"`).

Rules for this step:

- **`verified: true` is the ONLY basis for `RESOLVED`.** The result
  object is your evidence; quote its `attempts` / `elapsed_seconds` in
  the summary.
- **`verified: false` is a finding, not a failure.** It means "I watched
  for N seconds and it did not converge" — exactly what an UNRESOLVED
  incident needs to be credible. Include the last observation.
- `wait_and_verify` refuses to poll anything it can't classify as
  read-only, so it can never re-apply anything in a loop. MCP servers
  don't advertise that classification, so the operator names the
  pollable MCP tools in `tools.wait_and_verify.poll_allow`; this recipe
  registers the five `gke` read tools. If the tool you need isn't
  there, say so in the summary rather than looping by hand.

## Step 4 — close the incident

Decide the status from the evidence you actually have:

| Status | When |
|---|---|
| `RESOLVED` | `wait_and_verify` observed the failure clear. Nothing to apply. |
| `UNRESOLVED` | It did not clear, and you have a concrete proposal from the reference table. |
| `ESCALATED` | It did not clear and no row matched, or the fix is out of scope (infra, data-loss risk, cluster-wide). |

**For UNRESOLVED and ESCALATED, call `alert` before you write the
summary** — the eventlog is an audit trail, not a page:

```
alert(
  target:  "oncall",
  level:   "critical",        // "warning" for a degraded-but-serving workload
  summary: "<reason> in <namespace>/<name> — <one line>",
  details: {
    "cluster":   "<cluster>",
    "namespace": "<namespace>",
    "name":      "<name>",
    "uid":       "<uid>",
    "reason":    "<reason>",
    "status":    "UNRESOLVED",
    "proposal":  "<the exact change you propose>",
    "evidence":  "<what wait_and_verify observed>",
    "session":   "<the /sessions/<sid> URL>"
  }
)
```

If the `alert` call fails (unset webhook, target refused), say so in the
summary — a silent escalation failure is the worst outcome here.

Then post the structured summary as your final message. Use this
template verbatim so downstream tooling (Cloud Logging filters, ticket
MCPs) can parse it:

```
INCIDENT SUMMARY
================
Status: RESOLVED | UNRESOLVED | ESCALATED
Incident: {namespace}/{name} ({uid})
Reason: {reason}
Cluster: {cluster}
Reference used: references/{reason}.md
Root cause: <one line>
Evidence:
  1. <tool call>  → <what it showed>
  2. wait_and_verify(<tool>, <condition>) → verified=<bool> after <n> attempts / <s>s
Proposal: <the exact change a human should apply, or "none — resolved" / "none — escalated">
Escalation: alert(oncall) sent | not sent (<why>)
Final state: <one line — pod state, deployment status, or similar>
Session URL: <the /sessions/<sid> URL a human operator can attach to>
```

For UNRESOLVED / ESCALATED incidents, include EXTRA detail beyond the
summary block:

- What you checked (every tool call + what it returned).
- What you'd check next if you had more budget.
- Any suspected root cause you couldn't confirm.

## Meta

- **Never invent tool names.** If a reference names a tool you don't see
  in your registered toolset, use the closest read tool you do have; if
  none covers the check, report that step as unavailable. There is no
  shell to fall back to.
- **Never claim an action you cannot take.** This agent has no write
  path to the cluster. "Restarted the pod", "applied the patch",
  "rolled back the deployment" are all false here — write them as
  proposals.
- **Cluster scope.** The payload's `cluster` field is authoritative and
  must match the cluster named in AGENTS.md. If it doesn't, escalate:
  you cannot switch context, and acting on the wrong cluster's data is
  worse than not acting.
- **Don't chase symptoms across pods.** A `CrashLoopBackOff` in pod A
  and a `FailedMount` in pod B are two incidents. Focus on the
  incident triple in the payload.
