# BackOff

Generic backoff event. Kubelet emits this alongside `CrashLoopBackOff`,
`ImagePullBackOff`, and a few other retry scenarios. If you got here
without a more-specific reason, chain first.

## Chain first

Read the pod's actual waiting reason before anything else:
`gke_get_k8s_resource` (kind `Pod`, `{namespace}`, `{name}`) →
`status.containerStatuses[].state.waiting.reason`.

- `CrashLoopBackOff` → chain to `references/CrashLoopBackOff.md`
- `ImagePullBackOff` or `ErrImagePull` → chain to `references/ImagePullBackOff.md`
- Empty → the container isn't waiting; the BackOff is from an init container or a controller-level retry. Continue below.

## Budget

- Max turns: 4
- Max wall time: 4 min

## Diagnose (read-only)

1. `gke_describe_k8s_resource` (kind `Pod`) — the full Events section.
2. The controller: `gke_describe_k8s_resource` on the
   Deployment/StatefulSet/DaemonSet named in `{context.controller_ref}` —
   its events may show ReplicaSet backoffs.
3. If it's a Job or CronJob: `gke_get_k8s_resource` (kind `Job`) →
   `spec.backoffLimit` and `status.failed`.

## Convergence check

```
wait_and_verify(
  tool:            "gke_get_k8s_resource",
  args_json:       "{\"kind\": \"Pod\", \"namespace\": \"{namespace}\", \"name\": \"{name}\"}",
  expect_not_contains: "BackOff",
  interval_seconds: 15,
  timeout_seconds: 120
)
```

## Remediation proposals

| Evidence | Proposed change | Verify (interval → check) |
|---|---|---|
| Job hit `backoffLimit` | Raise `spec.backoffLimit`, or fix the underlying container failure (chain to `CrashLoopBackOff.md`) — say which, and why | 2m → job progresses |
| ReplicaSet backoff on a Deployment | The backoff is a symptom; chain to `CrashLoopBackOff.md` | See CrashLoopBackOff |
| Init-container retry loop | Read the init container's logs (`gke_get_k8s_logs`, previous container) and propose the fix for what they show | 3m → init container completes |

## When to escalate

- No specific reason surfaces and the pod is stuck in backoff without a clear trigger. Include the full describe output in the escalation.
