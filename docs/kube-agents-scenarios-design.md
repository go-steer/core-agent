# kube-agents scenarios: can the go-steer stack run them?

Status: design, draft (2026-10-02). Not filed. Nothing here is built; this
doc is the gap analysis and the proposal, so we can decide what to build
before anyone builds it.

Evidence base: read-only passes over these trees on 2026-10-02. **Several
local main checkouts are stale**, so the newest worktree was used and is
named here:

| Component | Tree read | Version |
|---|---|---|
| core-agent | this worktree, `29d4a382` | v2.10.0-dev (main) |
| k8s-lookout | `k8s-lookout/.claude/worktrees/leeway` (branch `leeway-unknown-domain`) | v0.29.0 + `[Unreleased]` (local main is v0.25.0) |
| switchboard | `switchboard/.claude/worktrees/v0.4` | v0.4.0 (local main shows only `[Unreleased]`) |
| mast | `mast/.claude/worktrees/open-rl` | v0.9.0 + v1.0-prep `[Unreleased]` (local main is v0.6.0) |
| simian-agent | main | M1–M3, chart 0.1.9 |
| kube-agents | main | ~0.4.0, on Hermes |

Every "SHIPPED" claim below comes from reading code, not from running it.
Anything marked *unverified* needs a run before a demo leans on it.

## 1. Starting premise

