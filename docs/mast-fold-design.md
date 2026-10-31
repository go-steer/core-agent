# One harness: folding mast into core-agent

**Status:** settled, 2026-10-30. Decision recorded on
[go-steer/mast#596](https://github.com/go-steer/mast/issues/596); tracked by
[#1411](https://github.com/go-steer/core-agent/issues/1411) (epic) under the
*One harness: mast fold-in* milestone, label `mast-port`. This document is the
design-of-record for the fold: what moves, in what shape, in what order, and
what is deliberately left behind. The per-mechanism designs
(`durable-write-gate-design.md` and friends) are written as their phase
opens and registered beside this one.

## Why

[go-steer/mast](https://github.com/go-steer/mast) forked from core-agent on
2026-07-26 under the "sibling products with divergent agendas" motivation:
mast would be the unattended platform-agent runtime on ADK v2, core-agent the
interactive and experimentation substrate on ADK v1. Both halves of that
premise have expired. core-agent moved to ADK v2 (#1291) and both products
price and talk to providers through core-models; core-agent v3.0's theme was
unattended GKE autonomy — the same platform/SRE user and the same GKE-triage
demo as mast. Meanwhile mast's development stopped on 2026-10-15, it has one
maintainer and no external adopters, 15 package names exist in both trees,
and its sync ledger against this repo lapsed on 2026-09-21. Four open v3.1
issues here (#1359, #1361, #1368, #1360) describe mechanisms mast already
has, and `kube-agents-scenarios-design.md` §3.3 already credits mast's write
gate and outbox as "stronger" while proposing core-agent as the runtime for
the set. The evidence is laid out in mast's
[`docs/assessments/2026-10-29-mast-assessment.md`](https://github.com/go-steer/mast/blob/main/docs/assessments/2026-10-29-mast-assessment.md).

Of the four options on #596 — invest and tag mast 1.0; keep it on 0.x; fold it
into core-agent; rebase it on core-agent as a library — the third is the one
whose costs are one-off. The fourth was the tempting alternative: it would
require core-agent to stabilise ~1,450 exported symbols as seams before it
has an apidiff gate, couple two release cadences for one maintainer, and keep
two repositories answering "which one is the product?".

## Settled decisions (do not relitigate)

1. **core-agent is go-steer's single agent harness.** mast is frozen as of
   2026-10-30 (security fixes, plus whatever go-steer/shipmind runs, until
   shipmind has cut over), tags v0.10.0 as its terminal feature release, and
   is archived afterwards.
2. **Fold capabilities, not files.** Each mast mechanism lands as a
   core-agent feature built against core-agent's shapes — the recipe
   (`AGENTS.md` + `.agents/config.json`), `permissions.Gate`, `pkg/eventlog`,
   the attach protocol. mast's implementation is the reference; mast's tests
   travel as the acceptance criteria. Code lifts verbatim only where it is
   runtime-free (the A2A client and server, the change-set types, the apidiff
   tooling, the Terraform module).
3. **mast's ported copies of core-agent packages are discarded**, not
   re-synced: `attach`, `attachadapter`, `auth`, `compose`, `digest`,
   `eventlog`, `instruction`, `mcp`, `modeltier`, `permissions`, `taskclass`,
   `transcript`, `watchdog`, `agent`. Sibling-sync (mast #153, #592) ends.
4. **One config model, two authoring forms.** `workload.yaml` retires; its
   semantics map onto `config.json` (see #1402). The specialist template
   survives as `.agents/subagents/<name>.md` — YAML frontmatter plus a
   Markdown body as the instructions — loading into the same
   `config.SubagentSpec` as the JSON `subagents[]` form. `mode:` is
   **required** in the Markdown form; the JSON form defaults to `chat`,
   today's behaviour, so no existing recipe changes meaning (see #1401).
5. **A gated dispatch runs in the parent's session log.** mast proved that a
   write-gate park cannot cross into a private sub-session: a park writes its
   question to the log and returns; a resume re-enters at the root. The
   durable-gate design (#1399) chooses between running `task` /
   `single_turn` subagent tools in the parent session (mast's proven shape)
   and making park/resume sub-session-aware; it may not leave the question
   open.
6. **Interactive mode is unchanged by the durable gate.** The durable park is
   a strict superset of today's `PromptBroker` behaviour — `approval_timeout`,
   `approval_notify`, auto mode and allow-always grants are preserved. This
   is an acceptance box in #1399, not an aspiration.
7. **Helm is retired.** kustomize is the first-party install path (#957).
   mast's Terraform module (WIF + release) comes across because GCP IAM is
   orthogonal to the install dialect.
8. **AG-UI is deferred** until a consumer exists. #1409 records that the port
   is a near-verbatim lift of mast's `internal/agui`.
9. **There is no "mast as a library".** No consumer for that shape exists
   (mast #301: nothing outside mast imports its `pkg/`), and core-agent's
   `pkg/` import graph is already slim — `pkg/agent`, `pkg/permissions` and
   `pkg/usage` reference core-tui, charm and prometheus only in comments. The
   weight is API surface, and the fix is the apidiff + promised-subset
   discipline (#1368, #1398) plus a one-call facade (#1404), inside
   core-agent. **Extraction trigger:** when a second real consumer needs the
   governance plugins (gate, outbox, meter, guardrail store) without the
   daemon, extract them from core-agent into a new leaf module — core-models
   is the template — under a new name. Not before, and not as "mast".
10. **shipmind is the forcing function and the acceptance test.**
    go-steer/shipmind runs mast as an AX task on Agent Substrate and is the
    only production consumer. Its critical path is P1 → P3 → cutover
    (go-steer/shipmind#48). The demo cluster runs full snapshots, so an
    in-memory approval park survives a suspend and the durable gate (P2) is a
    follow-on pin bump rather than a cutover gate. P4 and P5 touch nothing
    shipmind uses and follow the cutover.
11. **The AX fork is the source.** shipmind's `versions.yaml` records that
    `google/ax` does not take PRs. core-agent's AX example pins the runner to
    follow shipmind's `ax-runner` row, and the weekly check (#1410) watches
    upstream for what the fork has not rebased onto. The earlier gRPC
    `AgentService` adapter (`ax-integration-audit.md`, the `axplore` branch)
    is retired; if AX-as-orchestrator returns, A2A is the protocol (#1407).

## What moves, and in what shape

| mast mechanism | core-agent home | Issue |
|---|---|---|
| Per-call spend ledger priced at call time, restored on resume (`pkg/budget/durable.go`, `eventlog.SpendStore`) | `pkg/usage` + `pkg/eventlog`; rebuild over the session tree, never re-priced | #1359 |
| Single scheduling lease that consumes `Lost()` (`internal/cli/schedlease.go`) | `pkg/eventlog` lease consumer in `autonomous.Resume` only. The wake loop deliberately takes no lease: ordinary turns are not session-level mutual exclusion, and the one race that matters there — two daemons continuing the same interrupted tail — is closed by the durable continuation claim (`pkg/eventlog/claim.go`), not a lock. "Single replica, safe restart" documented | #1361 |
| apidiff + `dev/api-surface.txt` + `dev/api-breaks.txt` + compatibility policy + `deprecation_test.go` | `dev/tools/verify-apidiff`, a promised subset of `pkg/`, the 4.0 removal list | #1368, #1398 |
| Guardrail state in an owned, append-only table (`eventlog.GuardrailStore`) | replaces `drainOutOfBandEvents` | #1360 |
| AX task-runner deployment (`examples/deploy/ax`, `internal/injectedauth`) | `examples/…/deploy/ax`, `CORE_AGENT_GOOGLE_AUTH=injected` | #1395, #1410 |
| A2A client behind the federation seam | the shipped `RemoteAgentSpawner`; a `remote:` subagent target through the `spawn_agent` door | #1396 |
| Slim-embed guarantee (`scripts/check-slim-deps.sh`, `examples/deploy/slim`) | `dev/ci/presubmits/` + `examples/embed/slim` | #1397 |
| Write gate (`pkg/approval`) + effect outbox (`internal/effects`), `/parks`, `/ack-effects`, decision records | two ADK runner plugins, outbox first; the event log is the outbox; `PromptBroker` becomes a view over durable parks | #1399, #1400 |
| Specialists: `mode: Task \| SingleTurn`, enforced `output_schema`, `tier`, `capability`, per-tool MCP filter, the Markdown template | additive fields on `config.SubagentSpec`; mode constructors into `pkg/agent`; the Markdown loader | #1401 |
| Workload bundles (`tool_catalog` classification, `hitl`, budgets, `builtin_tools`) | `config.json` + `mcp.json` + `permissions.allow`; `gke-triage` merges into `examples/gke-platform-agent` | #1402 |
| Nightly judged board + kind-graded outcome tier | `dev/evals`, release gate | #1403 |
| `mast.Run` / `mast.RunWorkload` one-call library front door | a thin facade over `pkg/compose` | #1404 |
| Scheduled fires with a durable anchor; boot auto-resume | in-process, behind the lease; closes the #202 design question | #1405 |
| `monitor.collect` / `transitions_from` / `notify` / `monitor-ack` | the deterministic monitoring cycle; contracts kept verbatim | #1406 |
| A2A server (JSON-RPC: `message/send`, `message/stream`, `tasks/get`, `tasks/cancel`) | mounted beside the agent card on the attach mux; backend = the session factory; a park is `input-required` | #1407 |
| Terraform module; SLSA provenance | `examples/gke-platform-agent/deploy/terraform`; `release.yml` | #1408 |

## Out of scope

- mast's watchdog pool and its three hand-written turn drivers — core-agent's
  guardrails and the turn-outcome state machine (#1389) own that.
- mast's `graph` and `planner` dispatch shapes. mast's own #504 found one
  generalist beat the routed roster 17/18 to 13/18; `fanout` comes only
  behind an eval that says it wins.
- mast's `serverauth` — the A2A server admits through core-agent's bearer
  table and multi-session identity.
- A2A push notifications (`tasks/pushNotificationConfig/*`).
- The Helm chart; AG-UI (deferred, above); the `axplore` gRPC adapter.
- Renaming mast-web. core-agent is its primary backend; the name is a
  separate decision.
- A shared "governance" module. See decision 9 for the trigger.

## Phases

The spine is **P1 → P3 → shipmind cutover**; P2 runs beside P3; P4 and P5
follow the cutover; P6 archives mast. The checklist with live state is
#1411.

| Phase | Content | Exit |
|---|---|---|
| P1 | #1359, #1361, #1368, #1360 with mast's code; #1395 AX example + injected auth; #1396 A2A client; #1397 slim-embed; #1398 promised subset; #1410 ax-pin-check | shipmind's task template runs against a core-agent image with only image + command line changed; a session and a pending approval survive suspend/resume; credentials stay gateway-injected; `GET /sessions` / `status` / `events` satisfy shipmind's scenario runner |
| P2 | #1399 design, #1400 port; mast #561 and #562 fixed in the port | kill -9 during a park → the park is offered again; an approved call cut off mid-execution → ambiguous-effect block, never a second execution; interactive `ask` byte-identical |
| P3 | #1401 subagent modes + Markdown form; #1402 converter + `gke-triage` merge; #1403 evals; #1404 facade | every shipped recipe's resolved subagent tools are byte-identical with `mode` unset; converted `gke-triage-f` gives the same answers on shipmind's nightly; `dev/uat/gke-drill` A1–A7 pass on the merged recipe |
| cutover | go-steer/shipmind#48: `images/agent` from a core-agent pin, bundles in recipe form, two pins each with a scenario run, a new task template with a drain | nightly `deploy/e2e/substrate` green on core-agent, including an approval surviving suspend/resume; `versions.yaml` no longer pins mast |
| P4 | #1405 scheduled fires + auto-resume; #1406 monitoring cycle | the S6 scenario (`kube-agents-scenarios-design.md`) runs on core-agent |
| P5 | #1407 A2A server; #1408 Terraform + SLSA; #1409 AG-UI recorded | the agent card advertises the transport and a third-party A2A client completes a task |
| P6 | archive mast; repoint core-sre-agent and ephemeris; mast-web README | mast's README points here; `gh issue list -l mast-port --state open` is empty or re-filed |

## Compatibility rules the fold must keep

- Every new field on `config.SubagentSpec` and `agent.SubagentOptions` is
  additive, with the zero value meaning today's behaviour; `MCP []string`
  keeps its type. The apidiff gate (#1368) lands before #1401.
- A golden test pins the resolved subagent tool declarations of every
  shipped recipe (the root `reviewer`, gke-platform-agent's `cluster`, the
  self-dev recipe) before and after #1401.
- #1375 (unknown nested keys are silently ignored) lands with, or before,
  `mode:`, so an older binary cannot silently run a `task` subagent as
  `chat`.
- The attach protocol's *shape* stays wire-compatible with mast-web; the
  parks frames are aligned so the embedded UI keeps working through P2.
