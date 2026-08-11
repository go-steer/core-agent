# core-agent design docs

Internal design notes — the *why* behind the code. The published
[Astro Starlight](https://starlight.astro.build) site lives in
[`site/`](site/); this directory is reference material for
contributors.

[`DESIGN.md`](DESIGN.md) is the architectural entry point. Everything
else is per-feature reasoning, per-milestone acceptance criteria,
research + friction logs, or handover notes.

> **Registry policy.** Per [`AGENTS.md`](../AGENTS.md), every design
> doc gets registered here so it stays discoverable. New
> `docs/<feature>-design.md` files must add a bullet to the relevant
> section below in the same PR.

## Architectural + acceptance criteria

- [`DESIGN.md`](DESIGN.md) — package layout, the Anthropic adapter, deliberate non-goals
- [`agent-package-split-design.md`](agent-package-split-design.md) — decomposing the `pkg/agent` god package (autonomous driver / background manager / attach adapter) behind a narrow seam before the v2 surface freezes
- [`compose-extraction-design.md`](compose-extraction-design.md) — lifting reusable substrate wiring (multi-session construction, grant persistence, agentic/digest assembly) out of `cmd/core-agent` into `pkg/compose` so cogo/scion/ax stop re-implementing it
- [`north-star.md`](north-star.md) — north-star goals + readiness assessment
- [`v1-acceptance.md`](v1-acceptance.md) — v1.0 acceptance criteria
- [`acceptance-m1.md`](acceptance-m1.md) — M1 acceptance plan (library + CLI extraction)
- [`acceptance-m2.md`](acceptance-m2.md) — M2 acceptance plan (Anthropic via Vertex AI)

## Feature designs

### Autonomy, subagents + durable sessions (M3 → v1.x)

- [`autonomous.md`](autonomous.md), [`autonomous-plan.md`](autonomous-plan.md) — `RunAutonomous` driver
- [`eventlog-decisions.md`](eventlog-decisions.md), [`eventlog-plan.md`](eventlog-plan.md) — durable session backend + audit log
- [`subagents-plan.md`](subagents-plan.md) — in-process subagents (`WithSubagents`)
- [`tools-plan.md`](tools-plan.md) — built-in tool suite (glob + grep)
- [`m3-followups-decisions.md`](m3-followups-decisions.md), [`m3-followups-plan.md`](m3-followups-plan.md) — M3 follow-up scope
- [`background-subagents-design.md`](background-subagents-design.md) — runtime-spawned background subagents (in-process) + `RemoteAgentSpawner` seam
- [`scion-harness-improvements-design.md`](scion-harness-improvements-design.md) — `Agent.Inject` + `AutonomousHandle` + mid-turn REPL interrupt
- [`gemini-tier1-followup-plan.md`](gemini-tier1-followup-plan.md) — parallelism mandate, tool-description rewrites, `read_many_files`
- [`scion-research-demo-design.md`](scion-research-demo-design.md) — Scion `RemoteAgentSpawner` reference + parallel-research demo

### Sessions, durability + multi-tenancy

- [`multi-session-design.md`](multi-session-design.md) — one daemon, many sessions: per-user auth + cross-session isolation + ACLs
- [`session-resume-design.md`](session-resume-design.md) — transparent session resume on daemon restart
- [`auto-continue-design.md`](auto-continue-design.md) — auto-continuation of restart-interrupted turns, on by default for daemons since #559 (detection from eventlog tails, crash-loop breaker)
- [`shared-memory-design.md`](shared-memory-design.md) — `Memory` interface + FTS5-over-eventlog in-tree + audit-derived recall + Redis AMS extras adapter

### Context, cost + model management

- [`context-management-design.md`](context-management-design.md) — compaction + micro-subagents + checkpoints + memory
- [`pricing-design.md`](pricing-design.md) — extensible, current, honest per-model pricing
- [`model-selection-design.md`](model-selection-design.md) — task-class model selection: operator hint + watchdog escalation
- [`vertex-context-caching-design.md`](vertex-context-caching-design.md) — eager system-prompt context cache on Vertex
- [`digest-design.md`](digest-design.md) — `pkg/digest` local digesting primitives
- [`backlog-cost-stack-2026-07-14.md`](backlog-cost-stack-2026-07-14.md) — post-v2.7.0-dev.2 cost-reduction plan

### MCP, tools + code mode

- [`fetch-url-design.md`](fetch-url-design.md) — `fetch_url` built-in (HTTP GET, no JS, no POST) + `URLScopeConfig` allow/deny grammar
- [`agentic-mcp-design.md`](agentic-mcp-design.md) — transparent agentic wrapping for MCP tool calls
- [`bidirectional-mcp-design.md`](bidirectional-mcp-design.md) — core-agent exposes itself as an MCP server (agent-as-tool default; tool-palette opt-in)
- [`mcp-oauth-design.md`](mcp-oauth-design.md) — MCP Streamable HTTP transport + OAuth 2.0 client authentication
- [`mcp-credential-resolution-design.md`](mcp-credential-resolution-design.md) — per-MCP-server credential resolution: pluggable providers + Auth Manager
- [`code-mode-design.md`](code-mode-design.md) — Phase 1 in-process Go execution via Yaegi; project-symbol injection as the differentiator
- [`tiered-tool-integration-design.md`](tiered-tool-integration-design.md) — tiered diagnostics: a small set of pre-built sensors (`k8s-sensors`) + long-tail LLM-authored Go via [kode-gopher](https://github.com/gke-demos/kode-gopher) (proposed, [#200](https://github.com/go-steer/core-agent/issues/200))

### Instructions, skills + discovery

- [`instruction-loader-v2-design.md`](instruction-loader-v2-design.md) — composition + multi-file system instructions (`AGENTS.d/*.md`, `@include`)
- [`agent-card-design.md`](agent-card-design.md) — `/.well-known/agent-card.json` for agent discovery

### Observability, safety + scheduling

- [`metrics-design.md`](metrics-design.md) — OTel MeterProvider (primary) + Prometheus scrape (secondary)
- [`alert-tool-design.md`](alert-tool-design.md) — native `alert` tool for headless escalation
- [`plan-first-design.md`](plan-first-design.md) — gate-level "plan before action" enforcement
- [`scheduled-monitoring-design.md`](scheduled-monitoring-design.md) — `Scheduler` primitive for paced autonomous loops; combines with `BackgroundAgentManager` for the K8s fleet-monitor topology
- [`scheduled-ops-design.md`](scheduled-ops-design.md) — `core-agent-cron` companion sidecar firing scheduled prompts into the daemon for proactive autonomous ops (compliance sweeps, drift detection, capacity forecasts) (proposed, [#202](https://github.com/go-steer/core-agent/issues/202))

### Attach mode, remote + TUI

- [`attach-mode-design.md`](attach-mode-design.md) — HTTP/SSE + Unix socket; mTLS + bearer; `POST /inject` for live observability of headless agents
- [`attach-tui-design.md`](attach-tui-design.md) — bubble-tea TUI consumer for attach-mode (`cmd/core-agent-tui/`)
- [`core-tui-adapter-design.md`](core-tui-adapter-design.md) — adapter onto `go-steer/core-tui` for the remote TUI client
- [`operator-input-design.md`](operator-input-design.md) — operator input during turns: queue panel, auto-continue, `/btw`, `/subagent`
- [`remote-tui-observer-mode.md`](remote-tui-observer-mode.md) — read-only observer mode for the remote TUI (PR E, v2.2 target)
- [`embedded-tui-design-v2.md`](embedded-tui-design-v2.md) — **current** embedded-TUI design: `core-agent-tui --local` spawn-and-attach (single TUI codebase serves local + remote); the code cites this doc
- [`embedded-tui-design.md`](embedded-tui-design.md) — *superseded* by `embedded-tui-design-v2.md`; kept for history
- [`peer-registration-design.md`](peer-registration-design.md) — hub-and-spoke peer discovery (`POST /peers` / heartbeat / `GET /peers`) for multi-agent K8s deployments

### Kubernetes + platform integrations

- [`k8s-event-agent-design.md`](k8s-event-agent-design.md) — K8s-event-driven troubleshooting agent (watcher source now lives in [go-steer/k8s-lookout](https://github.com/go-steer/k8s-lookout))
- [`kube-agents-platform-fit.md`](kube-agents-platform-fit.md) — running `core-agent` as the `kube-agents` platform agent
- [`scion-core-agent-architecture.md`](scion-core-agent-architecture.md) — layered architecture for Scion-managed agent runtimes
- [`ax-integration-audit.md`](ax-integration-audit.md) — gap audit for `extras/ax-agent/`; don't build a parallel coordinator

## Research + friction logs

- [`library-friction-log.md`](library-friction-log.md) — canonical per-library friction record (ADK Go, Charm, OTel, genai/Vertex, MCP SDK, gorm+sqlite, …)
- [`agent-runtime-go-friction-log.md`](agent-runtime-go-friction-log.md) — deploying a Go agent to Google Cloud's Agent Engine / Agent Runtime
- [`adk-skills-issue.md`](adk-skills-issue.md) — strict YAML unmarshaling of `SKILL.md` frontmatter vs. Claude Skills interop
- [`compaction.md`](compaction.md) — context-window compaction research (Crush + Antigravity prior art)
- [`coding-agent-instructions.md`](coding-agent-instructions.md) — Crush system-prompt + instructions reference

## Audits, strategy + handover notes

- [`cogo-core-agent-integration.md`](cogo-core-agent-integration.md) — cogo + core-agent integration strategy (Option C, sequenced through A)
- [`cogo-flip-readiness-audit.md`](cogo-flip-readiness-audit.md) — cogo → core-agent flip readiness audit (2026-05-26)
- [`pkg-reorg-option-1.md`](pkg-reorg-option-1.md) — lifting public packages into `pkg/`
- [`docsy-migration-notes.md`](docsy-migration-notes.md) — historical: lessons from an earlier Hugo/Docsy site (the site has since moved to Astro Starlight)

## Smoke tests + UAT records

- [`compaction-smoke-2026-05-27.md`](compaction-smoke-2026-05-27.md) — compaction (Mechanism A) smoke sweep
- [`subtasks-checkpoints-smoke-2026-05-27.md`](subtasks-checkpoints-smoke-2026-05-27.md) — subtasks (B) + checkpoints (C) smoke sweep
- [`core-tui-smoke-2026-05-26.md`](core-tui-smoke-2026-05-26.md) — core-tui adapter smoke sweep
- [`remote-tui-smoketest.md`](remote-tui-smoketest.md), [`remote-tui-smoketest-v2.2.md`](remote-tui-smoketest-v2.2.md) — remote-TUI smoke tests
- [`v2.3-smoketest.md`](v2.3-smoketest.md) — v2.3 smoke test

## Release

- [`release-process.md`](release-process.md) — dev/GA tag cutting (`cut-dev-tag.sh` / `cut-ga-tag.sh`) + release conventions

## Published site

[`site/`](site/) — [Astro Starlight](https://starlight.astro.build)
source for <https://go-steer.github.io/core-agent/>. See the root
[`README.md`](../README.md) for preview instructions.
