# North-star goals + readiness assessment

Captured 2026-05-26 as a strategic anchor. Goes stale fast — re-read
quarterly and prune.

> **Status update (2026-07-25).** The "v2.0 blockers" and "critical
> path" sections below are the mid-2026 pre-v2.0 snapshot and are now
> largely **historical**: v2.0 shipped, and **v2.7.0 GA shipped
> 2026-07-20** (first `go install`-able `/v2` release — E2E OTel, full
> cost stack, multi-daemon fleet). Main is on `v2.8.0-dev`. The
> context-management quartet (compaction, micro-subagents,
> task-boundary checkpoints, persistent memory) that this doc lists as
> "designed, not built" has since landed. Treat the two goals and the
> "risks worth naming" section as the still-current strategic anchor;
> treat the dated milestone/critical-path lists as a record of how we
> got here, not a live plan. A full rewrite of the readiness assessment
> is due at the next quarterly prune.

## The two goals

1. **core-agent should be one of the best autonomous agent runtimes
   out there, and super cost efficient.** Substrate-level. Library
   consumers (cogo, scion-adapter, ax-adapter, future others) get
   a fast, durable, multi-provider agent loop with first-class
   subagents, autonomous, and budgets.

2. **core-agent + core-tui + cogo-specific tools should be a
   first-class agentic coding CLI** — good enough that the project
   owner uses it as their daily driver instead of Claude Code and
   Antigravity. Consumer-level.

These are not in tension. (1) is the substrate; (2) is the
flagship consumer that validates (1) is actually good.

## Honest readiness assessment

### Substrate ("best agent runtime")

**Done:**

- ADK-backed agent loop with streaming + multi-turn session
  history (`agent.Agent`, `agent.Run`).
- Autonomous loop with token/cost/turn/wallclock budgets
  (`agent.RunAutonomous`).
- Background subagents with branch isolation, alert channel,
  spawn / list / check / report tools
  (`BackgroundAgentManager`, `/subagent` slash).
- Multi-provider (Anthropic, Vertex, Gemini, mock) via
  `models.Provider` + per-call provider resolution.
- Durable session + crash-resume via `eventlog` (SQLite +
  Postgres + in-memory).
- Permissions gate with all four chip modes (ask, accept-edits,
  plan, yolo) + per-tool / per-verb / always-allow persistence.
- MCP servers (stdio + HTTP) with bidirectional elicitation.
- Skills loader.
- Attach mode (HTTP/SSE) for remote drive.
- Layered pricing with daily LiteLLM refresh + per-turn cost
  attribution.
- Inbox + auto-continue + system-note framing (PR α).
- `/btw` parallel side queries (PR β).
- `/subagent` direct spawn slash (PR γ).

**Designed, not built as of the 2026-05 snapshot** (statuses
re-verified 2026-07-29 against v2.8.0-dev — see annotations):

- Context management quartet — compaction, micro-subagents,
  task-boundary checkpoints, persistent memory
  ([`context-management-design.md`](context-management-design.md)).
  **SHIPPED** (see the 2026-07-25 status note above).
- Agent-hooks public extension surface. **SHIPPED 2026-06 in #298**
  (`pkg/hooks`: config-driven command hooks on four events with a
  Scion-compatible JSON envelope; execution gate-hardened in #378)
  — the implementation landed directly, so the once-planned
  `agent-hooks-design.md` was never written and never will be; the
  living spec is the site's [Hooks concepts page](site/src/content/docs/concepts/hooks.md)
  plus `pkg/hooks` itself. In-process Go-level hooks for library
  embedders also exist: the exported `agent.WithEventHook`
  (single-slot onEvent/onTurnEnd callbacks) — deliberately minimal;
  anything richer is YAGNI until a consumer asks.
- Shared-memory in-tree FTS5-over-eventlog backend
  ([`shared-memory-design.md`](shared-memory-design.md)) — still
  design-only; the genuinely open item on this list.
- core-tui as default TUI. **SHIPPED** (default since v2.0).

**Cost-efficiency specifically:** unusually well-positioned.
Micro-subagents on cheap models is *the* cost lever; combined
with checkpoint compaction keeping prompt-cache hit rates high,
this routinely beats single-model-everywhere by 5–10× on long
sessions. We already track per-turn cost cleanly; CC and
Antigravity both hide that. The combination of "frontier model
for parent reasoning + haiku/flash-tier for tool digesting +
auto-compaction keeping cache warm" is something we'd be the
cleanest exposer of on the market.

### Coding CLI ("daily driver replacement")

**Closing the gap with CC / Antigravity:**

- Slash command parity: `/help`, `/clear`, `/quit`, `/memory`,
  `/stats`, `/model`, `/mcp`, `/skills`, `/tools`, `/interrupt`,
  `/pricing`, `/reload`, `/mouse`, `/permissions`, `/allow`,
  `/deny`, `/btw`, `/subagent` already shipped.
- Coming: `/compact`, `/done`, `/recall`, `/remember`, `/wake`,
  `/theme`, `/transcript`.
- Permissions UX: matches or exceeds CC's (chip switcher, per-
  verb / per-tool allow, always-persist).
