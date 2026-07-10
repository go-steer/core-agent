---
title: Kubernetes troubleshooting agent
weight: 85
---

Semi-autonomous Kubernetes triage running as `core-agent` inside your cluster. A `k8s-event-watcher` sidecar streams filtered Events into per-incident sessions on the daemon; a router skill (`k8s-triage`) loads reason-specific references and drives the diagnose → fix → verify loop. In v2.6, incident summaries land in the eventlog for downstream consumption (turnkey Slack/webhook escalation lands in v2.7 via a native `alert` tool).

Shipped in **v2.6**. Requires v2.4's multi-session substrate + v2.5's session-resume (both on by default in the recipe).

Full recipe: `examples/gke-troubleshoot-agent/` in the repo. Design doc: `docs/k8s-event-agent-design.md`.

---

## When to use it

- **You have a GKE (or any conformant Kubernetes) cluster** and want structured, auditable first-responder coverage for common failure modes without paging a human on every event.
- **CrashLoopBackOff, ImagePullBackOff, OOMKilled, FailedMount, FailedScheduling, and probe failures cover 80% of your incidents** and you'd like the agent to handle them autonomously (with plan-first artifacts for audit) rather than posting a Slack digest for a human to act on.
- **You already run one long-lived `core-agent` daemon** (per the `examples/gke-deploy/` recipe) and want to layer an event-driven trigger on top.

If none apply — you don't have K8s to triage, or you'd rather see events in your existing observability stack and page humans — skip this. The recipe adds a small sidecar container and a ClusterRole; not zero-cost.

---

## Architecture

Two Deployments in the cluster:

- **`core-agent` daemon**: multi-session enabled, plan-first on, session-resume on. Exposes `/sessions` endpoints on port 7777. This is a regular `core-agent` — nothing k8s-specific in the daemon.
- **`k8s-event-watcher` sidecar**: separate Deployment. Uses client-go informer to watch `core/v1.Events`, filters by `reason`, dedupes on `(uid, reason)` in a rolling window, POSTs matched events to the daemon's session inject endpoint.

Both talk multi-session bearer tokens; the sidecar authenticates as `sa:k8s-event-watcher` (a proxy identity) and asserts `X-Asserted-Caller: sre-oncall@example.com` on POST /sessions so incidents show up in the on-call team's session list.

## The trigger flow

```
1. Pod enters CrashLoopBackOff on the cluster.
2. Kubelet emits a `Warning CrashLoopBackOff` Event.
3. Sidecar's informer fires; filter accepts (CrashLoopBackOff is
   in the default allow-list); dedup cache miss for (uid, reason).
4. Sidecar POSTs /sessions with X-Asserted-Caller → daemon creates
   an owned session and returns its SessionID.
5. Sidecar POSTs /sessions/<sid>/inject with a structured JSON
   payload: {"kind":"k8s-event","reason":"CrashLoopBackOff",...}.
6. Session's wake loop drives a turn. Agent invokes k8s-triage skill.
7. Skill's router loads references/CrashLoopBackOff.md.
8. Agent diagnoses (fetches logs, checks exit code), proposes a fix
   via record_plan, applies it via the GKE MCP, waits, verifies.
9. Agent closes the incident with a structured summary in the eventlog.
```

Every incident gets its own session, its own audit trail, its own permission grants. Two concurrent incidents in different namespaces don't cross-contaminate.

## Triage router skill

Triage guidance ships as **one router skill** with per-reason reference files loaded on demand via ADK's native `load_skill_resource` tool. The router owns:

- Envelope framing (parse the inject payload, identify the incident triple)
- Reference lookup (`load_skill_resource` with `resource_path: references/{reason}.md`)
- Fix-and-verify loop enforcement
- Escalation on budget exhaustion
- Structured close-summary format

The reference files own:

- Reason-specific diagnose steps
- Common-fixes table (Symptom → Fix → Verify)
- When-to-escalate guidance

Shipped reference set covers the top 10 real-world failure modes:

| Reason | Playbook covers |
|---|---|
| `CrashLoopBackOff` | Exit-code routing; log fetch; init-container timeouts; ConfigMap rollback; deployment undo |
| `ImagePullBackOff` / `ErrImagePull` | Registry auth; wrong tag; pull-secret misconfig; Docker Hub rate limits; GKE WI / Artifact Registry |
| `OOMKilled` | Memory-limit tuning; JVM/Node.js heap sizing; leak vs spike detection |
| `FailedMount` | PVC binding; StorageClass; RBAC on Secret/ConfigMap; zone mismatches; CSI driver |
| `FailedScheduling` | Insufficient resources; taints/tolerations; nodeSelector; hostPort conflicts; ResourceQuota |
| `BackOff` | Generic backoff router (chains to CrashLoopBackOff / ImagePullBackOff) |
| `Unhealthy` | Probe misconfig; startup timing; downstream dependency issues; chain to CrashLoopBackOff for real app failures |
| `NetworkNotReady` | CNI DaemonSet health; pod IP exhaustion; GKE Dataplane V2 upgrades |
| `NodeNotReady` | Single vs multi-node scope; GKE auto-repair; kubelet OOM |
| `Evicted` | QoS class; node pressure; noisy neighbors; chronic evictions |
| `_fallback` | Generic playbook for unknown reasons — meta-fixes + conservative escalation |

