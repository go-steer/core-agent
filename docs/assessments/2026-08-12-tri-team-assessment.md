# Tri-team assessment — core-agent as an autonomous harness (2026-08-12)

**Method.** Nine agents in three teams, run as a single workflow against the tree
at `53d2349`. Team 1 (three advocates) argued for the codebase's strengths; Team 2
(three critics) argued the opposite and tested premise-vs-shipped-reality; Team 3
(three arbitrators, high reasoning effort) received both teams' full findings,
spot-checked the code themselves to break ties, and ruled. Every finding below is
grounded in a file:line reference, a doc quote, or command output — the teams were
instructed to ground or drop each claim, and the arbitrators re-verified the
load-bearing ones rather than taking either side at its word.

**Focus areas** (as scoped): code quality/complexity, the new subagent system,
autonomous capabilities, platform-engineering/SRE/troubleshooting fitness, and the
ability to write code. Team 3 additionally hunted for gaps *outside* the project's
stated claims.

**Scope caveat.** Findings about `examples/gke-troubleshoot-agent` and
`examples/kube-platform-agent` should be read against their intent: those recipes
were deliberately kept basic, and were partly an experiment in taking the
`kube-agents` Hermes config as-is and running it on core-agent. That is a fair
defense of the *recipes*. It is not a defense of the *docs*, which market them as
the flagship SRE story without that caveat. See the addendum for the
core-agent-native rebuild.

---

## Bottom line

> **core-agent is a high-quality tool-calling harness, but not yet a first-class
> autonomous harness.** The leaf primitives — permission gate, file toolset, MCP
> layer, test discipline — are genuinely excellent. The autonomous-runtime posture
> is opt-in, non-durable, and the flagship end-to-end recipes do not execute under
> their own constraints.

Where the teams collided, a consistent pattern emerged: **the critic won on
shipped-and-on-by-default reality; the advocate won on architecture-in-principle.**
Both were usually right about the facts and disagreed about framing and severity.

The critic overstated exactly two things, and neither overturns its case:

- `pkg/compose`'s `ReproduceAgent` is at 71.4% coverage, not ~0%.
- Subagent session-row/eventlog concurrency safety is real (`DeriveSessionID` +
  `BranchInjectingService`) and orthogonal to the filesystem-isolation gap.

---

## What is genuinely strong

These survived adversarial review and were conceded by the critics:

- **Test discipline outweighs the production code.** Across `pkg/ internal/ cmd/`:
  64,717 test LOC vs 61,730 non-test LOC; 268 `_test.go` files vs 239 sources.
  (Whole-repo the margin is much thinner — 65,682 vs 65,572 — because `extras/`
  and `examples/` are nearly untested; the ratio above is the core.)
  Coverage: watchdog 98.1%,
  hooks 96.5%, instruction 89.1%, permissions 87.8%, agent 85.5%. `AGENTS.md:100-102`
  makes it a merge rule, and the adversarial-review gate requires a bug-fix test to
  fail on the pre-fix commit.
- **Low-debt, idiomatic Go.** 22 TODO/FIXME/HACK markers and 10 `panic()` calls
  across ~62k non-test LOC; 339 `%w` wrap sites; clean `go vet`; 62 interfaces
  enabling the coverage above.
