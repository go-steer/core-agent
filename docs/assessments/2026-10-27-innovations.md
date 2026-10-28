# Where core-agent is differentiated: innovations at v3.0.0 (2026-10-27)

**Method.** Two agents worked in isolation from each other:

1. One inventoried core-agent's distinctive mechanisms from code, CHANGELOG and
   design docs at `67c1b7ac`. It counted a mechanism only if it found it in
   `pkg/`, `cmd/` or `dev/`, not just in a design doc.
2. The other researched the field from current official docs and changelogs as of
   2026-10-27, and was barred from reading this repo. The field covered: Claude
   Code (CLI, Agent SDK, cloud), Google Antigravity 2.0 (app, `agy` CLI, SDK),
   Gemini CLI, OpenAI Codex (CLI, app-server, cloud), ADK Go 2, OpenHands SDK,
   Goose, Aider, Cursor Cloud Agents, LangGraph, and the OpenAI Agents SDK with
   Temporal.

The isolation was deliberate: the comparison should not be anchored on how we
see ourselves. The synthesizer matched the two reports and spot-checked the
core-agent side in code.

**Caveats.**
- Competitor claims are only as good as their public docs. Closed-source products
  may do things internally that they don't document. "Nobody documents X" is the
  strongest claim this document makes, never "nobody does X".
- Some competitor details came from third-party sources (marked in the research);
  the matrix below uses only the documented ones.
- Companion document:
  [`2026-10-27-v3-good-bad-ugly.md`](2026-10-27-v3-good-bad-ugly.md). Several
  innovations below have caveats there, and this document links to them rather
  than hiding them.

---

## The one-paragraph answer

Every other product in this comparison is a **developer-at-the-keyboard** agent
that is growing background modes. Claude Code has `--bg` and Remote Control.
Antigravity has Remote Control and sidecars. Codex has the app-server and cloud.
core-agent was built the other way around: as an **agent that runs unattended,
for hours, on a cluster, with nobody watching**, and that a human attaches to
afterwards.

Nearly all of its differentiated work sits in the gap the keyboard-first tools
never had to close:
- budgets that refuse *before* spending, per turn as well as per session;
- approvals that expire and page someone;
- halts that survive a pod roll;
- sessions in Postgres that a fleet of daemons can continue safely after a crash;
- an open, versioned protocol for whoever attaches later;
- a test regime that grades what the agent did to the world, not what it said.

It is also, as far as the research found, the only **open-source, Go-native,
multi-provider agent harness you can embed as a library**.

Where the field converged on the same answer, often the same month, we say so.
Where the field is ahead, we say that too.

---

## Tier 1: differentiated (no peer documents an equivalent)

### 1. Cost ceilings that refuse before spending, per turn, plus an unpriced-model refusal

- **What core-agent does:**
  - Per-turn and per-session USD ceilings. A per-turn trip ends that turn; three
    trips in a row (`maxConsecutiveTurnCeilingTrips`, `pkg/agent/cost_ceiling.go:482`)
    escalate to a session halt.
  - Each request's estimated input is priced *before* it is sent, with
    reservation and settlement (`pkg/agent/cost_preflight.go`).
  - Subagent budgets bind even when the parent keeps no tracker.
  - A model nobody prices would cost "$0" to every ceiling. So a ceiling over an
    unpriced model is **refused at startup, at `/model` and at spawn**, with no
    fallback rate (`cmd/core-agent/guardrails.go`).
  - Unattended runs get a $10 session ceiling by default.
- **The field:**
  - No product documents a per-turn cost ceiling.
  - Claude Code's `--max-budget-usd` is per run and print-mode only, enforced on a
    client-side estimate, and documented as able to overshoot.
  - Codex `/goal` has a soft token budget.
  - Antigravity and Gemini CLI document no USD budget.
- **Caveat:** after a resume, the multi-session rebuild forgets subagent spend
  (good-bad-ugly **R1**). The property holds inside one process lifetime and
  needs that fix to hold across one.

### 2. Approvals that expire, notify and fail closed

- **What core-agent does:**
  - `approval_timeout`: an expired prompt fails as `ErrPromptExpired`.
  - `approval_notify`: alerts a target when a prompt opens with nobody attached.
  - `approval_notify_after`: alerts when an attached client sits on a prompt.
  - Deny-with-reason, where the operator's words land in the refused call's
    result.
  - `escalation: refuse` for deployments where nobody will ever answer.
  - Turn-scoped refusal memory (`pkg/permissions/turnrefusal.go`): an identical
    refused request isn't re-asked for the rest of the turn.