- MCP support: matches CC.
- Skills: matches CC's published format.
- Multi-provider: beats CC (Anthropic-locked).
- Self-hostable / open-source: beats CC and Antigravity.

**Real remaining gaps (named honestly):**

1. **IDE integration.** Both CC and Antigravity have rich editor
   sidecars; we have a terminal. The TUI is great but if the
   workflow is "edit in VS Code, ask agent in sidebar," we're
   v3-away from parity. LSP-style sidecar is the answer.
2. **Onboarding polish.** CC's setup flow is unusually smooth.
   We're closer than people think but the first-launch experience
   needs a sweep before v2.0.
3. **Polished web fetch + research workflow.** AgenticFetchURL +
   AgenticResearch (in PR II of context-management) close most
   of this — but they have to actually ship.
4. **Memory-grounded "remember what we decided last week"** —
   shipping with D.

### Risks worth naming

1. **Feature focus discipline.** The design docs are sprawling
   and it would be easy to keep designing instead of shipping.
   Heuristic (as resolved in practice: agent-hooks shipped as
   code in #298 with no separate design doc): execute through
   context-management trilogy + memory + cogo flip + v2.0
   release before the next new design doc.
2. **Cogo flip drift.** Every week we don't flip cogo to
   consume core-agent, the two codebases diverge more. The
   flip should land soon after v2.0 — not "eventually."
3. **Bus factor.** Most of the design + execution is one
   person right now. Documentation discipline (which is good)
   partially mitigates; the design docs ARE the spec.

## Critical path (mid-2026 window)

Sequenced so each step unblocks the next.

**v2.0 blockers (must land before the v2.0 cut):**

1. **Land `feat/core-tui-adapter`** — DONE 2026-05-26 (merged).
2. **Wait on core-tui v0.6.0** to close issues
   [#7](https://github.com/go-steer/core-tui/issues/7),
   [#8](https://github.com/go-steer/core-tui/issues/8),
   [#9](https://github.com/go-steer/core-tui/issues/9),
   [#10](https://github.com/go-steer/core-tui/issues/10).
   On bump, flip `MidTurnInjectionMode: QueueForNext` →
   `AutoContinueFromInbox` per the memory note.
3. **Ship context-management PRs I–III** — compaction +
   micro-subagents + task-boundary checkpoints
   (tasks #91/#92/#93). This is the v2.0 blocker the user
   called out 2026-05-27 — shipping v2.0 without compaction
   means every consumer hits the context wall in long
   sessions, which is parity-failure vs Claude Code +
   Antigravity. ~1200–1700 LoC across three PRs.
4. **Smoke sweep on the merged core-tui v0.6.0 + context-
   management trilogy** — walk the now-bigger acceptance
   checklist; capture any new gaps.
5. **Flip launcher default** `launchTUI` → `launchTUIv2` in
   `cmd/core-agent/main.go` (one-line change, keep an
   escape-hatch env var for one release).
6. **Site docs sweep** — `docs/site/content/docs/*` mostly
   still describes `internal/tui` as the default.
7. **Cut v2.0.**

**v2.1 (close-on-heels, not blocking v2.0):**

8. **Ship context-management PRs IV–VI** — memory tools on
   shared-memory backend + optional pre-turn recall +
   coordination polish (tasks #94/#95/#96). Depends on the
   shared-memory in-tree backend landing in parallel.
9. **Flip cogo** to consume core-agent as a library (see
   [`cogo-flip-readiness-audit.md`](cogo-flip-readiness-audit.md)).
   Delete cogo's duplicate internals; register 6 Go tools +
   coder-instruction overrides.
10. **Ship the agent-hooks public surface** — DONE (shipped
    implementation-first as `pkg/hooks` in #298, gate-hardened in
    #378; no separate design doc was written).
11. **Polish pass for v2.x release** — onboarding sweep, README,
    site docs, goreleaser matrix, slim build verification, attach
    mode docs.

After that completes:

- We're in the conversation with CC + Antigravity on every axis
  except IDE integration.
- Cogo is the daily-driver coding CLI for the project owner.
- v3 work begins: IDE sidecar (LSP-style), multi-tab TUI,
  hosted-mode option (if there's demand).

## Verdict

**Both goals are doable, gated on execution not design.** The
hard substrate work is largely done; what remains is shipping
the planned PRs from the existing design docs without inventing
new ones. Mid-2026 is realistic for v2.0 + cogo flip + daily-
driver readiness. IDE-parity is v3.

The single biggest risk to both goals is **design-doc drift** —
spending the next month writing six more design docs instead of
shipping the ten PRs already designed. The discipline call (since
resolved: agent-hooks shipped as code, context-management landed):
no new design docs until the already-designed PRs are merged.
