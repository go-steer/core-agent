# Fallback triage — unknown reason

You loaded this file because the k8s Event `reason` doesn't have a
dedicated reference. Follow this generic playbook.

## Budget

- Max turns: 5
- Max wall time: 6 min (be conservative; unknown = higher risk of chasing tangents)

## Diagnose

1. Establish the target's current state:
   `kubectl -n {namespace} describe {kind_of_object} {name}` (e.g. `describe pod`, `describe deployment`).
   Read the full Events section at the bottom — it usually explains why the reason fired.

2. Look at surrounding events on the same object:
   `kubectl -n {namespace} get events --field-selector involvedObject.name={name} --sort-by='.lastTimestamp'`
   The full event timeline often shows a cascade (e.g. FailedScheduling → NotReady → SomeCustomReason).

3. Look at events cluster-wide with the same reason:
   `kubectl get events --all-namespaces --field-selector reason={reason} --sort-by='.lastTimestamp'`
   If ALL pods in a namespace have this event, it's probably a namespace-wide issue (RBAC, quota, admission controller).
   If ALL pods across namespaces on the SAME node have it, it's a node issue.

4. Understand what emits this reason:
   `reason` values come from either kubelet (built-in reasons like `CrashLoopBackOff`) or from custom controllers (Istio, cert-manager, Prometheus operator, Argo, etc.). A reason like `AdmissionWebhookFailed` names its source in the message.

## Common fixes

Without a specific reference, these are the meta-fixes worth trying — each is safe (reversible) and covers a broad failure class:

| Symptom class | Fix | Verify |
|---|---|---|
| Recent deploy caused it (event started <30m ago; recent Deployment change visible in `kubectl rollout history`) | `kubectl -n {namespace} rollout undo deployment <controller>` | 5m → event stops firing |
| Custom controller is stuck | Restart the controller pod: `kubectl -n <controller-ns> delete pod <controller-pod>` | 3m → new controller reconciles; event stops |
| Admission webhook is broken (event mentions `admission webhook`) | Check the webhook's pod is Ready: `kubectl -n <webhook-ns> get pods`. If down, investigate/restart. If the webhook is optional, remove the ValidatingWebhookConfiguration temporarily to unblock. | 3m → resource creates succeed |
| Rate-limiting from k8s API (event mentions `429 Too Many Requests`) | Reduce polling frequency of whatever's hammering the API. Common culprits: kube-state-metrics with low interval, poorly-written custom controllers. | 5m+ (may exceed budget) |

## When to escalate (probably right away)

Unknown reasons deserve conservative escalation. Post the full
`describe` output + surrounding events + your best hypothesis to the
escalation MCP. Include:

- The specific `reason` string that hit fallback.
- Whether the reason is cluster-wide, namespace-wide, or single-pod.
- The controller / operator you think emits this reason (guess from the message).
- What safe/reversible actions you took (if any).

A human can quickly pattern-match on the reason string; agents guessing at unknown reasons is a common failure mode. When in doubt: escalate.
