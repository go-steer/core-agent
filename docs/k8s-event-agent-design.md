# K8s-event-driven troubleshooting agent

Design doc for the v2.6 follow-up to session-resume (#178 / `docs/session-resume-design.md`): turn `core-agent` into a semi-autonomous k8s troubleshooting agent by wiring cluster events as a push-signal source, routing them into per-incident sessions, and applying structured playbooks with a fix-and-verify loop.

**Status:** proposed (2026-07-02). Awaiting approval before implementation. v2.6 candidate. Tracking issue: [#186](https://github.com/go-steer/core-agent/issues/186).

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

### Playbook convention

Playbooks are Markdown files under a scope-agnostic path — either the daemon-wide `.agents/playbooks/` or a per-caller `<users_dir>/<identity>/.agents/playbooks/`. Naming: `<reason>.md`, one file per k8s Event reason. The daemon-wide instruction loader (v2.3's `@include` + `AGENTS.d/`) picks them up automatically; no new loader primitive.

Playbook shape:

```markdown
---
reason: CrashLoopBackOff
severity: high
budget:
  turns: 8
  wall_time: 10m
---

## Playbook: CrashLoopBackOff

### Diagnose

1. Fetch the container's last N=200 log lines (`gke-mcp: logs.tail`).
2. Fetch the pod's events (`gke-mcp: events.for-pod`).
3. Check the container's exit code + reason from PodStatus.
4. If exit code == 137: OOMKilled — jump to memory-limit playbook.
5. If exit code == 1 and logs contain a stack trace: application-level failure.
6. If image pull error surfaces in events: jump to ImagePullBackOff playbook.
...

### Common fixes

| Symptom | Fix | Verify |
|---|---|---|
| OOMKilled | Raise memory limit by 25% | Wait 90s; check no new `OOMKilled` events for this UID |
| Startup timeout on init container | Extend `initialDelaySeconds` | Wait 2m; pod status Running |
| Bad config in ConfigMap | Roll back to prior ConfigMap revision | Watch for pod restart + steady state |

### Fix-and-verify

After applying any fix:
1. Wait for the verify condition (per row above).
2. Re-run the diagnostic. If green: post a summary + close.
3. If red after 2 attempts: revert + escalate.
```

Playbooks are just Markdown instructions loaded on session start (see [PR γ / #162](https://github.com/go-steer/core-agent/issues/162)). The agent reads them like it reads any other AGENTS.md content — no new primitive. Custom playbooks compose additively via the `AGENTS.d/` directory pattern.

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

Two shipping shapes:

1. **Sidecar container in the same pod as `core-agent`** (recommended for GKE). Shares the pod's SA, so RBAC configuration is one binding. Pod network is `localhost:7777` — no cross-pod auth complexity.
2. **Standalone Deployment** (for operators running `core-agent` outside a k8s pod, or watching a cluster different from where core-agent runs). Separate SA + RBAC + network path to the daemon.

Both variants ship as YAML in `examples/gke-troubleshoot-agent/` — a new recipe that layers on `examples/gke-deploy/`. The recipe includes:

- RBAC: `ClusterRole` granting `list`/`watch` on `core/v1.Events` + `get` on Pods (for status enrichment).
- Sidecar container spec.
- Default playbooks under `.agents/playbooks/` (the ~10 reason-specific files).
- Suggested `.agents/config.json` additions (proxy_identities, playbook loader path).

### Playbook loading

Playbooks are Markdown files under `.agents/playbooks/`. The instruction loader already walks `.agents/` recursively via the `@include` + `AGENTS.d/` primitives ([v2.3 loader design](docs/instruction-loader-v2-design.md)), so the loader needs no changes.

Playbooks appear in the agent's system prompt in `AGENTS.md` order, after the operator's primary instructions. Convention:

```
.agents/
├── AGENTS.md                    <-- primary operator instructions
├── AGENTS.d/
│   └── 01-sre-role.md           <-- role framing ("you are an on-call agent")
└── playbooks/
    ├── AGENTS.md                <-- @include directives for every playbook
    ├── CrashLoopBackOff.md
    ├── ImagePullBackOff.md
    ├── OOMKilled.md
    ├── FailedMount.md
    └── ...
```

`.agents/playbooks/AGENTS.md` is a discovery index:

```markdown
# Playbooks

You are handed an inject with `kind: "k8s-event"`. Match `reason` against
the playbooks below and follow the matched one. If no playbook matches,
fall back to general k8s knowledge.

@include CrashLoopBackOff.md
@include ImagePullBackOff.md
@include OOMKilled.md
@include FailedMount.md
@include FailedScheduling.md
...
```

Operators customize by dropping their own playbooks into `.agents/playbooks/` or into a per-caller overlay via the existing `users_dir` mechanism.

### Per-incident session lifecycle

1. Sidecar sees an event matching the filter.
2. Checks its dedup cache for `(uid, reason)`. If present within window: increment count, suppress inject.
3. Cache miss → sidecar calls `POST /sessions` with `X-Asserted-Caller: <owner>`, gets back `sessionID`.
4. Caches `(uid, reason) → sessionID` locally so follow-up events for the same incident route to the same session (via `POST /sessions/<sid>/inject`).
5. Sidecar POSTs the initial inject payload to the new session.
6. Agent starts investigating; session's per-session wake loop drives turns.
7. When agent completes (fix applied + verified, or budget-exhausted + escalated), it emits a final "incident closed" event.
8. Sidecar drops the incident from its cache when the daemon reports the session is closed OR when the eviction sweep evicts it (session-resume v2.5).

Recurrence handling: if the same `(uid, reason)` fires again *after* the session was closed, the sidecar creates a new session. Prior investigations are still on disk (session-resume) so an operator can review the history.

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

## Open questions

### 1. Sidecar repo location

**Options:**
- **In `core-agent` monorepo** (`cmd/k8s-event-watcher/`) — simpler CI, one release cycle, easy shared-code path if the daemon ever grows k8s awareness.
- **Separate `go-steer/k8s-event-watcher` repo** — clean separation, independent release cadence, easier to grow into `agent-triggers` (Cloud Monitoring, PagerDuty, etc.).

**Recommendation**: in-tree for v2.6. Split when the second signal source ships or when the sidecar accumulates non-trivial k8s-specific code (>1000 LoC).

### 2. Default owner identity

Per-incident sessions need an X-Asserted-Caller value. Options:
- **A single configured owner** (`--owner sre-oncall@example.com`) — simplest, all incidents to one team.
- **Label-driven routing** (`--owner-label team` → uses `pod.labels.team` as owner) — routes each incident to the affected team's session list.
- **Namespace-driven routing** (`--owner-per-namespace default=sre,checkout=checkout-team`) — routes by k8s namespace.

**Recommendation**: single owner for v2.6 (matches the "one team is the audit trail" MVP shape). Label + namespace routing as v2.7+ enhancements.

### 3. Playbook overlay for custom failure modes

Operators will want to add their own playbooks (custom controller reasons, org-specific runbooks). The `.agents/playbooks/` + `AGENTS.d/` shape supports this via additive overlays, but the discovery-index file (`playbooks/AGENTS.md`) is a coordination point.

**Options:**
- **Convention: operator edits the index file** to `@include` their custom playbooks.
- **Auto-discovery**: loader picks up every `*.md` in `playbooks/` automatically (skip the index file).

**Recommendation**: auto-discovery. Matches the AGENTS.d convention and removes a coordination point. Index file becomes optional documentation, not load-critical.

### 4. Fix-and-verify tool vs. prompt pattern

Section "Fix-and-verify primitive" leans toward prompt-pattern for v2.6. **Confirm**: is that the right call for v2.6, or should a `wait_and_verify(predicate, timeout, interval)` tool ship in the same release?

**Recommendation**: prompt-pattern for v2.6, tool for v2.7+ if data justifies. Keeps this PR-stack focused.

### 5. Multi-cluster support

One sidecar watches one cluster. For operators with N clusters:

- **N sidecars, all posting to one daemon** — the daemon audit trail covers all clusters, each incident carries the `cluster` field for filtering. Works today with per-incident sessions.
- **N daemons, one per cluster** — matches the pod-per-cluster deployment model. AX / distributed-coordinator territory. Out of scope here.

**Recommendation**: document the N-sidecars-one-daemon pattern in the recipe as the multi-cluster answer for v2.6.

### 6. Sidecar restart resilience

Sidecar restart resets its dedup cache. Immediate consequences:
- Persistent issues that fired 3 minutes before restart re-fire once each after restart.
- Duplicate incident-session creation (same `(uid, reason)` → two sessions).

**Options:**
- **Accept it** — a small burst of duplicates on restart is acceptable; the agent can dedup via session-list query.
- **Persist dedup cache** — sidecar writes cache to a PVC or configmap.
- **Query the daemon for open incident sessions** — sidecar asks GET /sessions and reconstructs the incident cache from the naming convention.

**Recommendation**: accept the small duplicate burst for v2.6. Revisit if operators report it as pain.

### 7. When is an incident "closed"

Playbook budget-exhaustion is one signal. But operators might close incidents from the TUI too (e.g., "false positive, don't want the agent to re-fire on the next event"). Options:

- **Session close = incident close** — operator closes the session via a future DELETE /sessions; sidecar sees the session is gone and drops the incident from its cache.
- **Sentinel event in eventlog** — agent (or operator) writes an "incident-closed" event; sidecar tails the eventlog for it.

**Recommendation**: session-close pattern (waits on future DELETE /sessions). For v2.6, sidecar caches (uid, reason) → sessionID for `dedup-window` duration; if the same event re-fires after that window, new session. Not perfect but bounded.

### 8. Integration with existing k8s event exporters

`kube-event-exporter`, `robusta`, and similar tools already handle the "watch + filter + forward" part. Building our own sidecar duplicates capability.

**Options:**
- **Build our own** (this design) — full control, simple deploy, no external dependency.
- **Adapter mode**: sidecar accepts webhook POSTs from an existing exporter; forwards to core-agent. Reuses community tooling.
- **Both** — ship the sidecar for green-field ops + an adapter shim for operators already running robusta.

**Recommendation**: build our own for v2.6 (simplest). Adapter shim as v2.7+ if operators ask.

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
