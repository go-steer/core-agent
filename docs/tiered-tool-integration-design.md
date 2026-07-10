# Tiered tool integration — pre-built sensors + kode-gopher for dynamic tools

Design doc for a v2.7 addition: a tiered model where a small set of highest-value cluster sensors ship pre-built (for latency + always-on semantics) and the long tail of specialized diagnostic tools is authored on-the-fly by the LLM and executed via [kode-gopher](https://github.com/gke-demos/kode-gopher) — its sandbox pod compiles the agent-authored Go against real GCP SDKs + `client-go` and returns structured JSON.

**Status:** proposed (2026-07-10). Awaiting approval before implementation. v2.7 candidate. Tracking issue: [#200](https://github.com/go-steer/core-agent/issues/200).

## Motivation

Two independent recent findings pushed us to reframe how the triage agent (and future platform agents) consume specialized cluster diagnostics.

**Finding 1 — the "distill telemetry at the edge" thesis is architecturally right.** An external proposal for a 24-binary sensor matrix (`ev-sifter`, `log-condenser`, `edge-tracer`, `node-pressure-sifter`, and 20 more) landed with sound reasoning: purpose-built binaries running on-cluster pre-process raw kubectl output / logs / metrics into structured JSON so the LLM never spends inference tokens parsing verbose telemetry. Token cost drops, decision latency drops, prompt-injection surface shrinks (structured JSON has a schema we control; raw log content is attacker-influenced), and behavior becomes deterministic (same input → same JSON → same LLM decision, unlike "same logs, LLM reads them differently each turn").

**Finding 2 — kode-gopher (GKE Demos, pre-alpha) collapses the shipping-24-binaries burden.** It lets the LLM AUTHOR a diagnostic tool in Go — importing real `cloud.google.com/go/...` SDKs and `k8s.io/client-go` — and executes it in a sandbox pod via `sigs.k8s.io/agent-sandbox`. Credentials forward from the daemon's ADC. Compile-and-run path is 5–30s; an experimental Yaegi (Go interpreter) backend brings latency to ~700ms — fast enough for interactive triage. Exposed as an MCP stdio server (`execute_go_code`, `lookup_package_docs`).

Together these two land differently than either does alone:

- **Without kode-gopher**: the 24-tool matrix is a multi-quarter engineering commitment. Every specialized query the operator wants forces us into a "we need to ship a binary for that" backlog.
- **Without pre-built sensors**: kode-gopher alone is fine for one-shot diagnostics but wrong for always-on watchers (a sensor firing every 30s can't afford the sandbox compile/start cost) and wrong for the hot triage path (5-30s per invocation compounds badly across a multi-tool investigation).
- **Both together (tiered)**: ship 5-8 pre-built sensors covering always-on + hot-path cases; kode-gopher handles everything else. Best of both.

Also worth capturing (correcting an earlier over-conservative reading): `distroless/static-debian12:nonroot` does not prevent us from shipping additional binaries. It removes the shell (`/bin/sh`, coreutils, curl, …) but not `os/exec` in Go — a static Go binary `COPY`'d into the image and invoked via `exec.CommandContext(ctx, "/usr/local/bin/name", ...)` runs directly via `execve`, no shell involved. Pre-built sensor binaries fit distroless cleanly.

## Goals

- **Ship the sensor pattern at 20% of the engineering cost** of the full 24-binary matrix by picking the 5-8 that MUST be pre-built and pushing the rest to kode-gopher's dynamic authoring.
- **Zero waiting for us to author binaries** for bespoke operator queries. Any diagnostic the LLM can express in Go is executable via kode-gopher.
- **Composable with existing v2.6 substrate.** Tier 1 sensors either sit inside the daemon as MCP tools or ship as a companion sidecar (mirrors `k8s-event-watcher`). Kode-gopher slots into `.agents/mcp.json` as another MCP stdio server. No daemon primitive changes.
- **Distroless-safe end-to-end.** Everything ships as static Go binaries or MCP servers; nothing shells out to coreutils.
- **Tighter credential scope than kode-gopher's default.** By default the sandbox forwards the daemon's ADC — that gives LLM-authored code full daemon-KSA permissions. We ship the recipe with a separate `kode-gopher-sandbox` KSA that has reader-only IAM by default. Writes route through the daemon's own KSA via the GKE MCP + plan-first, not through generated code.

## Non-goals (v2.7)

- **Building all 24 sensors from the external spec.** We're picking a starter set; the rest is either kode-gopher-territory or a later release if operator experience shows a specific tool needs to be pre-built.
- **Building our own sandbox runtime.** We're consuming kode-gopher, not reinventing it.
- **Custom LLM-authored non-Go languages.** kode-gopher is Go-only; that's the wedge. Other languages via sandbox tech are v2.8+ material if someone actually asks.
- **Fine-grained per-invocation IAM.** The sandbox KSA gets a fixed permission set; per-call credential scoping is possible via GCP IAM Conditions but not v2.7 scope.
- **Yaegi feature completeness.** Yaegi's stdlib coverage is incomplete; we default to Yaegi where it works, fall back to full-compile elsewhere. Making Yaegi feature-complete is upstream work.
- **Cross-cluster sandbox pool.** kode-gopher's sandbox pods run in the daemon's cluster. Multi-cluster tool execution is out of scope — matches the multi-cluster posture from #178 (session resume) and #186 (k8s-event agent) which both keep per-cluster deployments.

## Conceptual model

### Two tiers, one prompting convention

**Tier 1 — pre-built, always-available.** Ships as a companion binary (`k8s-sensors`) — mirrors the `k8s-event-watcher` deployment shape. Exposes an MCP stdio server with N tools. Each tool has a stable schema; each is one `execve` away for the agent. Latency: single-digit ms.

**Tier 2 — kode-gopher-generated, always-available.** Deploys `kode-gopher` as a second companion binary (also MCP stdio). Exposes `execute_go_code(code)` — the LLM writes a Go program that uses `cloud.google.com/go/...` + `client-go` + arbitrary third-party packages, kode-gopher sandbox-compiles-and-runs, returns JSON. Latency: ~700ms (Yaegi) or 5-30s (full compile) depending on Go feature usage.

**Agent selection convention** (in the base AGENTS.md that every recipe consuming this pattern loads):

```markdown
When you need diagnostic data about the cluster:

1. Check the tool list for a purpose-built sensor. If one matches
   the exact question, invoke it — deterministic schema, single-digit ms.
2. Otherwise call `execute_go_code` with a small Go snippet importing
   `k8s.io/client-go` or `cloud.google.com/go/...`. Return format:
   compact JSON in a struct you define.
3. Do NOT parse raw kubectl output when a Tier 1 sensor OR a
   trivial Go snippet can pre-parse it. Raw parsing burns tokens
   and is prompt-injection-shaped.
```

Tier 1's schema is stable; the LLM learns it once and calls it thousands of times identically. Tier 2's schema is per-invocation but constrained to whatever the Go snippet returns — still JSON, still schema-checked at the outer wrapper.

### Starter Tier 1 set

Eight sensors covering always-on + hot-path cases. Chosen for either "must be always-on" or "hit-in-almost-every-triage-flow":

| Sensor | Why Tier 1 | Existing status |
|---|---|---|
| `ev-sifter` | Already shipped as `k8s-event-watcher` sidecar; add on-demand MCP variant here | ✓ v2.6 sidecar; add MCP surface |
| `log-condenser` | Hit on nearly every CrashLoopBackOff / Unhealthy triage | New |
| `edge-tracer` | Hit on every triage that needs RBAC / SA / ConfigMap / Secret enrichment | New |
| `node-pressure-sifter` | Always-on; flapping node states need immediate signal | New |
| `spot-countdown` | Always-on; time-critical (preemption warning must fire seconds before reclaim) | New |
| `hpa-loop-catcher` | Always-on; scaling inversions cascade fast | New |
| `top-analyzer` | Hot path for OOMKilled / CPU-throttling triage | New |
| `field-sentinel` | GitOps drift detection; called by scheduled sweeps | New |

Everything else from the 24-tool matrix — including specialized state analyzers (`webhook-inspector`, `endpoint-resolver`, `wi-scout`, `volume-binder`), performance sensors (`api-latency-sifter`, `apf-inspector`, `etcd-sentry`, `startup-profiler`), compliance sweepers (`disk-orphan-scout`, `lb-ghost-buster`, `stale-object-sweeper`, `ip-space-monitor`, `stockout-sentry`), and stability observers (`disruption-budget-analyzer`, `drain-blocker`, `kernel-sentry`, `exec-spy`) — is **Tier 2 territory** unless usage data proves otherwise. That's 16 diagnostic capabilities we get "for free" from kode-gopher without shipping a binary per query.

## Detailed design

### Tier 1 shipping shape — one companion binary, MCP stdio

Ships as `cmd/k8s-sensors/` — a new companion binary in the core-agent monorepo alongside `cmd/k8s-event-watcher/`. Structure:

```
cmd/k8s-sensors/
├── main.go                  — MCP stdio server + top-level dispatcher
├── log_condenser.go         — one file per sensor
├── edge_tracer.go
├── node_pressure_sifter.go
├── spot_countdown.go
├── hpa_loop_catcher.go
├── top_analyzer.go
├── field_sentinel.go
├── ev_sifter_ondemand.go    — on-demand variant of what k8s-event-watcher does continuously
└── *_test.go                — one per sensor
```

Why one binary (not eight):
- **Dependency dedup**: `client-go` is ~5MB static; imported once, not eight times.
- **Image size**: one binary in the recipe's deployment vs. eight in the ConfigMap manifest.
- **Discovery**: MCP `list_tools` enumerates all sensors natively; separate binaries would need a wrapper.
- **Test isolation**: same Go module boundaries; tests share fixtures cheaply.

Not shipped as in-process tools inside `core-agent`:
- Keeps `core-agent` k8s-agnostic (client-go dep would bloat every deployment)
- Independent release cadence (a sensor bump doesn't force a daemon release)
- Fits the sidecar pattern operators already understand from `k8s-event-watcher`

Deployment: same pod as `core-agent` in the recipe's default (localhost stdio between the two containers), or standalone Deployment for centralized shape.

### kode-gopher deployment

Wired into the recipe as a companion sidecar via `.agents/mcp.json`:

```json
{
  "servers": {
    "kode-gopher": {
      "transport": "stdio",
      "command": "kode-gopher",
      "args": ["serve"],
      "env": {
        "GOOGLE_CLOUD_PROJECT": "{{.ProjectID}}",
        "KODE_GOPHER_NAMESPACE": "codemode",
        "KODE_GOPHER_KSA": "kode-gopher-sandbox"
      }
    }
  }
}
```

Prereqs the recipe README documents:

1. Install the `sigs.k8s.io/agent-sandbox` operator into the cluster.
2. Create a `codemode` namespace with the SandboxTemplate + SandboxWarmPool CRs.
3. Provision the `kode-gopher-sandbox` KSA with **reader-only IAM roles**:
   - `roles/container.viewer` (read cluster state; NOT `container.developer` which allows writes)
   - `roles/logging.viewer`
   - `roles/monitoring.viewer`
   - `roles/aiplatform.user` (for tools that need Vertex AI)
4. Deploy `kode-gopher` as a sidecar container in the core-agent pod (shares pod network so stdio works via a shared FIFO or the recipe defers to `kode-gopher`'s built-in daemon-mode).

Point 3 is the important one — it's where we deliberately diverge from kode-gopher's default forward-ADC behavior. The default gives the sandbox the daemon's full IAM; we substitute a read-only sandbox KSA. That prevents LLM-authored Go from mutating the cluster with the daemon's write privileges. Writes still work — they route through the daemon's KSA via the GKE MCP (already gated by plan-first + permission modes). This split keeps the "diagnosis is agentic; writes are policy-gated" property from the original 24-tool spec.

### Credential scoping — the tricky one

kode-gopher's default `Forwarded` credentials source materializes ADC + env into the sandbox. That's convenient but wrong for our threat model:

- The daemon has `roles/container.developer` (needed to apply manifests during triage fixes).
- Forward that to the sandbox → LLM-authored Go can call `client.AppsV1().Deployments().Update()` directly, bypassing plan-first + permission gates.
- This is the exact "LLM has direct write authority" outcome the original spec explicitly cautioned against.

Fix: sandbox runs as a separate KSA (`kode-gopher-sandbox`) with **read-only** IAM. LLM-authored Go can `Get`/`List`/`Watch`/read logs; it CANNOT mutate. Any fix action goes through the GKE MCP (which uses the daemon's KSA), which goes through the permission gate, which enforces plan-first + operator approval per config.

The recipe's kustomize base ships this with the manifests. Operators who genuinely want the sandbox to have write capability (single-purpose triage clusters, dev/test) override by editing the KSA's IAM binding — deliberate opt-out.

### Yaegi vs full compile — default to Yaegi where possible

kode-gopher's roadmap includes a Yaegi backend (`experiments/yaegi-poc/`). Latency: ~700ms end-to-end. That's within interactive-triage tolerance; full compile at 5-30s isn't.

Recipe default: Yaegi. Fall back to compile automatically when the LLM's Go snippet uses stdlib features Yaegi doesn't cover (kode-gopher already has the framework for this; we just consume it). Explicit override for operators who need reproducibility (compile mode gives deterministic module resolution; Yaegi does approximate lookups against a bundled stdlib subset).

Track Yaegi maturity; when it's feature-complete, promote from experimental to default in kode-gopher itself.

### Recipe integration

The `examples/gke-troubleshoot-agent/` recipe grows in three places:

1. **`.agents/mcp.json`** — adds `kode-gopher` entry. Sensor MCP server also registered if we ship Tier 1 as a separate sidecar.
2. **`.agents/AGENTS.md`** — adds the tier-selection paragraph (see "Agent selection convention" above).
3. **`deploy/base/`** — new manifests: `20-serviceaccount-kode-gopher-sandbox.yaml` (with read-only IAM guidance in comments), `52-deployment-k8s-sensors.yaml` (if Tier 1 is a separate sidecar), kustomization additions.

Reference files under `.agents/skills/k8s-triage/references/*.md` get lightly updated: where they currently say `kubectl -n {namespace} logs {name} -c {container} --previous --tail=200`, they can now say `invoke log-condenser` for the same signal at 10x less token cost. Backwards-compatible: agents that don't have the Tier 1 sensor available fall through to `kubectl` guidance (or `execute_go_code`).

## Per-substrate impact

### `cmd/k8s-sensors/` (new)

- New in-tree Go binary. Depends on `k8s.io/client-go` + `github.com/modelcontextprotocol/go-sdk` (same deps we already have from `k8s-event-watcher` + `pkg/mcp`).
- MCP stdio server. Tools exposed match Tier 1 list above.
- Container image on GHCR alongside the existing four (`core-agent`, `core-agent-slim`, `core-agent-tui`, `k8s-event-watcher`). Fifth entry in the `release-images.yml` matrix.

### `core-agent` (daemon)

**Zero code changes.** Both Tier 1 and kode-gopher wire in via `.agents/mcp.json` — existing MCP integration handles the rest.

### `examples/gke-troubleshoot-agent/` (existing recipe)

- Adds `kode-gopher` sidecar Deployment + sandbox KSA + agent-sandbox-operator install docs.
- Adds `k8s-sensors` sidecar Deployment.
- Updates `.agents/mcp.json` to register both.
- Updates AGENTS.md with tier-selection guidance.
- Updates references/*.md to prefer sensor tools over raw kubectl where applicable.

### `docs/site/content/docs/reference/`

- New page `tiered-tools.md` covering the tier model, Tier 1 sensor reference, kode-gopher setup + IAM guidance, examples of agent-authored diagnostic snippets.

### `go.mod`

- No new deps for `core-agent` itself.
- `cmd/k8s-sensors/` reuses `k8s.io/client-go` + MCP SDK we already have.
- Kode-gopher is a runtime dependency (container image), not a build-time Go dep.

## Migration story

Net-new feature. Additive.

- **Existing v2.6 deployments** — no behavior change until an operator adds the `kode-gopher` and/or `k8s-sensors` MCP entries + deploys the companion sidecars.
- **New deployments using the updated recipe** — get both tiers by default.
- **Operators who want only Tier 1** — deploy `k8s-sensors`, skip `kode-gopher`. Recipe supports both modes.
- **Operators who want only kode-gopher** — inverse; also supported. Reference-file guidance falls back cleanly (`invoke log-condenser or, if unavailable, execute_go_code with a client-go LogTail snippet`).

## Implementation phases

### Phase 1 — Tier 1 sensor binary + top-3 sensors (PR ε.1 of #200)

- `cmd/k8s-sensors/` skeleton: main.go, MCP stdio server bootstrap, dispatcher.
- First three sensors: `log-condenser`, `edge-tracer`, `top-analyzer`. Chosen for highest hit rate in existing triage references.
- Unit tests using fake client-go per sensor.
- Container image added to `release-images.yml` matrix.

Estimate: ~800 LoC prod + ~600 LoC tests. ~4 days.

### Phase 2 — Remaining Tier 1 sensors (PR ε.2 of #200)

- `node-pressure-sifter`, `spot-countdown`, `hpa-loop-catcher`, `field-sentinel`.
- `ev-sifter` on-demand variant (complement to the always-on `k8s-event-watcher`).
- Tests per sensor.

Estimate: ~1000 LoC prod + ~800 LoC tests. ~5 days.

### Phase 3 — kode-gopher integration in the recipe (PR ε.3 of #200)

- kode-gopher sidecar Deployment + sandbox KSA + IAM binding docs.
- Recipe README section on kode-gopher setup (agent-sandbox operator install, SandboxTemplate CR, credential scoping).
- `.agents/mcp.json` update.
- AGENTS.md tier-selection guidance.
- References updated to prefer sensor tools where applicable.

Estimate: ~200 LoC manifests + ~400 LoC docs. ~3 days.

### Phase 4 — Hugo docs + CHANGELOG + design status flip (PR ε.4 of #200)

- New `docs/site/content/docs/reference/tiered-tools.md`.
- CHANGELOG v2.7.0 entry (paired with MCP-OAuth #190 + alert tool #192).
- Design doc status flip.

Estimate: ~250 LoC docs. ~1 day.

**Total**: ~4,050 LoC across 4 PRs, ~13 days of focused work. Larger than MCP-OAuth or the alert tool individually but smaller than shipping all 24 sensors would be.

## Open questions

### 1. Ship Tier 1 as one binary (`k8s-sensors`) or in-process inside `core-agent`

Two shapes:
- **Separate binary + MCP stdio** (current design). Same shape as `k8s-event-watcher`. Ships as a companion sidecar. Independent release cadence.
- **In-process inside `core-agent`** — the daemon imports the sensor packages, exposes them via its own tool surface. No separate sidecar to deploy; fewer moving parts.

Trade-off:
- Separate binary keeps `core-agent` k8s-agnostic (client-go stays out of the daemon's dep graph — meaningful for library consumers).
- In-process is simpler to deploy for the recipe's users.

**Recommendation**: separate binary. The `k8s-agnostic` property matters — `core-agent` is meant to be usable outside k8s, and forcing every user to pull `client-go` (~5MB static, ~500 transitive packages) contradicts that.

### 2. Deploy kode-gopher as a sidecar in the same pod or as a separate Deployment

- **Sidecar in same pod** — stdio between daemon and kode-gopher works via shared volume / FIFO. Localhost networking. Simple.
- **Separate Deployment** — cleaner scaling. Sandbox pool lifecycle independent from daemon lifecycle.

**Recommendation**: sidecar for the starter recipe (simpler getting-started). Document the separate-Deployment pattern in the README for operators who need it (scaling, sandbox pool sizing, cross-cluster).

### 3. Should the recipe's sandbox KSA have `container.developer` (write) or only `container.viewer` (read)

- **Read-only** (current design) — safer default. LLM-authored Go can't mutate the cluster. Writes route through daemon + plan-first.
- **Read-write** — enables LLM-authored fixes (novel diagnostic-plus-fix routines). More powerful, more dangerous.

**Recommendation**: read-only default. Operators who want write capability edit one IAM binding. Documents the risk explicitly in the recipe README.

### 4. Default to Yaegi (fast) or compile (slow, more capable) for kode-gopher

Yaegi ~700ms, compile 5-30s. Yaegi has incomplete stdlib.

**Recommendation**: default to Yaegi; kode-gopher's framework already falls back to compile when Yaegi can't handle the code. Explicit `--compile-mode` override in the recipe for operators who need reproducibility.

### 5. Prompt guidance for tier selection — how does the agent decide which tier to use

The base AGENTS.md addition (see "Agent selection convention") is what steers the model. Question: should we ALSO expose a helper tool like `list_sensors()` that returns the Tier 1 catalog with descriptions, so the LLM can programmatically enumerate what's pre-built vs. what needs authoring?

- **Helper tool** — more discoverable; agent can be sure without reading system prompt carefully.
- **Prompt guidance only** — simpler; leverages the model's tool-list awareness.

**Recommendation**: helper tool. MCP's `list_tools` gives us this natively for free — no code needed, just documentation pointing the agent at it.

### 6. Fallback when kode-gopher is unavailable

Recipe should work in three deployment shapes:
- Both Tier 1 + kode-gopher present.
- Tier 1 only (kode-gopher not deployed).
- Neither (falls through to GKE MCP + LLM parsing raw kubectl output).

The reference-file guidance should be written to handle all three cleanly — "invoke `log-condenser` if available, else `execute_go_code`, else raw `kubectl logs`."

**Recommendation**: yes, three-way fallback in every reference. The v2.7 recipe ships with both by default, but operators who deploy only one aren't broken.

### 7. Pinning kode-gopher's pre-alpha status

kode-gopher is pre-alpha. What version do we pin against?

- **Pin to a specific commit/tag** — reproducible, but requires manual bumps as kode-gopher evolves.
- **Track `:latest`** — always-current, but breaking changes in kode-gopher break our recipe.

**Recommendation**: pin to a specific tag in the recipe's manifests. Bump the pin in a small recipe PR when kode-gopher ships new features we want. Document the pinning + bump process in the recipe README so operators understand the coupling.

### 8. Should we upstream anything to kode-gopher

Two candidates for upstream contribution:
- Yaegi feature completeness (making the fast path work for more stdlib coverage).
- Separate-KSA credential source (the read-only-sandbox pattern we're deploying).

**Recommendation**: use kode-gopher as-shipped for v2.7. If our recipe uncovers substantive gaps, upstream them as separate contributions after v2.7 stabilizes. Don't block our release on upstream work.

## Security considerations

- **Sandbox pod isolation.** kode-gopher runs LLM-authored code inside a Kubernetes pod with gVisor isolation on GKE Autopilot. Escape is possible via Yaegi/compile-runtime bugs but the attack surface is well-understood.
- **Sandbox KSA scoping.** Explicit read-only by default (§Credential scoping). Recipe README calls this out with the specific IAM bindings.
- **LLM-authored code executes with sandbox privileges, not daemon privileges.** The tier separation is what makes this safe. Fix actions must route through the daemon's plan-first-gated MCP calls; they can't be smuggled through `execute_go_code`.
- **Data exfil surface.** kode-gopher's sandbox has network access to the k8s API and to any GCP services the sandbox KSA can reach. LLM-authored code could conceivably write cluster data to an external endpoint via `net/http`. Mitigation: sandbox pod's NetworkPolicy restricts egress to k8s API + specific GCP endpoints only. Recipe manifests include a default NetworkPolicy.
- **Cost surface.** kode-gopher compiles Go per invocation (in full-compile mode). Cache invalidation attacks (LLM crafts snippets that force cache misses) would drive up build cost. Mitigation: rate-limit `execute_go_code` calls per session; audit anomalous invocation patterns.
- **Prompt injection into the sandbox.** LLM-authored code IS influenced by the LLM's input, which can carry attacker content. But the sandbox's IAM + NetworkPolicy bound what any hostile code can actually do. Same defense-in-depth model as any other agent-authored-action pattern.

## Out of scope (deferred to v2.8+)

- **All 16 additional sensors from the 24-tool matrix** — kode-gopher covers them dynamically for v2.7. If usage data shows a specific one is called often enough to justify pre-building, it graduates to Tier 1 later.
- **Fine-grained per-invocation IAM.** Sandbox KSA gets a fixed permission set; per-call IAM Conditions is v2.8+ if operators demand it.
- **Non-Go languages via sandbox.** kode-gopher is Go-only. Other-language sandboxes are separate design work.
- **Cross-cluster sandbox pool.** Sandbox runs in the daemon's cluster; multi-cluster tool execution matches the multi-cluster posture already established (per-cluster daemon deploys).
- **Sandbox-authored self-healing.** Fix actions still route through the daemon + GKE MCP + plan-first. LLM can propose fixes as Go code but can't execute writes from the sandbox.

## Dependencies and related work

- **[kode-gopher](https://github.com/gke-demos/kode-gopher)** — the sandbox-code execution primitive. Pre-alpha, Apache 2.0.
- **[sigs.k8s.io/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox)** — kode-gopher's execution substrate. K8s SIG project; provides SandboxClaim + SandboxTemplate + SandboxWarmPool CRDs.
- **[Cloudflare Code Mode](https://blog.cloudflare.com/code-mode/)** — the original pattern kode-gopher adapts.
- **[#186](https://github.com/go-steer/core-agent/issues/186) v2.6 k8s-event agent** — the existing consumer of Tier 1 sensors (via reference-file updates in ε.3 of this design).
- **[#190](https://github.com/go-steer/core-agent/issues/190) MCP-OAuth** — orthogonal but complementary; kode-gopher's MCP interface uses stdio (no OAuth needed), so #190 doesn't gate this.
- **[#192](https://github.com/go-steer/core-agent/issues/192) alert tool** — orthogonal; escalation goes through the alert tool regardless of tier for diagnostic sourcing.
- **Proposed scheduled-ops primitive** — complements this: the scheduled sweeps (`disk-orphan-scout`, `lb-ghost-buster`, `stale-object-sweeper`, `ip-space-monitor`) are Tier 2 (kode-gopher-generated) executed on schedule.

## When this lands

- Phase 1 (top-3 sensors): ~4 days
- Phase 2 (remaining Tier 1): ~5 days
- Phase 3 (kode-gopher recipe integration): ~3 days
- Phase 4 (docs + CHANGELOG): ~1 day

~2 weeks of focused work across 4 PRs. Ships in v2.7 alongside MCP-OAuth (#190/#191) + alert tool (#192/#193) + scheduled-ops primitive. Combined, v2.7 becomes a substantive release: multi-target escalation, OAuth-authenticated MCP consumption, scheduled autonomous operations, and tiered specialized tooling. That's the release that closes the "kube-agents parity + our unique substrate advantages" gap identified in the recent competitive read.