- **A correct, hardened file toolset.** `atomicWrite` temp+rename
  (`pkg/tools/file.go:263-283`); symlink resolution so gate and I/O see the same
  real target (`absolutize`, `:227-245`, #374); `edit_file` refuses ambiguous
  multi-match (`:166-172`); `bash.go:101` sets `cmd.WaitDelay`, SIGKILLing
  subprocesses still holding pipes and defeating hung timeouts.
- **One audited permission chokepoint.** `pkg/permissions/gate.go:80` — five
  modes, per-session sub-gates, grant persistence, approval audit log. Network and
  escalation tools register *conditionally*: `fetch_url` only with a non-empty URL
  allowlist, `alert` only with configured targets — SSRF-safe by construction.
- **Plan-first is a real runtime gate, not a prompt.** `record_plan` flips the
  gate's flag, and `plan_first_test.go` proves mutations are blocked pre-plan
  *even under `ModeYolo`* (`gate.go:421-429`).
- **The design corpus is rigorous and self-critical** where it is current —
  `kube-agents-platform-fit.md` and `scheduled-ops-design.md` honestly self-label
  as investigative/superseded/proposed and list their own carried-forward gaps.

---

## Findings by focus area

### Code quality and complexity

*Advocate wins the craft; critic wins the complexity.*

Complexity is **structurally unenforced**: `dev/tools/.golangci.yml` enables a
respectable bug-finding set (errcheck, govet, ineffassign, staticcheck, unused,
misspell, unconvert, unparam, gocritic, goheader, gosec) but **not a single
complexity or length linter** — no gocyclo, cyclop, funlen, gocognit, nestif, or
revive — with an in-file comment admitting it is a "conservative starting set."
The consequence is method-level: `Agent.Run()` spans
`pkg/agent/agent.go:1263-1519` (257 lines), `New()` spans `:748-946` (199 lines),
and `Run()`'s ordering-fragile pre-turn pipeline cites bug trail #362, #145, #144,
#623, #537 in a single method's comments.

The advocate's rebuttal — that file-level separation refutes the "god object"
charge (`agent.go` exports 3 types, 25 functional options, each concern in its own
file) — is true but does not address method-level complexity, which is where the
evidence lands.

Separately, **the wiring layer that boots every agent is the least-tested core
package**: `pkg/compose` at 60.3%, with `persist.go`, `pricing.go`, `agentic.go`,
`BuildSessionFactory`, `BuildMultiSessionAuthn`, and `FormatStartupSummary` all at
0.0%. `pkg/session` shows 0% statement coverage. This is the boot/assembly seam,
not the tool-execution or safety core — a real but bounded finding.

### Ability to write code

*Critic wins.*

- **Code Mode / `execute_go` is 100% unbuilt.** `docs/code-mode-design.md` is 1635
  lines and pitches it as "the moat" and something "no other agent framework can
  do." `grep` for `execute_go|yaegi|GoExecutor|CodeMode` across `pkg/ internal/
  cmd/ go.mod` returns nothing; `yaegi` is not a dependency. Git history mentions
  it in exactly one commit (`251907f`, docs only).
- **`edit_file` lacks `replace_all`/multi-edit.** `pkg/tools/file.go:166-177`:
  `count > 1` errors, `Replacements` hardcoded to 1. Refusing ambiguity is a
  defensible *default*; offering no opt-in is a capability gap versus peers on
  repetitive refactors.
- **No first-party coder instruction layer.** `docs/coding-agent-instructions.md`
  studies Charmbracelet's Crush and Claude Code; `pkg/instruction` bundles no
  core-agent-native coder prompt.

### The subagent system

*Critic wins on shipped reality.*

- **`wait:true` double-delivers.** Every successful synchronous result surfaces
  twice — inline plus a redundant `[Background reports]` line
  (`pkg/agent/background/tools.go:71-109`, `awaitResult`). Honestly disclosed as
  "noisy but not incorrect," but it is the #626 headline path shipping with a
  known defect.
- **`wait:true` returns free text, not structured findings** (`DoneDetail`/
  `FinalText`) — #641. The parent re-does delegated work.
- **No roster enum.** The `agent` field is prose-only, so the model guesses valid
  subagent names — #640. `docs/north-star.md:46` still advertises the four-tool
  spawn/list/check/report surface that #625 folded to spawn+stop.
- **No filesystem isolation for in-process fan-out.** "Branch" is only an eventlog
  label (`ComposeBranch`); `spawn.go` sets no cwd/worktree/chroot. Parallel
  subagents calling `write_file`/`edit_file`/`bash` share the parent's working
  directory. Only the out-of-process `RemoteAgentSpawner` isolates. The advertised
  "independent fan-out" is unsafe for mutating tools.

### Autonomous capabilities

*Critic wins decisively. This is the most important cluster.*

- **All three runaway guardrails ship OFF by default.** `cmd/core-agent/main.go:188-191`:
  `--watchdog` defaults to `warn` (observe/log only), `--max-turn-cost-usd` and
  `--max-session-cost-usd` both default to `0` (disabled). An out-of-box unattended
  daemon has **no active in-session halt**. The #623–#627 framing "guardrails ALL
  SHIPPED" describes machinery that is coherent, well-factored, well-tested — and
  inert as configured.
- **Trip-state is not durable.** Watchdog trips and cost halts are in-memory and
  require an operator reset. A crash/OOM/pod restart re-initializes fresh. Combined
  with off-by-default, the exact runaway → crash → restart scenario the #623–#627
  train was built for can loop indefinitely across restarts. *This gap was named by
  neither finder team; the cross-corpus arbitrator surfaced it.*
- **One of five designed watchdog signals ships, and it is evadable.**
  `NewDefaultWatchdog` wires only `NewRepeatedToolCallSignal(5)`, catching
  consecutive, literal-arg-identical calls. Its own docstring flags the evasions:
  alternating `a→b→a→b` loops and arg-normalized repeats (`main.go` vs
  `/workspace/main.go`) escape even in enforce mode.

### Platform-engineering / SRE / troubleshooting fitness

*Strong foundation, unshipped roof.* The primitives are real — SSRF-safe alert
tool shipped to its design with a token-bucket limiter and 15 tests; plan-first
gate tested; hooks fail closed; production-grade MCP namespacing. The critics
conceded all of it. The end-to-end artifact is where it breaks:

- **The flagship `gke-troubleshoot-agent` recipe cannot run its own
  diagnose→fix→verify loop.** `deploy/base/config/config.json` sets `mode:yolo` +
  `tools.disable:["bash"]` and the image is distroless, yet SKILL.md and 11
  reference files carry 64 `kubectl` lines plus `gcloud` and `sleep`. `AGENTS.md`
  separately forbids kubectl/gcloud — SKILL.md says to "degrade gracefully to bash
  + kubectl," so the shipped content contradicts itself.
- **The wired MCP is the wrong surface.** `mcp.json` exposes one server at
  `container.googleapis.com/mcp` — GKE cluster-lifecycle — not the pod-level
  `apply_manifest`/`patch_resource`/`rollout_undo` the SKILL names.
- **No in-turn wait/verify primitive.** The protocol demands fix-and-verify "in ONE
  turn" including "Sleep the verify interval," but `schedule_next_turn` *ends* the
  turn and no `wait_and_verify`/`verify_state` tool exists. Every "RESOLVED"
  outcome is therefore unverifiable (cf. #639).
- **`require_plan_artifact` is absent from the flagship config** despite the design
  prescribing it — the differentiating safety property is off in practice.
- **The alert tool is never wired.** SKILL.md says "For now: eventlog only."
  Escalation is "write text and hope someone tails it."
- **Structural tests validate file shape, not executability** — which is exactly
  how a non-runnable recipe shipped green.

### Premise vs. docs

Mixed, tilting toward over-claiming *executable readiness*:

- `docs/k8s-event-agent-design.md` is materially stale — still says the watcher is
  an in-tree binary at `cmd/k8s-event-watcher/` depending on `k8s.io/client-go`,
  "shipped in v2.6." `ls cmd/` shows only `core-agent` + `core-agent-tui`, `go.mod`
  has zero `k8s.io/client-go`, and the recipe deploys `ghcr.io/go-steer/lookout`.
- `docs/north-star.md:46` advertises the folded-away spawn tool surface.
- `docs/kube-agents-platform-fit.md:18` ("80% reusable today") and
  `examples/gke-troubleshoot-agent/DEMO.md:694` ("v2.7 filled it out —
  native alert-tool escalation to Slack/PagerDuty/webhook") overstate readiness.
  The alert tool ships **only** `template: "generic"`; `slack` and
  `pagerduty_events_v2` are rejected at config load as designed-not-implemented
  (`pkg/config/alerts.go:142-143`).
- **Hermes-replacement runtime gaps remain open.** `pkg/attach/peers.go` is an
  in-memory map + RWMutex with no persistence (peers lost on pod restart);
  `grep` for `call_peer` and `v1/responses` returns nothing. The platform-fit doc
  itself says these "carry forward." Note the advocate conflated two recipes: the
  #619/#613/#617 fixes closed *content-distribution* gaps, not the runtime peer
  fabric.

---

## Gaps outside the stated claims

Surfaced by Team 3 rather than either finder team — these are what separate
core-agent from a first-class autonomous harness:

| Gap | Severity | Why it matters |
| --- | --- | --- |
| **No built-in execution sandbox** | blocker | `gate.go:759-767` explicitly delegates blast-radius containment to an external sandbox the harness does not provide. Peers (Claude Code `sandbox-exec`, Codex nsjail/Landlock) sandbox by default. Unattended in yolo, one bad tool call reaches `~/.ssh`, cron, systemd units, or sibling tenants. |
| **Guardrail trip-state not durable** | blocker | A halt an autonomous daemon forgets on restart is not a backstop. |
| **No out-of-band approval channel** | major | The gate offers synchronous `ask` or no gate. This is *why* every autonomous recipe is forced into `yolo`, defeating the gate the codebase invests in. The missing bridge between a strong gate and safe unattended operation. |
| **No verify/wait-for-condition primitive** | major | Closed-loop remediation — the difference between an advisor and an operator — is inexpressible. |
| **No behavioral eval harness** | major | Unit coverage proves functions work; nothing measures task success, loop rate, or cost per task. Its absence is precisely why a non-executable recipe shipped green. |
| **State is not daemon-grade** | major | In-memory peers, 0%-covered session layer, design-only cron, shared-cwd multi-tenancy. Autonomous daemons are defined by surviving restarts and isolating tenants. |
| **No first-party persona library** | nice-to-have | Recipes ship borrowed prose; behavior is fragile and unowned. |

---

## Roadmap and issue map

Two milestones were created from this assessment:
[**v2.9 — Fully functional autonomous harness**](https://github.com/go-steer/core-agent/milestone/7)
and [**v3.0 — Flagship autonomous agent harness**](https://github.com/go-steer/core-agent/milestone/8).

### v2.9 — must

| Issue | Item |
| --- | --- |
| [#642](https://github.com/go-steer/core-agent/issues/642) | Safe autonomous defaults — enforce watchdog + session cost ceiling in daemon/headless mode |
| [#643](https://github.com/go-steer/core-agent/issues/643) | Persist guardrail trip-state across process/pod restart |
| [#644](https://github.com/go-steer/core-agent/issues/644) | Make the gke-troubleshoot recipe executable end-to-end (or mark non-functional) |
| [#646](https://github.com/go-steer/core-agent/issues/646) | `spawn_agent(wait:true)` — dedup the double-delivered result |
| [#660](https://github.com/go-steer/core-agent/issues/660) | `safety.watchdog` config field — recipes cannot ship their own backstop |
| [#640](https://github.com/go-steer/core-agent/issues/640), [#641](https://github.com/go-steer/core-agent/issues/641) | Roster enum on `agent`; structured findings from `wait:true` (pre-existing) |

### v2.9 — should

| Issue | Item |
| --- | --- |
| [#645](https://github.com/go-steer/core-agent/issues/645) | Recipe integration test — assert every SKILL-named tool is reachable |
| [#647](https://github.com/go-steer/core-agent/issues/647) | Out-of-band deferred-approval channel |
| [#648](https://github.com/go-steer/core-agent/issues/648) | `wait_and_verify` poll-until-condition primitive |
| [#649](https://github.com/go-steer/core-agent/issues/649) | Second watchdog signal — arg-canonicalized + alternating-cycle |
| [#650](https://github.com/go-steer/core-agent/issues/650) | Fix stale and over-claiming docs |
| [#662](https://github.com/go-steer/core-agent/issues/662) | Ratchet `funlen`/`gocognit` so new god-methods can't land (config-only) |
| [#663](https://github.com/go-steer/core-agent/issues/663) | Backfill `pkg/compose` boot-seam coverage |

Also moved into v2.9: the Hermes epic [#589](https://github.com/go-steer/core-agent/issues/589)
and work-streams #590/#591/#592/#595/#611; subagent issues #637/#638/#639;
guardrail issues #624/#159/#331; recipe issues #618/#620/#621/#160/#215/#158.

### v3.0 — flagship

| Issue | Item |
| --- | --- |
| [#651](https://github.com/go-steer/core-agent/issues/651) | Built-in execution sandboxing |
| [#652](https://github.com/go-steer/core-agent/issues/652) | Behavioral eval/benchmark harness in CI |
| [#653](https://github.com/go-steer/core-agent/issues/653) | Filesystem isolation for in-process subagent fan-out |
| [#654](https://github.com/go-steer/core-agent/issues/654) | Code Mode — implement for real or formally retire |
| [#655](https://github.com/go-steer/core-agent/issues/655) | Deferred semantic watchdog signals |
| [#656](https://github.com/go-steer/core-agent/issues/656) | First-party core-agent-native persona library |
| [#657](https://github.com/go-steer/core-agent/issues/657) | `edit_file` opt-in `replace_all` / multi-edit |
| [#658](https://github.com/go-steer/core-agent/issues/658) | `/v1/responses` handler |
| [#659](https://github.com/go-steer/core-agent/issues/659) | Make `Agent.Run()`'s pre-turn pipeline ordering explicit and testable |
| [#202](https://github.com/go-steer/core-agent/issues/202) | Scheduled-ops cron sidecar (moved) |

---

## Addendum — config-layer hardening in `core-agent-demo-3`

`core-agent-demo-3` is the out-of-tree UAT rebuilding the GKE platform agent
core-agent-native (identity → equipment → conduct) instead of adopting Hermes's
`SOUL.md` verbatim. Reviewed against this assessment, it **already fixed** four
findings before any change: `require_plan_artifact: true`, propose-only at the MCP
layer (single read-only endpoint), a native persona, and a per-subagent `root`.

It had one instance of the assessment's central pattern, though — **stating a
safety property the runtime did not enforce.** The persona said "you have no write
path" while `write_file`/`edit_file`/`delete_file` were registered and `mode: yolo`
would wave them through once a plan was recorded. "Don't hunt the filesystem" was a
politely-worded request against the exact loop that burned the previous UAT.

Hardening applied at the config layer, making the persona *descriptive* rather than
*aspirational*:

| Persona claim | Now enforced by |
| --- | --- |
| Propose-only, no write path | `tools.disable: write_file, edit_file, delete_file` |
| Don't hunt the filesystem | `tools.disable: glob, grep, list_dir` |
| No shell | `tools.disable: bash` (pre-existing) |
| Plan before any change | `require_plan_artifact: true` (pre-existing) |
| Escalation goes somewhere | `alerts.targets[oncall]`, generic webhook via `url_env`, 5/min |
| Bounded spend | `max_turn_cost_usd: 0.50`, `max_session_cost_usd: 5.00` |

Two things worth carrying forward:

1. **One `tools.disable` hardens the subagent too.** `WithCatalog` takes "the
   parent's already-gated tool list" and "tools not listed here can't be requested"
   (`pkg/agent/background/manager.go:325-332`), so the `cluster` specialist inherits
   the restriction with no per-subagent duplication.
2. **`mode: yolo` is safe in this recipe** — not because yolo is safe, but because
   with no shell, no writes, no fetch, and a read-only MCP, the set of things it
   auto-approves is empty. The safety comes from the toolset, not the mode. This is
   the shape a hardened recipe should take generally.

### The wall this exercise found

Config-layer hardening covers every item in this assessment **except the single
most important one**. `--watchdog` is CLI-flag-only; `grep -rn watchdog pkg/config/`
returns nothing. A recipe cannot ship its own runaway-loop backstop, so every
invocation and deploy manifest must pass `--watchdog=enforce` by hand or silently
get observe-and-log. Filed as
[#660](https://github.com/go-steer/core-agent/issues/660), proposing
`safety.watchdog` alongside the existing `safety.small_tier_parent`, which already
has the same warn/refuse/allow + CLI-override shape.

#660 is complementary to #642, not a duplicate: #642 changes the *default*, #660
makes the setting *expressible per-recipe*. A recipe may reasonably want enforce
even where the daemon default is warn.
