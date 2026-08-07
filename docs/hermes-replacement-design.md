# Replacing Hermes: `core-agent` as the full `kube-agents` runtime

Design doc for the v2.9 goal of **fully replacing Hermes** as the
runtime under [`kube-agents`](https://github.com/gke-labs/kube-agents)
— not just the interactive Platform Agent, but the whole runtime
surface: interactive provisioning, periodic scans / cron, event
triage, chat (Slack + Google Chat), and GitOps — while keeping
`core-agent` distroless.

**Status:** design (2026-08-05). Target: **v2.9**. Supersedes the
investigative fit assessment in `kube-agents-platform-fit.md`
(2026-06-03), which is now partly stale (it predates the k8s-lookout
extraction and had an incomplete picture of Hermes — see
[§2](#2-what-hermes-actually-is-corrected-picture)). Sibling design
inputs: `docs/k8s-event-agent-design.md`,
`docs/scheduled-monitoring-design.md`, `docs/alert-tool-design.md`,
`docs/peer-registration-design.md`, `docs/mcp-oauth-design.md`,
`docs/mcp-credential-resolution-design.md` (per-caller credentials,
#106/#204), `#142` (identity-gateway propagation), and k8s-lookout's
`DESIGN.md` + `docs/agent-sink-design.md`.

## 1. Motivation

The June fit assessment concluded core-agent could stand in for the
**interactive** Platform Agent at "80% reusable today." Since then the
stack has moved decisively toward being able to replace *all* of
Hermes, not just the front door:

- **Triage was extracted and productized.** `k8s-event-watcher` left
  this repo (#393) and ships as `go-steer/k8s-lookout` — a hardened,
  distroless, per-cluster sentinel with a frozen two-verb daemon
  contract (Signal Schema v1). The reactive half of Hermes' triage is
  effectively already re-implemented and better.
- **The scheduled-ops gap has a filed design** (#202): a
  `cmd/core-agent-cron` companion sidecar that fires the daemon's
  inject contract on a cron schedule — the proactive counterpart to
  the watcher. It slipped v2.7; v2.9 is where it lands.
- **The remaining true gaps are now visible and small in number:**
  chat, and GitOps execution. Everything else is either shipped, a
  companion we've already designed, or skill/config authoring.

The strategic framing: Hermes is a ~monolith (NousResearch
`hermes-agent`) that bundles a gateway, a cron scheduler, a kanban
delegation board, chat adapters, a credential-isolation stack, and a
GitOps clone manager into one Debian image. **core-agent takes the
opposite shape** — a small distroless *brain* (daemon + sessions +
MCP + subagents) with each operational concern pushed into a separate
companion that speaks one contract. That's not a limitation to work
around; it's the architecture that lets us stay distroless.

## 2. What Hermes actually is (corrected picture)

The June doc modeled Hermes as "portable markdown content + an
OpenAI-style Responses API on :8642 + a peer registry." That was
incomplete. Hermes here is the **NousResearch `hermes-agent`**
framework, consumed by kube-agents as an upstream **base container
image** (`nousresearch/hermes-agent`) with personas, config, skills,
cron jobs, MCP servers, plugins, and Dockerfile patches layered on
top. The runtime primitives kube-agents actually depends on:

| Hermes primitive | What it does in kube-agents |
|---|---|
| `hermes gateway run` | multi-platform chat gateway (Slack/gChat) — the process the pod runs |
| Multi-profile-in-one-pod | chat / platform / per-cluster profiles co-located, discovered at runtime |
| Kanban delegation board | async delegate-and-stream-progress-back-to-thread (the interactive model) |
| Cron scheduler (`cron/jobs.json`) | 12 governance jobs; LLM path **and** `no_agent` script path + wake-gate |
| Plugin hook bus (`pre_gateway_dispatch`, `pre_llm_call`) | incident-context reply enrichment, slash commands, onboarding |
| REST API server (`/api/sessions`, `/api/sessions/{id}/chat`) | the daemon the triage bridge calls |
| Bundled `google_chat` + `slack` adapters | inbound gChat (Pub/Sub) + Slack (Socket Mode) |
| Credential-isolation stack | Envoy proxy sidecar + same-named CLI shims + Slack/Chat relay monkey-patches + build-time leak guard |
| GitOps clone manager | lease-per-operation git clones on a shared PVC + Minty/KMS token minting |

Two findings matter most for scoping:

1. **The triage migration is already in flight.** kube-agents' own Go
   `k8s-event-watcher` describes its target as *"the core-agent
   daemon"* and speaks our exact contract (`POST /sessions`,
   `POST /sessions/{id}/inject`, `X-Asserted-Caller` /
   `proxy_identities`). Today it points at a Python shim
   (`session_kv_server.py`), but it was written against us. k8s-lookout
   is the productized version of that same watcher.

2. **Hermes' hardest-to-replicate machinery exists only because it
   runs real CLIs in-sandbox.** The Envoy proxy + CLI shims + relay
   monkey-patches + leak guard are an entire subsystem whose sole job
   is to safely run `git`/`kubectl`/`gcloud`/chat-SDKs inside the
   agent container. We don't have that container, so we don't need
   that subsystem — creds live in MCP servers and companions instead.

## 3. The unifying insight: one contract, many companions

Everything Hermes does at runtime funnels through *its* daemon
(`session_kv_server.py` + the Hermes REST API). core-agent already
ships the equivalent as a first-class, authenticated surface:

```
POST /sessions                     create a (per-incident / per-job / per-thread) session
POST /sessions/<sid>/inject        queue a message → wakes the turn
POST /sessions/<sid>/wake          out-of-band wake
GET  /sessions/<sid>/events (SSE)  operator event stream (progress, tool calls, results)
+ Bearer auth, X-Asserted-Caller proxy identity, ACLs, session resume
```

Every Hermes subsystem maps onto **a companion process that speaks
this contract** — inbound via `/sessions`+`/inject`, outbound via the
SSE event stream. This is exactly the k8s-lookout pattern, generalized:

```
                        ┌───────────────────────────────┐
                        │   core-agent daemon (BRAIN)    │  distroless, nonroot
   inbound              │   sessions · subagents · MCP    │
   /sessions + /inject  │   plan-first · ACLs · eventlog  │
 ───────────────────────▶   POST /sessions /inject /wake  │
                        │   GET  /events (SSE) ───────────┼──── outbound
                        └───────────────────────────────┘
        ▲            ▲              ▲                 │
        │            │              │                 │ SSE / notification
   k8s-lookout   core-agent-cron  chat-gateway ◀──────┘
   (triage)      (scheduled-ops)  (Slack + gChat)
   SHIPPED       #202, designed   NEW for 2.9

   Action surface (all HTTP MCP, OAuth-scoped — no in-container CLIs):
     GKE MCP (container.googleapis.com/mcp) · GitHub MCP · Slack MCP (outbound)
```

The brain stays k8s-agnostic, chat-agnostic, and CLI-free. Each
companion owns its own credentials and its own release cadence.

## 4. Capability mapping (the four runtime use cases + GitOps)

| Hermes runtime use case | core-agent stack | Status | 2.9 work |
|---|---|---|---|
| **Interactive provisioning** — chat → delegate → specialist w/ GKE MCP | daemon + multi-session + subagents + peer registry + **GKE MCP already wired** (`examples/gke-troubleshoot-agent`) | provisioning pattern SHIPPED; front-door missing | chat-gateway; port skills; optional `call_peer` |
| **Periodic scans / cron** — 12 governance jobs | `schedule_next_turn` + `ExitOnDeferScheduler` (paced loops); no declarative recurrence | primitive SHIPPED; declarative layer designed not built | **#202 core-agent-cron companion** |
| **Triage** — events → per-incident session → chat thread | **k8s-lookout** (ingest/dedup/storm/route/enrich/open/close), frozen contract | reactive half SHIPPED & hardened | chat round-trip (via chat-gateway); alert-tool impl |
| **Chat** — Slack (Socket Mode) + gChat (Pub/Sub) | none native (outbound-only via MCP) | NOT PRESENT | **chat-gateway companion (long pole)** |
| **GitOps** — PR-based fixes, no live mutation | none; lookout drafts PR payloads but never executes | NOT PRESENT | **GitHub MCP + remediation skill** |
| **Credential isolation** — sandbox never holds tokens (Envoy proxy + shims + relay) | per-caller credential resolution + proxy identity; distroless brain runs no CLIs | substrate SHIPPED (Caller/proxy); resolution DESIGNED (#106/#204/#142) | **W0 — per-caller resolution + Auth Manager** |
| **Distribution** | distroless static, nonroot, pure-Go | SHIPPED | keep it — see §6 |
| **Models** | Gemini/Vertex/Claude/Vertex-Claude; default `gemini-3.6-flash` | SHIPPED | LiteLLM/OpenAI parity only if a consumer needs it |

## 5. Workstreams for v2.9

### W0 — Per-caller credential resolution (keystone)

This is the load-bearing dependency for multi-user chat and
GitOps-as-the-human, and it is **our analog to Hermes'
credential-isolation subsystem — strictly more powerful** (see §6).
Substrate is partly shipped, the rest is designed:

- **Shipped:** per-turn `Caller` + proxy identity
  (`pkg/auth.CallerFromContext`, `X-Asserted-Caller` /
  `ProxyByFromContext`) from multi-session (v2.4); daemon-scope MCP
  OAuth (`google_oauth`, one identity for all callers) from v2.7.
- **Designed, not built:** `docs/mcp-credential-resolution-design.md`
  (#106, re-scoped to v2.8+) — a `pkg/mcp/auth` `CredentialProvider`
  interface + `Registry` with a per-`(provider, caller, scopes)`
  cache, the `auth_manager` (Google Agent Identity, 3LO) and
  `oauth2_direct` providers, and shared named `auth_providers`. **PR
  #204** (open, docs-only) adds the 8 forward-compat guardrails so
  v2.7's daemon-scope OAuth extends additively rather than being
  rewritten. **#142** (open) is the sibling: propagate identity from
  identity gateways (Cloud Run IAM / IAP / Cloudflare / ALB) into the
  Caller so attribution works when the brain is fronted directly.

The chat-gateway (W1) **is** an identity gateway — it authenticates
the human and supplies the Caller. So W0 and W1 are co-requisites for
the multi-user story; W0 also underpins GitOps-as-the-human (W3). If
2.9 scope forces a cut, single-identity chat can ship on daemon-scope
OAuth first, with per-caller 3LO following — but the guardrails (#204)
should land before any new OAuth code so we don't build a wall to tear
down.

### W1 — Chat gateway companion (NEW, the long pole)

A new companion in its own repo, **`go-steer/switchboard`**, mirroring
k8s-lookout's deployment and contract model (separate release cadence,
distroless, coupled to core-agent only through the frozen HTTP contract
+ `core-agent/v2` imported as a library for OTel trace propagation). It
is the single unlock for three of Hermes' use cases at once — the
interactive front door, the triage chat round-trip, and delegation
progress streaming.

**Build-order note:** W1 has *no* blocking dependency on the rest of
this epic. Its entire dependency surface — `POST /sessions` +
`/inject`, `GET /events` (SSE), and `X-Asserted-Caller` /
`proxy_identities` — is already shipped and frozen (v2.4 multi-session;
the same seam lookout uses). Per-caller credentials (W0) resolve
*inside* the daemon's MCP outbound path, which the gateway never
touches — so W0 and W1 are independent tracks that meet at the
already-shipped `X-Asserted-Caller` seam. switchboard can be built and
shipped against the current daemon today; when W0 lands, the identity
switchboard already stamps starts driving per-user token resolution
with zero gateway changes.

- **Inbound:** subscribe to Slack Socket Mode (`slack-bolt`-shape) and
  Google Chat Pub/Sub; map each chat thread → a core-agent session
  (`POST /sessions` on first message in a thread, `POST /inject`
  thereafter); maintain a durable **thread ↔ session routing** table.
- **Outbound:** drain the session's SSE event stream and relay
  progress/results back into the originating thread (this replaces
  Hermes' kanban auto-subscribe streaming).
- **Auth/identity:** Bearer token; stamp the chat user as
  `X-Asserted-Caller` (proxy identity) so ACLs and audit attribute to
  the human, not the gateway.
- **Credentials stay in the gateway**, never in the brain — the
  distroless analog of Hermes' relay isolation, without monkey-patching
  upstream SDKs.
- **Scope calls:** decide Socket Mode vs. Events API for Slack; whether
  gChat inbound is Pub/Sub (matches Hermes) or HTTP webhook; and the
  outbound formatting/threading contract. Outbound *posting* can reuse
  an MCP path, but the inbound gateway + routing table is genuinely new.

Language decision (Go, matching lookout) is the leaning default; the
Slack/gChat Go SDK maturity is the thing to validate first.

### W2 — Scheduled-ops companion (#202, resurrect)

`cmd/core-agent-cron` per the already-filed #202 design: reads a
`jobs.json` (cron/interval + prompt + owner + session-mode), fires the
inject contract per job. Open design points from #202 to settle:
missed-schedule policy, concurrency policy, timezone, and whether to
port Hermes' **`no_agent` script path + wake-gate** (run a script,
only wake the LLM if it emits work) — that pattern has no analog today
and is what keeps Hermes' cheap governance scans cheap.

### W3 — GitOps via GitHub MCP + remediation skill

Distroless-friendly GitOps = the **GitHub MCP server** (branch,
commit, open PR over the API — no local clone, which *eliminates*
Hermes' lease-clone manager and Minty token-minter complexity) plus a
`propose-fix-as-PR` skill. lookout already emits GitOps-PR-draft
payloads (`payload.go`) and the triage prompt already asks for
"two GitOps fix options" — this wires the execution half. No Flux/Argo
in scope (Hermes has none either; downstream CD is the operator's).

### W4 — Alert tool implementation (close the #192 gap)

`docs/alert-tool-design.md` is **merged as design (#193)** but the
tool is **not implemented** (CHANGELOG claims it; code and the doc's
own status say "proposed"). It's the natural escalation target for
both triage and scheduled sweeps ("report findings via alert"). Build
the actual tool. Chat delivery of the alert then routes through the
chat-gateway or Slack MCP.

### W5 — Skill / persona port

Mechanical but meaningful: port Hermes' Platform Agent skills/personas
(`gke-cluster-creator`, `manage-cluster`, `gke-app-onboarding`,
`fleet-audit`, `submit-suggestion`, `github-issue-resolver`, the
governance SOPs) into `.agents/` skills + `AGENTS.d/` overlays. The
June doc showed this is largely drop-in via the v2 instruction loader
(`@include`, `AGENTS.d/`, SKILL.md compatibility).

**Shipped (Phase 0):** `examples/kube-platform-agent/` vendors a
faithful, unmodified snapshot of `agents/platform/` (persona, 10
governance SOPs + inventory scan, all 18 skills) and runs it on the v2
loader + native-HTTP `gke`/`developer_knowledge` MCP + the attach hub,
guarded by a credential-free loader test. Two facts surfaced during the
port that the June "symlink a sibling checkout" sketch missed, and each
becomes a small concrete-consumer-driven framework enabler rather than a
recipe hack:

- `@include`/`AGENTS.d` are confined to the *including file's* scope
  root, so the persona lives at the recipe root to reach a sibling
  `upstream/`. Running a genuinely **unmodified** kube-agents checkout
  (adding nothing to their tree) needs an **external content-root**
  capability (operator-declared trusted roots). Follow-on — designed in
  `docs/external-content-root-design.md` (#600).
- Hermes "profiles" (`platform`/`cluster`) map to core-agent subagents,
  but there is no **declarative subagent** config yet — subagents are Go
  code or runtime `spawn_agent` only. The `cluster` profile lands as a
  declarative subagent (own model + read-only MCP scope). Follow-on —
  designed in `docs/declarative-subagents-design.md` (#599).

Both are general capabilities (useful beyond kube-agents); each has its
own design doc (above), reviewed before code.

### W6 — Small substrate carry-overs (from the June doc)

- **File-backed `PeerRegistry`** (Gap 1, ~50 LoC) — cross-restart peer
  durability for the multi-cluster fleet.
- **`call_peer` built-in tool** (Gap 2, ~150 LoC) — named delegation
  to peer agents; can stay in a recipe MCP server if we prefer.
- `/v1/responses` wire compat (Gap 4) stays **deferred** unless a
  concrete consumer needs it.

## 6. Credentials, identity & distroless posture

These three are one story. Hermes' credential-isolation subsystem
(Envoy proxy sidecar + same-named CLI shims + Slack/Chat relay
monkey-patches + build-time leak guard) exists *only* to safely run
real `git`/`kubectl`/`gcloud`/chat-SDKs **inside** the agent
container. Its property is "the sandbox never holds tokens" — but every
action still runs under **one bot identity**; "who authorized this"
lives in audit prose, not in the credential.

### Our answer is more powerful, not just different

core-agent achieves the same isolation property **by construction**
(distroless brain never runs those CLIs) and adds what Hermes
structurally can't: **per-caller attribution at the token level.**

| Property | Hermes | core-agent |
|---|---|---|
| Tokens kept out of the agent process | Envoy proxy + CLI shims + relay monkey-patch + leak guard | By construction — no CLIs in-image; 3LO tokens fetched just-in-time, in-memory only, evicted on session end |
| Identity of an action | Single bot identity; human in audit prose | **Per turn-originator** — `Registry.Resolve` reads `CallerFromContext`; Alice's token ≠ Bob's |
| Extensibility | Monkey-patch upstream SDKs (brittle) | Typed `CredentialProvider` interface (additive) |
| Coupling | Deep into Hermes gateway/adapter internals | One contact point: `pkg/auth.CallerFromContext` |

The end-to-end flow the chat-gateway unlocks (W0 + W1 + W3 together):

```
alice@ in Slack  ──▶ chat-gateway authenticates her,
                     stamps X-Asserted-Caller: alice@
                 ──▶ daemon session; each turn carries Caller=alice@
                 ──▶ agent calls GitHub MCP
                 ──▶ Registry.Resolve(provider="github_3lo", caller=alice@)
                 ──▶ Auth Manager GenerateAccessToken(subject=alice@)
                 ──▶ PR opened AS Alice, not a shared bot
```

In a shared `#incident-response` channel, Bob's next turn resolves to
Bob's token automatically — the `(provider, caller, scopes)` cache key
isolates them. That's true multi-tenant credential isolation on a
single distroless daemon, which is the thing Hermes' whole relay
subsystem is a workaround for.

### Distroless posture (why it holds, and its cost)

Keeping the brain distroless is what *forces* the cleaner design above,
not an obstacle to it:

- **No in-container CLIs.** k8s/GCP actions → GKE MCP; GitOps →
  GitHub MCP; chat egress → Slack MCP or chat-gateway. Each MCP holds
  its own OAuth-scoped creds.
- **No inbound listeners in the brain.** Chat, cron, and triage
  ingress live in companions; the brain only exposes the authenticated
  session/inject/SSE surface.
- **The `bash` tool is inert in-image** (no shell) — recipes disable
  it and rely on MCP, as `examples/gke-troubleshoot-agent` already
  does.

**Cost to be honest about:** anything not exposed by an MCP server or a
companion is simply not doable in-image. That means new capabilities
arrive as MCP servers/companions, not as "shell out to a CLI." A
`-debug` image variant (shell + curl + kubectl) is noted as deferred
in the Dockerfile and remains out of scope. This is the trade we're
choosing: a smaller, signable, lower-CVE attack surface in exchange for
routing every action through an explicit, credentialed, auditable
integration point.

## 7. Phased plan

1. **Phase 0 — validation recipe + credential guardrails.**
   `examples/kube-platform-agent/` (shipped): a vendored, unmodified
   snapshot of `agents/platform/` on the v2 loader; native-HTTP GKE +
   developer_knowledge MCP; plan-first gate; attach-hub config. Proves
   the skill/persona port (W5) end-to-end with a credential-free loader
   test — **no framework code in the config-only recipe itself.** The
   port surfaced two small framework enablers (external content-root,
   declarative subagents; see W5) that make the recipe run an
   *unmodified* checkout and express profiles-as-subagents; each is a
   concrete-consumer-driven follow-on with its own design doc, not a
   Phase 0 blocker. **Land PR #204 (W0 guardrails) here** so any
   subsequent OAuth code is per-caller-ready. (Extends the June doc's
   Phase 1.)
2. **Phase 1 — chat-gateway MVP (W1) + per-caller resolution (W0).**
   Slack first (Socket Mode), thread↔session routing, inbound inject +
   outbound SSE relay, `X-Asserted-Caller` stamping; land the
   `CredentialProvider`/`Registry` substrate + `auth_manager` so chat
   users' actions resolve to their own tokens. Release-defining;
   everything interactive depends on it.
3. **Phase 2 — scheduled-ops (W2) + alert tool (W4).** #202 companion
   + real alert tool → proactive governance sweeps that escalate to
   chat. Closes the "always-working platform agent" gap.
4. **Phase 3 — GitOps execution (W3, PRs opened as the caller via W0)**
   + Google Chat inbound (W1 second adapter) + `#142` identity-gateway
   propagation + substrate carry-overs (W6). Full parity.

Triage (k8s-lookout) needs no new phase — it's shipped; it only
gains the chat round-trip for free once Phase 1 lands.

## 8. Open questions / decisions

1. **Chat-gateway repo home.** *Decided (2026-08-06):* separate repo
   **`go-steer/switchboard`**, like k8s-lookout — cadence isolation and
   a clean brain dep tree (no chat SDKs). Builds in parallel; no
   blocking dependency on W0 (see W1 build-order note).
2. **`no_agent` / wake-gate parity in W2.** Port Hermes' cheap
   script-first cron path, or accept LLM-per-fire and lean on cheap
   default models? Affects governance-scan cost at fleet scale.
3. **gChat inbound transport.** Pub/Sub (Hermes parity, needs GCP
   plumbing) vs. HTTP webhook (simpler, different setup).
4. **Delegation model.** Reproduce Hermes' kanban async board, or
   express delegation as core-agent subagents + peer calls surfaced
   through the SSE stream? Leaning the latter (no new board primitive).
5. **Coordinate with the kube-agents team** on a multi-runtime story
   vs. shipping the recipe on our side only (carried from June doc).
6. **Credential scope for 2.9 (W0).** Full per-caller 3LO in 2.9, or
   daemon-scope OAuth first with per-caller as a fast-follow? And which
   3LO backend to lead with — `auth_manager` (Google Agent Identity,
   fits GKE/GCP tenants but GCP-coupled) vs. `oauth2_direct`
   (self-managed refresh-token store, provider-agnostic)? Lean:
   guardrails now (#204), `auth_manager` first for the GKE story,
   `oauth2_direct` as fast-follow.

## 9. Out of scope

- KCC / Config Connector glue, GKE Fleet Hub / MCS DNS / VPC peering —
  operator-side infra, not framework (carried from June doc).
- Flux/Argo CD reconciler — Hermes has none; downstream CD is the
  operator's.
- Replacing LiteLLM / OpenAI provider parity — only if a consumer
  needs it.
- Reimplementing FastMCP or Hermes' relay/leak-guard subsystem — moot
  under distroless.
- `-debug` image variant with in-container CLIs.