Six scenarios were written **for [`kube-agents`](https://github.com/gke-labs/kube-agents)**:
the GKE agentic harness (Chat/Planning agent, Platform Agent, per-cluster
read-only Cluster Agents, GitOps PRs instead of live mutation, a
credential-isolation broker, a LiteLLM inference gateway, and an A2A
gateway over NATS JetStream). kube-agents runs on **Hermes**, not on
core-agent or mast. Replacing Hermes with core-agent is the subject of
[`hermes-replacement-design.md`](hermes-replacement-design.md). This doc
asks the same question, using six concrete demos as the test.

The scenarios, verbatim:

| Scenario | Audience | Situation | What the agent does |
| :---- | :---- | :---- | :---- |
| **Silent Gateway Controller Stall Remediation (The 'Frozen Ingress' Live Fix)** | Platform Engineers, SREs, Network Architects | An engineer rolls out a new Gateway API HTTPRoute referencing a cert-manager TLS Certificate. An unowned DNS domain causes cert-manager to hang silently. Standard Kubernetes readiness probes report the Gateway as 'Ready', but external traffic drops with 503 errors. | The agent spots that the ingress controller has made no progress for 5 minutes. It traces the hang to a missing TLS secret and a failed DNS check, posts the root cause in chat, and opens a pull request to fix the route. |
| **Declarative Multi-Forge GitOps Remediation with Zero-Leak LLM Gateway** | SecOps Directors, Enterprise Platform Leads, Security Auditors | A misconfigured deployment triggers intermittent OOM kills and CPU throttling in production. The platform lead asks the agent in Slack to investigate and fix the problem, under strict enterprise security mandates prohibiting raw cluster mutation or secret leaks to external AI models. | The agent reads cluster logs and metrics to find why the pod is crashing and throttled. Before sending data to the model, the gateway masks internal IPs, project numbers, and secrets. The agent then opens a GitHub pull request with updated CPU and memory limits for an engineer to review and merge. |
| **Fleet-Wide K8s 1.31 Upgrade Readiness & Deprecation Scanner (50-Cluster Fleet)** | Enterprise Fleet Administrators, Infrastructure Directors | A major enterprise needs to upgrade 100 production GKE clusters from K8s 1.30 to 1.31, matching the M4 soak fleet size. Manually auditing thousands of microservice manifests and CRDs for deprecated API versions is error-prone and takes weeks. | The agent scans up to 100 clusters in parallel before an upgrade. It checks Kubernetes audit logs and manifests to find workloads still using outdated APIs, posts a cluster readiness report, and opens pull requests to update the affected workloads. |
| **High-Cardinality Zonal Skew & Scheduler Anomaly Investigation** | High-Scale Cloud Architects, Batch/AI Infrastructure Leads | During a high-traffic spike on a 500-node cluster, tail latency spikes in one availability zone. Standard dashboards show aggregate CPU is fine, masking that 85% of critical backend pods were scheduled onto nodes in us-central1-a due to an anti-affinity misconfiguration. | The agent checks pod placement across availability zones and flags that 85% of backend pods landed in a single zone. It traces the cause to a bad scheduling rule and opens a pull request with updated pod spread settings to balance traffic across zones. |
| **Collaborative Agent-to-Agent (A2A) Multi-Cluster Incident Swarm** | CTOs, VP of Engineering, Modern DevOps Enthusiasts | A complex multi-fault incident strikes: an expired network policy causes DNS lookup failures, triggering a retry storm that saturates an internal Redis cache. | When an outage alert fires, a lead agent splits the investigation across a network agent and a cache agent over the in-cluster event bus. Both agents investigate at the same time, combine their findings into one Slack timeline, and open pull requests to fix the network policy and cache limits. |
| **Catching Slow-Burning Memory Leaks & Capacity Creep Days Before an Outage (Chronic Trend Analysis)** | SRE Leads, Platform Engineers, Capacity Planners | After a routine release, a payment service slowly leaks 2% of its memory each day and gradually uses up regional Pod IP addresses. Because no threshold is crossed yet, standard alerts stay green—until the service crashes during peak traffic a week later. | During its daily scheduled check, the agent queries 14 days of Cloud Monitoring and Prometheus metrics. It spots a steady upward climb in memory and IP usage that will hit its limit in 4 days, traces the start of the climb to a specific Git commit from last week, warns the team in Slack, and opens a pull request to fix the config and raise limits before any pods crash. |

What kube-agents itself has for these today, for comparison:
`gke-stall-detection` and `gke-workload-troubleshooting` cluster skills
(diagnose and propose only), `fleet-upgrade-verification` (scans GitOps
manifests for removed apiVersions), a draft A2A/NATS spec, LiteLLM as a
provider proxy with **no masking**, and nothing for chronic trends or zonal
skew beyond a requirements doc (`docs/designs/fleet-anomaly-detection-checks.md`).
"M4 soak" appears nowhere in kube-agents, core-agent, mast or lookout.

## 2. Making the scenarios realistic

The audience is Kubernetes practitioners, and several premises would not
survive a hallway question. Each fix below keeps the scenario's point and
only corrects the mechanism.

**S1 — Frozen Ingress.**
- Gateways have no readiness probe. A listener whose TLS Secret is missing
  shows `ResolvedRefs=False` / `Programmed=False` in status. The honest
  hook is: *the pods' probes are green, the Gateway's status is False, and
  nobody watches Gateway status.* That is exactly what lookout's `gateway`
  source flags, after a 5-minute grace, so the "5 minutes" in the original
  survives.
- cert-manager does not "hang silently". The Certificate sits at
  `Ready=False` with a Challenge pending on a DNS name nobody controls. The
  hook is again that nobody looks.
- A missing serving certificate usually fails the TLS handshake. It does
  not produce 503s. Either say "HTTPS fails, HTTP returns 503 from a
  redirect-only listener", or keep it to "external traffic fails".
- The fix belongs in the **Certificate's DNS name or Issuer** (or the
  HTTPRoute hostname, if the hostname is the typo). "Fix the route" is only
  right when the route carries the wrong host.

**S2 — Zero-leak gateway.**
- "Multi-Forge" is in the title, but only GitHub appears in the story. Pick
  one: show two forges, or drop the word.
- "Zero-leak" is an absolute claim, and auditors will press on it. Use
  "masks named classes of data before they leave the cluster", and list
  the classes.

**S3 — Fleet upgrade.**
- The title says a 50-cluster fleet, and the body says 100 twice.
- "Matching the M4 soak fleet size" is an internal reference. Delete it.
- To our knowledge, 1.31 removed no served beta APIs, so a 1.30→1.31 scan
  finds nothing. Use a pair with real removals: 1.31→1.32 removes
  `flowcontrol.apiserver.k8s.io/v1beta3`, or use an older pair such as
  1.24→1.25. *Check the upstream deprecated-API migration guide before
  committing to a pair.*
- GKE already surfaces deprecated-API usage from audit logs as deprecation
  insights. The demo has to add something on top of that: the PRs,
  manifests that are not yet deployed, CRDs.

