# OOMKilled

Kubelet SIGKILL'd the container because its memory usage exceeded the
container's memory limit (or in rare cases, cgroup OOM at the node
level).

## Budget

- Max turns: 6
- Max wall time: 8 min

## Diagnose (read-only)

1. Confirm OOMKilled from the pod object:
   `gke_get_k8s_resource` (kind `Pod`) → `status.containerStatuses[].lastState.terminated.reason`
   should read `OOMKilled`; `exitCode: 137` is the correlated exit code (128 + SIGKILL 9).

2. Get the container's current memory limit from the same object:
   `spec.containers[?(@.name=="{container}")].resources.limits.memory`.
   If empty → the container has no memory limit, and the OOMKill came from
   NODE pressure. Go to step 5.

3. Compare usage to the limit. You have no metrics tool here: read what
   the app logged before it died (`gke_get_k8s_logs`, previous container)
   and note the limit and the request. If the workload exposes its own
   memory metrics in logs, quote them; otherwise say usage is unmeasured
   rather than estimating it.

4. Chronic or one-off? `gke_list_k8s_events` scoped to `{namespace}` and
   `{name}`, sorted by last-seen. Multiple OOMKilled events over hours =
   chronic; a single event = spike.

5. If the limit is missing (from step 2), look at the node:
   `gke_describe_k8s_resource` (kind `Node`, name `{context.node}`) —
   check the `MemoryPressure` condition and how much is allocated.

## Convergence check

An OOMKilled pod usually restarts. Confirm whether it is holding:

```
wait_and_verify(
  tool:            "gke_get_k8s_resource",
  args_json:       "{\"kind\": \"Pod\", \"namespace\": \"{namespace}\", \"name\": \"{name}\"}",
  expect_jq:       ".status.phase == \"Running\" and ([.status.containerStatuses[]?.ready] | all)",
  interval_seconds: 20,
  timeout_seconds: 300
)
```

`verified: true` on a one-off spike is a legitimate `RESOLVED` — say
explicitly that the pod recovered on restart and that the limit is
unchanged, so the next spike will do the same thing.

## Remediation proposals

| Evidence | Proposed change | Verify (interval → check) |
|---|---|---|
| Chronic OOM, peak usage close to the limit | Raise `{container}`'s memory limit on `<controller>` by 25–50%; give the exact new value | 5m → no new OOMKilled events; pod steady `Running` |
| One-off OOM right after a deploy (regression) | Roll `<controller>` back to the previous revision | 5m → ReplicaSet transitions; no OOMKilled |
| No memory limit AND the node reports MemoryPressure | Set a limit on `{container}` (historical peak + ~30%), or move the workload to a larger node pool. The limit is the immediate change; capacity is follow-on | 5m → pod stable; node pressure clears (or pod moved) |
| JVM/Node.js/Python heap not bounded to the container | Set `-Xmx` / `--max-old-space-size` / equivalent to ~75% of the container's memory limit, via the workload's env | 5m → memory plateau below the limit |
| Memory leak (usage climbs monotonically across restarts) | Raise the limit as a stop-gap and file a bug for the app team with a heap-dump request. Don't chase a leak from triage | 5m → interim stability; escalate |

## When to escalate

- Chronic OOM AND raising the limit would exceed the node's allocatable memory (needs a bigger node pool — infra decision).
- Suspected memory leak (needs a heap dump + app team).
- OOM affects multiple pods on the same node concurrently — likely a node-level issue (cgroup misconfig, runtime bug).

Escalation summary: the limit, the request, whether the OOM is chronic or acute, restart count, and explicitly that peak usage is unmeasured if no metrics source was available.
