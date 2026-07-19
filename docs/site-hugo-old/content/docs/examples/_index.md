---
title: Examples
linkTitle: Examples
weight: 5
menu:
  main:
    weight: 35
---

Every example in this list lives under [`examples/`](https://github.com/go-steer/core-agent/tree/main/examples) in the repo. Two shapes:

- **Config-only recipes** — a self-contained `.agents/` directory. Drop in, run `core-agent`, done. No Go code, no custom binary.
- **Library examples** — a single `main.go` you `go run`. Shows how to wire core-agent into your own Go program.

Pick by what you're building.

---

## Config-only recipes

Run with the bundled binary; no Go code on your side.

### [`gke-parallel-triage`](https://github.com/go-steer/core-agent/tree/main/examples/gke-parallel-triage)

GKE incident-triage agent that fans out one investigator per service in parallel via `spawn_agent`, then synthesizes a root-cause report. Wires the [GKE MCP server](https://docs.cloud.google.com/kubernetes-engine/docs/reference/mcp) (read-only endpoint) via Application Default Credentials. Use when you have a GKE cluster and want the platform-engineering pattern.

**Highlights:** parallel subagent fan-out · MCP server integration · read-only by design · multi-model routing tunable (Pro orchestrator + Flash investigators)

### [`plan-first`](https://github.com/go-steer/core-agent/tree/main/examples/plan-first)

Substrate-enforced plan-before-action. The agent must call `record_plan` before any `write_file`/`bash`/etc. tool call succeeds — read tools stay open during research. Ships three `config.json` variants (`ask` / `acceptEdits` / `yolo` × `require_plan_artifact`) so you pick the post-plan friction level. Use when you want the safety of a written plan before the agent touches anything.

**Highlights:** gate-level enforcement (not just AGENTS.md convention) · plan artifacts on disk under `.agents/plans/` · `/replan` slash to revoke + redraft · composes with every existing mode

### [`gke-deploy`](https://github.com/go-steer/core-agent/tree/main/examples/gke-deploy)

Deploy `core-agent` as a long-lived pod in a GKE cluster, reachable by operators over an **internal** HTTP LoadBalancer. Uses **Workload Identity Federation for GKE direct binding** (no Google Service Account in the middle — IAM roles bind directly to the KSA's `principal://...` identifier) for credential-free Vertex AI inference + GKE read-only MCP access. Publishes an A2A AgentCard at `/.well-known/agent-card.json` for [Google Cloud Agent Registry](https://docs.cloud.google.com/agent-registry/register-agents) discovery, and opts into [GKE Managed Workload Identity](https://docs.cloud.google.com/iam/docs/create-managed-workload-identities-gke) for auto-rotated SPIFFE certs (mTLS-ready; on-ramp to Google Cloud Agent Identity when GA). No Dockerfile in the recipe — uses the published `ghcr.io/go-steer/core-agent:2.3.1` image. Use when you want a managed-runtime deployment of core-agent for a platform team or a long-running fleet auditor.

**Highlights:** WIF-for-GKE direct binding (no GSA / no key files) · internal LoadBalancer (VPC-only) · Agent Registry registration + A2A AgentCard discovery · GKE Managed Workload Identity (SPIFFE certs) · GKE read-only MCP wired · agentic small-model cost routing (Pro orchestrator + Flash tool subagents) · 10Gi PVC for session DB + plans · variant configs for Anthropic-on-Vertex + plan-first + slim image · operator attach via Cloud Workstations / IAP / VPN

---

## Library quickstarts

Embedding core-agent in your own Go binary. Each is one `main.go` you `go run`.

### [`basic`](https://github.com/go-steer/core-agent/tree/main/examples/basic)

Minimal multi-turn agent — `agent.New` + a single `Run` loop. Gemini by default; `GOOGLE_API_KEY` required. Start here if you want the simplest "how do I drive the agent" answer.

### [`with-tools`](https://github.com/go-steer/core-agent/tree/main/examples/with-tools)

One custom tool plus MCP servers from `.agents/mcp.json` and skills from `.agents/skills/`. Shows how operator-defined and library-defined tools coexist.

### [`with-subagent`](https://github.com/go-steer/core-agent/tree/main/examples/with-subagent)

Parent + subagent end-to-end with no LLM credentials — two scripted-mock providers drive both sides deterministically. The shape to copy when you want a fan-out structure in your own binary.

### [`streaming`](https://github.com/go-steer/core-agent/tree/main/examples/streaming)

The standard built-in tools (`read_file`, `list_dir`, `bash`, …) wired into an interactive chat. Closest to "what the CLI does, but you own the binary."

---

## Autonomous (headless) patterns

Long-running agents driven by a goal rather than turn-by-turn operator prompts. All use `agent.RunAutonomous`.

### [`autonomous`](https://github.com/go-steer/core-agent/tree/main/examples/autonomous)

End-to-end `agent.RunAutonomous` against the mock "scripted" provider. No LLM credentials needed. Shows the full Goal → cost-bounded loop → terminal-report shape.

### [`autonomous-handle`](https://github.com/go-steer/core-agent/tree/main/examples/autonomous-handle)

Same as above plus the `AutonomousHandle` API — `Pause` / `Resume` / `Inject` / `Stop` an in-flight run from another goroutine. Pattern for "long task + operator can steer mid-run."

### [`autonomous-resume`](https://github.com/go-steer/core-agent/tree/main/examples/autonomous-resume)

Drive a run, hit a tight `max_turns` budget (simulated crash), then continue from the eventlog. Shows the crash-resume contract.

### [`background-monitor`](https://github.com/go-steer/core-agent/tree/main/examples/background-monitor)

Wire `BackgroundAgentManager` and demonstrate in-process spawn end-to-end with no LLM credentials. Use as the template for "parent agent + background subagent workers."

### [`scheduled-monitor`](https://github.com/go-steer/core-agent/tree/main/examples/scheduled-monitor)

The supervision-tree topology from [`docs/scheduled-monitoring-design.md`](https://github.com/go-steer/core-agent/blob/main/docs/scheduled-monitoring-design.md) — periodic health sweeps with a scheduler + supervisor + worker layout. Pattern for cron-style monitoring agents.

---

## Testing & debugging

### [`replay`](https://github.com/go-steer/core-agent/tree/main/examples/replay)

Drive the agent loop offline by replaying a recorded JSONL transcript through the mock "scripted" provider. Useful for regression tests, reproducing bugs from production captures, and CI runs that don't need real LLM calls.

---

## Composing recipes

The config-only recipes are designed to layer. With the [v2 instruction loader]({{< relref "/docs/reference/configuration.md#multi-file-instructions-v23" >}}), you can drop a recipe's `AGENTS.md` into your existing project's `AGENTS.d/` and merge their `config.json` settings:

```bash
# Layer plan-first into an existing GKE-triage setup
mkdir -p <your-project>/.agents/AGENTS.d
cp examples/plan-first/.agents/AGENTS.md \
   <your-project>/.agents/AGENTS.d/00-plan-first.md

# Merge plan-first's permissions into the existing config.json
# (require_plan_artifact: true + read-tool allowlist)
```

The recipe READMEs (`examples/<name>/README.md`) each cover their own composition + tuning notes — read those before forking.

---

## Don't see what you need?

- **Want help picking?** [Getting started]({{< relref "/docs/getting-started.md" >}}) walks the same decision tree end-to-end.
- **Building something new?** The patterns in [Agent design]({{< relref "/docs/agent-design/_index.md" >}}) generalize across these examples — start there for prompt + tool-description guidance.
- **Idea for a recipe?** Open a [GitHub discussion](https://github.com/go-steer/core-agent/discussions). Recipes ship as PRs against `examples/`.