Custom coverage: drop a new `references/<Reason>.md` into your overlay. Update the ConfigMap generator and the daemon's projected-volume `items:` list. No SKILL.md changes; the router auto-falls-through.

## Configuration

The recipe's `.agents/config.json` turns on the four features this use case needs:

```json
{
  "permissions": {
    "mode": "yolo",
    "require_plan_artifact": true
  },
  "attach": {
    "listen": "0.0.0.0:7777",
    "multi_session": {
      "enabled": true,
      "session_idle_timeout": "6h",
      "proxy_identities": ["sa:k8s-event-watcher"]
    }
  }
}
```

- **`mode: yolo` + `require_plan_artifact: true`** — agent writes plans for every mutating action (audit trail) but doesn't wait for a human to approve. Right for autonomous first-responder posture.
- **`multi_session.enabled: true`** — each incident gets its own session.
- **`session_idle_timeout: "6h"`** — resolved incidents evict from memory after 6h idle; sessions still resumable from disk if operators want to review.
- **`proxy_identities`** — allows the sidecar to assert the on-call team's identity as session owner.

## Multi-cluster fleet

The recipe defaults to single-cluster (daemon + sidecar in the same cluster). To watch multiple clusters from one central daemon:

1. Deploy the full recipe in your "control-plane" cluster.
2. In each additional cluster, deploy only the sidecar + its ClusterRoleBinding (skip the daemon Deployment, Service, PVC, config ConfigMap).
3. Override the sidecar's `--daemon-url` to point at the central daemon's external endpoint (internal LB, IAP, VPN).
4. Give each sidecar a unique `--cluster-name`; every inject payload carries it.

Every cluster's incidents surface in the same central daemon's session list, distinguishable by the `cluster` field.

## Escalation in v2.6 (eventlog-based) — turnkey escalation ships v2.7

The v2.6 recipe closes every incident with a structured `INCIDENT SUMMARY: RESOLVED|UNRESOLVED|ESCALATED` block written to the eventlog. Operators consume via:

- **Cloud Logging sink** filtering for `INCIDENT SUMMARY: UNRESOLVED` → Pub/Sub → Cloud Function → Slack. The GCP-native "route logs to notifications" pattern.
- **`stern` or `kubectl logs -f`** during active development.
- **Direct SQL** against the eventlog SQLite file for post-hoc analysis.

Why not a turnkey Slack MCP or webhook call in v2.6? Two independent gaps:

- The shipped image is **distroless** — no `bash`, no `curl`. The naïve "agent shells out to POST a webhook" pattern doesn't work.
- Slack's official MCP requires **Streamable HTTP + OAuth 2.0**, which `pkg/mcp` doesn't support yet.

Both gaps are designed and tracked for v2.7:

- **Native `alert` tool** — config-driven webhook targets (Slack Incoming Webhook, Discord, PagerDuty Events v2, generic JSON), SSRF-safe by construction (operator pre-registers named targets), per-target rate limiting, audit through eventlog. Design: `docs/alert-tool-design.md`. Tracked: [#192](https://github.com/go-steer/core-agent/issues/192).
- **MCP Streamable HTTP + OAuth 2.0** — enables consumption of Slack's official MCP (and other RFC 8414-compliant MCP servers as they ship). Design: `docs/mcp-oauth-design.md`. Tracked: [#190](https://github.com/go-steer/core-agent/issues/190).

Once shipped, the recipe will grow an `alerts.targets[]` config section and the router will call `alert()` directly. Filed as a v2.7 recipe update.

## What's not in v2.6 (other than escalation)

Designed but explicitly deferred:

- **`wait_and_verify(predicate, timeout, interval)` built-in tool.** For v2.6 fix-and-verify is a prompt pattern in the router. Revisit if operators report prompt-flakiness.
- **Non-k8s signal sources** (Cloud Monitoring alerts, PagerDuty pages, generic webhooks). Same "sidecar POSTs to /inject" shape; will ship separately as parallel sidecars in v2.7+.
- **Automatic PR generation for GitOps-flavored fixes** (Argo, Flux). The agent applies fixes directly today; a GitOps mode is v2.7+ material.
- **Multi-cluster fleet coordinator** with unified session queries across N daemons. This is AX-integration territory.

## Recipe

See `examples/gke-troubleshoot-agent/` in the repo for the full recipe (RBAC, Deployments, config, triage skill + references) with a `deploy/overlays/example/` you copy + customize.

## Design detail

`docs/k8s-event-agent-design.md` in the repo covers the full design — sidecar CLI, event filter allow-list, dedup semantics, per-incident session lifecycle, router / reference conventions, integration with plan-first, and the 8 open questions with their resolutions.
