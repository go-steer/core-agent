# Scion + `core-agent`: layered architecture for managed agent runtimes

Companion to [`docs/kube-agents-platform-fit.md`](./kube-agents-platform-fit.md).
That doc analyzed running `core-agent` standalone as a replacement for
Hermes in the kube-agents Platform Agent role. This doc analyzes
adding [Scion](https://github.com/GoogleCloudPlatform/scion) at the
runtime/lifecycle layer between core-agent and the operator.

**Status:** investigative (2026-06-03). Sibling to the kube-agents
platform-fit analysis; same caveat — not a commitment to ship,
this captures the architectural framing.

**TL;DR:** Scion plugs in cleanly as a separate layer above
`core-agent`. It provides container lifecycle, sticky agent state,
templates, dashboard, and within-runtime message routing — none of
which we offer or want to. It does NOT replace the multi-cluster
peer-coordination story (our `pkg/attach/peers.go` + `--attach-
register-to`); the two are complementary, not competing.

## What Scion provides

From `extras/scion-agent/` and Scion's public docs:

- **Container runtime for agents** — agents run as Scion-managed
  containers; Scion handles lifecycle, restart, isolation.
- **Template-declared agent configurations** —
  `templates/scion/scion-agent.yaml` declares the image, harness,
  args; operators ask Scion to launch agents from templates rather
  than crafting raw container manifests.
- **Sticky lifecycle state machine** — `ask_user`, `blocked`,
  `task_completed`, `limits_exceeded` declared via the `sciontool`
  binary; Scion's hub tracks state across agent restarts.
- **Transient activity emission** — agents write
  `thinking` / `executing` / `working` to `$HOME/agent-info.json`;
  Scion's UI renders live progress per agent.
- **Within-runtime message routing via tmux** —
  `scion message <agent>` delivers operator nudges through
  tmux send-keys to the agent's stdin.
- **Dashboard / UI** — observe what's running, drill in, send
  messages, see lifecycle state at a glance.

We already integrate via `extras/scion-agent/` —
`scion-agent` is a thin Go binary that wraps `core-agent`'s library
with the lifecycle/activity hooks Scion expects. See
`docs/site/content/docs/reference/scion-adapter.md` for the
existing surface.

## What Scion does NOT provide (for the kube-agents-platform-agent
case specifically)

- **GKE cluster fleet management.** Scion's container runtime
  isn't a multi-cluster GKE orchestrator. The kube-agents pattern
  is "platform agent provisions a *new GKE cluster* + deploys an
  operator pod *into that cluster*" — that's KCC/Config-Connector
  territory.
- **Multi-cluster network coordination.** MCS DNS, VPC peering,
  fleet hub registration — none of that is Scion.
- **Inter-cluster agent-to-agent calls.** Scion routes messages
  between agents *in its own runtime*; calling an agent that
  Scion didn't launch (e.g. an operator agent in a member GKE
  cluster) needs our peer-registry pattern.
- **LLM provider integration, tools, MCP, skills, memory, etc.**
  — that's core-agent's whole job; Scion delegates it.

## The layered architecture with Scion in the mix

```
Layer 4: Operational bindings (GKE Fleet Hub, MCS DNS, VPC peering)
         — owned by GCP / your platform team
Layer 3a: Cluster provisioning (KCC / Config-Connector — provisions
          GKE clusters declaratively)
Layer 3b: Peer coordination across clusters (our pkg/attach/peers.go —
          operator pods self-register back to platform agent across
          MCS DNS)
Layer 2: Agent runtime + lifecycle (Scion — container management,
         templates, state machine, UI/dashboard, within-runtime
         message routing)
Layer 1: LLM agent loop (core-agent — multi-turn, tools, MCP, memory,
         plan-first, eventlog)
Layer 0: Agent-friendly markdown (SOUL.md, SKILL.md, governance SOPs,
         AGENTS.md) — runtime-agnostic content
```

Each layer is independently swappable:
- Swap Layer 4 (GKE → EKS / on-prem) without touching the others
- Swap Layer 3a (KCC → Crossplane / Terraform operator) without
  touching the others