**S4 — Zonal skew.** This is the soundest of the six. Anti-affinity with
`topologyKey: kubernetes.io/hostname` really does not spread pods across
zones, and `topologySpreadConstraints` on `topology.kubernetes.io/zone` is
the real fix. The only edit is to the story: existing pods are not
rescheduled until the PR's rollout replaces them.

**S5 — A2A swarm.**
- NetworkPolicies do not expire. Use "a newly applied egress policy drops
  UDP/TCP 53 to kube-dns".
- Make the causal chain coherent. If DNS fails, clients cannot reach Redis
  by name either. Use: DNS failures for an *upstream* dependency → clients
  retry → each retry re-reads hot keys from Redis → Redis saturates.

**S6 — Chronic leak.**
- A memory leak does not consume pod IPs. The two connect only through an
  HPA that scales on memory (the leak adds replicas, the replicas use IPs)
  or through crash-loop churn. Say which.
- Pod IPs come from node and cluster ranges, so "regional Pod IP addresses"
  is not accurate.
- The arithmetic is inconsistent. At 2% a day, hitting the limit in 4 days
  means usage is already about 92%, which conflicts with "a week later".
  Pick one horizon.
- "Raise limits" hides a leak. Present the PR as a stopgap plus a revert of
  the commit that started the climb.

**Across all six:** every scenario ends in "posts to Slack, opens a PR".
Six identical endings blur together. Vary at least two: one gated apply
with approval (S2 is the natural fit, since that is what the security
audience wants to see), and one forecast with no action.

## 3. Gap analysis

### 3.1 Capability matrix

Status codes: **S** shipped · **P** in progress (worktree or `[Unreleased]`) ·
**D** designed only · **—** absent.

