# CrashLoopBackOff

## Budget

- Max turns: 8
- Max wall time: 10 min

## Diagnose

1. Get the pod's current status:
   `kubectl -n {namespace} get pod {name} -o yaml`
   Note the `status.containerStatuses[].state.waiting.reason` (should be `CrashLoopBackOff`), `state.waiting.message`, `lastState.terminated.reason`, `lastState.terminated.exitCode`.

2. Fetch the container's last logs:
   `kubectl -n {namespace} logs {name} -c {container} --previous --tail=200`
   (Add `--previous` to see the crashed container's output, not the currently-restarting one.)

3. Check the pod's own events:
   `kubectl -n {namespace} describe pod {name}` (Events section at the bottom.)

4. Route by exit code:
   - **exit code 137** → chain to `references/OOMKilled.md`. Kubelet SIGKILLs a container that exceeded its memory limit.
   - **exit code 143** → SIGTERM'd; usually a liveness probe. Chain to `references/Unhealthy.md`.
   - **exit code 1** with a stack trace or Python traceback → application-level failure; continue to Common fixes.
   - **exit code 2** → usually misuse of a shell builtin or bad command-line flags to the entrypoint.
   - **exit code 127** → command not found in the image; likely wrong `command:` or missing binary.
   - **exit code 126** → command found but not executable; permission or wrong architecture.
   - **exit code 128 + n** → fatal signal n (SIGSEGV = 139, SIGBUS = 138, SIGABRT = 134).

5. If logs mention `ImagePull*`, chain to `references/ImagePullBackOff.md`.

## Common fixes

| Symptom | Fix | Verify (interval → check) |
|---|---|---|
| Init container timed out (`state.waiting.reason: PodInitializing` for >2m before crash) | Extend init container's `initialDelaySeconds` or add a longer `startupProbe`. `kubectl -n {namespace} edit deployment <controller>` | 2m → `kubectl get pod {name}` shows `Running` |
| Bad config in ConfigMap (recent change; logs show config parse errors) | `kubectl rollout undo` the ConfigMap by re-applying prior revision (fetch from `git log` or `kubectl get cm <name> -o yaml` from a backup) then trigger pod restart | 3m → new pod comes up `Ready` and no new CrashLoopBackOff events |
| Application crash from bad deploy (logs show recent code error) | `kubectl -n {namespace} rollout undo deployment <controller>` | 5m → replicaset transitions to old revision; pods `Ready` |
| Secret rotated / stale (logs show auth failure, 401, JWT expired) | Re-mount the Secret via `kubectl rollout restart deployment <controller>` OR update the Secret if the credential itself is stale | 2m → pod `Ready`; no new auth-error logs |
| Missing dependency (logs show DNS failure to sibling service) | Verify the dep exists: `kubectl -n {namespace} get svc <dep>`. If missing → deploy it. If present → check NetworkPolicy allows the pod's SA. | 3m → pod `Ready`; no DNS errors in logs |
| exit code 127/126 (bad entrypoint) | Fix the Deployment's `command:`/`args:` field. Requires knowing the correct entrypoint — check the image's Dockerfile ENTRYPOINT. | 2m → pod starts + `Ready` |

## When to escalate

- No matching row in Common fixes AND you've exhausted the Diagnose steps.
- Exit code you don't recognize.
- Multi-container pod where the crashing container isn't the one the payload names (payload's `container` field may be empty — infer from `containerStatuses[]`).
- Application logs are cryptic; you'd be guessing at the code path.
- Fix applied twice, both reverted; issue persists.

Escalation summary should include: exit code, first 10 lines of logs, whether the pod was Ready recently (check `kubectl get pod` `LastRestart` timestamp), any recent `kubectl` operations on the controller.
