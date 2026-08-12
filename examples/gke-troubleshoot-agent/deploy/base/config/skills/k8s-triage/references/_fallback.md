# Fallback triage — unknown reason

You loaded this file because the k8s Event `reason` doesn't have a
dedicated reference. Follow this generic playbook, and lean toward
escalation: unknown reasons are where guessing does the most damage.

## Budget

- Max turns: 5
- Max wall time: 6 min (be conservative; unknown = higher risk of chasing tangents)

## Diagnose (read-only)

1. Establish the target's current state:
   `gke_describe_k8s_resource` for `{kind_of_object}` `{name}` in
   `{namespace}`. Read the Events section — it usually explains why the
   reason fired.

2. Look at surrounding events on the same object:
   `gke_list_k8s_events` scoped to `{namespace}` / `{name}`, newest last.
   The timeline often shows a cascade (e.g. FailedScheduling → NotReady →
   SomeCustomReason).

3. Look at the same reason cluster-wide:
   `gke_list_k8s_events` filtered to `reason={reason}` across namespaces.
   - Every pod in one namespace → namespace-wide cause (RBAC, quota, admission webhook).
   - Every pod on one node → node issue.

4. Work out what emits this reason. Built-in reasons come from kubelet
   or a core controller; the rest come from an operator (Istio,
   cert-manager, Prometheus operator, Argo, …), and the message usually
   names its source. If you can't attribute it, say so.

## Convergence check

Before concluding anything about an unfamiliar reason, establish whether
the object is actually still unhealthy:

```
wait_and_verify(
  tool:            "gke_get_k8s_resource",
  args_json:       "{\"kind\": \"{kind_of_object}\", \"namespace\": \"{namespace}\", \"name\": \"{name}\"}",
  expect_jq:       ".status.phase == \"Running\"",
  interval_seconds: 20,
  timeout_seconds: 180
)
```

Adjust the condition to the object kind — a Deployment's health is
`.status.readyReplicas == .spec.replicas`, a Job's is
`.status.succeeded >= 1`. If you can't express a condition you trust for
this kind, skip the wait and say why.

## Remediation proposals

Without a specific reference, these are the broad, reversible changes
worth proposing. Each is something a human applies:

| Evidence | Proposed change | Verify (interval → check) |
|---|---|---|
| A recent deploy correlates (event started <30m ago; the controller's `metadata.generation` / rollout history changed) | Roll `<controller>` back to the previous revision | 5m → the event stops firing |
| A custom controller looks stuck (its pod is up but not reconciling) | Restart the controller pod in its namespace; name the pod | 3m → the controller reconciles; the event stops |
| An admission webhook is failing (message mentions `admission webhook`) | Check the webhook's backing pod with `gke_get_k8s_resource`; if it's down, that's the fix. Removing a ValidatingWebhookConfiguration is a last resort and needs explicit human approval — never propose it silently | 3m → resource operations succeed |
| API rate limiting (message mentions `429 Too Many Requests`) | Identify the caller hammering the API (kube-state-metrics with a low interval, a custom controller) and reduce its polling | 5m+ (may exceed budget) |

## When to escalate (probably right away)

Unknown reasons deserve conservative escalation. Call
`alert(target: "oncall", ...)` and include:

- The specific `reason` string that hit fallback.
- Whether it's cluster-wide, namespace-wide, or single-pod (step 3).
- The controller / operator you think emits it, and how confident you are.
- What the convergence check observed.
- Explicitly: that you took no action, because this agent cannot.

A human can pattern-match on a reason string in seconds; an agent
guessing at unknown reasons is a well-known way to make an incident
worse. When in doubt: escalate.
