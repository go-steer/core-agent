# OOMKilled

Kubelet SIGKILL'd the container because its memory usage exceeded the
container's memory limit (or in rare cases, cgroup OOM at the node
level).

## Budget

- Max turns: 6
- Max wall time: 8 min

## Diagnose

1. Confirm OOMKilled from the pod spec:
   `kubectl -n {namespace} get pod {name} -o jsonpath='{.status.containerStatuses[*].lastState.terminated.reason}'`
   Should print `OOMKilled`. `exitCode: 137` is the correlated exit code (128 + SIGKILL 9).

2. Get the container's current memory limit:
   `kubectl -n {namespace} get pod {name} -o jsonpath='{.spec.containers[?(@.name=="{container}")].resources.limits.memory}'`
   If empty → container has no memory limit; OOMKill happened because the NODE ran out of memory. Chain to Node investigation below.

3. Get recent memory usage from metrics (if metrics-server is installed):
   `kubectl -n {namespace} top pod {name} --containers`
   Note the peak; compare to the limit.

4. Check whether this is a chronic issue or a one-off:
   `kubectl -n {namespace} get events --field-selector involvedObject.name={name} --sort-by='.lastTimestamp'`
   Multiple OOMKilled events over hours = chronic; single event = spike.

5. If limit is missing (from Step 2): investigate the node.
   `kubectl describe node {context.node}` and check `MemoryPressure` condition and pod density.

## Common fixes

| Symptom | Fix | Verify (interval → check) |
|---|---|---|
| Chronic OOM, peak usage close to limit | Raise the container's memory limit by 25–50%. `kubectl -n {namespace} set resources deployment <controller> -c {container} --limits=memory=<new>` | 5m → no new OOMKilled events; pod steady in `Running` |
| One-off OOM after deploy (regression) | `kubectl -n {namespace} rollout undo deployment <controller>` | 5m → replicaset transitions; no OOMKilled |
| No memory limit AND node under MemoryPressure | Set a limit on the container (whatever the historical peak was + 30%) OR migrate the pod to a bigger node pool. Setting a limit is the immediate fix; capacity work is follow-on. | 5m → pod stable; NodePressure condition clears (or pod moved to healthy node) |
| JVM/Node.js/Python heap tuning missing | Set `-Xmx`/`--max-old-space-size`/similar to ~75% of the container's memory limit. Requires editing the entrypoint or env vars. | 5m → memory usage plateau below limit |
| Memory leak (usage climbs monotonically) | Raise limit as a stop-gap; file a bug for the app team. Don't chase a leak from triage — this is dev work, not SRE work. | 5m → interim stability; escalate with heap-dump request |

## When to escalate

- Chronic OOM AND raising the limit puts the pod above the node's available memory (would need a bigger node pool — infra decision).
- Suspected memory leak (need heap dump + app-team involvement).
- OOM affects multiple pods on the same node concurrently — likely a node-level issue (cgroup misconfig, container runtime bug).

Escalation summary: peak memory, current limit, requested-to-limit ratio, whether metrics-server is installed, whether the OOM is chronic or acute.