| Capability | core-agent | k8s-lookout | switchboard | mast | Other go-steer |
|---|---|---|---|---|---|
| Push an incident into an agent session | **S** `POST /sessions` + `/inject` | **S** inject sink, storm/reattach, enrichment | — | **—** `POST /sessions` returns 501 (no `SessionFactory`); `/inject` works only on existing sessions | |
| Gateway API stall detection | — | **S** `pkg/sources/gateway` (Programmed=False after 5 min; HTTPRoute ResolvedRefs) | | — | |
| cert-manager | — | **S** Certificate renewal-failed and expiry; **—** Challenge/Order/CertificateRequest | | — | |
| Zonal placement drift | — | **S** leeway `topology-drift` (v0.26–v0.29); **P** placement-drift and zone-unavailable scenarios | | — | |
| Trend / time-to-limit | — | **S** `saturation` (least squares), `quota` (Cloud Monitoring); no history lookback | | — | |
| Metrics query by the agent | — (mountable MCP, *unverified*) | — (export only) | | **S** via the Cloud Monitoring MCP (mast-sre-agent `workload.yaml`) | |
| Scheduled runs | **S** `schedule_next_turn` + sleep/exit_on_defer; CronJob + `-p`; **D** `core-agent-cron` (#202) | **S** distiller interval only | | **S** `edge_trigger.scheduled` (durable, anchored); `monitor.collect` wakes the model only on transitions | |
| Slack inbound ("@agent …") | | | **S** Socket Mode → session | — (501 above) | |
| Slack outbound / timeline | **S** `alert` tool, `switchboard` template | | **S** ingress `POST` + `PATCH append` | **S** `--park-notify`, `pkg/notify` | |
| Approvals in Slack | **S** `approval_notify` + switchboard `--approvals` | | **S** Block Kit buttons, `--approvers` | **S** server side (#364); "no live switchboard→mast press executed" | |
| Open a PR | bash + `gh` only (self-dev recipe); **D** W3 GitHub MCP (#592) | — (emits PR-draft payloads) | — | — (GitHub MCP as a catalog entry, not done) | kube-agents: **S** GitHub via broker; **D** GitLab/Bitbucket |
| Gated apply / no raw mutation | **S** gate modes, plan-first, `approval_timeout`; gated-apply leg D1/D2 | **S** strictly read-only RBAC | | **S** write gate, outbox, preconditions, change-set grants (stronger) | |
| Pre-model redaction | **—** hooks are observe-only | **S** Secrets/JWT/PEM in what *lookout* emits only | — | **—** in the daemon; a library `model.LLM` wrapper can rewrite input | kube-agents: — (LiteLLM proxy, no masking) |
| Multi-agent fan-out | **S** background subagents (cap 8, depth 2), `call_peer` | — (storms merge, never fan out) | | **S** `dispatch: fanout` + `_synthesis` (fixed roster, default concurrency 4) | |
| A2A protocol | agent card only | | | **S** server; client is library code only, not wired | kube-agents: **D** NATS/JetStream spec |
| Event bus | — | — | | — | kube-agents: **D** |
| Multi-cluster | one cluster per recipe; GKE MCP takes a `parent` arg | **S** N clusters per process (small fleets); **D** fleet rollup #189/#188 | | — (one cluster per process) | |
| Deprecated-API scan | — | — (`audit upgrades` = version lag only) | | — | kube-agents: **S** `fleet-upgrade-verification` (manifests) |
| Fault injection | gke-drill A–D | 14 kind scenarios, kwok, GKE drills | | | simian-agent (NetworkPolicy, Chaos Mesh, 13 kube-state faults); core-sre-agent `fault.sh` |
| 500-node scale | — | **S** kwok `leeway-scale` (one cluster) | | | |

### 3.2 Per-scenario gaps (core-agent + go-steer suite)

**S1 — Frozen Ingress: close.**
Works: lookout `gateway` → critical → core-agent session with enrichment
→ GKE MCP read → `alert` to switchboard.
Gaps:
- No Gateway or cert-manager fault scenario exists (lookout has the
  sources but no fixture).
- lookout does not watch Challenges. The agent can read them itself during
  investigation, so this is not blocking.
- The PR path (see §3.4).

**S2 — Zero-leak gateway: the main loop is close; the headline is missing.**
Works:
- Slack inbound (switchboard).
- Read-only GKE MCP and read-only RBAC, which genuinely enforce "no raw
  cluster mutation".
- OOM faults: drill B, lookout `oom`, `cpu-pressure`.

Gaps:
- **No masking before the model, anywhere** (lookout's sanitizer covers
  only its own payloads, and not IPs or project numbers).
- Throttling evidence needs metrics; the agent has no metrics tool today.
- Only one forge.

**S3 — Fleet upgrade: not feasible as written.**
- No deprecated-API scanner in any go-steer repo.
- The background-subagent cap is 8 and the binary does not expose it.
- lookout's multi-cluster mode targets small fleets.
- kwok scales nodes, not clusters.
- The premise problem from §2.

**S4 — Zonal skew: best fit; this is what leeway is for.**
Works: leeway `topology-drift` → `leeway.contract_violated` → core-agent
session, plus a Grafana dashboard, the LeewayPolicy CRD and kwok at 500
nodes.
Gaps:
- The demo scenarios and the unknown-domain false-positive fix are not on
  any lookout tag.
- **Only Tier A is critical** (only critical opens a session). When the
  misconfigured rule *is* the declared intent, Tier A compares against the
  bad rule, and the catch may come from Tier B (inferred) or Tier C
  (baseline), which route to the digest or metrics. *Verify which tier
  fires for the exact misconfiguration before scripting the demo.*
- kwok pods do not run, so "tail latency in one zone" has to come from a
  small real cluster or be narrated.
- The PR path.

**S5 — A2A swarm: far as written, feasible reframed.**
- core-agent has no A2A transport and no event bus.
- What exists: a lead agent plus two read-only background subagents (well
  inside the cap; they pass the #653 parallel-write guard), or `call_peer`
  to two specialist daemons. lookout's storm correlation already lands
  "one incident, several symptoms" in a single session for the lead.
- The merged Slack timeline: switchboard `PATCH append` builds it, but
  nothing merges several agents' posts, so the lead must be the only
  poster.
- simian-agent's NetworkPolicy engine supplies the DNS break. The Redis
  saturation needs a load generator.

**S6 — Chronic leak: medium to far.**
- lookout `saturation` regresses over what it observed itself. There is no
  14-day lookback.
- core-agent has no metrics tool. Mounting the Cloud Monitoring MCP that
  mast-sre-agent already uses is probably config only (*unverified on
  core-agent*).
- Scheduling works today through a CronJob + `-p` or a `scheduler: sleep`
  subagent.
- "Traced to a Git commit" means `git log` over the GitOps repo, through
  bash or the GitHub MCP.
- A live demo needs backfilled history, or a compressed timescale, which
  undercuts "chronic".

### 3.3 core-agent or mast?

mast is a lean ADK-v2 fork of core-agent for unattended SRE workloads. It
is better engineered for some of this: a durable exactly-once write gate,
change-set grants, a deterministic `monitor.collect` that wakes the model
only on change, an anchored durable schedule, a nightly judged eval corpus,
and about ten releases in eight weeks. Its only real consumer is
core-sre-agent, and it uses very little of it (pinned at v0.4.0, in-memory
runner).

Per scenario, mast compared with core-agent:

| | Verdict | Why |
|---|---|---|
| S1 | same | mast's graph router and durable approval are nicer, but lookout cannot open a session on it (501) |
| S2 | worse | switchboard inbound needs `POST /sessions`; redaction is possible only as custom Go in a library `model.LLM` wrapper |
| S3 | same or worse | fixed-roster fan-out, default concurrency 4, per-input fan-out `not_implemented`, one cluster per process |
| S4 | worse as a push demo | leeway pushes through sinks mast does not accept; better only if reframed as a scheduled pull of `k8s_findings_diff` |
| S5 | worse for A2A, better in-process | the outbound A2A client is not wired; `dispatch: fanout` + `_synthesis` is a clean in-process equivalent |
| S6 | **better** | durable `24h` trigger, Cloud Monitoring `query_range` over MCP, notify-on-change via switchboard, gated writes |

**The deciding fact:** lookout push and switchboard inbound, the two
companions that make these demos look like kube-agents, both need
`POST /sessions`. mast returns 501 for it. Until mast wires a
`SessionFactory`, it can only run pull-shaped demos (S6, and S4 reframed).

**Proposed:** core-agent is the runtime for the set. Splitting six demos
across two runtimes muddies the story ("which one is the product?") for a
one-scenario gain. Revisit if S6 is the headline and mast gains a
`SessionFactory`.

### 3.4 Opening PRs: two designs, and they disagree

- **kube-agents' `docs/designs/version-control-support.md`** (design of
  record, GitHub built). The sandbox holds no token and runs a
  credential-free git. A broker serves forge-neutral `/v1/vcs/*` verbs
  through `providers/`, and the skill calls the object a *proposal*.
  GitLab and Bitbucket are designed but not built.
  `fleet-audit-issue-ledger.md` adds the lifecycle: one PR per finding,
  critical manifests opened automatically, everything else waiting on
  `/remediate <id>`, stale PRs closed, and the harness never merges.
- **core-agent's W3 in [`hermes-replacement-design.md`](hermes-replacement-design.md)**
  (#592, design sketch). GitHub MCP server (branch, commit, PR over the
  API, no clone) plus a `propose-fix-as-PR` skill, opening PRs as the
  calling user through W0 per-caller credentials, which are designed but
  not built. GitHub only.
- **Shipped today:** `bash` + `gh pr create` in the self-dev recipe
  (#1116). This does not work in the distroless image (no shell, no `gh`),
  so it is a laptop-only path for a cluster demo.

The two designs disagree on where the credential lives (a broker vs per
caller) and on multi-forge support. For the demos, the GitHub MCP path is
the least work and runs distroless. It covers every scenario except S2's
"multi-forge". Converging W3 with kube-agents' proposal verbs is a separate
decision (see §6).

## 4. Proposal

### 4.1 What we can show now (glue only, no new product code)

| Demo | Shape | Needs |
|---|---|---|
| **S4 zonal skew** (lead demo) | leeway → critical → core-agent session → diagnosis → Slack post via `alert`/switchboard → PR | a lookout tag with the placement-drift scenario; the Tier check (§3.2); PR glue |
| **S1 frozen ingress** | lookout `gateway` → session → reads Certificate/Challenge → Slack → PR | a Gateway + cert-manager fault fixture; PR glue |
| **S2, minus the gateway claim** | Slack "@agent investigate" → read-only investigation → **gated apply with Slack approval** (D1 leg) instead of a PR | metrics MCP mounted; retitle |
| **S5 reframed** | storm → one session → lead spawns network + cache subagents → lead posts one timeline | simian NetworkPolicy fault + a Redis load generator; retitle away from A2A/event bus |

"PR glue" for the now column means a demo GitOps repo, a GitHub MCP server
entry in the recipe's `mcp.json`, and a short PR skill. This is the W3
shape, scoped to one demo identity, with no W0.

### 4.2 Gaps that have a plan

| Gap | Plan | Status |
|---|---|---|
| Agent-opened PRs | W3 GitHub MCP + `propose-fix-as-PR` (#592); kube-agents `version-control-support.md` | designed |
| Per-caller PR identity | W0 `mcp-credential-resolution-design.md` (#106, #204) | designed |
| Declarative daily schedule | `core-agent-cron` (#202, W2) | designed |
| Agent-judged approvals (no human in the loop for low-risk calls) | auto mode #1175 phases 4–5 | phases 1–3 shipped; not selectable |
| Fleet rollup / fleet drift | lookout #189 / #188 | roadmap |
| Slack reply → session correlation | switchboard DESIGN §3 | deferred |
| mast A2A outbound, federation | mast `federation-design.md` | designed |

### 4.3 Missing: no design yet

| Gap | Blocks | Smallest credible fix |
|---|---|---|
| **Pre-model redaction** | S2 headline | A request-rewriting seam in core-agent (an `LLM` wrapper or a mutating `model-start` hook) with named classes (IPv4/6, GCP project numbers, Secret-shaped tokens) and a golden test. Alternative: document base-URL pointing at a third-party masking proxy, after verifying `ANTHROPIC_BASE_URL` survives the Vertex path |
| **Deprecated-API scanner** | S3 | A lookout `audit` check reading `apiserver_requested_deprecated_apis` plus a manifest scan, or reuse kube-agents' `fleet-upgrade-verification` skill |
| **Fan-out beyond 8 / per-input fan-out** | S3 | Expose the subagent cap in config, or shrink S3 to about 5 clusters |
| **A2A transport / event bus** | S5 as written | None proposed. Reframing is cheaper than building, and AX is the distributed-runtime layer to look at before building one here |
| **Metrics recipe** | S2 evidence, S6 | Verify the Cloud Monitoring MCP endpoint on core-agent; add it to the platform recipe |
| **14-day history fixture** | S6 | Backfill a custom metric, or record a real two-week run and replay it |
| **Gateway / cert-manager fault scenario** | S1 | A lookout `examples/scenarios` entry |
| **cert-manager Challenge watch** | S1 (nice to have) | A lookout source extension |
| **Multi-agent Slack timeline merge** | S5 polish | Lead-only posting is enough for the demo |
| **mast `SessionFactory`** | any mast push demo | Only if §3.3 is revisited |

## 5. Proposed decisions

These are proposals for the reader to accept or overturn. They are not
yet settled.

1. **core-agent is the runtime for all demos.** Reason: lookout push and
   switchboard inbound both need `POST /sessions` (§3.3).
2. **S4 leads, S1 second.** These are the two that exercise companions
   nobody else has (leeway, the gateway source) and need the least new
   code.
3. **S2 ships as a gated apply with Slack approval, not a PR, and without
   "zero-leak" in the title until redaction exists.** This also gives the
   set its varied ending.
4. **S3 is dropped or rebuilt** (about 5 clusters, a version pair with real
   removals, the scanner as glue).
5. **S6 waits** for the metrics recipe and a history fixture.
6. **Demo PRs use the GitHub MCP path**, scoped to one demo identity. W0
   and the kube-agents broker convergence are not on the demo's critical
   path.

## 6. Open questions

- Should W3 adopt kube-agents' forge-neutral *proposal* verbs instead of
  binding to GitHub MCP? Settle this before #592 is built, not after.
- Is pre-model redaction a core-agent feature (it would be the first
  request-*rewriting* seam) or a deployment concern (a masking proxy)? The
  S2 audience will ask which.
- Which leeway tier fires for "the declared rule is the bug"? If it is not
  Tier A, does S4 need a routing override, or a LeewayPolicy that declares
  the intended spread?
- Do these demos replace kube-agents' own demos (on Hermes), or run beside
  them as "the same scenarios on go-steer"?

## 7. Out of scope

- Building any of §4.3 in this doc's PR.
- A fleet-orchestration layer in core-agent (AX's territory).
- Flux/Argo or any CD integration; downstream delivery stays the
  operator's concern.
- Making mast accept lookout/switchboard push, unless §3.3 is reopened.
