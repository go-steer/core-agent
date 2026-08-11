# Scheduled operations — cron-driven autonomous platform agent

Design doc for a v2.7 addition: a companion `core-agent-cron` sidecar that fires prompts into the core-agent daemon on a cron schedule, enabling proactive autonomous operations (compliance audits, blueprint drift detection, capacity forecasting, cost sweeps) — the "always-working platform agent" mode that reactive event-driven triage (v2.6 `k8s-event-watcher`) doesn't cover.

**Status:** proposed (2026-07-10). Awaiting approval before implementation. v2.7 candidate. Tracking issue: [#202](https://github.com/go-steer/core-agent/issues/202).

## Motivation

The `k8s-event-watcher` sidecar shipped in v2.6 gives us **reactive** autonomous triage — the agent wakes up when a specific Kubernetes Event fires, investigates, and closes the incident. That covers "something broke, respond to it."

But a substantial class of platform-agent use cases is **proactive**: sweeps that fire on a cadence regardless of whether anything's obviously wrong. Examples from the field:

- **Compliance audit** — nightly sweep of pod-security-standards violations, unsigned images, missing NetworkPolicies.
- **Blueprint / GitOps drift detection** — hourly diff of live cluster state against the declared source of truth; flag anything modified out-of-band.
- **Capacity forecasting** — daily "will we run out of Pod CIDRs / node quota / IPs in the next 7 days?" analysis.
- **Cost analysis** — weekly per-team spend report; flag runaway consumers.
- **Cluster hygiene** — daily "which pods have been Pending >24h, which PVCs are orphaned, which LoadBalancers have zero backends?"
- **Security patch cadence** — periodic check of node OS versions, kubelet versions, GKE control-plane versions against the fleet's baseline.

None of these are event-driven. All of them require the agent to be "always working" — the operator's expectation is that these sweeps happen without human intervention, with the same audit trail + plan-first safety + escalation that reactive triage gets.

**What kube-agents ships that we don't:** 10 cron jobs driving exactly this class of operations (`blueprint-sync`, `compliance-audit`, `policy-propagation`, `global-capacity-orchestrator`, `security-patch`, `lifecycle-deprecation`, `fleet-wide-cost`, `standardization-validator`, `obtainability-audit`, `github-issue-resolver`). Each is a cron entry → prompt → skill invocation. It's the differentiator between "reactive first-responder" (what we ship today) and "always-working platform agent" (what they ship).

**Current workaround:** operators would need to run an external Kubernetes CronJob that POSTs to `/inject` on a schedule — exactly the kind of glue that `k8s-event-watcher` exists to eliminate for the reactive path. Baking this into a first-class companion sidecar closes the last-mile gap between "the daemon can autonomously act on incidents" and "the daemon can autonomously initiate work."

## Goals

- **First-class cron primitive** — operator declares jobs in one config file; sidecar fires them on schedule; daemon runs them through the same session + plan-first + audit substrate as any operator-initiated request.
- **Same architectural pattern as `k8s-event-watcher`** — companion sidecar POSTing to `/sessions[/<sid>]/inject` via bearer + proxy identity. Zero daemon primitives.
- **Per-job session isolation** available (each fire creates a new session for full audit-trail scoping) OR shared-session mode (one long-running session accumulating context across fires).
- **Distroless-safe** — pure Go binary, no shell, no external deps.
- **Testable locally** — dry-run mode + force-fire flag so operators can validate a job's prompt without waiting for the schedule.
- **Composable with the rest of v2.7** — scheduled jobs that need turnkey escalation call the alert tool ([#192](https://github.com/go-steer/core-agent/issues/192)); jobs that need bespoke diagnostics use kode-gopher via [#200](https://github.com/go-steer/core-agent/issues/200); jobs that talk to Slack for reports use MCP-OAuth ([#190](https://github.com/go-steer/core-agent/issues/190)).

## Non-goals (v2.7)

- **Kubernetes CronJob as the implementation.** We could theoretically wrap each job in a K8s CronJob object that curls `/inject`, but that pushes operator complexity into K8s manifest management and loses per-job features (dedup, dry-run, force-fire, dependency ordering). First-class sidecar is the right shape.
- **Job-dependency DAGs.** Cron entries fire independently; no "job A must complete before job B" semantics. If operators want that, they compose it into one prompt.
- **Distributed job scheduling across a fleet.** One sidecar owns its schedule. Multi-sidecar deployments (one per cluster) don't coordinate — matches the multi-cluster posture from #186 (per-cluster daemon) and #200 (per-cluster kode-gopher sandbox).
- **Persistent job history / metrics beyond the eventlog.** Every job fires an inject; the resulting session is already in the eventlog. External tooling scrapes eventlog for job-run metrics if needed. No separate `agent_scheduled_runs` table.
- **Backfill of missed schedules.** If the sidecar is down at fire time, that fire is missed. No catchup. Operators who need "must-run-once-per-day-even-if-delayed" semantics can wrap in an external K8s CronJob with `startingDeadlineSeconds`.
- **UI for editing schedules.** Config-file only. `kubectl apply` the ConfigMap update + `kubectl rollout restart` the sidecar pod. Same posture as every other core-agent config.
- **Job-level rate limiting or fair scheduling.** Two jobs firing simultaneously both run; no serialization. If they overlap on cluster resources that matters at plan-first / permission-gate level, not scheduler level.

## Conceptual model

### Sidecar shape

New companion binary `cmd/core-agent-cron` — mirrors `k8s-event-watcher`'s deployment pattern exactly:

- Reads a job schedule from a config file (`jobs.json`).
- For each job, evaluates cron on a ticker. When a job's next-fire time elapses, sidecar calls `POST /sessions[/<sid>]/inject` on the daemon with the job's prompt.
- Runs as a sidecar in the daemon's pod (localhost stdio; simplest deploy) or as a separate Deployment (fleet / multi-cluster).
- Authenticates via bearer token in a Secret. Each job's `owner` maps to a proxy-identity `X-Asserted-Caller` so sessions surface in the right team's audit trail.

### Job shape

A job is one entry in `jobs.json`:

```jsonc
{
  "version": 1,
  "jobs": [
    {
      // Human-readable id; used in metrics + logs + session naming.
      "name": "compliance-audit-nightly",

      // Standard cron expression. @hourly / @daily / @weekly aliases
      // supported (from the underlying cron library).
      "schedule": "0 2 * * *",
      "timezone": "UTC",

      // The prompt the sidecar injects. Free-form; typically instructs
      // the agent to invoke a specific skill and post results via the
      // alert tool.
      "prompt": "Run the compliance-audit skill. Report any violations via alert(target: 'slack-oncall', level: 'warning').",

      // Which identity owns the created session. Must be listed in
      // the daemon's proxy_identities. Determines whose /sessions
      // list the run shows up in.
      "owner": "sre-oncall@example.com",

      // per-execution: create a new session for each fire (audit-scoped)
      // shared: post to a single long-lived session accumulating context
      "session_mode": "per-execution",

      // Only used in shared mode: the fixed SessionID prefix. If a
      // session with this ID doesn't exist, the sidecar creates it
      // once and reuses. Distinct jobs can share OR use distinct
      // prefixes — operator's call.
      "target_session_id": "",

      // Optional: max wall-time budget for the injected prompt.
      // Sidecar doesn't enforce this directly (the daemon doesn't
      // support kill-a-turn); it's a signal to the agent (LLM sees
      // the value in the payload and self-regulates).
      "max_wall_time": "10m",

      // Optional: what to do if a prior fire's session is still active
      // when the next fire comes due.
      // "allow"  (default) — new session starts alongside the old
      // "skip"           — this fire is dropped; log it, wait for next
      // "replace"        — cancel the prior session, start new
      "concurrency_policy": "allow"
    }
  ]
}
```

### Session mode — the trade-off

**Per-execution** (recommended default for most jobs):
- Each fire creates a fresh session via `POST /sessions`.
- Full audit trail per run — operator can `GET /sessions` and see one entry per compliance-audit invocation.
- Session eviction (v2.5's `session_idle_timeout`) cleans them up naturally.
- Right for jobs where "each run should be independent" — audits, drift checks, sweeps.

**Shared** (for jobs where accumulated context matters):
- All fires target the same SessionID.
- One long-running conversation, agent has memory of prior fires.
- Right for jobs like "track this specific incident's convergence over time" where the agent's prior conclusions matter.
- Session naming: sidecar creates the shared session once (idempotent — reuses existing), fires per schedule.

### Missed-schedule policy — skip, don't backfill

If the sidecar is down at a fire time (crashed, restarting, cluster maintenance), that fire is missed. No catchup on next start.

Rationale:
- **Correctness**: backfilling a compliance sweep from 24h ago doesn't tell you today's state.
- **Blast-radius**: sidecar restart storm shouldn't fire N missed jobs at once.
- **Simpler**: matches k8s CronJob semantics with default `startingDeadlineSeconds`.

Operators who need "must-run-once-per-day-even-if-delayed" semantics wrap the whole thing in an external K8s CronJob that calls the sidecar's force-fire endpoint (see below).

### Failure semantics — log and move on

Job's inject POST fails (daemon down, bad prompt, auth error): sidecar logs the error, moves on to the next scheduled fire. No retry.

Rationale: retries can amplify problems (agent stuck in a bad state; retrying just re-triggers the failure). The alert tool is the right escalation path for "job repeatedly failing" — separate cron job that queries the sidecar's metrics endpoint.

### Force-fire (testing + emergency)

Sidecar exposes a small HTTP endpoint (`--force-fire-addr :9091`) with:

- `POST /jobs/<name>/fire` — trigger a job's next injection immediately. Same code path as scheduled fire. For testing during development and for "run this compliance sweep right now" emergency operator invocations.
- `GET /jobs` — enumerate configured jobs with their next-fire timestamps + last-fire outcome.
- `GET /metrics` — Prometheus counters + gauges (fires_total, inject_errors_total, active_incidents).

Auth: bound to `127.0.0.1` by default (in-pod use only). Bind to `0.0.0.0` explicitly if operators want kubectl-port-forward-shaped access.

## Detailed design

### Sidecar CLI

```
Usage: core-agent-cron [flags]

Reads a jobs.json config; fires prompts into a core-agent daemon per
the configured cron schedule. See docs/scheduled-ops-design.md.

Required:
  --daemon-url URL       Base URL of the core-agent daemon
  --token-env NAME       Env var name holding the bearer token
  --jobs PATH            Path to jobs.json

Job routing:
  --owner-default IDENTITY   Fallback X-Asserted-Caller when a job
                             doesn't specify its own `owner`

Operational:
  --dry-run              Print each fire's payload instead of POSTing.
                         For testing schedule + prompt content.
  --force-fire-addr HOST:PORT   HTTP endpoint for /jobs, /jobs/<name>/fire,
                             /metrics. Default: 127.0.0.1:9091.
  --tz TZ                Default timezone for jobs without their own.
                         Default: UTC.
  --log-level {debug,info,warn,error}   Default: info.

Kubernetes-adjacent:
  --namespace NS         Only used for the sidecar's log context — the
                         cron sidecar doesn't itself talk to k8s API.
```

### Runtime model

- One goroutine per job. Each goroutine sleeps until the next cron-fire, calls the daemon's inject endpoint, records outcome to metrics, loops.
- No shared state between jobs beyond the shared HTTP client + config.
- Config reload: SIGHUP re-reads `jobs.json`. Live jobs' next-fire re-computes; added jobs start; removed jobs' goroutines exit.
- Graceful shutdown: SIGTERM cancels all job contexts; in-flight injects abort at their next context check.

### Session lifecycle

**Per-execution mode:**
1. Fire time reached.
2. Sidecar `POST /sessions` with `X-Asserted-Caller: <owner>`; gets back a fresh SessionID.
3. Sidecar `POST /sessions/<sid>/inject` with the job's prompt (structured JSON envelope naming the job, run ID, scheduled time, max_wall_time).
4. Session's wake loop drives a turn; agent runs; session persists in the eventlog.
5. Idle eviction (v2.5) cleans up in-memory when the session ages out; ACL row + eventlog stay for audit.

**Shared mode:**
1. On first fire (or config load), sidecar checks if a session with the configured `target_session_id` exists (`GET /sessions/<sid>/events` — 404 = doesn't exist).
2. If missing, `POST /sessions` with X-Asserted-Caller — asks the daemon to create with a specific SessionID (this is a v2.7 daemon extension: existing POST /sessions mints an ID; shared mode needs a variant that accepts a caller-supplied ID). ← flagged as an open question below.
3. Subsequent fires inject into the same session.

### Inject payload

Structured JSON so the agent can pattern-match (mirrors `k8s-event` from #186):

```json
{
  "kind": "scheduled-op",
  "job": "compliance-audit-nightly",
  "run_id": "run-2026-07-11T02:00:00Z-abc123",
  "scheduled_time": "2026-07-11T02:00:00Z",
  "actual_fire_time": "2026-07-11T02:00:03Z",
  "prompt": "Run the compliance-audit skill. Report any violations via alert(target: 'slack-oncall', level: 'warning').",
  "max_wall_time": "10m",
  "job_metadata": {
    "concurrency_policy": "allow",
    "session_mode": "per-execution"
  }
}
```

The `prompt` field is the human-readable instruction the LLM acts on; the envelope fields (`kind`, `job`, `run_id`) let skills scope the work (e.g., a compliance-audit skill checks `kind == "scheduled-op"` and picks a different path than an operator-initiated invocation).

### Metrics

Prometheus counters + gauges (matches `k8s-event-watcher`'s shape):

| Metric | Type | Labels |
|---|---|---|
| `core_agent_cron_fires_total` | counter | job, outcome (success / inject_error / session_create_error) |
| `core_agent_cron_next_fire_seconds` | gauge | job — seconds until next scheduled fire |
| `core_agent_cron_last_fire_timestamp` | gauge | job — Unix time of last fire attempt |
| `core_agent_cron_concurrent_active_sessions` | gauge | job — for concurrency-policy monitoring |

## Per-substrate impact

### `cmd/core-agent-cron/` (new)

- New in-tree Go binary. Depends on `github.com/robfig/cron/v3` (well-known cron library, active, MIT-licensed) + net/http standard library.
- Fifth (or sixth, after #200's `k8s-sensors`) image in the release pipeline: `ghcr.io/go-steer/core-agent-cron`.

### `core-agent` (daemon)

**One small extension** — the shared-session mode requires the daemon to accept a caller-supplied SessionID on `POST /sessions`. Today it mints one. Add:

- New optional field in the create-session request body: `{ "session_id": "..." }`.
- If supplied AND the identity is a proxy caller AND the (app, sid) triple doesn't exist yet: create with that ID.
- If supplied AND the triple exists: return the existing session (idempotent — mirrors resume behavior).

That's ~40 LoC in `pkg/attach/handlers_create_session.go` + tests.

### `examples/gke-troubleshoot-agent/` (existing recipe)

Optional add-on: `deploy/base/53-deployment-core-agent-cron.yaml` sidecar with a starter `jobs.json` demonstrating one job (e.g., a nightly cluster-hygiene sweep). Operators customize their own job set.

### `docs/site/content/docs/reference/`

New page `scheduled-operations.md` covering job config shape, session-mode trade-offs, force-fire endpoint, integration with alert tool + kode-gopher for compliance-audit-shaped work.

## Migration story

Net-new feature.

- **Existing v2.6 deployments** — no behavior change. Sidecar not deployed → no scheduled ops.
- **New deployments** — add sidecar to the pod (or as a separate Deployment), mount a `jobs.json` ConfigMap, wire owner-identity token via Secret. Rolling restart.
- **Operators adding jobs** — edit `jobs.json` in the ConfigMap, `kubectl apply` the ConfigMap, `kubectl exec` a SIGHUP (or restart the sidecar). Live-reload semantics keep the daemon undisturbed.

## Implementation phases

### Phase 1 — Sidecar core + per-execution mode (PR ε.1 of #202)

- `cmd/core-agent-cron/` skeleton: CLI, config parser, one-goroutine-per-job cron loop.
- Per-execution session mode: POST /sessions per fire.
- Force-fire HTTP endpoint (`/jobs`, `/jobs/<name>/fire`, `/metrics`, `/healthz`).
- Unit tests: cron parsing, fire timing (via injectable clock), inject-payload shape.

Estimate: ~500 LoC prod + ~350 LoC tests. ~3 days.

### Phase 2 — Shared-session mode + daemon extension (PR ε.2 of #202)

- Daemon extension: `POST /sessions` accepts optional `session_id` field; returns existing session (idempotent) or creates with the supplied ID.
- Sidecar: shared-session mode with configurable `target_session_id`. Idempotent session-create on first fire.
- SIGHUP config reload.
- Tests + integration test using an in-process fake daemon.

Estimate: ~250 LoC prod + ~200 LoC tests. ~2 days.

### Phase 3 — Recipe integration + release infra (PR ε.3 of #202)

- `examples/gke-troubleshoot-agent/deploy/base/53-deployment-core-agent-cron.yaml` sidecar Deployment (optional in kustomization; operators enable per taste).
- Starter `jobs.json` with one example (nightly cluster hygiene sweep — invokes k8s-triage skill with a "sweep and report" prompt).
- Recipe README section on adding scheduled ops.
- `release-images.yml` matrix entry for `core-agent-cron`.

Estimate: ~150 LoC manifests + ~250 LoC docs. ~2 days.

### Phase 4 — Hugo docs + CHANGELOG + status flip (PR ε.4 of #202)

- New `docs/site/content/docs/reference/scheduled-operations.md`.
- Design doc status flip.
- CHANGELOG v2.7.0 entry (paired with other v2.7 additions).

Estimate: ~200 LoC docs. ~1 day.

**Total**: ~1,900 LoC across 4 PRs, ~8 days of focused work. Similar shape and scope to the k8s-event-watcher stack from v2.6.

## Open questions

### 1. Cron library — `robfig/cron/v3` or roll our own

- **`robfig/cron/v3`** — well-known, stable, MIT, 15k+ stars. Handles `@hourly`/`@daily` aliases, timezone-aware scheduling, standard 5-field cron syntax.
- **Roll our own** — tighter binary, no external cron dep, but reinventing a well-tested wheel.

**Recommendation**: `robfig/cron/v3`. The dep is small and mature; not worth engineering.

### 2. Daemon extension: caller-supplied SessionID on POST /sessions

The shared-session mode needs a way to create a session with a known ID (so the sidecar can idempotently reuse it across restarts). Today's `POST /sessions` always mints a fresh UUID.

Options:
- **Add optional `session_id` field** (current design). Small, backward-compatible.
- **Separate endpoint** `POST /sessions/named/<id>` — more RESTful; more surface.
- **Skip shared-session mode entirely for v2.7** — every job is per-execution. Simpler; loses the "long-running conversation" use case.

**Recommendation**: add optional `session_id` field. Small extension, unlocks shared mode, no new endpoint surface.

### 3. Timezone default

- **UTC** (current design) — unambiguous, no daylight-saving surprises.
- **Sidecar's local time** — matches operator intuition ("2am" means their local 2am).
- **Explicit per-job only, no default** — refuse to fire a job that doesn't specify.

**Recommendation**: UTC default; per-job override supported. Matches every other infra-cron tool.

### 4. Concurrency policy — default

Three options ship: `allow` (parallel), `skip` (drop), `replace` (cancel prior). Which is the default when a job doesn't specify?

- **`allow`** (current design) — simplest; most cron-like semantics.
- **`skip`** — safer default; avoids overlap surprises.
- **`replace`** — most work-conservative; latest run always wins.

**Recommendation**: `allow`. Matches every other cron tool operators know. If overlap causes problems, operator sets `skip` per-job explicitly.

### 5. Dry-run scope

- **`--dry-run` on the whole sidecar** (current design) — every job prints instead of POSTing. Useful for testing new configs end-to-end.
- **Per-job `dry_run: true` field** — only specific jobs skip firing. Useful for staged rollout.
- **Both** — global override + per-job knob.

**Recommendation**: both. Global `--dry-run` for development / CI; per-job `dry_run` for phased production rollout ("this new compliance-audit job stays in dry-run for a week while we tune the prompt").

### 6. Force-fire auth

The force-fire HTTP endpoint bypasses the schedule. Options for gating it:
- **Bound to `127.0.0.1` by default** (current design) — in-pod use only; `kubectl exec` if you need remote.
- **Require the same bearer token as the daemon** — allows binding to `0.0.0.0` safely.
- **Both** — default localhost, opt-in bearer-authenticated remote.

**Recommendation**: both. Simplest deployment defaults to localhost-only; operators who want remote access enable the bearer-auth path explicitly.

### 7. Missed-schedule catchup

Confirmed no catchup in this design (see "Missed-schedule policy"). Worth stating explicitly:

**Recommendation**: no catchup. Documented as v2.8+ if operators demonstrate demand.

### 8. Integration with plan-first

Scheduled ops that mutate cluster state need to compose with plan-first. Question: how does the injected prompt tell the LLM to expect plan-first, or does it always know?

- **Session inherits daemon's `permissions.mode`** — scheduled jobs get whatever mode the daemon runs in (typically `yolo + require_plan_artifact` for autonomous). Nothing new.
- **Per-job override** — job config can force a specific mode for its session.

**Recommendation**: session inherits. Simpler; matches how POST /sessions already works. Operators who need per-job mode overrides can invoke via a skill that manages the gate itself.

## Security considerations

- **Bearer token = long-lived credential.** Same posture as `k8s-event-watcher`'s token — mount from Secret; rotate on any suspected compromise.
- **Proxy identity scoping.** The sidecar's identity must be in the daemon's `proxy_identities` list; each job's `owner` must be a real identity in `users.json`. The daemon rejects otherwise.
- **Prompt injection surface.** Jobs' prompts are operator-authored config; no untrusted input. Different from `k8s-event-watcher`'s inject payloads, which can carry hostile-controller-generated content in message fields. Scheduled-op prompts are trust-boundary-authored.
- **Force-fire endpoint.** Localhost-bound default limits blast radius. Bearer-authenticated remote mode is optional and requires explicit config.
- **Job runaway.** A misconfigured cron entry (`* * * * *` fires every minute) could DoS the daemon. Mitigation: metrics endpoint surfaces `fires_total`; operators can alert on anomalous rates. Cron parser can also warn on suspiciously-frequent schedules at startup ("schedule fires every minute; are you sure?").
- **Missed-schedule silent drops.** If the sidecar is down when a compliance sweep should have fired, that sweep is dropped. Operators need to alert on the sidecar's own health (Prometheus scrape of `/healthz`) — same as any other daemon. Not the scheduled-ops sidecar's job to guarantee uptime for downstream policy.

## Out of scope (deferred to v2.8+)

- **Backfill / catchup for missed schedules** (OQ #7).
- **Job dependency DAGs** (jobs A before B).
- **Distributed job scheduling** across multiple sidecars.
- **Persistent job-run history table** — the eventlog covers this; separate table is redundant.
- **UI for editing schedules** — config file is the interface.
- **Job-level rate limiting** across the fleet.
- **Rich retry policies** — log-and-skip is the v2.7 posture.
- **Cron expressions with second-level precision** — `robfig/cron/v3` supports; we don't expose.
- **Scheduled-op cancellation via `/jobs/<name>/cancel`** — kill an in-flight run. Nice-to-have; not v2.7.

## Dependencies and related work

- **`github.com/robfig/cron/v3`** — cron parser + scheduler. New Go dep.
- **[#186](https://github.com/go-steer/core-agent/issues/186) k8s-event-watcher** — the reactive-triage sibling this design mirrors. Same sidecar shape, same POST-to-inject wire protocol.
- **[#190](https://github.com/go-steer/core-agent/issues/190) MCP-OAuth** — enables scheduled jobs that consume OAuth-authenticated MCPs (e.g., a weekly Slack digest).
- **[#192](https://github.com/go-steer/core-agent/issues/192) alert tool** — the natural escalation target for scheduled sweeps ("compliance audit found 3 violations → alert Slack").
- **[#200](https://github.com/go-steer/core-agent/issues/200) tiered tools + kode-gopher** — many scheduled sweeps (`disk-orphan-scout`, `lb-ghost-buster`, `stale-object-sweeper`) will execute as kode-gopher-generated Go invoked by the scheduled prompt.
- **kube-agents (external)** — the "10 cron jobs" that motivated this design.

## When this lands

- Phase 1 (sidecar core + per-execution mode): ~3 days
- Phase 2 (shared mode + daemon extension): ~2 days
- Phase 3 (recipe integration + release infra): ~2 days
- Phase 4 (docs + CHANGELOG): ~1 day

~8 days of focused work across 4 PRs. Ships in v2.7 alongside MCP-OAuth (#190 design merged), alert tool (#192 design merged), tiered tools + kode-gopher (#200 design in review, PR #201). Combined, v2.7 becomes a substantive release that closes both the escalation gap and the "always-working platform agent" gap identified in the kube-agents comparison.
