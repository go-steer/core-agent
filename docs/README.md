# core-agent design docs

Internal design notes — the *why* behind the code. The published
[Astro Starlight](https://starlight.astro.build) site lives in
[`site/`](site/); this directory is reference material for
contributors.

[`DESIGN.md`](DESIGN.md) is the architectural entry point. Active
strategy, compatibility policy, contributor rules, and open (`proposed`)
design docs live at the top level of `docs/`; shipped, superseded, and
abandoned design docs, milestone plans, and historical smoke records
live in [`archive/`](archive/).

> **Registry & status policy.** Per [`AGENTS.md`](../AGENTS.md) and
> [#1388](https://github.com/go-steer/core-agent/issues/1388), every
> design doc carries a `**Status:** shipped | proposed | abandoned | superseded`
> line near the top and is registered in this file in the same PR. When
> a design ships or is superseded/abandoned, flip its `Status:` line and
> `git mv` it to [`docs/archive/`](archive/). Enforced by
> `dev/tools/docs-lint` (`verify-docs-lint`).

## Architecture, policy & release

- [`DESIGN.md`](DESIGN.md) — package layout, the Anthropic adapter, deliberate non-goals
- [`north-star.md`](north-star.md) — **proposed**: north-star goals (`core-agent` runtime + `core-agent` + `core-tui` + `builtin:coder` daily-driver CLI) and open `v3.2+` strategic priorities ([#1388](https://github.com/go-steer/core-agent/issues/1388))
- [`compatibility-policy.md`](compatibility-policy.md) — **shipped**: promised vs. unsupported `pkg/` surface (`dev/api-surface.txt`), the `verify-apidiff` CI gate against the latest GA tag, `Deprecated:` removal-version markers, and the v4.0 removal schedule ([#1368](https://github.com/go-steer/core-agent/issues/1368), [#1373](https://github.com/go-steer/core-agent/issues/1373), [#1398](https://github.com/go-steer/core-agent/issues/1398))
- [`persona-library.md`](persona-library.md) — **shipped**: the first-party builtin personas (`@include builtin:core|coder|sre|propose-change`), the identity → equipment → conduct split, and the incident each rule came from ([#656](https://github.com/go-steer/core-agent/issues/656))
- [`contributor-pitfalls.md`](contributor-pitfalls.md) — mechanics, incident history, and worked examples behind each rule in `AGENTS.md` ([#1384](https://github.com/go-steer/core-agent/issues/1384))
- [`release-process.md`](release-process.md) — dev/GA tag cutting (`cut-dev-tag.sh` / `cut-ga-tag.sh`) + release conventions
- [`changelog-archive/pre-v3.0.md`](changelog-archive/pre-v3.0.md) — archived `CHANGELOG.md` sections for `v0.1.0` through `v2.9.0` ([#1387](https://github.com/go-steer/core-agent/issues/1387))

## Open (`proposed`) feature designs

- [`external-state-design.md`](external-state-design.md) — **proposed**: failover without a shared database: pluggable transcript / control / index / memory stores (SQL, Redis, Agent Engine Sessions + Memory Bank, Kubernetes Lease) and single-writer residency leases
- [`memory-service-design.md`](memory-service-design.md) — **proposed**: stateless agent-specific (`agent`) and shared (`shared:<pool>`) long-term memory with multi-scope `Router` fan-out, Gemini Enterprise Agent Platform Memory Bank, and `--memory-ref` ([#1506](https://github.com/go-steer/core-agent/pull/1506))
- [`bidirectional-mcp-design.md`](bidirectional-mcp-design.md) — **proposed**: `core-agent` exposing itself as an MCP server (agent-as-tool default; tool-palette opt-in)
- [`mcp-oauth-design.md`](mcp-oauth-design.md) — **proposed**: MCP Streamable HTTP transport + OAuth 2.0 client authentication ([#190](https://github.com/go-steer/core-agent/issues/190))
- [`mcp-credential-resolution-design.md`](mcp-credential-resolution-design.md) — **proposed**: per-MCP-server credential resolution (pluggable providers + Auth Manager)
- [`tiered-tool-integration-design.md`](tiered-tool-integration-design.md) — **proposed**: tiered diagnostics (`k8s-sensors` + LLM-authored Go via `kode-gopher`, [#200](https://github.com/go-steer/core-agent/issues/200))
- [`kube-agents-scenarios-design.md`](kube-agents-scenarios-design.md) — **proposed**: the six `kube-agents` demo scenarios checked against `core-agent` + `lookout`/`leeway` + `switchboard`
- [`selfdev-historical-replay-design.md`](selfdev-historical-replay-design.md) — **proposed**: historical replay corpus of past fix/feature tasks scored against behavioural oracles ([#1116](https://github.com/go-steer/core-agent/issues/1116), [#1332](https://github.com/go-steer/core-agent/issues/1332))
- [`transcript-archival-design.md`](transcript-archival-design.md) — **proposed**: attach-mode transcript archival and export
- [`purser/docs/DESIGN.md`](https://github.com/go-steer/purser/blob/main/docs/DESIGN.md) — **proposed** (lives in [go-steer/purser](https://github.com/go-steer/purser)): extracting `pkg/auth` into a shared SPIFFE/mTLS/OIDC module

## Research, friction logs & assessments

- [`library-friction-log.md`](library-friction-log.md) — canonical per-library friction record (ADK Go, Charm, OTel, genai/Vertex, MCP SDK, gorm+sqlite)
- [`agent-runtime-go-friction-log.md`](agent-runtime-go-friction-log.md) — deploying a Go agent to Google Cloud's Agent Engine / Agent Runtime
- [`adk-skills-issue.md`](adk-skills-issue.md) — strict YAML unmarshaling of `SKILL.md` frontmatter vs. Claude Skills interop
- [`compaction.md`](compaction.md) — context-window compaction research (Crush + Antigravity prior art)
- [`coding-agent-instructions.md`](coding-agent-instructions.md) — Crush + Claude Code system-prompt survey
- [`assessments/`](assessments/) — dated whole-project assessments and the milestones they seeded:
  - [`assessments/2026-08-12-tri-team-assessment.md`](assessments/2026-08-12-tri-team-assessment.md) — advocate/critic/arbitrator review of v2.8; seeded v2.9 and v3.0
  - [`assessments/2026-10-27-v3-good-bad-ugly.md`](assessments/2026-10-27-v3-good-bad-ugly.md) — five-lane post-GA review of v3.0.0; seeded v3.1 and v3.2
  - [`assessments/2026-10-27-innovations.md`](assessments/2026-10-27-innovations.md) — what `core-agent` does that peer harnesses don't document

## Archived design docs, plans & smoke records (`docs/archive/`)

### Shipped designs & milestone plans (`Status: shipped`)

- [`archive/acceptance-m1.md`](archive/acceptance-m1.md) — **shipped**: M1 acceptance plan (library + CLI extraction)
- [`archive/acceptance-m2.md`](archive/acceptance-m2.md) — **shipped**: M2 acceptance plan (Anthropic via Vertex AI)
- [`archive/v1-acceptance.md`](archive/v1-acceptance.md) — **shipped**: v1.0 acceptance criteria
- [`archive/agent-card-design.md`](archive/agent-card-design.md) — **shipped**: `/.well-known/agent-card.json` discovery endpoint
- [`archive/agent-content-distribution-design.md`](archive/agent-content-distribution-design.md) — **shipped**: OCI image volume + initContainer content distribution ([#611](https://github.com/go-steer/core-agent/issues/611))
- [`archive/agent-package-split-design.md`](archive/agent-package-split-design.md) — **shipped**: decomposing `pkg/agent` into `pkg/agent/autonomous`, `pkg/agent/background`, and `pkg/attachadapter`
- [`archive/agentic-mcp-design.md`](archive/agentic-mcp-design.md) — **shipped**: transparent agentic wrapping for MCP tool calls
- [`archive/alert-tool-design.md`](archive/alert-tool-design.md) — **shipped**: native `alert` tool (`generic`, `slack`, `discord`, `pagerduty_events_v2`, `switchboard`)
- [`archive/anthropic-prompt-caching-design.md`](archive/anthropic-prompt-caching-design.md) — **shipped**: Anthropic prompt caching ([#714](https://github.com/go-steer/core-agent/issues/714))
- [`archive/attach-mode-design.md`](archive/attach-mode-design.md) — **shipped**: HTTP/SSE + Unix socket remote-attach server
- [`archive/attach-tui-design.md`](archive/attach-tui-design.md) — **shipped**: standalone attach TUI (`cmd/core-agent-tui`)
- [`archive/auto-continue-design.md`](archive/auto-continue-design.md) — **shipped**: auto-continuation of restart-interrupted turns
- [`archive/auto-mode-design.md`](archive/auto-mode-design.md) — **shipped**: permission mode `auto` (`pkg/approver`, [#1175](https://github.com/go-steer/core-agent/issues/1175))
- [`archive/auto-mode-unattended-design.md`](archive/auto-mode-unattended-design.md) — **shipped**: unattended `auto` mode (`permissions.auto.escalation: "refuse"`)
- [`archive/autonomous.md`](archive/autonomous.md), [`archive/autonomous-plan.md`](archive/autonomous-plan.md) — **shipped**: autonomous loop driver (`pkg/agent/autonomous`)
- [`archive/background-subagents-design.md`](archive/background-subagents-design.md) — **shipped**: runtime-spawned background subagents + `RemoteAgentSpawner`
- [`archive/backlog-cost-stack-2026-07-14.md`](archive/backlog-cost-stack-2026-07-14.md) — **shipped**: v2.7 cost-reduction stack
- [`archive/cleanup-execution-plan.md`](archive/cleanup-execution-plan.md) — **shipped**: pre-v2.7 structural cleanup waves
- [`archive/compose-extraction-design.md`](archive/compose-extraction-design.md) — **shipped**: lifting reusable substrate wiring into `pkg/compose`
- [`archive/context-management-design.md`](archive/context-management-design.md) — **shipped**: compaction + `agentic_*` subtasks + task-boundary checkpoints
- [`archive/core-tui-adapter-design.md`](archive/core-tui-adapter-design.md) — **shipped**: `go-steer/core-tui` remote adapter
- [`archive/credential-files-design.md`](archive/credential-files-design.md) — **shipped**: keeping the multi-session bearer table out of agent tools ([#1201](https://github.com/go-steer/core-agent/issues/1201))
- [`archive/declarative-subagents-design.md`](archive/declarative-subagents-design.md) — **shipped**: declarative `subagents[]` and per-subagent content roots ([#599](https://github.com/go-steer/core-agent/issues/599))
- [`archive/digest-design.md`](archive/digest-design.md) — **shipped**: `pkg/digest` structural JSON pruning and `retrieve_raw`
- [`archive/durable-turn-failures-design.md`](archive/durable-turn-failures-design.md) — **shipped**: durable `guardrail-turn-trip` and `turn-error` eventlog rows ([#1258](https://github.com/go-steer/core-agent/issues/1258))
- [`archive/durable-write-gate-design.md`](archive/durable-write-gate-design.md) — **shipped**: `pkg/approval` durable write gate + `internal/effects` recorded-effect outbox ([#1399](https://github.com/go-steer/core-agent/issues/1399))
- [`archive/embedded-tui-design-v2.md`](archive/embedded-tui-design-v2.md) — **shipped**: in-process and spawn-and-attach `core-tui` integration
- [`archive/eventlog-decisions.md`](archive/eventlog-decisions.md), [`archive/eventlog-plan.md`](archive/eventlog-plan.md) — **shipped**: SQLite/Postgres/MySQL durable session backend (`pkg/eventlog`)
- [`archive/external-content-root-design.md`](archive/external-content-root-design.md) — **shipped**: operator-declared external content roots (`content_roots`, [#600](https://github.com/go-steer/core-agent/issues/600))
- [`archive/fetch-url-design.md`](archive/fetch-url-design.md) — **shipped**: `fetch_url` built-in + `url_scope` allow/deny rules
- [`archive/gated-apply-design.md`](archive/gated-apply-design.md) — **shipped**: `gke-platform-agent` two-leg gated apply (`mode: ask` vs `mode: allow`, [#1042](https://github.com/go-steer/core-agent/issues/1042))
- [`archive/gemini-tier1-followup-plan.md`](archive/gemini-tier1-followup-plan.md) — **shipped**: parallelism mandate, tool-description rewrites, `read_many_files`
- [`archive/gitops-vcs-design.md`](archive/gitops-vcs-design.md) — **shipped**: pure-Go `vcs_*` tool suite (`pkg/tools/vcs` over `core-vcs`) and `config.vcs` ([#1490](https://github.com/go-steer/core-agent/issues/1490))
- [`archive/guardrail-reset-design.md`](archive/guardrail-reset-design.md) — **shipped**: operator recovery from a tripped watchdog / cost ceiling (`GET`/`POST /guardrails`, `/guardrail reset`)
- [`archive/hashed-bearer-tokens-design.md`](archive/hashed-bearer-tokens-design.md) — **shipped**: `token_sha256` digests in the multi-session bearer table ([#1213](https://github.com/go-steer/core-agent/issues/1213))
- [`archive/hermes-replacement-design.md`](archive/hermes-replacement-design.md) — **shipped**: `kube-platform-agent` coordinator + specialist architecture ([#589](https://github.com/go-steer/core-agent/issues/589))
- [`archive/instruction-loader-v2-design.md`](archive/instruction-loader-v2-design.md) — **shipped**: `AGENTS.d/*.md` overlay and `@include` composition
- [`archive/k8s-event-agent-design.md`](archive/k8s-event-agent-design.md) — **shipped**: Kubernetes event-driven troubleshooting agent ([#186](https://github.com/go-steer/core-agent/issues/186))
- [`archive/local-listener-auth-design.md`](archive/local-listener-auth-design.md) — **shipped**: loopback/Unix-socket listener authentication and `--attach-token-file` ([#1201](https://github.com/go-steer/core-agent/issues/1201))
- [`archive/m3-followups-decisions.md`](archive/m3-followups-decisions.md), [`archive/m3-followups-plan.md`](archive/m3-followups-plan.md) — **shipped**: M3 follow-up scope
- [`archive/mast-fold-design.md`](archive/mast-fold-design.md) — **shipped**: folding `go-steer/mast` into `core-agent` v3.1 ([#1411](https://github.com/go-steer/core-agent/issues/1411))
- [`archive/metrics-design.md`](archive/metrics-design.md) — **shipped**: OTel MeterProvider + Prometheus `/metrics`
- [`archive/model-selection-design.md`](archive/model-selection-design.md) — **shipped**: task-class model selection + watchdog escalation
- [`archive/model-support-design.md`](archive/model-support-design.md) — **shipped**: multi-provider adapters and profiles via `go-steer/core-models`
- [`archive/multi-session-design.md`](archive/multi-session-design.md) — **shipped**: multi-session daemon, per-caller auth, and session ACLs
- [`archive/operator-input-design.md`](archive/operator-input-design.md) — **shipped**: mid-turn queue panel, auto-continue, `/btw`, `/subagent`
- [`archive/operator-interrupt-design.md`](archive/operator-interrupt-design.md) — **shipped**: operator hold/pause/resume state machine ([#799](https://github.com/go-steer/core-agent/issues/799))
- [`archive/peer-registration-design.md`](archive/peer-registration-design.md) — **shipped**: hub-and-spoke peer registration (`POST /peers`, `GET /peers`)
- [`archive/pkg-reorg-option-1.md`](archive/pkg-reorg-option-1.md) — **shipped**: lifting public packages into `pkg/`
- [`archive/plan-first-design.md`](archive/plan-first-design.md) — **shipped**: gate-level `plan_mode` (`record_plan`) enforcement
- [`archive/pricing-design.md`](archive/pricing-design.md) — **shipped**: layered per-model pricing (now in `go-steer/core-models`)
- [`archive/provider-retry-transcript-design.md`](archive/provider-retry-transcript-design.md) — **shipped**: `provider_retry` transcript stamps ([#1206](https://github.com/go-steer/core-agent/issues/1206))
- [`archive/remote-tui-observer-mode.md`](archive/remote-tui-observer-mode.md) — **shipped**: read-only observer mode for `cmd/core-agent-tui`
- [`archive/retry-bare-invalid-argument-design.md`](archive/retry-bare-invalid-argument-design.md) — **shipped**: single retry on mid-session Vertex bare `400 INVALID_ARGUMENT` ([#1247](https://github.com/go-steer/core-agent/issues/1247))
- [`archive/scheduled-monitoring-design.md`](archive/scheduled-monitoring-design.md) — **shipped**: `schedule_next_turn` autonomous pacing primitive
- [`archive/scheduled-ops-design.md`](archive/scheduled-ops-design.md) — **shipped**: in-process `compose.ScheduledTrigger` + fleet scheduling lease ([#202](https://github.com/go-steer/core-agent/issues/202), [#1405](https://github.com/go-steer/core-agent/issues/1405))
- [`archive/scion-core-agent-architecture.md`](archive/scion-core-agent-architecture.md) — **shipped**: layered architecture for Scion-managed agent runtimes
- [`archive/scion-harness-improvements-design.md`](archive/scion-harness-improvements-design.md) — **shipped**: `Agent.Inject` + `AutonomousHandle` + mid-turn interrupt
- [`archive/scion-research-demo-design.md`](archive/scion-research-demo-design.md) — **shipped**: Scion `RemoteAgentSpawner` reference + parallel-research demo
- [`archive/self-development-design.md`](archive/self-development-design.md) — **shipped**: committed `/.agents/` self-development recipe and skills ([#1116](https://github.com/go-steer/core-agent/issues/1116))
- [`archive/selfdev-soak-design.md`](archive/selfdev-soak-design.md) — **shipped**: unattended `auto`-mode self-development soak on GKE ([#1213](https://github.com/go-steer/core-agent/issues/1213))
- [`archive/selfdev-soak-dispatcher-design.md`](archive/selfdev-soak-dispatcher-design.md) — **shipped**: A7 soak dispatcher (`dev/uat/selfdev-soak/dispatcher`, [#1213](https://github.com/go-steer/core-agent/issues/1213))
- [`archive/session-resume-design.md`](archive/session-resume-design.md) — **shipped**: transparent lazy session resume on daemon restart ([#178](https://github.com/go-steer/core-agent/issues/178))
- [`archive/subagent-return-contract-design.md`](archive/subagent-return-contract-design.md) — **shipped**: `return_result` / `finish_task`, bounded subagent termination, typed stop reasons ([#727](https://github.com/go-steer/core-agent/issues/727))
- [`archive/system-prompt-layering-design.md`](archive/system-prompt-layering-design.md) — **shipped**: layered system-prompt assembly ([#459](https://github.com/go-steer/core-agent/issues/459))
- [`archive/tools-plan.md`](archive/tools-plan.md) — **shipped**: built-in file, search (`glob`/`grep`), shell, and todo tool suite
- [`archive/unified-subagent-invocation-design.md`](archive/unified-subagent-invocation-design.md) — **shipped**: unified sync tool + `spawn_agent` by-reference invocation
- [`archive/vertex-context-caching-design.md`](archive/vertex-context-caching-design.md) — **shipped**: Vertex explicit `CachedContent` prefix caching
- [`archive/wait-and-verify-design.md`](archive/wait-and-verify-design.md) — **shipped**: `wait_and_verify` read-only poll-until-condition tool ([#648](https://github.com/go-steer/core-agent/issues/648))

### Historical smoke records (`Status: shipped`)

- [`archive/compaction-smoke-2026-05-27.md`](archive/compaction-smoke-2026-05-27.md) — **shipped**: compaction (Mechanism A) smoke sweep
- [`archive/core-tui-smoke-2026-05-26.md`](archive/core-tui-smoke-2026-05-26.md) — **shipped**: `core-tui` adapter smoke sweep
- [`archive/operator-hold-smoke-2026-08-31.md`](archive/operator-hold-smoke-2026-08-31.md) — **shipped**: operator hold + `/btw` sweep ([#799](https://github.com/go-steer/core-agent/issues/799))
- [`archive/remote-tui-smoketest.md`](archive/remote-tui-smoketest.md), [`archive/remote-tui-smoketest-v2.2.md`](archive/remote-tui-smoketest-v2.2.md) — **shipped**: remote-TUI smoke sweeps
- [`archive/subtasks-checkpoints-smoke-2026-05-27.md`](archive/subtasks-checkpoints-smoke-2026-05-27.md) — **shipped**: subtasks (B) + checkpoints (C) smoke sweep
- [`archive/v2.3-smoketest.md`](archive/v2.3-smoketest.md) — **shipped**: v2.3 smoke sweep

### Superseded or abandoned designs (`Status: superseded | abandoned`)

- [`archive/ax-integration-audit.md`](archive/ax-integration-audit.md) — **superseded**: replaced by the v3.1 AX task-runner deployment (`archive/mast-fold-design.md`)
- [`archive/code-mode-design.md`](archive/code-mode-design.md) — **superseded**: Code Mode shipped out of tree as [gke-demos/kode-gopher](https://github.com/gke-demos/kode-gopher)
- [`archive/cogo-core-agent-integration.md`](archive/cogo-core-agent-integration.md) — **abandoned**: Goal 2 runs directly on `core-agent` + `core-tui` + `builtin:coder` (`north-star.md`)
- [`archive/cogo-flip-readiness-audit.md`](archive/cogo-flip-readiness-audit.md) — **abandoned**: `go-steer/cogo` was not flipped (`north-star.md`)
- [`archive/docsy-migration-notes.md`](archive/docsy-migration-notes.md) — **superseded**: site migrated from Hugo/Docsy to Astro Starlight
- [`archive/embedded-tui-design.md`](archive/embedded-tui-design.md) — **superseded**: replaced by `archive/embedded-tui-design-v2.md`
- [`archive/kube-agents-platform-fit.md`](archive/kube-agents-platform-fit.md) — **superseded**: replaced by `archive/hermes-replacement-design.md` and `archive/mast-fold-design.md`
- [`archive/shared-memory-design.md`](archive/shared-memory-design.md) — **superseded**: replaced by `memory-service-design.md` ([#1506](https://github.com/go-steer/core-agent/pull/1506))
- [`archive/subagents-plan.md`](archive/subagents-plan.md) — **superseded**: replaced by `archive/declarative-subagents-design.md` and `archive/background-subagents-design.md`

## Published site

[`site/`](site/) — [Astro Starlight](https://starlight.astro.build)
source for <https://go-steer.github.io/core-agent/>. See the root
[`README.md`](../README.md) for preview instructions.
