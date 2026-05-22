# core-agent design docs

Internal design notes — the *why* behind the code. The published Hugo
site lives in [`site/`](site/); this directory is reference material
for contributors.

[`DESIGN.md`](DESIGN.md) is the architectural entry point. Everything
else is per-feature reasoning, per-milestone acceptance criteria, or
handover notes.

## Architectural + acceptance criteria

- [`DESIGN.md`](DESIGN.md) — package layout, the Anthropic adapter, deliberate non-goals
- [`v1-acceptance.md`](v1-acceptance.md) — v1.0 acceptance criteria
- [`acceptance-m1.md`](acceptance-m1.md) — M1 acceptance plan (library + CLI extraction)
- [`acceptance-m2.md`](acceptance-m2.md) — M2 acceptance plan (Anthropic via Vertex AI)

## Feature designs (chronological)

### M3 — autonomy + durable sessions + subagents

- [`autonomous.md`](autonomous.md), [`autonomous-plan.md`](autonomous-plan.md) — `RunAutonomous` driver
- [`eventlog-decisions.md`](eventlog-decisions.md), [`eventlog-plan.md`](eventlog-plan.md) — durable session backend + audit log
- [`subagents-plan.md`](subagents-plan.md) — in-process subagents (`WithSubagents`)
- [`tools-plan.md`](tools-plan.md) — built-in tool suite
- [`m3-followups-decisions.md`](m3-followups-decisions.md), [`m3-followups-plan.md`](m3-followups-plan.md) — M3 follow-up scope

### v1.2.0 — dynamic + remote background subagents

- [`background-subagents-design.md`](background-subagents-design.md) — runtime-spawned background subagents (in-process) + `RemoteAgentSpawner` seam

### v1.3.0 — interrupt machinery

- [`scion-harness-improvements-design.md`](scion-harness-improvements-design.md) — `Agent.Inject` + `AutonomousHandle` + mid-turn REPL interrupt

### v1.4.0 — Gemini tool-calling + Scion reference spawner

- [`gemini-tier1-followup-plan.md`](gemini-tier1-followup-plan.md) — parallelism mandate, tool-description rewrites, `read_many_files` (shipped)
- [`scion-research-demo-design.md`](scion-research-demo-design.md) — Scion `RemoteAgentSpawner` reference + parallel-research demo

### M4+ killer-feature short list (2026-05-19 brainstorm)

- [`attach-mode-design.md`](attach-mode-design.md) — HTTP/SSE + Unix socket; mTLS + bearer; `POST /inject` for live observability of headless agents
- [`bidirectional-mcp-design.md`](bidirectional-mcp-design.md) — core-agent exposes itself as an MCP server (agent-as-tool default; tool-palette opt-in)
- [`code-mode-design.md`](code-mode-design.md) — Phase 1 in-process Go execution via Yaegi; project-symbol-injection as the differentiator
- [`ax-integration-audit.md`](ax-integration-audit.md) — gap audit for `extras/ax-agent/` on the `axplore` branch; don't build a parallel coordinator
- [`scheduled-monitoring-design.md`](scheduled-monitoring-design.md) — `Scheduler` primitive for paced autonomous loops; combines with `BackgroundAgentManager` for the canonical K8s fleet-monitor topology
- [`peer-registration-design.md`](peer-registration-design.md) — fast-follow-on PR after attach-mode; hub-and-spoke peer discovery (`POST /peers` / heartbeat / `GET /peers`) for multi-agent K8s deployments
- [`fetch-url-design.md`](fetch-url-design.md) — `fetch_url` built-in (HTTP GET, no JS, no POST) + `URLScopeConfig` allow/deny grammar mirroring `PathScopeConfig`; agent-level egress control without an outer sandbox

## Cross-cutting handover notes

- [`cogo-core-agent-integration.md`](cogo-core-agent-integration.md) — strategy for cogo + core-agent (recommendation: Option C, sequenced through A)
- [`docsy-migration-notes.md`](docsy-migration-notes.md) — lessons migrating the Hugo site from Hextra to Docsy v0.15.0+

## Published site

[`site/`](site/) — Hugo source for <https://go-steer.github.io/core-agent/>. See the root [`README.md`](../README.md) for preview instructions.
