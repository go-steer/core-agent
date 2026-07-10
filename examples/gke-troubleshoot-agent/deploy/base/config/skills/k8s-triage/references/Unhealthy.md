# Unhealthy

A probe (liveness, readiness, or startup) failed. Kubelet emits `Unhealthy`
per failed probe. The sidecar's default filter requires 3 consecutive
Unhealthy events before firing so probe flapping doesn't drown triage.

## Budget

- Max turns: 6
- Max wall time: 8 min

## Diagnose

1. Get the pod's probe definitions:
   `kubectl -n {namespace} get pod {name} -o jsonpath='{.spec.containers[?(@.name=="{container}")].livenessProbe}{.spec.containers[?(@.name=="{container}")].readinessProbe}{.spec.containers[?(@.name=="{container}")].startupProbe}'`

2. Identify WHICH probe is failing from the events:
   `kubectl -n {namespace} describe pod {name}` — Events section says e.g. `Liveness probe failed: HTTP probe failed with statuscode: 500`.

3. Test the probe manually:
   - HTTP: `kubectl -n {namespace} exec {name} -c {container} -- wget -qO- --timeout=2 http://localhost:<port><path>`
   - TCP: `kubectl -n {namespace} exec {name} -c {container} -- nc -zv localhost <port>`
   - exec: reproduce the exec command inline: `kubectl -n {namespace} exec {name} -c {container} -- <cmd>`

4. Distinguish transient (once every N minutes) from persistent (every probe fails):
   `kubectl -n {namespace} get events --field-selector involvedObject.name={name},reason=Unhealthy --sort-by='.lastTimestamp'`

## Common fixes

| Symptom | Fix | Verify (interval → check) |
|---|---|---|
| App is slow to start; startup probe timing out | Add / extend `startupProbe.failureThreshold` or `initialDelaySeconds`. Startup probes disable liveness/readiness until they pass — the right primitive for "container needs 90s to warm up." | 3m → pod Ready + no new Unhealthy events |
| App has a real bug (probe endpoint returns 500) | Chain to `references/CrashLoopBackOff.md` — treat as application failure; the fix is a rollback or code change. | See CrashLoopBackOff. |
| Probe misconfigured (wrong path, wrong port) | `kubectl edit deployment <controller>` → fix `livenessProbe.httpGet.path` or `port`. | 2m → probes pass |
| Timeout too aggressive (`timeoutSeconds: 1` on a service that takes 800ms) | Raise `timeoutSeconds` to 3–5s. | 3m → no new Unhealthy events |
| Downstream dependency is slow (probe hits an /health endpoint that depends on a DB) | Fix the dependency OR make the probe local-only (don't require deps to be healthy for liveness). Better probe design: readiness gates on deps, liveness only on process life. | 5m → probes stabilize |

## When to escalate

- Real application bug (probe endpoint is correct but the app can't serve it). Escalate to app team.
- Probe is testing a real dependency that's down (chain investigation upstream).
- Cluster-wide network issue causing probes to timeout for many pods (chain to `NetworkNotReady.md`).
