# Running `core-agent` as the `kube-agents` Platform Agent

Real-world readiness assessment: can `core-agent` replace Hermes as
the long-running **Platform Agent** in the
[`kube-agents`](https://github.com/gke-labs/kube-agents) Kubernetes
Agentic Harness?

**Status:** investigative (2026-06-03). Not a commitment to ship;
this captures the architectural mapping + gap list so we can decide
whether to pursue.

**TL;DR:** 80% reusable today via the v2 multi-file instruction
loader (PR #98) and the existing peer-registration primitives
(`pkg/attach/peers.go` + `--attach-register-to`). Remaining 20% is
~280 LoC of optional framework work + one example recipe, OR zero
LoC if we accept that some pieces live in application-side code
(which is where Hermes has them anyway).

## Motivation

We've shipped a substantial v2.X feature set (multi-file
instruction loader, plan-first gating, embedded TUI, remote attach
observer mode, BackgroundAgentManager + peer registry). It's time
to validate against a real distributed-agent workload rather than
unit tests and toy examples.

`kube-agents` is the right fit:
- **Open-source, Apache-2** — we can mirror the layout in
  `examples/` or co-deploy without licensing friction.
- **Distributed multi-agent** — three roles (platform, operator,
  devteam) deployed as pods across GKE clusters; exercises remote
  peer coordination, not just in-process subagents.
- **Long-running** — platform agents live for days or weeks
  managing fleet state, not one-shot LLM calls.
- **Agent-friendly documentation IS the project** — 95% markdown
  (`SOUL.md`, `SKILL.md`, governance SOPs); only 5% Python glue.
  This is exactly the cross-runtime portability model the v2
  instruction loader was designed to enable. Validating against
  `kube-agents` proves the thesis.

## The Platform Agent's role

From `kube-agents/agents/platform/SOUL.md` and `README.md`:

- **Master custodian + agent architect.** Manages GKE fleet
  lifecycle, multi-tenancy boundaries, RBAC, network policies.
- **Strictly GitOps-PR-driven.** Zero direct cluster mutations;
  every change goes through a PR (enforced in prose, not gated).
- **Dynamic subagent provisioning.** When a new cluster is
  registered, provisions an Operator Agent pod INTO that cluster.
  When a new namespace is registered, provisions a DevTeam Agent
  pod into the namespace.
- **Query delegation.** Routes cluster-related queries to the
  matching operator (`@operator-<cluster>-<location>`); namespace
  queries to the matching devteam (`@devteam-<cluster>-<location>-<ns>`).
- **Scheduled cron tasks.** Health patrols, CVE scans, log
  rotations, certificate scans via `defaults/cron/jobs.json`.

Deployment shape (from `agents/platform/deployment.yaml`):
- Pod in management cluster's `agent-system/` namespace
- Bearer-token-secured HTTP API on `:8642` (Responses API shape)
- Persistent volume at `/opt/data` for state (`operator_agents.jsonl`,
  transcript logs, session cache)
- LiteLLM inference gateway service in same namespace
- LeastPrivilege K8s RBAC scoped to KCC ContainerCluster resource
  management only

## Three-role architecture

```
                   ┌──────────────────────┐
                   │  Platform Agent Pod  │   mgmt cluster /
                   │  (Hub)               │   agent-system ns
                   │  port :8642 + Hub    │
                   │  /peers registry     │
                   └──────────┬───────────┘
                              │
         ┌────────────────────┼─────────────────────┐
         │                    │                     │
         │  GitOps PR         │  peer call          │  peer call
         │  → KCC provisions  │  (@operator-X-Y)    │  (@devteam-X-Y-Z)
         ▼                    ▼                     ▼
    [new GKE                Operator Agent       DevTeam Agent
     cluster X-Y]           Pod (member          Pod (member
                            cluster X-Y)         cluster X-Y / ns Z)
                            port :8642           port :8642
                            self-registers       self-registers
                            back to Hub          back to Hub
```

Subagents are **remote, long-lived, self-registering peers** —
not in-process goroutines, not synchronous tool calls. Each is a
full agent deployment with its own model, MCP servers, and state.

## Hermes ↔ core-agent mapping

### Configuration & content

| Hermes construct | core-agent equivalent | Reusable? |
|---|---|---|
| `agents/platform/SOUL.md` | `AGENTS.md` (or `AGENTS.d/00-soul.md`) | ✅ As-is via `@include SOUL.md` |
| `agents/platform/skills/dev-team-provisioner/SKILL.md` | `.agents/skills/dev-team-provisioner/SKILL.md` | ✅ As-is (same SKILL.md format) |
| `agents/platform/defaults/skills/submit-suggestion/` | `.agents/skills/submit-suggestion/` | ✅ As-is |
| `agents/platform/defaults/governance/*.md` (9 SOPs) | `.agents/AGENTS.d/*.md` (lexical order) | ✅ As-is — drop-in for the v2 loader |
| `agents/platform/config.yaml` (MCP servers) | `.agents/mcp.json` | ⚠️ Format translation (YAML→JSON), one-time |
| `agents/platform/defaults/cron/jobs.json` | `Scheduler` + `agent.RunAutonomous` wrapper | ⚠️ Wrap with `examples/scheduled-monitor/` pattern |
| `agents/platform/defaults/scripts/platform_mcp_server.py` | Same Python MCP server, declared in `.agents/mcp.json` | ✅ As-is (we run MCP servers, not implement them) |

### Distributed-peer model

| Hermes piece | core-agent equivalent | State |
|---|---|---|
| Operator pod self-registers on boot | `core-agent --attach-register-to=<hub> --attach-register-endpoint=<self> --attach-register-name=<id>` | ✅ Built-in CLI flags |
| Platform's `operator_agents.jsonl` registry | `attach.PeerRegistry` on the hub (`pkg/attach/peers.go`) | ✅ Built-in (in-memory) |
| Stable URL per operator | `Peer{Endpoint}` field | ✅ |
| Bearer-token auth | `--attach-token` (hub) + `WithPeerBearerToken` (peer client) | ✅ |
| MCS DNS routing | Hub indifferent to DNS layer — operator's `--attach-register-endpoint` is whatever URL the hub can reach (Service / Pod IP / `clusterset.local`) | ✅ |
| Liveness | `PeerRegistry` heartbeat with TTL; auto-prune on lease expiry | ✅ **Better than Hermes** — Hermes has no liveness model documented; we do |
| Platform → operator call | `attachclient.Client` against `Peer.Endpoint` | ✅ |

### Runtime / API surface

| Hermes piece | core-agent equivalent | State |
|---|---|---|
| Hermes Responses API (`POST /v1/responses`, port 8642) | `--attach-listen=:7777` (different wire shape) | ❌ Wire format differs — see Gap 4 below |
| Bearer-token inter-agent HTTP | `--attach-token` + our HTTP API | ⚠️ Different endpoints but same auth pattern |
| `/opt/data/operator_agents.jsonl` persistent registry | `--session-db <path>` + custom data on PVC | ✅ Persistence model fits; just a different file format |
| Stateful conversation IDs | Our `sessionID` | ✅ Equivalent semantics |

## What's reusable today

If we set aside the `/v1/responses` wire-format question (which
only matters if existing Hermes API consumers must keep working),
**we can replace Hermes with core-agent today** by:

1. **Drop a `.agents/` directory next to the existing files:**

```
agents/platform/                          # left intact
├── SOUL.md                                # ← @included by .agents/AGENTS.md
├── README.md
├── deployment.yaml                        # swap image: hermes-agent → core-agent
├── config.yaml                            # legacy (Hermes co-existence)
├── skills/dev-team-provisioner/SKILL.md   # ← loaded by our skills scanner
└── defaults/
    ├── governance/*.md                    # ← loaded via AGENTS.d/ overlay
    ├── skills/submit-suggestion/SKILL.md  # ← loaded
    ├── cron/jobs.json                     # ← wrapped by Scheduler at startup
    └── scripts/                           # ← invoked from skill SKILL.md instructions

.agents/                                   # NEW — drop core-agent in alongside
├── AGENTS.md                              # @include ../SOUL.md (+ overlay extras)
├── AGENTS.d/                              # symlinks into defaults/governance/
│   ├── 10-blueprint-sync.md → ../../defaults/governance/blueprint_sync_sop.md
│   ├── 20-compliance-audit.md → ...
│   └── ...  (one per SOP, prefixed for lexical ordering)
├── config.json                            # mode, permissions, attach config
├── mcp.json                               # translated from config.yaml
└── skills/                                # symlinks into ../skills/ and ../defaults/skills/
    ├── dev-team-provisioner → ../../skills/dev-team-provisioner
    └── submit-suggestion → ../../defaults/skills/submit-suggestion
```

   This keeps the `kube-agents` repo's existing layout untouched —
   Hermes still works; core-agent loads the same content via the
   v2 instruction loader. Operators can swap runtimes without
   restructuring.

2. **Boot the platform as the hub:**

   ```bash
   core-agent \
     --attach-listen=:8642 \
     --attach-token=$API_SERVER_KEY \
     --no-repl \
     --session-db /opt/data/sessions.db
   ```

3. **Boot each operator/devteam as a peer:**

   ```bash
   core-agent \
     --attach-listen=:8642 \
     --attach-register-to=https://platform-agent.agent-system.svc.cluster.local:8642 \
     --attach-register-endpoint=https://$POD_IP:8642 \
     --attach-register-name=operator-mercury-04 \
     --attach-token=$PEER_TOKEN \
     --no-repl
   ```

4. **Optionally enforce GitOps-only mutations via plan-first**
   (PR #100, just shipped):

   ```json
   {
     "permissions": {
       "mode": "ask",
       "require_plan_artifact": true,
       "deny": ["bash:kubectl apply *", "bash:gcloud * create *"]
     }
   }
   ```

   The plan-first gate makes "you must propose via PR, not raw
   kubectl" a hard constraint rather than a prompt.

## Agent-friendly documentation as the lowest-layer entry point

`kube-agents` proves an architectural thesis worth naming
explicitly:

> **Agent-friendly documentation IS the lowest entry point of the
> agentic stack.**

`kube-agents` ships:
- 95% structured markdown (`SOUL.md` per role, `SKILL.md` per
  capability, governance SOPs per concern, `INSTALL.md` for
  bootstrap)
- 5% Python glue (one MCP server, one token refresh script,
  Kubernetes manifests)
- Zero framework-specific code

Any agentic runtime can pick it up:
- Hermes (their default)
- core-agent (this analysis)
- per their README, CrewAI / Microsoft AutoGen / LangGraph also

The runtime layer is **swappable**, because the contract isn't
"agent X's API" — it's "well-structured prompts and skills in
markdown." This is a more durable form of agentic interop than
shared API protocols or shared Go interfaces.

The v2 multi-file instruction loader (PR #98 — `@include` +
`AGENTS.d/`) was designed precisely to make this kind of
cross-runtime portability work. Validating against `kube-agents`
is the proof point that the loader's primitives match real-world
multi-file shapes that already exist in the ecosystem.

This positioning has implications for how we frame `core-agent`
externally:

- **Layer 0:** Agent-friendly markdown (SOUL/SKILL/governance/cron
  config). The "content" of an agentic system.
- **Layer 1:** Runtime loader (`pkg/instruction` v2, `pkg/skills`,
  `pkg/mcp`). Reads layer-0 content. Cross-runtime portable IF
  the loader honors the conventions.
- **Layer 2:** Agent loop + model + tools (`pkg/agent`,
  `pkg/permissions`, `pkg/tools`). The "engine."
- **Layer 3:** Multi-agent coordination (`pkg/attach`, peer
  registry, spawn primitives). What turns a single agent into a
  fleet.
- **Layer 4:** Operational bindings (Kubernetes deployment, GCP
  IAM, fleet management). Owned by the consumer, not us.

Most agentic frameworks position themselves at layer 2 or
3 and force the operator to author layer-0 content in a
framework-specific format. By honoring portable layer-0 conventions
(SOUL.md, SKILL.md, AGENTS.md, governance SOPs) we let operators
move their content between runtimes without rewriting it. This is
a differentiator worth naming in the marketing pitch + docs.

## Gap analysis (what we DON'T have)

Four real gaps for full Hermes parity. Three are small framework
work; one is an architectural decision.

### Gap 1: File-backed `PeerRegistry` for cross-restart durability

**Today:** `pkg/attach/peers.go` keeps the registry in-memory.
Platform-pod restart loses all peers; they'll re-register on their
next heartbeat (~20-60s blackout window).

**Hermes:** Persists to `operator_agents.jsonl` on PVC for
instant cold-start recovery.

**Fix:** Add `WithPersistFile(path string)` option to
`NewPeerRegistry`. Write JSONL on every Register / Heartbeat /
Deregister. Load on startup. Crash-safe via temp+rename. **~50
LoC.**

### Gap 2: Named-routing built-in tool (`call_peer`)

**Today:** Platform's MCP server can implement a `call_peer(name,
prompt)` tool on top of `attachclient.Client` + registry lookup
(~150 LoC of application code). The substrate doesn't ship a
built-in.

**Hermes:** Ships `mcp_platform_control_call_agent(agent_id,
prompt)` as a native FastMCP tool. Same shape, just packaged
with the framework.

**Fix options:**
- **A.** Leave it to application code (each kube-agents recipe
  ships its own MCP server with this tool). Pros: simple,
  zero substrate work. Cons: every consumer reimplements it.
- **B.** Ship as a core-agent built-in tool gated on a
  `--peer-routing-enabled` flag, registered when the agent has
  both `--attach-listen` and a hub-side registry. **~150 LoC.**
  Pros: zero per-recipe glue. Cons: opinionated semantics
  (what if peers want different routing rules?).

Lean: **B**, with a config knob for the tool's name + prompt
shape so recipes can override.

### Gap 3: Cron-jobs.json declarative loader

**Today:** Operators write Go code wrapping `Scheduler` +
`RunAutonomous` (the pattern in `examples/scheduled-monitor/`).

**Hermes:** Ships a `defaults/cron/jobs.json` declarative format
loaded at startup. Each entry = one scheduled prompt + recurrence.

**Fix:** Add a `cron.json` reader in `cmd/core-agent` that
registers each entry against the existing `Scheduler` primitive.
**~80 LoC.** Or expose as a small library function so consumers
can build their own equivalents.

### Gap 4: Responses API wire-format compatibility (`/v1/responses`)

**Today:** Our attach mode exposes a custom HTTP surface (POST
`/sessions/<sid>/inject`, GET `/sessions/<sid>/events`, etc.).

**Hermes:** Exposes the OpenAI-style Responses API (POST
`/v1/responses` with `model`/`conversation`/`input` body; GET
`/v1/responses/<id>` for trajectory retrieval).

**Question:** Does dropping core-agent in require keeping the
`/v1/responses` endpoints working? Two camps:
- **Yes:** Existing tooling (benchmark harnesses, the
  `mcp_platform_control_call_agent` impl, any operator-side
  scripts) calls `/v1/responses`. A swap requires a compat
  adapter.
- **No:** A runtime swap is also an API swap; operator-side
  callers update their integration. Cleaner long-term.

**Fix (if yes):** Add a `pkg/attach/openai_responses.go` handler
that translates `/v1/responses` POST → our internal session
inject + event stream → response synthesis. **~150 LoC.** Doesn't
need 100% parity — just enough to satisfy the call patterns the
kube-agents codebase actually uses.

Lean: **fix it, but only if a concrete consumer surfaces.** A
recipe-driven drop-in (where the operator swaps `image:` in
`deployment.yaml`) probably doesn't need the wire compat because
the platform agent is the one calling its own API and we control
the call site. Cross-runtime benchmarks would need it.

## Proposed path forward

Three pieces, in dependency order:

### Phase 1: Recipe (`examples/kube-platform-agent/`) — validation

Build a config-only recipe that points at the kube-agents repo's
existing `agents/platform/` structure:

```
examples/kube-platform-agent/
├── README.md                      # walkthrough + deployment-image swap recipe
├── .agents/
│   ├── AGENTS.md                  # @include relative paths into ../../kube-agents
│   ├── AGENTS.d/                  # symlinks to governance SOPs
│   ├── config.json                # plan-first + attach + path scope
│   ├── mcp.json                   # YAML→JSON translation of platform_control
│   └── skills/                    # symlinks to existing SKILL.md dirs
└── deploy/
    └── deployment.yaml            # kustomize overlay swapping the image
```

Validates: the v2 loader's `@include` + `AGENTS.d/` actually
reaches into a sibling repo cleanly; symlink semantics are
correct; plan-first composes; the GitOps skill's `submit_suggestion.py`
script works under our skills scanner.

**No framework changes.** ~300 LoC of config + docs.

### Phase 2: Three small substrate PRs

Filed as backlog tasks (see below). Build only what the recipe
needs first; defer the rest until a real consumer asks.

1. File-backed `PeerRegistry` (Gap 1) — needed for
   platform-restart resilience.
2. `call_peer` built-in tool (Gap 2) — nice-to-have; can be
   deferred if the recipe ships its own MCP server.
3. `cron.json` declarative loader (Gap 3) — nice-to-have; can be
   deferred if the recipe wraps `examples/scheduled-monitor/`
   pattern in a tiny Go binary instead.

### Phase 3: Responses API compat (Gap 4) — only if needed

Build only if a concrete consumer surfaces that calls
`/v1/responses` and can't be easily migrated. Don't build
speculatively.

## Out of scope

- **Implementing the K8s Operator (KCC) glue.** Hermes relies on
  Google Cloud Config Connector for the GKE-cluster-provisioning
  side. That's an operator-side dependency, not framework.
- **GKE Fleet Hub registration / MCS DNS / VPC peering.** All
  GKE-specific infrastructure that the platform agent uses but
  doesn't own. We just need to be reachable at whatever URL the
  operator publishes.
- **Reimplementing FastMCP.** We run MCP servers via
  `.agents/mcp.json`; the platform's existing Python MCP server
  works unchanged.
- **Replacing LiteLLM.** Their LLM gateway choice; we'd use
  whatever `model.provider` config the operator picks (could be
  LiteLLM via OpenAI-compatible URL, could be direct
  Anthropic/Gemini/Vertex).
- **Marketing positioning of `core-agent` as a Hermes
  competitor.** This doc is for technical validation only.
  Marketing framing (e.g. how to talk about runtime portability
  in our README pitch) is a separate conversation.

## Open questions

1. **Do we coordinate with the `kube-agents` team?** They might
   welcome a multi-runtime story or might prefer to stay
   Hermes-specific. A friendly PR adding a "Running with
   core-agent" section to their docs vs. just shipping
   `examples/kube-platform-agent/` on our side.

2. **Do we ship `call_peer` as a built-in (Gap 2 option B), or
   leave it in application code?** Hinges on whether we expect
   multiple consumers building peer-routed platform agents.

3. **Marketing the runtime-portability story.** The
   "agent-friendly documentation as the lowest layer" framing
   wants a place in our README / pitch. Discussion separate from
   this doc.

4. **`/v1/responses` compat decision.** Build speculatively for
   ecosystem reach, or wait for a concrete consumer? (Default
   recommended: wait.)
