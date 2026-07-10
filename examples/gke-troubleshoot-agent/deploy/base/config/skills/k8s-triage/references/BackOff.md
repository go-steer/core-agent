# BackOff

Generic backoff event. Kubelet emits this alongside `CrashLoopBackOff`,
`ImagePullBackOff`, and a few other retry scenarios. If you got here
without a more-specific reason, chain first.

## Chain first

Try more-specific references based on the pod's actual state:

1. `kubectl -n {namespace} get pod {name} -o jsonpath='{.status.containerStatuses[*].state.waiting.reason}'`
2. Based on the result:
   - `CrashLoopBackOff` → chain to `references/CrashLoopBackOff.md`
   - `ImagePullBackOff` or `ErrImagePull` → chain to `references/ImagePullBackOff.md`
   - Empty → the container isn't waiting; the BackOff may be from init containers or a controller-level retry. Continue below.

## Budget

- Max turns: 4
- Max wall time: 4 min

## Diagnose

1. `kubectl -n {namespace} describe pod {name}` — full Events section.
2. Check the controller (Deployment/StatefulSet/DaemonSet):
   `kubectl -n {namespace} describe deployment <controller-name>` — Events may show `ReplicaSet` backoffs.
3. If Job or CronJob: `kubectl -n {namespace} describe job <name>` — check `spec.backoffLimit`.

## Common fixes

| Symptom | Fix | Verify |
|---|---|---|
| Job hit backoffLimit | Raise `spec.backoffLimit` OR fix the underlying container failure (chain to `CrashLoopBackOff.md`) | 2m → job progresses |
| ReplicaSet backoff (Deployment) | Chain to `CrashLoopBackOff.md`; the backoff is a symptom | See CrashLoopBackOff. |
| Init-container retry loop | Fetch init container logs (`kubectl logs {name} -c <init-container> --previous`), fix the underlying issue | 3m → init container completes |

## When to escalate

- No specific reason surfaces; pod is stuck in backoff without a clear trigger. Post the full `describe pod` output in the escalation summary.