- **The field:** the research matrix row "approval timeout / auto-deny" is **N for
  every product**. Claude Code keeps permission prompts open indefinitely.
  Antigravity and Codex document nothing.
- **Caveat:** in core-agent the default is still "wait forever", even unattended
  (**R5**). The mechanism is unique; the default should change.

### 3. Fleet-safe crash continuation on a SQL eventlog

- **What core-agent does:**
  - A durable eventlog on SQLite, Postgres or MySQL, layered on ADK. The index
    row is written *inside ADK's own transaction* through a GORM callback
    (`pkg/eventlog/atomic.go`, #976).
  - Tool-call tail repair: a synthetic durable response for any call a SIGKILL
    left unanswered, which tells the model the call may or may not have taken
    effect (`pkg/agent/tail_repair.go`). It never re-executes the call.
  - Auto-continue classifies an interrupted tail and resumes it. It is
    crash-loop-safe through a write-ahead boot log (≥3 boots with continuation
    attempts in 10 minutes disables auto-continue for that boot) and per-session
    caps.
  - Fleet-aware refunds: a daemon that loses the lock doesn't burn the shared
    cap.
  - Durable continuation *claims*, so two daemons can't both continue one tail
    (#977).
- **The field:**
  - Coding harnesses store sessions as local JSONL or SQLite. Claude Code's
    supervisor restarts crashed processes, but sessions don't survive a reboot.
  - Postgres-class durability exists only in *libraries*: LangGraph
    checkpointers, the OpenAI Agents SDK with Temporal.
  - No harness documents self-continuation across a fleet that shares a
    database.
- **Caveat:** general cross-daemon exclusion is not wired. `SessionLock.Lost()`
  has no consumer (**R3**), so today's honest HA story is "single replica, with
  safe restart", not active-active.

### 4. Guardrails scoped by what stopping costs, durable across restarts

- **What core-agent does:**
  - Every guardrail signal carries two orthogonal labels: `Severity` (how sure
    the signal is) and `AlertScope` (what stopping it costs, turn or session)
    (#1090).
  - A turn-scoped critical cuts only that turn. Three consecutive turn cuts
    escalate to a session halt (`pkg/agent/watchdog.go:815`).
  - The same ladder governs the per-turn cost ceiling and the refusal storm.
  - Halts are durable eventlog facts, restored lazily at the first `Run`, so a
    crash or pod roll cannot hand a runaway loop a fresh budget (#643, #1258).
  - On the wire they are `guardrail-trip` frames, distinct from `turn-error`
    (protocol 1.13).
- **Why it exists:** the approval-gate drill (#1068 → #1090). One human "no" made
  the model retry, and the only lever that fired was session-scoped, so a single
  denial took an unattended daemon offline until a human came back.
- **The field converged on part of this.** Claude Code auto mode pauses after 3
  consecutive or 20 total denials. Codex auto-review interrupts the turn after 3
  consecutive or 10 of the last 50. Those are the same instinct as our refusal
  storm.
- **What nobody else documents:**
  - the severity/scope split as a general rule across watchdog, cost and refusal;
  - turn-to-session escalation;
  - **persisting kill-switch state across restarts**.
- **Caveat:** the coupling between these mechanisms is the "ugly" item **U3**.
  The concept is right; the implementation wants a single state machine for turn
  outcomes.

### 5. An open-source, Go-native, multi-provider harness you can embed

- **What core-agent offers:**
  - A Go library on ADK v2: `config.DefaultConfig`, `models.Resolve`, `agent.New`
    and `Run` (about four concepts to start).
  - Providers: Gemini (API and Vertex), Anthropic (native adapter, API and
    Vertex), plus OpenAI, xAI, vLLM, SGLang, Ollama and Vertex MaaS through
    core-models profiles.
- **The field:**
  - Antigravity's CLI is Go but closed-source.
  - ADK Go is a framework without a harness (no permissions, coding tools,
    budgets, pricing, loop guards or attach).
  - Claude Code's and Codex's SDKs wrap a binary from Python or TypeScript.
    OpenHands and the OpenAI Agents SDK are Python.
  - Claude Code is Claude-only and Gemini CLI is Gemini-only.
- **Caveat:** the v3.0 safety *defaults* live in `package main`, not in the
  library (**A2**), and minor releases have broken the API without notice
  (**A1**). The embeddability claim needs both fixed to be fully honest.

### 6. A watchdog that reads outcomes, and tells the model

- **What core-agent does:**
  - Eight deterministic signals, with no LLM calls.
  - The later detectors read *outcomes* rather than call shapes:
    - no-op streak (results that each declare they changed nothing);
    - no-new-state (payloads the turn has already seen);
    - tools-without-text (12 calls with no word to anyone, threshold measured
      against a recorded corpus).
  - Argument-keyed detectors share one evasion: vary the arguments.
    tools-without-text "compares nothing", so the only way past it is to tell
    someone something.
  - Feedback mode injects model-facing `Guidance` as a `[watchdog]` block, so the
    party making the looping call finds out. Enforce mode is the default when
    unattended.
- **The field:** OpenHands' StuckDetector is the one real peer. It matches
  repeated action/observation, action/error, monologue and ping-pong patterns,
  and is on by default. Claude Code and Codex count classifier denials and
  restart stalled agents, but document no repeated-action detection.
- **Differentiators:** detectors that read outcomes, the model-facing feedback
  channel, and durable enforcement.

### 7. Evals graded against the world, with a baseline that must score zero

- **What core-agent does:**
  - Checks read a *witness the world wrote*, such as a kubectl shim's own log of
    what it was asked, instead of the agent's transcript.
  - Every case runs twice, one flag apart (`--no-builtin-tools`). The no-access
    tier must score zero, which proves the prompt didn't leak the answer.
  - Preconditions are checked against the agent's own startup report. When one
    fails, the verdict is *indeterminate* (exit 2), not green.
  - The corpus is replanted real incidents.
  - Graders ship self-tests that run in presubmit.
  - Auto mode's approver has a labelled 34-case corpus (`dev/evals/approver`).
- **The field:** `claude plugin eval` independently arrived at **ablation**
  (with and without the plugin), which is our two-tier idea. Nobody documents
  grading against external world state, or indeterminate-is-not-pass.
- **Caveat:** the *live* v3.0 drill evidence is not reproducible by anyone but
  its author (**T1**). The eval method is differentiated; the evidence trail
  needs archiving and countersignature.

### 8. Tool schemas treated as checked prompt text

- **What core-agent does:**
  - `ModelFacingBans` sweeps every registered tool's description *and* marshalled
    arg schema for banned phrasing ("generously", "one-paragraph", "completion
    summary"…).
  - A meta-test (`TestEveryToolRegisteringPackageHasASweep`) fails if any package
    that calls `functiontool.New(` lacks a sweep.
  - "Unadvertise what you can't do": descriptions name only tools this build
    actually registered (`whenTool(gate.HasTool(...))`).
- **Why it exists:** #905. A `jsonschema` arg tag, "one-paragraph completion
  summary", shaped the model's visible output and outranked a persona that
  forbade it.
- **The field:** nobody documents this, but closed vendors may do it internally.
  It is publicly unusual, and it is cheap for any harness to adopt.

---

## Tier 2: distinctive in depth (the field has the feature; our version goes further)

### 9. A typed subagent return contract

- **What core-agent does:**
  - One return tool, `return_result`, with an alias net for the near-synonyms
    models invent.
  - The contract lives in the subagent's system instruction, so it holds on every
    termination path.
  - Typed stop reason: `natural`, `max_steps`, `budget` or `error`.
  - Substantive `FinalText` carried across `Resume`.
  - Delegations return the **child's tool calls**. Trajectory analysis showed
    parents re-issuing their child's reads in 8 of 15 runs.
  - Mandatory disclosure when a parent absorbs a failed delegation (#1036).
  - Lineage guard: a subagent can't spawn itself or an ancestor.
- **The field:** everyone has subagents. Claude Code workflows offer
  schema-validated JSON returns and label subagent reports as carrying "no
  authority from the user". That is the same insight as our trust framing, and
  Claude Code got there independently.
- **What's ours:** returned call lists, absorbed-delegation disclosure, and the
  incident-driven alias net.

### 10. A versioned, open attach protocol for an unattended daemon

- **What core-agent offers:** HTTP/SSE at protocol 1.20.0.
  - Major-version negotiation, typed frames, and `prompt_id` correlation from
    inject to terminal frame.
  - Operator controls:
    - inject vs queue (`wake:false`);
    - interrupt, pause and interrupt-and-hold. An inject queues; only a resume
      opens the gate (#878).
    - `/perms/stream` and `/perms/respond` with a reason;
    - runtime permission-mode switching;
    - guardrail reset;
    - per-subagent event streams.
  - An A2A agent card.
  - A multi-session daemon with per-session ACLs.
  - Three clients (web UI, remote TUI, in-process TUI) on one core-tui library.
- **The field:** **Codex's app-server is the real peer.** It is open-source
  JSON-RPC with `turn/steer`, `turn/interrupt`, approval requests and
  multi-client subscriptions, though its WebSocket transport is labelled not
  production-ready. Claude Code and Antigravity use proprietary, account-bound
  relays, and Claude's needs a subscription.
- **What's ours:** guardrail and approval semantics as first-class frames, the
  inject/hold distinction, and semver discipline with conformance tests.
- **Caveat:** multi-session isolates conversations, not execution (**S4**).

### 11. The agent cannot approve its own prompts

- **The threat:** the attach bearer token answers permission prompts. So an agent
  that writes a test, which an approved `go test` then runs, could curl the
  daemon to approve its own pending prompt.
- **The defences:**
  - The token is scrubbed from every child environment (bash, hooks, MCP stdio).
  - The daemon makes itself non-dumpable, so `/proc/$PPID/environ` can't leak it
    (`pkg/childenv/protect_linux.go:38`).
  - The token can come from a file.
  - A daemon whose agent has `bash` refuses to start an unauthenticated loopback
    listener (3.0, breaking).
  - The bearer table is unreadable by every tool.
  - Writing `.agents/config.json` or `mcp.json` needs an interactive approval
    that no mode can satisfy.
  - "Allow always" on a multi-session daemon is admin-only (#1179).
- **Honest framing:** a local approval endpoint is partly *our own* architecture's
  creation. Tools without a local HTTP approval surface don't have this exact
  hole. The distinctive part is that the threat was written down and closed
  end-to-end.
- **Caveats:** the control-plane list is too short (**S3**), and there is no OS
  sandbox (**U1**). Every peer except Aider ships one.

### 12. Auto mode with eligibility decided in code

- **The field converged here.** Claude Code (Sonnet classifier, tool results
  stripped, no-verdict means deny), Codex auto-review (open `policy.md`),
  Antigravity Auto (Preview) and OpenHands' LLM analyzer all shipped an approver
  model in 2026. It is table stakes now.
- **What's ours:**
  - Only recipe-eligible calls ever reach the approver (patterns or the bundles
    `coding`, `k8s_read`, `github`). A code-side never-list always goes to a
    person.
  - Allow-once only.
  - Oversized args escalate instead of being truncated.
  - It requires `approval_timeout`.
  - Verdicts are written to the eventlog, and the approver's cost is billed to
    the turn.
  - It ships with a published labelled eval corpus.
- **Caveat:** #1348 records that one approver-instruction wording flipped two
  deny cases to allow, 4/4 each, on Haiku. Inside the eligible set the gate is a
  prompt, which is why it is still labelled experimental.

### 13. Designing for the context manager's own failure modes

- **Compaction is universal.** What isn't: a context budget enforced *inside* a
  turn. It counts measured usage plus an estimate of unmeasured tool output,
  compacts mid-turn, and cuts at 95% instead of taking a provider 400 (#975).
- **Degraded modes:** an unknown window assumes a small one instead of "never
  full", and a summarizer that fails in a 429 storm falls back to mechanical
  truncation. Both are announced on the eventlog (#974).
- **Digest of oversized tool output** (`pkg/digest/`):
  - a structural JSON pruner that never truncates identifier-shaped keys;
  - the raw payload kept for `retrieve_raw`;
  - savings reported per tool.
- **The field:** Claude Code leads on cache *visibility* (hit-rate diagnostics,
  likely cause of misses). Both products allow contracted-rate pricing overrides
  (Claude Code `modelPricing`, ours `pricing.json` layers).

### 14. Proof as a release criterion, and governance of an agent-written codebase

- **v3.0's boxes were each gated on a drill or soak with a checked-in grader:**
  - GKE drill scenarios A–D;
  - an eight-hour soak with PASS / FAIL / NOT EXERCISED / UNKNOWN per clause;
  - **A7 self-development**: core-agent, as a GKE daemon, took a seeded issue and
    opened a PR that passed its own review gate.
- **The repository is agent-written,** and gates keep it honest:
  - a required no-agent-attribution check scanning every branch commit;
  - a review gate demanding proof that a regression test fails on pre-fix
    behaviour;
  - a bash lexer asserted sound against real bash, for the config-pin gate;
  - a core-tui capability gate;
  - a complexity ratchet;
  - a pre-turn pipeline tested under permuted orders.
- **The field:** frontier vendors say their agents build themselves. A checked-in
  grader and rig for it is unusual.
- **Caveat:** this is also the self-referential loop in **U4**, and the reason
  the harness now outweighs the product (**T2**). It is a differentiator to keep
  and to budget.

### 15. Smaller originals worth naming

- **Skill framing trailer** (`pkg/skills/framing.go`): every skill body is told it
  may not change the task. The trigger was #711, where a skill re-derived the task
  and spent $1.33 against a $0.26 baseline.
- **Skills `requires:`**: a skill whose capability (shell, kubectl) this build
  lacks is withheld rather than advertised.
- **Plan-first as an artifact precondition:** writes, shell and spawning are
  denied until `record_plan` writes a plan, while reads stay open. It enforces
  ordering, not consent; the plan is self-attested.
- **`wait_and_verify`:** bounded, shell-free polling for closed-loop remediation
  in distroless images.
- **Persona library:** `@include builtin:core|coder|sre`, graded by a persona
  eval.

---

## Where we converged with the field

This list is a sign the instincts were right, not a claim of originality:
- classifier-gated auto approval;
- denial-count circuit breakers;
- labelling subagent output as untrusted;
- with/without ablation in evals;
- background daemons with crash restart plus remote control;
- MCP, skills and hooks;
- AGENTS.md;
- plan mode;
- headless `-p`.

The research classifies all of these as **table stakes** in late 2026.

## Where the field is ahead of us

| Gap | Who has it | Our status |
|---|---|---|
| OS sandbox for shell and tools | Claude Code, Antigravity, Gemini CLI (most options), Codex, OpenHands, Cursor (VM) | **None** (#651 open) |
| Worktree / VM isolation per subagent | Claude Code, Antigravity, Cursor | Refuse a second background writer (#653); no isolation |
| Workspace trust | Gemini CLI (trusted folders) and others | **None** (S2) |
| Session resume UX (`--resume`, `--continue`, fork) | Claude Code, Antigravity, Codex | One global `"default"` session locally (P1) |
| Rewind / checkpoint restore of edits | Gemini CLI (`/restore`), Codex (thread revert) | None |
| Hook breadth | Claude Code: 33 events, 5 handler types, fail-closed | Narrower |
| Cache-hit diagnostics | Claude Code | Savings shown, no miss diagnosis |
| Artifacts, browser agent, inline comments | Antigravity | None |
| Image input, IDE integration | Most | None |
| Remote approval via chat | Claude Code Channels (preview), Antigravity, Codex mobile | `approval_notify` to a target, answered via attach |

The first three are the ones that matter for the "point it at an untrusted task"
story. They are the same items as good-bad-ugly **U1**, **S2** and **S4**.

---

## Most distinctive, ranked

1. **Ceilings that refuse before spending, per turn, plus the unpriced-model
   refusal.** No peer documents a per-turn ceiling.
2. **Approvals that expire, notify and fail closed.** No peer documents any
   approval timeout.
3. **Fleet-safe crash continuation on a SQL eventlog.** Durability that only
   libraries offer, inside a harness, with distributed-systems discipline.
4. **Guardrails scoped by what stopping costs, and durable across restarts.**
   Peers have denial breakers; nobody persists the halt.
5. **An embeddable, open-source, Go, multi-provider harness.** A category of one.
6. **An outcome-reading watchdog with a model feedback channel.** OpenHands is the
   only peer, and it reads call patterns.
7. **Evals graded against world state, with indeterminate-is-not-pass.** Claude
   Code shares the ablation idea, not the witness.
8. **Tool schemas as checked prompt text.** Publicly unique, and cheap to copy.
9. **An open, versioned attach protocol with guardrail and approval frames.**
   Codex's app-server is the one peer.
10. **The self-approval threat, written down and closed.**

**The pattern:** our differentiation is concentrated where an agent runs without
a human present, and our gaps are concentrated where an agent runs on untrusted
input with a human present (sandbox, trust, isolation, resume). The v3.1 list in
the companion document closes most of the second set without giving up any of the
first.