- Swap Layer 2 (Scion → bare Kubernetes Deployment / Cloud Run /
  Nomad) without touching Layer 1
- Swap Layer 1 (`core-agent` → another agentic runtime) without
  touching Layer 0 — IF the new runtime honors the
  `SOUL.md`/`SKILL.md`/`AGENTS.md` conventions

That's the durable interop story: **layered separation with
markdown-based content at the bottom.**

## Scion vs. plain `core-agent` for the kube-agents-platform-agent
role

| Concern | Plain `core-agent` | Scion + `core-agent` |
|---|---|---|
| Container lifecycle / restart | Operator writes K8s Deployment YAML | Scion template declares it; Scion restarts |
| "Agent is asking the user" state | Operator polls our HTTP API or watches the eventlog | Sticky state via `sciontool_status("ask_user", ...)`; Scion's hub knows |
| Live activity (`thinking` / `executing`) | Operator reads our SSE event stream | `$HOME/agent-info.json` ticks; Scion UI shows it |
| Dashboard | Operator builds one against our attach API | Scion ships one |
| Template-based agent provisioning | Operator copies + edits `.agents/` directories | Scion template `scion-agent.yaml` parameterizes per-agent |
| Message-to-running-agent | `POST /sessions/<sid>/inject` (our HTTP API) | `scion message <agent>` (tmux send-keys via Scion CLI) |
| Cross-cluster agent calls (platform → operator-in-member-cluster) | `attachclient` against peer registry endpoints | **Same — Scion doesn't replace this** |
| LLM provider, tools, MCP, skills, memory, plan-first | `core-agent` | `core-agent` (unchanged inside the Scion container) |

## When does Scion-in-the-mix make sense?

| If your priority is... | Recommendation |
|---|---|
| One platform agent + operator pods in member GKE clusters, minimal stack, you own the dashboard story | **plain `core-agent` + KCC + our peer registry** (the original kube-agents-platform-fit analysis) |
| Adding a managed lifecycle + UI + template story on top of the above, no other Scion-specific consumers | **plain `core-agent` + KCC + our peer registry**, with Scion **just** for the dashboard. (Might be overkill — easier to point Grafana at our eventlog.) |
| You're already in the Scion ecosystem and the team is comfortable with Scion's tooling / dashboards / agent management UX | **Scion + `core-agent` + KCC + our peer registry** (Layer 2 = Scion, Layer 1 = core-agent) |
| You want Scion to manage agent teams *within one cluster*, no multi-cluster GKE story | **Scion + `core-agent`** — drop KCC and our peer registry from the diagram |

## What changes vs the original kube-agents-platform-fit analysis

Adding Scion at Layer 2 doesn't materially change the substrate
gap list:

- **Doesn't change the 80% reusable bit.** `SOUL.md`, `SKILL.md`,
  governance SOPs still load via the v2 instruction loader. The
  Layer-0 content is runtime-agnostic.
- **Doesn't change the four gaps.** File-backed `PeerRegistry`,
  `call_peer` built-in, `cron.json` declarative loader,
  `/v1/responses` wire-format compat — all sit at Layer 1
  (`core-agent`) or Layer 3b (peer coord). Scion doesn't supply
  any of them.
- **Adds optional value.** If the consumer already wants a
  managed-runtime story, Scion gives it; if not, we don't push it.
- **Reinforces the layered thesis.** Scion is genuinely Layer 2;
  we're genuinely Layer 1; KCC is genuinely Layer 3a; the
  kube-agents markdown is genuinely Layer 0.

## What's already in place

We already have `extras/scion-agent/` as a working Scion adapter:

- **`scion-agent` binary** — wraps `core-agent`'s library with
  Scion's lifecycle/activity hooks.
- **`sciontool_status` tool** — model declares sticky states
  (`ask_user` / `blocked` / `task_completed` / `limits_exceeded`).
- **`agent-info.json` emitter** — transient activity (thinking /
  executing / working) per agent / tool boundary.
- **`--input <task>` flag** — Scion's harness seeds the first
  turn.
