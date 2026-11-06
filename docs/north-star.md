# North-star goals + readiness assessment

**Status:** proposed (strategic anchor; refreshed 2026-11-06 for `v3.2.0-dev`, [#1388](https://github.com/go-steer/core-agent/issues/1388))

## The two goals

1. **`core-agent` is a durable, cost-efficient autonomous Go agent runtime.**
   Substrate-level. Library and deployment consumers (`pkg/compose`, Scion
   harnesses under `extras/scion`, Kubernetes platform/troubleshooting agents,
   and AX task-runner deployments) get a multi-provider ADK v2 loop
   (`agent.Agent`, `agent.Run`) with bounded autonomous execution
   (`autonomous.Run`, `autonomous.Resume`), in-process and remote A2A v0.3
   subagents (`background.Manager`, `agent.WithSubagents`), durable write-gate
   approvals and recorded-effect outboxes (`pkg/approval`), SQL session and
   spend ledgers (`pkg/eventlog`), and pure-Go forge-neutral GitOps (`pkg/tools/vcs`).

2. **`core-agent` + `core-tui` + `builtin:coder` is the first-party agentic coding and operations CLI.**
   Consumer-level. Rather than routing the coding CLI through `go-steer/cogo`
   (the May 2026 ADK v1.2.0 prototype from which `core-agent` was extracted,
   which has no `core-agent` dependency and is now historical), Goal 2 runs
   **directly on `core-agent`**: the embedded Bubble Tea TUI (`cmd/core-agent`)
   and remote attach client (`cmd/core-agent-tui`) paired with `@include builtin:coder`
   (or `builtin:sre` / project `.agents/` recipes such as this repository's own
   self-development recipe).

These two goals reinforce each other: (1) is the runtime substrate; (2) is the
flagship interactive and self-driving consumer that exercises (1) on real
software engineering and cluster operations every day.

## Where `v3.1.0` GA stands

### Shipped foundation (in `docs/archive/`)

- **Core loop & autonomy:** `agent.Agent`, `autonomous.Run` / `autonomous.Resume`,
  `runner.WakeLoop`, `compose.ScheduledTrigger` with single-fleet scheduling
  leases, auto-continuation of interrupted turns, and per-turn / per-session
  token, cost, step, and wall-clock budgets.
- **Subagents:** declarative subagents (`subagents[]` and `.agents/subagents/*.md`)
  with `chat`, `task`, and `single_turn` modes, enforced `output_schema`,
  capability tiers (`small`, `mid`, `frontier`), `read_only` vs `change_executor`
  capability splits, in-process background spawning (`background.Manager`), and
  bidirectional A2A v0.3 JSON-RPC 2.0 delegation.
- **Safety, permissions & durability:** `permissions.Gate` (`ask`, `auto`,
  `acceptEdits`, `plan`, `allow`, `yolo`), bash safety profiles, control-plane
  and credential-file write guards, `pkg/approval` durable `adk_request_confirmation`
  write gate + recorded-effect replay outbox, and SQLite/Postgres/MySQL
  `pkg/eventlog`.
- **Context & cost efficiency:** post-turn compaction (~85% utilization +
  mechanical fallback), `agentic_*` subtask wrappers, `pkg/digest` structural
  JSON pruning with `retrieve_raw`, Vertex explicit context caching, Anthropic
  prompt caching, and `go-steer/core-models` layered pricing.
- **Distroless GitOps:** pure-Go `vcs_*` tools (`pkg/tools/vcs` over `core-vcs`)
  and `builtin:propose-change` for opening reviewable PRs/MRs from container
  images that ship neither `git` nor `gh`.

## Open strategic priorities (`v3.2+`)

1. **Multi-tenant & parallel-subagent isolation.**
   - Non-admin permission-mode ceiling (`attach.multi_session.max_mode`, [#1353](https://github.com/go-steer/core-agent/issues/1353))
     and honest documentation of shared-cwd tenancy boundaries.
   - Per-subagent `git worktree` isolation for concurrent write-capable
     background subagents ([#653](https://github.com/go-steer/core-agent/issues/653)).
   - Optional OS-level workspace sandboxing (Seatbelt on macOS, Landlock/bwrap
     on Linux) for `bash` and MCP stdio children ([#651](https://github.com/go-steer/core-agent/issues/651)).

2. **External state & shared memory.**
   - Pluggable transcript, control, index, and memory stores with single-writer
     residency leases for multi-replica failover without a shared SQL database
     ([`external-state-design.md`](external-state-design.md)).
   - In-tree FTS5-over-eventlog recall and shared memory
     ([`shared-memory-design.md`](shared-memory-design.md)).

3. **Reproducible self-development & historical replay.**
   - Unattended `auto`-mode soak bundle archival and human-countersigned
     scorecards ([#1213](https://github.com/go-steer/core-agent/issues/1213)).
   - Historical replay corpus of past fix/feature tasks scored against
     behavioural oracles ([`selfdev-historical-replay-design.md`](selfdev-historical-replay-design.md),
     [#1116](https://github.com/go-steer/core-agent/issues/1116)).

4. **Daily-driver CLI ergonomics & editor integration.**
   - Per-project default sessions with `--resume` / `--continue` ([#1372](https://github.com/go-steer/core-agent/issues/1372)),
     `--output-format json` and clean piped stdout ([#1389](https://github.com/go-steer/core-agent/issues/1389)),
     and `--permission-mode` + grouped `--help` ([#1378](https://github.com/go-steer/core-agent/issues/1378)).
   - Editor / IDE sidecar integration over the attach HTTP/SSE or MCP
     bidirectional surface ([`bidirectional-mcp-design.md`](bidirectional-mcp-design.md)).
