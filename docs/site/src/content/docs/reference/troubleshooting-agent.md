---
title: Kubernetes troubleshooting agent
---

Propose-only Kubernetes triage running as `core-agent` inside your cluster. An event-watcher sidecar ([k8s-lookout](https://github.com/go-steer/k8s-lookout)'s `lookout watch`, deployed under the `k8s-event-watcher` name) streams filtered Events into per-incident sessions on the daemon; a router skill (`k8s-triage`) loads reason-specific references and drives a **diagnose → verify → propose → escalate** loop. Every incident closes with a structured summary in the eventlog and, when it did not clear on its own, a page to the configured `oncall` alert target.

The agent does not mutate the cluster, and that is a property of the configuration rather than of the prompt: the only MCP server wired in is GKE's **read-only** endpoint, `bash`/`write_file`/`edit_file`/`delete_file`/`fetch_url` are in `tools.disable`, and the daemon's KSA holds `roles/container.viewer`. There is no mutating verb in the catalog for a persona to reach for. Remediation is written into the incident summary as a proposal; a human applies it.

Shipped in **v2.6**, re-scoped to propose-only in **v2.9**. Requires v2.4's multi-session substrate + v2.5's session-resume (both on by default in the recipe).

Full recipe: `examples/gke-troubleshoot-agent/` in the repo. Design doc: `docs/k8s-event-agent-design.md`.

> **Note:** the watcher ships from [go-steer/k8s-lookout](https://github.com/go-steer/k8s-lookout) as `ghcr.io/go-steer/lookout` (the recipe pins `v0.11.0` — the floor for daemons ≥ 2.8.0-dev.1, whose CSRF guard (#383) requires `Content-Type: application/json` on the watcher's bodyless `POST /sessions`). The image's entrypoint is `lookout watch`, a drop-in swap for the retired `ghcr.io/go-steer/k8s-event-watcher` image — same flags, RBAC, and Deployment shape. k8s-lookout also offers a `-gke` image flavor and a richer `--sources=` capability set beyond what this recipe uses. Every PR touching this recipe (or the daemon's Go source) runs a kind-based CI e2e that builds the daemon from the PR's checkout, deploys it against the pinned watcher image, and asserts the full pipeline: a broken pod's `BackOff` event → lookout's per-incident inject → a completed daemon turn.

---

## When to use it

- **You have a GKE (or any conformant Kubernetes) cluster** and want structured, auditable first-responder coverage for common failure modes without paging a human on every event.
- **CrashLoopBackOff, ImagePullBackOff, OOMKilled, FailedMount, FailedScheduling, and probe failures cover 80% of your incidents** and you'd like the investigation done — evidence gathered, transients filtered out, a specific change proposed — before a human is paged, rather than paging on the raw event.
- **You want an agent in the incident path but not in the mutation path.** This recipe's whole shape is "an agent may read anything and change nothing"; if you want autonomous remediation, this is the wrong starting point.
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
6. Session's wake loop drives a turn. Agent calls record_plan — it
   must: require_plan_artifact gates every gke MCP call, read-only
   ones included. Then it invokes the k8s-triage skill.
7. Skill's router loads references/CrashLoopBackOff.md.
8. Agent diagnoses read-only (gke_get_k8s_logs, gke_describe_k8s_resource,
   gke_list_k8s_events), then runs the reference's convergence check:
   one wait_and_verify call polling a gke_* read tool.
9. verified=true → RESOLVED (it cleared on its own; nothing was applied).
   verified=false → UNRESOLVED + a proposed change from the reference's
   remediation table + alert(target: "oncall").
10. Agent closes the incident with a structured summary in the eventlog.
```

Every incident gets its own session, its own audit trail, its own permission grants. Two concurrent incidents in different namespaces don't cross-contaminate.

## Triage router skill

Triage guidance ships as **one router skill** with per-reason reference files loaded on demand via ADK's native `load_skill_resource` tool. The router owns:

- Envelope framing (parse the inject payload, identify the incident triple)
- Plan-first ordering (`record_plan` before the first MCP call)
- Reference lookup (`load_skill_resource` with `resource_path: references/{reason}.md`)
- Convergence-check enforcement — `verified: true` is the only accepted basis for `RESOLVED`
- Escalation on budget exhaustion, on an unmatched diagnosis, and on every non-self-healing incident
- Structured close-summary format (Evidence / Proposal / Escalation lines)

The reference files own:

- Reason-specific **read-only** diagnose steps, each naming the `gke_*` tool that answers it
- A concrete `wait_and_verify(...)` convergence check for that failure mode
- A remediation-proposal table (Evidence → Proposed change → Verify)
- When-to-escalate guidance

Shipped reference set covers the top 10 real-world failure modes:

| Reason | Playbook covers |
|---|---|
| `CrashLoopBackOff` | Exit-code routing; log fetch; init-container timeouts; proposals for ConfigMap rollback / deployment undo |
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

The recipe's config is where the propose-only claim is actually made true:

```json
{
  "permissions": {
    "mode": "yolo",
    "require_plan_artifact": true
  },
  "tools": {
    "disable": ["bash", "write_file", "edit_file", "delete_file", "fetch_url"],
    "wait_and_verify": {
      "poll_allow": [
        "gke_get_k8s_resource", "gke_describe_k8s_resource",
        "gke_list_k8s_events", "gke_get_k8s_rollout_status",
        "gke_get_k8s_logs"
      ],
      "max_timeout_seconds": 300,
      "max_attempts": 40
    }
  },
  "alerts": {
    "rate_limit_per_target": "10/min",
    "targets": [
      { "name": "oncall", "url_env": "ONCALL_WEBHOOK_URL", "template": "generic",
        "description": "Page the on-call SRE…" }
    ]
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

- **`mode: yolo` + `require_plan_artifact: true`** — a no-TTY daemon can't answer an approval prompt, so `yolo` is the only workable mode; `require_plan_artifact` is what puts a gate back. Plan-first covers MCP, so *every* `gke` call — including read-only ones — is denied until `record_plan` has run, and no cluster introspection happens before a plan is on disk. The flag is per-session and sticky, so it binds the first incident of a session; a plan per subsequent incident is convention (`AGENTS.md` + the skill's Step 0), not enforcement. Artifacts land in the ephemeral `plans` emptyDir at `/etc/core-agent/.agents/plans/plan-<seq>.md`.
- **`tools.disable`** — removes the local ways to act on the world: the shell (absent from the distroless image anyway, but a disabled tool errors legibly instead of confusing the model), the three file-mutation tools, and `fetch_url` (arbitrary egress, including POSTs — `alert` is the sanctioned, target-allow-listed path).
- **`tools.wait_and_verify.poll_allow`** — MCP tools never self-classify as read-only (ADK's adapter drops `readOnlyHint`), so `wait_and_verify` would refuse them all. This is the operator asserting these five `gke` reads only observe. Without it the convergence check — the only thing that can justify `RESOLVED` — is refused at every call. Names are the ones the model sees: `<server>_<tool>`, one underscore.
- **`alerts.targets`** — the `alert` tool registers only when a target exists. `generic` is the only implemented template (a JSON POST); `slack` / `discord` / `pagerduty_events_v2` are rejected at config load. The URL comes from a Secret-backed env var and resolves at *call* time, so an unset webhook surfaces as a tool error on the first escalation, not at boot.
- **`multi_session.enabled: true`** — each incident gets its own session.
- **`session_idle_timeout: "6h"`** — resolved incidents evict from memory after 6h idle; sessions still resumable from disk if operators want to review.
- **`proxy_identities`** — allows the sidecar to assert the on-call team's identity as session owner.

The MCP side is one server, `gke`, pointed at `https://container.googleapis.com/mcp/read-only` with the `cloud-platform.read-only` OAuth scope, and `scripts/setup-wif.sh` binds `roles/container.viewer` rather than `roles/container.admin`. Re-pointing `mcp.json` at the full-access `/mcp` endpoint requires upgrading that IAM binding too — and puts you back to trusting the persona.

## Multi-cluster fleet

The recipe defaults to single-cluster (daemon + sidecar in the same cluster). To watch multiple clusters from one central daemon:

1. Deploy the full recipe in your "control-plane" cluster.
2. In each additional cluster, deploy only the sidecar + its ClusterRoleBinding (skip the daemon Deployment, Service, PVC, config ConfigMap).
3. Override the sidecar's `--daemon-url` to point at the central daemon's external endpoint (internal LB, IAP, VPN).
4. Give each sidecar a unique `--cluster-name`; every inject payload carries it.

Every cluster's incidents surface in the same central daemon's session list, distinguishable by the `cluster` field.

## Escalation

Because the agent can't apply fixes, escalation is the normal ending for any incident that doesn't self-heal — not a fallback. It runs on two channels.

**Push — the `alert` tool.** The router calls `alert(target: "oncall", level: "critical", summary: …, details: {…})` for every `UNRESOLVED` or `ESCALATED` incident. The `generic` template POSTs JSON; point `ONCALL_WEBHOOK_URL` at whatever ingests it (a Slack or Discord incoming webhook, an internal receiver, a Cloud Function that fans out to PagerDuty). The agent fires targets **by name** — there is no URL parameter — so a hallucinated destination is rejected rather than dialed, and `rate_limit_per_target` keeps an event storm from becoming a page storm.

**Pull — the eventlog.** Every incident also closes with a structured block:

```
INCIDENT SUMMARY
================
Status: RESOLVED | UNRESOLVED | ESCALATED
Root cause: <one line>
Evidence: <the tool calls that support it>
Proposal: <the change a human should apply, or "none">
Escalation: <alert sent to oncall | not needed>
```

`RESOLVED` is reserved for the case where `wait_and_verify` observed the failure clear on its own. The agent took no action, so a resolution it didn't observe would be one it made up. Consume the eventlog via a Cloud Logging sink filtering for `INCIDENT SUMMARY`, `stern` during development, or direct SQL against the SQLite file on the PVC.

## Not in scope

Designed but explicitly deferred:

- **Autonomous remediation.** Out of scope by design, not by omission — see the propose-only note at the top. A read-write variant means re-pointing `mcp.json` at `/mcp`, restoring `roles/container.admin`, and accepting that the safety story is back to being a prompt.
- **Provider-shaped alert templates** (`slack`, `discord`, `pagerduty_events_v2`). Designed in `docs/alert-tool-design.md`; rejected at config load until they ship. Use `generic` plus a receiver that fans out.
- **Non-k8s signal sources** (Cloud Monitoring alerts, PagerDuty pages, generic webhooks). Same "sidecar POSTs to /inject" shape; parallel sidecars.
- **Automatic PR generation for GitOps-flavored fixes** (Argo, Flux). The natural next step for a propose-only agent: the proposal becomes a pull request instead of a summary line.
- **Multi-cluster fleet coordinator** with unified session queries across N daemons. This is AX-integration territory.

## Recipe

See `examples/gke-troubleshoot-agent/` in the repo for the full recipe (RBAC, Deployments, config, triage skill + references) with a `deploy/overlays/example/` you copy + customize.

## Design detail

`docs/k8s-event-agent-design.md` in the repo covers the full design — sidecar CLI, event filter allow-list, dedup semantics, per-incident session lifecycle, router / reference conventions, integration with plan-first, and the 8 open questions with their resolutions.
