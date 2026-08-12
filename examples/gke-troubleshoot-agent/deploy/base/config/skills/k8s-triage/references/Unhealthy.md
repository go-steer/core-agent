# Unhealthy

A probe (liveness, readiness, or startup) failed. Kubelet emits `Unhealthy`
per failed probe. The sidecar's default filter requires 3 consecutive
Unhealthy events before firing so probe flapping doesn't drown triage.

## Budget

- Max turns: 6
- Max wall time: 8 min

## Diagnose (read-only)

1. Get the pod's probe definitions:
   `gke_get_k8s_resource` (kind `Pod`) →
   `spec.containers[?(@.name=="{container}")].livenessProbe` /
   `.readinessProbe` / `.startupProbe`.

2. Identify WHICH probe is failing:
   `gke_list_k8s_events` scoped to `{namespace}` / `{name}` — the message
   says e.g. `Liveness probe failed: HTTP probe failed with statuscode: 500`.

3. See what the app says about it:
   `gke_get_k8s_logs` for the container around the probe failures. A
   probe returning 500 usually leaves a matching request log.

   You cannot exec into the container to reproduce the probe — no shell,
   no exec tool. Reason from the probe spec, the event message and the
   logs, and say so rather than implying you ran the probe yourself.

4. Transient or persistent? Compare the event `count` and first/last
   timestamps from step 2: every-probe failures and once-per-N-minutes
   failures have different fixes.

## Convergence check

Probe failures during a slow start are the common false alarm — the pod
becomes `Ready` a minute later on its own:

```
wait_and_verify(
  tool:            "gke_get_k8s_resource",
  args_json:       "{\"kind\": \"Pod\", \"namespace\": \"{namespace}\", \"name\": \"{name}\"}",
  expect_jq:       "[.status.conditions[]? | select(.type == \"Ready\") | .status] | index(\"True\") != null",
  interval_seconds: 15,
  timeout_seconds: 180
)
```

## Remediation proposals

| Evidence | Proposed change | Verify (interval → check) |
|---|---|---|
| App is slow to start; startup probe times out | Add or extend `startupProbe.failureThreshold` / `initialDelaySeconds` on `<controller>`. Startup probes suspend liveness/readiness until they pass — the right primitive for "needs 90s to warm up" | 3m → pod `Ready`, no new Unhealthy events |
| Probe endpoint returns 500 (real app bug) | Chain to `references/CrashLoopBackOff.md` — treat it as an application failure; the change is a rollback or a code fix | See CrashLoopBackOff |
| Probe misconfigured (wrong path or port vs. the container's actual listener) | Correct `livenessProbe.httpGet.path` / `.port` on `<controller>`; quote the port the container spec actually exposes | 2m → probes pass |
| Timeout too aggressive (`timeoutSeconds: 1` against a ~800ms endpoint) | Raise `timeoutSeconds` to 3–5s | 3m → no new Unhealthy events |
| Probe depends on a downstream service | Make liveness local-only (process life) and let readiness gate on dependencies; name both probes' current definitions | 5m → probes stabilize |

## When to escalate

- Real application bug (the probe is correct but the app can't serve it). Escalate to the app team with the failing status code and log lines.
- The probe tests a dependency that is itself down (chain the investigation upstream, then escalate).
- Cluster-wide probe timeouts across many pods (chain to `NetworkNotReady.md`).
