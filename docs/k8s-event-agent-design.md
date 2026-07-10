# K8s-event-driven troubleshooting agent

Design doc for the v2.6 follow-up to session-resume (#178 / `docs/session-resume-design.md`): turn `core-agent` into a semi-autonomous k8s troubleshooting agent by wiring cluster events as a push-signal source, routing them into per-incident sessions, and applying structured playbooks with a fix-and-verify loop.

**Status:** proposed (2026-07-02); all 8 open questions resolved in review, ready for implementation. v2.6 candidate. Tracking issue: [#186](https://github.com/go-steer/core-agent/issues/186).

## Motivation

The multi-session substrate (v2.4, [#162](https://github.com/go-steer/core-agent/issues/162)) and session resume (v2.5, [#178](https://github.com/go-steer/core-agent/issues/178)) got `core-agent` to "long-lived operator-attached agent on GKE with per-user isolation and restart survival." That covers the **interactive** use case cleanly — an SRE opens their TUI, drives cluster operations, tears down.

The **semi-autonomous** use case — an agent that watches for problems, investigates, and either fixes them or hands off with a written diagnosis — is stuck at "runs on a cron, produces a Slack digest." The blockers:

- **No push signal.** Nothing calls `POST /inject` when a pod enters `CrashLoopBackOff`. Operators either script the trigger themselves or accept the polling delay.
- **No structured playbooks.** Agent troubleshoots from the LLM's general knowledge + whatever's in `.agents/`. No convention for "if symptom X, run these checks in this order." Investigations reinvent the wheel per incident.
- **No fix-and-verify.** Agent can apply a fix (with permission from plan-first / `/allow`), but there's nothing that says "check that the fix stuck within 3 min, else revert and escalate." Confident fixes go out; uncertain ones require human oversight.
- **No structured escalation.** When the agent runs out of ideas, it logs to stderr. No page-oncall, no Slack DM to the affected team, no ticket creation.

The consequence: an operator has to either watch the agent in real time (defeats "semi-autonomous") or accept that a real incident sits for N minutes until the next scheduled sweep (defeats "responsive"). Neither is production-grade.

This design closes those gaps by adding one new binary (a small watcher sidecar) and one new convention (playbook instructions), integrating cleanly with the multi-session + resume substrate that's already in place.

## Goals

- **Reactive to k8s events, not polling.** A `CrashLoopBackOff` event triggers an investigation within seconds, not minutes.
- **Per-incident session isolation.** Each incident gets its own session — separate conversation history, separate audit trail, separate permission grants. Two concurrent incidents don't cross-contaminate.
- **Playbook-driven investigation.** Well-known failure modes (the top ~10 k8s error reasons) have a written playbook the agent consults. Custom failure modes fall back to LLM-general knowledge as today.
- **Fix-and-verify loop.** For fixes the agent applies, the agent MUST confirm the symptom cleared within a configured budget. On failure: revert (when possible) + escalate.
- **Escalation path.** When the agent can't resolve within a budget (turns, time, cost), it produces a structured handoff — Slack summary, incident-ticket payload, or configured webhook — with the diagnosis, what was tried, and what didn't work.
- **No k8s-specific bloat in `core-agent`.** The daemon stays k8s-agnostic. All k8s awareness lives in (a) the sidecar, (b) the GKE MCP server, and (c) instruction-scope playbooks. Non-k8s deployments see zero surface change.

## Non-goals (v2.6)

- **Full k8s SRE replacement.** The agent handles routine issues; complex ones still escalate to a human. This isn't "AIOps SLO 100%."
- **Multi-cluster monitoring from one daemon.** One daemon watches one cluster. Multi-cluster is a fleet-level deployment concern (one daemon-per-cluster + a coordinator), which is [AX](https://github.com/go-steer/ax) territory — see `reference_ax_runtime.md`.
- **Cross-provider parity.** k8s events are the first push-signal source. Cloud Monitoring, PagerDuty, and generic webhook adapters compose on the same "sidecar POSTs to `/inject`" shape but ship separately.
- **Persistent incident tracking / triage database.** Sessions are the incident record; if operators want a durable ticket, escalation writes one via an external MCP tool. No new "incidents" table in the eventlog.
- **Automatic PR generation for fixes.** Fix means "apply to the running cluster" — GitOps-flavored fixes (push to Argo, wait for reconcile) are out of scope; can be added later without design change (agent uses the appropriate MCP tool).

## Conceptual model

### Signal source: `k8s-event-watcher` sidecar

A small Go binary (`~200 LoC + client-go`) that runs alongside the `core-agent` daemon. Uses a client-go informer to watch `core/v1.Event`, filters, dedupes, and calls `POST /sessions` + `POST /sessions/<sid>/inject` on the daemon.

Why sidecar (not in-daemon goroutine):
- **Layering.** `core-agent` stays k8s-agnostic. Adding client-go to `core-agent` bloats every deployment (~5 MB) and couples release cadence (a k8s API bump would force a core-agent release).
- **Reuse of existing pieces.** The sidecar is a thin adapter on top of a well-understood k8s primitive (Event informer). Community-maintained alternatives (`kube-event-exporter`, `kubewatch`) exist and could be adopted as the sidecar with a small config wrapper.
- **Independent scaling.** One `core-agent` daemon can serve multiple `k8s-event-watcher` sidecars (multiple clusters via different service-account bindings). The reverse doesn't work — the daemon is the compute.

Why not MCP event stream:
- MCP subscriptions are still rough in the SDK; we'd have to invent semantics for backpressure, replay-on-reconnect, per-subscriber filtering.
- Doesn't match the "external system triggers a session" shape — MCP is agent → server; we want server → agent.
- Might revisit in v2.7+ when subscriptions harden.

### Event filter allow-list

The sidecar watches ALL Events but injects only for a configured allow-list of `Event.Reason` values. Default set covers the top-frequency real failures:

| Reason | What it means | Playbook priority |
|---|---|---|
| `CrashLoopBackOff` | Container restarted N times, kubelet backing off | high |
| `ImagePullBackOff` / `ErrImagePull` | Registry auth or image doesn't exist | high |
| `OOMKilled` (from container status, surfaced by watcher) | Memory limit hit | high |
| `FailedMount` | PV/PVC binding, permissions, node topology | high |
| `FailedScheduling` | No node fits, taints/tolerations, resource pressure | high |
| `BackOff` (generic) | catch-all for restart-backoff scenarios | medium |
| `Unhealthy` | probe failure — narrow to N-consecutive to avoid transient flap | medium |
| `NetworkNotReady` / `NodeNotReady` | node-level issue | medium |
| `Evicted` | node pressure evicting pods | medium |
| `FailedCreate` (Deployment/ReplicaSet) | admission webhook, RBAC | low |

The allow-list is operator-configurable. Custom clusters with custom controllers can add their own reasons (`ExternalPolicyDenied`, `CertRotationFailed`, etc.).

### Dedup + windowing

k8s events are chatty. `CrashLoopBackOff` fires every restart cycle (30s-5min). Without dedup, a single crashlooping pod would spam the daemon with hundreds of `/inject` calls per hour.

Dedup key: `(involvedObject.uid, reason)`. First occurrence in a 5-min rolling window fires; subsequent occurrences within the window are counted + suppressed. When the window rolls, the counts flush into a follow-up inject (`{"kind": "k8s-event-followup", "count_since_first": N, ...}`) so the agent knows the issue persists.

Windowing state lives in the sidecar's memory (bounded LRU, ~10k keys). Sidecar restart resets state — the first N events after restart may re-fire once each, which is acceptable (better than dropping a persistent issue).

### Session routing: per-incident, not shared monitor

Two options considered:

1. **One long-lived "sre-monitor" session** — every inject goes to the same session. Simple, one audit trail.
2. **Per-incident sessions** — sidecar creates a fresh session per `(involvedObject.uid, reason)` and routes subsequent injects for the same incident to it.

**Decision: per-incident.** Reasons:
- Multi-session substrate already gives us the primitive; POST /sessions is cheap.
- Isolation: a runaway loop in one incident's investigation doesn't corrupt another's context.
- Audit: "show me everything the agent did about the checkout-svc CrashLoopBackOff on 2026-07-02" is one session, one eventlog subset.
- Permission scoping: an incident's `/allow` grants stay confined to that incident. No accidental privilege leak across incidents.
- Session-resume (v2.5) means idle incidents evict from memory but stay resumable — the sidecar can reference a prior session when the same incident recurs (see "Deduplication across restarts" below).

The "one long-lived monitor" pattern is still available for operators who prefer it — the sidecar has a `--target-session` mode where every inject goes to a pre-existing session. Default is per-incident.

### Session ownership: proxy pattern

The sidecar authenticates to `core-agent` as its own bearer identity (`sa:k8s-event-watcher` in `users.json`). But the sessions it creates aren't *owned* by the sidecar — they're owned by whoever the operator wants to hold the audit trail. Ownership resolves via the existing proxy-identity pattern (v2.4):

- Sidecar identity is listed in `attach.multi_session.proxy_identities`.
- Each POST /sessions carries `X-Asserted-Caller: <owner-identity>` — configurable per-namespace or globally.
- Result: sessions show up in `sre-oncall@example.com`'s (or whoever's) session list. That team can attach a TUI and take over any incident.

Composes cleanly with the existing chat-bot pattern; no new auth primitive needed.

### Inject payload shape

The sidecar POSTs a JSON message with a structured envelope so playbook instructions can pattern-match on `kind`:

```json
{
  "kind": "k8s-event",
  "reason": "CrashLoopBackOff",
  "namespace": "checkout",
  "kind_of_object": "Pod",
  "name": "checkout-svc-7b9d-x4kzq",
  "container": "checkout",
  "uid": "abc123-...",
  "message": "Back-off restarting failed container",
  "count": 5,
  "first_seen": "2026-07-02T14:22:03Z",
  "last_seen": "2026-07-02T14:24:11Z",
  "cluster": "prod-us-central1",
  "context": {
    "controller_ref": "Deployment/checkout-svc",
    "node": "gke-prod-pool-1-abc",
    "labels": {"team": "checkout", "env": "prod"}
  }
}
```

The `kind` field is what the agent's instructions look for; `context.labels` lets playbooks route conditionally (e.g., "if labels.env == 'prod', page immediately").

### Triage router skill + reference bundle

Triage ships as **one router skill** — the existing v2.1 Skills primitive (`pkg/skills`) — with per-reason detail files loaded on-demand via ADK's native `load_skill_resource` tool. The router owns the envelope (envelope framing, fix-and-verify loop, escalation, budget); the reference files hold the reason-specific diagnose + fix content.

Why router (not one skill per reason):

- **Native fallthrough.** Router branches on `reason` in the payload; unknown reasons hit `references/_fallback.md` directly. No "unmatched skill" edge case to design around.
- **Single `/skills` entry.** Discovery stays clean. Operators see what reasons are covered by inspecting `references/`; the recipe README lists them.
- **Deterministic dispatch.** LLM invokes ONE skill (the router) and does a text-level `switch` on the payload's `reason` field to pick the reference. More reliable than N-way description matching, where the LLM has to reason across competing skill Descriptions.
- **Cross-reason chaining is free.** Router in a single turn: read `CrashLoopBackOff.md` → notice exit code 137 → read `OOMKilled.md` — no skill re-invocation, no context switch.
- **Common concerns live once.** Envelope (structured summary), budget tracking, escalation call — the router owns them. Reference files stay focused on their diagnose + fix content.
- **Custom coverage is trivial.** Operators drop a new `references/<Reason>.md` file. No SKILL.md authoring, no frontmatter, no registration.
- **First-class ADK primitive.** ADK's `skilltoolset` exposes three tools (`list_skills`, `load_skill`, `load_skill_resource`); the router just calls the third. No new plumbing.

Skill layout:

```
.agents/skills/k8s-triage/
├── SKILL.md                          ← router (~50 lines)
└── references/
    ├── CrashLoopBackOff.md
    ├── ImagePullBackOff.md
    ├── ErrImagePull.md
    ├── OOMKilled.md
    ├── FailedMount.md
    ├── FailedScheduling.md
    ├── BackOff.md
    ├── Unhealthy.md
    ├── NetworkNotReady.md
    ├── NodeNotReady.md
    ├── Evicted.md
    └── _fallback.md                  ← generic playbook for unknown reasons
```

ADK's `LoadResource` implementation restricts skill-relative paths to `references/`, `assets/`, or `scripts/` subdirectories (path-traversal guard); the `references/` convention is exactly what our router uses.

Router SKILL.md shape:

```markdown
---
name: k8s-triage
description: |
  Handle a Kubernetes event inject (kind="k8s-event"). Reads the
  reason-specific reference and drives the diagnose → fix → verify
  loop for any k8s failure mode. Falls back to a generic playbook
  for unknown reasons.
---

# k8s triage router

You've been invoked with a payload like:
`{"kind": "k8s-event", "reason": "CrashLoopBackOff", "namespace": "...", ...}`.

## Step 1 — load the reference

Call `load_skill_resource`:
- skill_name: `k8s-triage`
- resource_path: `references/{reason}.md`

If it returns `ErrResourceNotFound`, retry with
`resource_path: references/_fallback.md`.

## Step 2 — follow the reference

The reference has diagnose / fixes / verify sections in that order.
Execute step-by-step. If a mutating action is needed AND plan-first
is on (`require_plan_artifact: true`), call `record_plan` before
applying.

## Step 3 — fix-and-verify

After every fix:
1. Wait the verify interval named in the reference row.
2. Re-run the diagnose step.
3. If green: proceed to Step 4.
4. If red after 2 attempts: revert (when possible) + escalate.

## Step 4 — close

Post a structured summary to the eventlog. If unresolved past the
reference's budget, call the escalation MCP (Slack `post_message`,
Jira `create_issue`, or configured webhook) with:
- Incident triple (namespace, name, uid)
- What was diagnosed
- What was tried and current state
- Session URL for a human to attach a TUI
```

Reference file shape (`references/CrashLoopBackOff.md`):

```markdown
# CrashLoopBackOff

## Budget

- Max turns: 8
- Max wall time: 10 min

## Diagnose

1. Fetch the container's last 200 log lines (`gke-mcp: logs.tail`).
2. Fetch pod events (`gke-mcp: events.for-pod`).
3. Check exit code + reason from PodStatus.
4. If exit code == 137 → chain to `references/OOMKilled.md`.
5. If image pull error → chain to `references/ImagePullBackOff.md`.
6. Otherwise: application-level failure. Categorize from logs.

## Common fixes

| Symptom | Fix | Verify (interval → check) |
|---|---|---|
| Startup timeout on init container | Extend initialDelaySeconds | 2m → pod status Running |
| Bad config in ConfigMap | Roll back to prior revision | 3m → pod restart + steady state |
| Application crash from bad deploy | Roll back Deployment | 5m → replicaset transitions |
```

Cross-reason chaining works via `load_skill_resource` — the router loads `CrashLoopBackOff.md`, sees a "chain to OOMKilled" instruction, loads `OOMKilled.md`, continues investigating. All in one turn stream.

Custom coverage: operators drop `references/<Reason>.md` into the skill directory. The overlay pattern (`pkg/skills.LoadAll`) still works — a user-global skills tree at `<userCoreHome>/skills/k8s-triage/references/` overlays the project-scope references file-by-file. No index file, no coordination.

### Fix-and-verify primitive: prompt-pattern first

For v2.6, fix-and-verify is a **prompt pattern documented in playbooks**, not a new tool. Playbooks instruct the agent to:

1. Apply a fix (subject to plan-first + `/allow`).
2. Wait for a configured interval (`sleep 90` via bash, or a new `wait` MCP tool).
3. Re-run the verify predicate (a bash command, kubectl query, or MCP tool call).
4. On verify pass: post summary, close incident.
5. On verify fail: revert if possible, escalate.

Why prompt-pattern not tool:
- Composes with existing tools (bash, MCP, spawn_agent) — no new API surface.
- Playbook is where the k8s domain knowledge lives; keeping the verify logic there keeps the substrate agnostic.
- If the pattern proves flaky in practice (agent skips the verify step, mis-parses results), we introduce a `wait_and_verify(predicate, timeout, interval)` tool in v2.7+ — but the migration cost is low because the shape is already in playbooks.

Explicit non-goal for v2.6: a `verify_state` built-in tool. Revisit when we have data on prompt-pattern reliability.

### Escalation: structured handoff

When the agent exhausts its budget (`budget.turns` or `budget.wall_time` from playbook frontmatter) without resolving, it must produce a structured handoff. Three shipping mechanisms:

1. **Slack** — call the Slack MCP server's `postMessage` with a formatted summary. Operator's Slack workspace gets a channel post with: incident summary, what was tried, current state, session URL to attach a TUI.
2. **Ticket** — call an issue-tracker MCP (Jira, GitHub Issues, Linear). Playbook decides which MCP to invoke.
3. **Generic webhook** — POST to a configured URL with a structured payload. For PagerDuty, Ops Genie, or bespoke on-call systems.

All three route through MCP tools + playbook instructions — no new escalation primitive in the daemon.

Budget-exhaustion is the agent's own responsibility. The sidecar doesn't enforce it; the playbook does. If the agent doesn't respect its budget, that's a playbook / instruction-tuning problem the operator handles.

## Detailed design

### Sidecar CLI

```
Usage: k8s-event-watcher [flags]

Watches Kubernetes events and posts filtered occurrences to a
core-agent daemon's session inject endpoint. See
docs/k8s-event-agent-design.md.

Required:
  --daemon-url URL       Base URL of the core-agent daemon (http://... or https://...)
  --token-env NAME       Environment variable name holding the bearer token

Session routing (default: per-incident):
  --mode {per-incident,shared}   Default: per-incident.
                                 per-incident: create a session per (uid, reason) via POST /sessions.
                                 shared: post every inject to --target-session.
  --target-session ID    Required when --mode=shared.
  --owner IDENTITY       X-Asserted-Caller value for POST /sessions (per-incident mode).
                         The sidecar must be in the daemon's proxy_identities list.

Event filtering:
  --reason REASON,...    Allow-list of Event.Reason values to fire on.
                         Default: CrashLoopBackOff,ImagePullBackOff,ErrImagePull,
                         FailedMount,FailedScheduling,BackOff,OOMKilled,Unhealthy,
                         NetworkNotReady,NodeNotReady,Evicted
  --namespace NS,...     Restrict to these namespaces. Empty = all namespaces.
  --exclude-namespace NS,...  Exclude these namespaces.

Dedup:
  --dedup-window DUR     Rolling window for (uid, reason) dedup. Default: 5m.
  --dedup-persist PATH   Persist dedup cache to disk (survives restart).
                         Optional; default is in-memory only (accept small
                         duplicate burst on restart). Set to a mounted PVC
                         path (or emptyDir) in the deploy manifest to opt in.
  --unhealthy-min-count N  Require this many consecutive Unhealthy events before firing.
                         Default: 3 (avoids flapping-probe noise).

Kubernetes client:
  --in-cluster           Use in-cluster service account. Default when running
                         inside a pod with a mounted SA token.
  --kubeconfig PATH      Explicit kubeconfig file. Used outside a pod.
  --cluster-name NAME    Human-readable cluster name included in every inject payload.

Operational:
  --log-level {debug,info,warn,error}   Default: info.
  --dry-run              Print inject payloads to stdout without calling the daemon.
  --metrics-addr HOST:PORT  Prometheus metrics endpoint (events_seen, events_injected,
                         events_dedup_suppressed, inject_errors_total).
```

### Sidecar deployment

The architecture supports **all four topologies**:

1. **1 daemon + 1 sidecar in the same pod** — single-cluster starter. Shares pod SA, localhost network path.
2. **1 daemon + N remote sidecars** — central-daemon pattern; each sidecar in its own cluster, all posting to one daemon. Single fleet-wide audit trail.
3. **N daemons + N co-located sidecars** — pod-per-cluster; isolated audit trails; blast radius contained per cluster.
4. **Mixed** — tiered SLA fleets where prod clusters get dedicated daemons and dev clusters share one.

None of these need new architecture; the sidecar just points at `--daemon-url` and the daemon accepts whatever comes in through the multi-session auth surface.

**Recipe default: central-daemon (shape #2).** Ships as `examples/gke-troubleshoot-agent/` with:

- One `core-agent` Deployment (the "control plane" daemon).
- Sidecar Deployment manifests operators duplicate per cluster, each with a distinct `--cluster-name` flag.
- RBAC: `ClusterRole` for the sidecar granting `list`/`watch` on `core/v1.Events` + `get` on Pods (for status enrichment).
- Default triage skills under `.agents/skills/k8s-triage-*/SKILL.md` (~10 skills covering the top reasons).
- `.agents/config.json` additions: `proxy_identities: ["sa:k8s-event-watcher"]`.
- README walking through: single-cluster starter (deploy both in one cluster) → multi-cluster fleet (deploy the daemon centrally, deploy sidecars in each watched cluster with cross-cluster network policy).

Why central-daemon default (vs. sidecar-in-daemon-pod as the doc originally recommended): positions operators for the fleet posture from day one. Single-cluster deployment is a special case of the fleet shape, not a different architecture. Operators who genuinely want isolated per-cluster audit trails follow the shape-#3 README section instead.

### Skills loading

The router skill lives at `.agents/skills/k8s-triage/SKILL.md`; reference files at `.agents/skills/k8s-triage/references/*.md`. The `pkg/skills.LoadAll` primitive handles project + user-global overlay merge and wires the resulting toolset via `agent.WithToolsets` — no code changes to the loader.

The three tools ADK's `skilltoolset` exposes to the LLM after loading:

| Tool | Purpose in the triage flow |
|---|---|
| `list_skills` | LLM sees `k8s-triage` in the catalog |
| `load_skill` | LLM invokes the router; router body drives Steps 1–4 |
| `load_skill_resource` | Router calls this per-reason to pull the matching reference |

Base instructions (in `.agents/AGENTS.md` or a scope overlay) frame the agent's role in a few lines:

```markdown
# Role: k8s troubleshooting agent

You receive inject payloads shaped like
`{"kind": "k8s-event", "reason": "...", ...}` from a k8s-event-watcher
sidecar. For each payload:

1. Invoke the `k8s-triage` skill.
2. The skill's router loads the reason-specific reference and
   drives diagnose → fix → verify.
3. On budget exhaustion, escalate via the configured Slack/webhook MCP.
```

Overlay layering works file-by-file — a user-global reference at `<userCoreHome>/skills/k8s-triage/references/<Reason>.md` shadows the project-scope reference with the same name. Custom coverage: drop a new `references/<Reason>.md`; no SKILL.md changes, no registration.

### Per-incident session lifecycle

1. Sidecar sees an event matching the filter.
2. Checks its dedup cache for `(uid, reason)`. If present within the rolling window: increment count, suppress inject.
3. Cache miss → sidecar calls `POST /sessions` with `X-Asserted-Caller: <owner>`, gets back `sessionID`.
4. Caches `(uid, reason) → sessionID` for the window duration so follow-up events for the same incident route to the same session (via `POST /sessions/<sid>/inject`).
5. Sidecar POSTs the initial inject payload to the new session.
6. Agent starts investigating; session's per-session wake loop drives turns.
7. When agent completes (fix applied + verified, or budget-exhausted + escalated), it emits a final "incident closed" event to the eventlog.
8. Sidecar's cache entry ages out after the window; the next matching event creates a new session.

**Incident-close is time-based (window expiry) in v2.6.** No dependency on DELETE /sessions (which doesn't ship yet) or eventlog tailing. The cost: if a persistent crashloop continues for 30 min, we get 6 sessions (one per 5-min window) instead of 1. That's acceptable — each captures a distinct investigation window, and session-resume means the agent can reference prior sessions if needed. The tradeoff of "always one session per incident" is not worth blocking on DELETE /sessions or coupling the sidecar to the eventlog.

Recurrence handling: cached entries expire after the window; the same `(uid, reason)` outside the window creates a new session. Prior investigations are still on disk (session-resume) so operators can review the history. In practice, most incidents resolve within one window; persistent ones surface as recurring sessions which is a useful "hey, this thing has been broken all afternoon" signal on its own.

### Interaction with plan-first

Playbooks that fix things MUST compose with plan-first. Recommended pattern in the playbook:

```markdown
### Fix

1. Compose a plan describing the fix (kind, target, expected effect, verify criterion).
2. Call `record_plan` with the plan.
3. Apply the fix (bash / MCP).
4. Verify.
```

For fully-autonomous mode (no human in the loop), the recipe's `.agents/config.json` sets:

```json
{
  "permissions": {
    "mode": "yolo",
    "require_plan_artifact": true
  }
}
```

This means: agent must write a plan before any mutating action, but doesn't wait for human approval — the written plan becomes the audit trail. If an operator wants human-in-the-loop, they set `mode: ask` and the TUI prompts on every mutating call.

### Metrics + observability

Sidecar exposes Prometheus metrics on `--metrics-addr`:

| Metric | Type | Labels |
|---|---|---|
| `k8s_event_watcher_events_seen_total` | counter | reason, namespace |
| `k8s_event_watcher_events_injected_total` | counter | reason, namespace |
| `k8s_event_watcher_events_deduped_total` | counter | reason, namespace |
| `k8s_event_watcher_inject_errors_total` | counter | reason, http_code |
| `k8s_event_watcher_session_creates_total` | counter | outcome |
| `k8s_event_watcher_active_incidents` | gauge | reason |

Daemon-side observability is unchanged — every inject shows up in the eventlog with the sidecar's proxy identity + the asserted owner identity, so the audit trail is "who invoked what."

## Per-substrate impact

### `core-agent` (daemon)

**Zero code changes.** The sidecar uses existing endpoints:
- `POST /sessions` with `X-Asserted-Caller` header — v2.4 proxy pattern.
- `POST /sessions/<sid>/inject` — v2.4 core API.

### `k8s-event-watcher` (new binary, `cmd/k8s-event-watcher/`)

New in-tree Go binary. Depends on `k8s.io/client-go`. ~300 LoC + client-go boilerplate. Own Dockerfile + release-image pipeline (published to `ghcr.io/go-steer/k8s-event-watcher:<tag>` alongside the existing `core-agent` images).

Open: **should this be in `core-agent` monorepo or a separate repo?** In-tree is simpler for v2.6 (one release cycle, one CI). If the sidecar grows to include other signal sources (Cloud Monitoring, PagerDuty, etc.), split into `go-steer/agent-triggers` repo. Ask before deciding.

### `docs/site/content/docs/reference/`

New page: `troubleshooting-agent.md` covering the sidecar + playbook convention + fix-and-verify recipe pattern.

### `examples/gke-troubleshoot-agent/`

New recipe layering on `examples/gke-deploy/`:
- RBAC manifests
- Sidecar container spec (patch on top of the base deployment)
- Default playbooks under `.agents/playbooks/`
- README walking through the deploy + a test-crash-loop scenario

## Config surface

New optional block under top-level config, consumed ONLY by the sidecar (daemon ignores it):

```jsonc
{
  "attach": {
    "multi_session": {
      "enabled": true,
      "proxy_identities": ["sa:k8s-event-watcher"],
      "asserted_caller_header": "X-Asserted-Caller"
      // ... existing fields ...
    }
  }
  // Nothing new at daemon config surface.
}
```

The sidecar reads its config from CLI flags (see "Sidecar CLI" above). Playbooks live under `.agents/playbooks/` and are picked up by the standard instruction loader — no new config.

## Migration story

Net-new feature. No migration.

- **Existing v2.5 deployments** — no behavior change until an operator deploys the sidecar + playbooks. Everything continues to work.
- **New deployments** — the recipe (`examples/gke-troubleshoot-agent/`) is the on-ramp. Copy, adjust owner identity + cluster name, `kubectl apply -k`.

## Implementation phases

### Phase 1 — Sidecar core (PR ε.1 of #186)

- New `cmd/k8s-event-watcher/` binary.
- CLI flag parsing per the shape above.
- client-go Event informer + filter + dedup.
- POST /sessions + POST /sessions/<sid>/inject wiring.
- Prometheus metrics.
- Unit tests: filter matching, dedup windowing, session routing (per-incident vs shared).

Estimate: ~500 LoC + ~350 LoC tests. ~3 days.

### Phase 2 — Playbooks + recipe (PR ε.2 of #186)

- `examples/gke-troubleshoot-agent/` recipe (RBAC, sidecar Deployment/pod-patch, config).
- Default playbook set under `.agents/playbooks/` — 10 files covering the top reasons.
- Recipe README walking through deploy + a test-crash-loop scenario.
- Hugo docs page.

Estimate: ~200 LoC YAML/config + ~800 lines of playbook content + ~400 lines of docs. ~4 days (most is playbook writing + testing on a real cluster).

### Phase 3 — Escalation MCP integration (PR ε.3 of #186)

- Wire the existing Slack MCP into the recipe with a canonical "post_summary" tool binding.
- Playbook additions: "if budget exhausted, call `slack.postMessage` with structured summary."
- Sample escalation payload doc.
- Manual UAT: exhaust the agent's budget, verify Slack gets the handoff.

Estimate: ~200 LoC config + ~100 lines of docs. ~2 days.

### Phase 4 — Container image + release (PR ε.4 of #186)

- Dockerfile.k8s-event-watcher on `gcr.io/distroless/static-debian12:nonroot`.
- Extend `.github/workflows/release-images.yml` to build + push a third image (`k8s-event-watcher`) on tag.
- CHANGELOG v2.6.0 entry.
- Design doc status flip.

Estimate: ~100 LoC infrastructure + ~50 LoC docs. ~1 day.

**Total**: ~1,850 lines across 4 PRs, ~10 days. Comparable to session-resume's scope but with a larger docs/config share.

## Decisions (open questions resolved)

All 8 design questions were reviewed and resolved on 2026-07-02. Summary:

### 1. Sidecar repo location

**Resolved: in-tree in `core-agent` monorepo (`cmd/k8s-event-watcher/`).** Simpler CI, one release cycle, easy shared-code path. Split into a separate `go-steer/agent-triggers` repo when the second signal source (Cloud Monitoring, PagerDuty, etc.) ships, or when the sidecar accumulates >1000 LoC of k8s-specific code.

### 2. Default owner identity

**Resolved: single configured owner** (`--owner sre-oncall@example.com`). All incidents surface in one team's session list — the "one team is the audit trail" MVP shape. Label-driven routing (`--owner-label team`) and namespace-driven routing (`--owner-per-namespace ...`) ship as v2.7+ enhancements when operators demonstrate the need.

### 3. Skills vs. instruction-scope playbooks

**Resolved: use the Skills primitive.** Triage guidance ships as Skills (`pkg/skills.LoadAll`), not as instruction-scope Markdown. See §"Triage skills" above for details — lazy loading, native "when to use" matching via Description, `/skills` discoverability, and per-skill permission scoping via `GateToolset`. No parallel "playbook" concept exists in the codebase. This resolves what was originally asked as "playbook auto-discovery vs index file" — moot, because Skills already handle discovery via `LoadAll`.

### 4. Fix-and-verify: prompt-pattern or dedicated tool

**Resolved: prompt-pattern in v2.6.** Skills' body instructs the agent through the apply → wait → re-check → revert-if-worse loop using existing tools (`bash`, MCP calls, `spawn_agent`). If operator experience shows the prompt-pattern is flaky (agent skips the verify step, mis-parses results), a `wait_and_verify(predicate, timeout, interval)` tool ships in v2.7+. Migration cost is low because the pattern is already documented in skill bodies.

### 5. Multi-cluster shape

**Resolved: all four topologies supported; recipe defaults to the central-daemon pattern.** Architecture doesn't constrain which shape operators pick — the sidecar just points at `--daemon-url`, the daemon accepts injects from anywhere. The recipe defaults to **1 central daemon + N remote sidecars** (each watching a different cluster) to position operators for the fleet posture from day one. Single-cluster deploys are documented as a special case (deploy both in one cluster with `--in-cluster`). Pod-per-cluster (N daemons) is documented for operators who want isolated audit trails; genuine fleet-view queries across N daemons remain [AX](reference_ax_runtime.md) territory.

### 6. Sidecar restart resilience

**Resolved: both accept-duplicates and PVC-persist supported; default is accept-duplicates.** The sidecar's `--dedup-persist PATH` flag opts in to disk-backed cache that survives restart. Default (unset) keeps the cache in memory only — restart re-fires each open incident once, which is acceptable for most operators. The persist mode is a one-line YAML addition (`--dedup-persist /var/lib/watcher/dedup.db` + a mounted PVC) for operators who want zero duplicate bursts. No mandatory PVC in the default recipe.

### 7. Incident-close signal

**Resolved: time-based expiry only for v2.6.** Cached `(uid, reason)` entries expire after the rolling window (default 5m). Same event outside the window creates a new session. No dependency on DELETE /sessions (not shipping yet) or eventlog tailing (adds coupling). Cost: a persistent 30-min crashloop generates 6 sessions instead of 1 — acceptable because each captures a distinct investigation window, and session-resume means operators can reference prior sessions. If v2.7+ ships DELETE /sessions, we revisit the "session close = incident close" tightening.

### 8. Integration with existing k8s exporters

**Resolved: build our own sidecar for v2.6.** Simplest — ~500 LoC of Go with client-go informer. Full control over payload shape, dedup semantics, session routing. Community tools (`kube-event-exporter`, `robusta`) have different mental models that would need adapting. An adapter shim (accept webhook POSTs from those tools + translate to `/inject`) is v2.7+ material if operators already running those tools ask for it.

## Security considerations

- **Sidecar RBAC**: minimum-necessary. `list`/`watch` on `events` cluster-wide, `get` on `pods` for status enrichment. NOT `patch`/`create`/`delete` on anything — the sidecar never modifies cluster state, the agent does.
- **Bearer token**: sidecar's token is in `users.json` alongside every other identity. Mount it via a Kubernetes Secret + env var, following the same posture as operator tokens. The `sa:k8s-event-watcher` identity is a proxy identity — it can assert other identities but must be listed in `proxy_identities`.
- **Injection payload**: the sidecar controls the payload structure but the underlying data comes from the k8s API. A hostile controller could inject arbitrary strings into event `message` fields. Playbooks should treat inject payloads as untrusted operator input — no direct interpolation into shell commands. Sidecar SHOULD apply basic sanitization (strip control characters, truncate to max length) before forwarding.
- **Cross-cluster leak prevention**: sidecar's `--cluster-name` flag is included in every inject payload. Playbooks that call MCP tools MUST scope operations to the payload's cluster — no "connect to cluster X to fix issue in cluster Y" cross-cluster confusion.
- **Escalation-loop guard**: if an escalation path itself fires an event (e.g., a Slack webhook adds a pod that then CrashLoopBackOffs), the sidecar might loop. Mitigation: playbooks exclude events from pods matching a configured namespace (`--exclude-namespace kube-system,observability`) and the recipe's default excludes core system namespaces.

## Out of scope (deferred to v2.7+)

- **Cloud Monitoring / Cloud Logging as signal sources** (parallel sidecars in the same shape).
- **PagerDuty push as a trigger** (page fires → session creates).
- **Label / namespace-driven owner routing** (v2.6 uses a single configured owner).
- **`wait_and_verify` built-in tool** (v2.6 uses prompt-pattern).
- **Automatic PR generation for GitOps-flavored fixes** (Argo, Flux, config-repo commits).
- **Multi-cluster coordinator** (AX integration territory).
- **Persistent dedup cache across sidecar restarts**.
- **Structured incident triage database** (sessions are the record).

## Dependencies and related work

- **[#162](https://github.com/go-steer/core-agent/issues/162) multi-session substrate** — landed in v2.4. Sidecar uses `POST /sessions` + `X-Asserted-Caller` from this.
- **[#178](https://github.com/go-steer/core-agent/issues/178) session resume** — landed in v2.5. Per-incident sessions survive daemon restart; sidecar can reference prior investigations.
- **[#171](https://github.com/go-steer/core-agent/pull/171) POST /sessions on-demand creation** — landed in v2.4. Sidecar's session-creation call.
- **Plan-first ([v2.3 design](docs/plan-first-design.md))** — sidecar recipe uses `require_plan_artifact: true` for autonomous mode.
- **Google GKE MCP server (`mcp.googleapis.com`)** — provides both cluster and workload-level k8s tools. Playbooks call it for diagnosis + fixes.
- **[v2.3 instruction loader](docs/instruction-loader-v2-design.md)** — `@include` + `AGENTS.d/` + per-caller overlays. Playbooks slot into the existing loader with no new primitive.
- **`examples/gke-deploy/`** — recipe this one layers on. Sidecar deploys as a container in the same pod OR as a separate Deployment; the recipe covers both variants.

## When this lands

- Phase 1 (sidecar core): ~3 days
- Phase 2 (playbooks + recipe): ~4 days
- Phase 3 (escalation MCP integration): ~2 days
- Phase 4 (container image + release): ~1 day

~2 weeks of focused work. Similar shape to session-resume (~1,850 LoC across 4 PRs). The sidecar is a small binary; the leverage comes from the playbooks + recipe + escalation composition on top of a substrate that's already in place.
