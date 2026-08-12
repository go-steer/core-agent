# CrashLoopBackOff

## Budget

- Max turns: 8
- Max wall time: 10 min

## Diagnose (read-only)

1. Get the pod's current status:
   `gke_get_k8s_resource` (kind `Pod`, `{namespace}`, `{name}`).
   Note `status.containerStatuses[].state.waiting.reason` (should be `CrashLoopBackOff`), `state.waiting.message`, `lastState.terminated.reason`, `lastState.terminated.exitCode`.

2. Fetch the container's last logs:
   `gke_get_k8s_logs` for `{namespace}/{name}`, container `{container}`,
   with the previous-container option set and a tail of ~200 lines.
   (The previous container is the one that crashed; the current one is
   still restarting and usually has nothing yet.)

3. Check the pod's own events:
   `gke_list_k8s_events` scoped to `{namespace}` and the pod name, or
   `gke_describe_k8s_resource` on the pod (its Events section).

4. Route by exit code:
   - **exit code 137** → chain to `references/OOMKilled.md`. Kubelet SIGKILLs a container that exceeded its memory limit.
   - **exit code 143** → SIGTERM'd; usually a liveness probe. Chain to `references/Unhealthy.md`.
   - **exit code 1** with a stack trace or Python traceback → application-level failure; continue to the proposals table.
   - **exit code 2** → usually misuse of a shell builtin or bad command-line flags to the entrypoint.
   - **exit code 127** → command not found in the image; likely wrong `command:` or missing binary.
   - **exit code 126** → command found but not executable; permission or wrong architecture.
   - **exit code 128 + n** → fatal signal n (SIGSEGV = 139, SIGBUS = 138, SIGABRT = 134).

5. If logs mention `ImagePull*`, chain to `references/ImagePullBackOff.md`.

## Convergence check

A crash loop with a long backoff can still recover (a dependency came
back, a config was fixed by someone else). Look twice before you
conclude:

```
wait_and_verify(
  tool:            "gke_get_k8s_resource",
  args_json:       "{\"kind\": \"Pod\", \"namespace\": \"{namespace}\", \"name\": \"{name}\"}",
  expect_jq:       ".status.phase == \"Running\" and ([.status.containerStatuses[]?.ready] | all)",
  interval_seconds: 15,
  timeout_seconds: 180
)
```

`verified: true` → `RESOLVED`. `verified: false` → pick a row below and
report `UNRESOLVED` with the observation trail.

## Remediation proposals

You propose; a human or a pipeline applies. Name the object, the field,
and the value.

| Evidence | Proposed change | Verify (interval → check) |
|---|---|---|
| Init container timed out (`state.waiting.reason: PodInitializing` for >2m before crash) | Extend the init container's `initialDelaySeconds`, or add a `startupProbe` on the app container, in `<controller>` | 2m → pod `Running` |
| Bad config in ConfigMap (recent change; logs show config parse errors) | Restore the prior ConfigMap revision (from Git, or the last known-good copy) and restart the workload | 3m → new pod `Ready`, no new CrashLoopBackOff events |
| Application crash from a bad deploy (logs show a recent code error) | Roll `<controller>` back to the previous revision | 5m → ReplicaSet on the old revision; pods `Ready` |
| Secret rotated / stale (logs show auth failure, 401, JWT expired) | Refresh the Secret's credential, then restart `<controller>` to re-mount it | 2m → pod `Ready`; no new auth errors in logs |
| Missing dependency (logs show DNS failure to a sibling service) | Report which Service is missing (check with `gke_get_k8s_resource` on the Service first). If it exists, the NetworkPolicy blocking the pod's SA is the change to make | 3m → pod `Ready`; no DNS errors in logs |
| exit code 127/126 (bad entrypoint) | Correct `command:`/`args:` on `<controller>`. Quote the image's expected entrypoint if the logs reveal it; do not guess | 2m → pod starts + `Ready` |

## When to escalate

- No matching row above AND you've exhausted the Diagnose steps.
- Exit code you don't recognize.
- Multi-container pod where the crashing container isn't the one the payload names (payload's `container` field may be empty — infer from `containerStatuses[]`).
- Application logs are cryptic; you'd be guessing at the code path.
- The change is riskier than a rollback (schema migrations, data-touching jobs).

Escalation summary should include: exit code, first 10 lines of logs, whether the pod was ever `Ready` (from `status.containerStatuses[].lastState`), and the restart count.