- **Non-blocking inbox loop (v1.3.0+)** — stdin reads pushed onto
  the agent's inbox via `Agent.Inject`; messages arriving mid-turn
  queue instead of blocking.
- **Templates under `extras/scion-agent/templates/scion/`** — ready
  to register with Scion's template directory.
- **Dockerfile + build flow** — `docker build` on top of
  `scion-base:latest` produces a Scion-ready image.

What's NOT in place specifically for the kube-agents platform-agent
shape:

- A `scion-agent` template variant for the **platform** /
  **operator** / **devteam** role-shaped agents — today's template
  is the generic Gemini agent.
- A demonstration of Scion + core-agent's peer-registry working
  together (e.g. a Scion-launched platform agent that hubs over
  our `/peers` API while operator pods in GKE clusters register
  via `--attach-register-to`).

Both are recipe-shaped follow-ups, not framework work.

## Honest uncertainty about Scion's deeper features

I haven't deeply explored Scion's full feature set beyond what
`extras/scion-agent/` exercises. Specifically:

- Does Scion ship its own multi-agent **team coordination**
  primitives (declarative team composition, leader election,
  fan-out templates) that would compete with or complement our
  `pkg/attach/peers.go`?
- Does Scion provide cross-cluster agent communication, or only
  within its own runtime?
- Does Scion's template system support inheritance / composition
  (e.g. "platform-agent is a specialization of base-agent with
  extra MCP servers") in a way that would benefit kube-agents'
  role-based hierarchy?
- Is there a Scion concept for "agent fleet" / "agent group" that
  maps to kube-agents' platform → operator → devteam hierarchy?

Filed as backlog task #11 to investigate before committing to the
"Scion at Layer 2, peer registry at Layer 3b" recommendation.
Today the answer "Scion is Layer 2, we own Layer 3b" is the
best-guess default — but if Scion has a credible Layer-3b answer
we should evaluate before duplicating effort.

## Proposed path forward (no action required yet)

If we want to pursue Scion as a recommended runtime layer for
multi-agent core-agent deployments:

1. **Investigate Scion's team-coordination story** (task #11).
   Resolve the open questions above before any code.
2. **Build a Scion-launched kube-agents-platform-agent demo.** Use
   the existing `extras/scion-agent/` adapter; add a
   role-specialized template under
   `extras/scion-agent/templates/scion/platform/`; demonstrate the
   self-registration story (Scion-launched platform agent at the
   peer-registry hub, GKE-cluster-launched operator agents
   registering in via `--attach-register-to`).
3. **Document the architecture as a recommended pattern.** Update
   `docs/site/content/docs/reference/scion-adapter.md` with the
   layered architecture diagram + a "multi-cluster fleet" section.
4. **Reference from the kube-agents-platform-fit recipe.** When
   we build `examples/kube-platform-agent/` (Phase 1 of the
   platform-fit doc), add a section: "Optional: deploy via Scion
   for managed lifecycle + dashboard."

None of this is urgent — the original "plain `core-agent` + KCC +
our peer registry" path works without Scion. Scion is the
"managed runtime for the platform agent" upsell, valuable when
the operator wants Scion's lifecycle / template / UI story.

## Out of scope

- **Reimplementing Scion features inside core-agent.** Lifecycle
  state machine, container management, agent-team templates — all
  belong at Layer 2; we shouldn't pull them into Layer 1.
- **Replacing Scion's UI / dashboard.** Operators who want a
  managed dashboard use Scion's; operators who don't pull
  Grafana/Prometheus against our eventlog. Either is fine.
- **Building a Scion replacement.** This doc is about layered
  composition, not displacement.

## Open questions

1. **Investigation result (task #11).** Does Scion have a
   Layer-3b answer (peer coordination, inter-agent calling) that
   we should defer to rather than promoting `pkg/attach/peers.go`
   as the canonical pattern?
2. **Platform/operator/devteam role templates.** Should those ship
   in `extras/scion-agent/templates/scion/` or in
   `examples/kube-platform-agent/`?
3. **Documentation positioning.** Is "Scion at Layer 2" worth a
   first-class section in the marketing pitch, or is it a
   reference-docs-only detail for operators who already know they
   want Scion?
