# The good, the bad and the ugly: core-agent at v3.0.0 (2026-10-27)

**Method.** Five independent reviewers, each assigned one lane: architecture and
code health; runtime correctness and reliability; security; tests, CI and
process; product, DX, docs and the cost layer. All ran against the v3.0.0 GA tree
at `67c1b7ac`. Each was told to ground every claim in a `file:line`, command output
or computed number, or else drop it. Each also checked which findings from
[`2026-08-12-tri-team-assessment.md`](2026-08-12-tri-team-assessment.md) and
[`2026-09-04-focus-assessment.md`](2026-09-04-focus-assessment.md) still stand.

Reviewers ran real commands, not just reads:
- `go test -race` across agent, attach, eventlog, compose and runner;
- `go test -cover ./...`;
- `deadcode`, and `apidiff` between v2.8.0 and v2.9.0;
- a ratchet re-measure;
- the built CLI, run against `--provider=echo`;
- gate decisions checked through `go test -overlay` scratch tests against the
  real `pkg/permissions`, with nothing written to the tree.

The synthesizer then re-checked the load-bearing claims against the code before
writing this. Each finding below carries one of these marks:

- **✔** re-verified by the synthesizer at the cited lines;
- **◐** confirmed by the reviewer (ran it, or traced it end to end), not re-checked;
- **○** plausible: a static trace, not reproduced.

A companion document, [`2026-10-27-innovations.md`](2026-10-27-innovations.md),
covers what core-agent does that the rest of the field does not.

---

## Bottom line

> **The runtime is genuinely good, and much better than in August. Three things
> lag behind it. The defaults around it are looser than their names. The fleet
> and multi-tenant claims run ahead of the implementation. And the project's
> weight is growing faster than its product.**

The core loop has the qualities that are hardest to retrofit later:
- concurrency hygiene (race-clean, every goroutine has an owner);
- crash-resume correctness (tail repair, atomic eventlog writes inside ADK's own
  transaction);
- guardrails that are on by default when unattended and survive a restart;
- a test suite that asserts behaviour (1.38 test:prod ratio, no mocking
  framework, 87–98% coverage on the risky packages).

Nearly every finding from the 08-12 assessment in the reliability lane is fixed.

The bad clusters in three places:

1. **The permission defaults are looser than their names, and nothing sandboxes
   execution.**
   - The `read_only` auto-allow bundle contains commands that write files, and
     they also run in plan mode.
   - A cloned repo's `.agents/` is trusted the moment you `cd` into it.
   - The control-plane write tier covers two filenames.
   - All of these land because #651 (a real sandbox) is still open.
2. **Several fleet and tenancy properties claimed for 3.0 hold for a single
   process but not across a restart, a peer or a tenant.**
   - Resumed sessions forget subagent spend against the ceiling.
   - Guardrail-trip rows can be dropped silently.
   - `SessionLock.Lost()` has no consumer.
   - Multi-session tenants share one OS user.
3. **Weight.**
   - `cmd/core-agent.run()` is 2,391 lines with 43 positional parameters, and its
     ratchet exemption is uncapped.
   - Harness code added in the last seven weeks outweighs product code.
   - AGENTS.md has doubled.
   - `CHANGELOG.md` is 1.08 MB.
   - 42% of production Go is comments.

The ugly is structural, not a bug. One person builds this with agents, agents
write the graders, grading subagents score the runs, and the raw evidence for the
v3.0 boxes sits on one machine. Every step is honest and well documented, and
nothing independent checks the loop.

**Is it mostly good?** Yes, on the runtime, which is the part that's hardest to
get right. The bad items are concrete and most are small; the v3.1 list at the end
is mostly days of work, not months. The ugly is about how the project proves
itself and how much it weighs, and that is the part worth a deliberate decision.

---

## Scorecard

| Lane | Verdict |
|---|---|
| Architecture | Acyclic, layered graph; leaf packages with zero internal imports; little dead code (26 funcs in 124k lines). Held back by `run()`, a 98-field `Agent`, policy living in `package main`, and no API-compat gate for a library-first project. |
| Reliability | Race-clean; only about 14 production `go` statements, each with an owned lifecycle; crash-resume never re-executes a tool call. Gaps appear across restarts and peers, not inside one process. |
| Security | The documentation is honest ("defense-in-depth, NOT a security boundary") and the parser, SSRF and token work is sound. The defaults and the control-plane list are too loose, there is no workspace trust, and there is no sandbox. |
| Tests / CI / process | Behaviour tests, high coverage, a 37s suite, 10.5-min CI, falling churn, 0 reverts. But the v3.0 proof cannot be re-graded by anyone else, and the meta-work now outweighs the product work. |
| Product / DX / docs | Docs cover 73 of 76 flags and 164 of 165 config keys; the startup summary is excellent; errors are actionable. But there is no session resume, nested config typos pass silently, the CLI is a wall of flags, and the cost claim is mechanism rather than measurement. |

---

## The Good

**Concurrency and lifecycle discipline.**
- ◐ `go test -race -count=1` over agent, attach, eventlog, compose and runner is
  clean, and so is `go vet`.
- There are about 14 production `go` statements in the runtime lane, each with an
  explicit owner:
  - The broadcaster joins its goroutines through a WaitGroup and evicts slow
    subscribers without blocking (`pkg/attach/broadcaster.go:853-870`).
  - `Manager.Close` has a bounded drain (`pkg/agent/background/manager.go:1108-1160`).
  - Turn cancel funcs are generation-keyed (`pkg/agent/agent.go:2183-2260`).
- Only 12 of the 313 `fix:` commits in the whole history mention a race, hang,
  leak, panic or nil. The last real concurrency fix was #516, in July.

**Crash-resume is correct where it most needs to be.**
- ✔ Tail repair appends a durable synthetic `functionResponse` instead of
  re-running the call, and tells the model the call "may or may not have taken
  effect" (`pkg/agent/tail_repair.go`).
- ✔ The eventlog's overlay row is inserted *inside ADK's own transaction* through
  a GORM create callback (`pkg/eventlog/atomic.go`, #976), so an event cannot
  commit without its index row.

**Guardrails are mature, on by default when unattended, and survive a restart.**
- ✔ `-p`, `--no-repl` and non-TTY runs default to `watchdog=enforce` and a $10
  session ceiling (#642).
- ✔ The turn-vs-session scope split comes with escalation counters:
  `maxConsecutiveWatchdogTurnCuts = 3` (`pkg/agent/watchdog.go:815`),
  `maxConsecutiveTurnCeilingTrips = 3` (`cost_ceiling.go:482`) and
  `refusalStormThreshold = 3` (`refusal_storm.go:92`).
- Trips are durable and re-armed on restart, and the restore latch is set only on
  success (`guardrail_persist.go:150-200`).
- The pre-turn pipeline is ordered data with permutation tests (`preturn.go`,
  #659).
- Subagent fix churn fell from 21 commits (Aug) to 5 (Sep) to 2 (Oct).

**The security work that exists is sound, and honest about its limits.**
- `pkg/permissions/denylist.go:30-55` says outright that the denylist "is
  defense-in-depth, NOT a security boundary".
- ◐ `safecmd.go` uses a real bash parser (mvdan/sh), auto-allows only a single
  literal simple command, and fails closed.
- ◐ Path scope resolves symlinks before checking, and the tool opens the resolved
  path.
- ◐ `fetch_url` checks the destination address inside `DialContext` (which defeats
  DNS rebinding), always refuses metadata ranges, and re-checks every redirect.
- ✔ `childenv` scrubs credentials from every child environment, and the daemon
  makes itself non-dumpable (`pkg/childenv/protect_linux.go:38`).
- Bearer tokens are stored as SHA-256 hashes and compared in constant time.
- The attach server refuses an unauthenticated listener on a non-loopback
  address, and since #1201 also refuses an unauthenticated loopback one when the
  agent has `bash`.
- The release image is `distroless/static:nonroot` and is cosign-signed;
  govulncheck is a required check.

**Tests assert behaviour, and there are a lot of them.**
- ◐ 147k test LOC against 107k production (ratio 1.38, up from 1.20 on 09-04);
  4,362 tests, 3,730 of them `t.Parallel()`.
- No gomock, testify/mock or mockery anywhere.
- Coverage:

  | Package | Coverage |
  |---|---|
  | agent | 90.3% |
  | permissions | 89.5% |
  | attach | 87.5% |
  | compose | 86.9% (was 60.3% in August) |
  | watchdog | 98.2% |

- The full suite runs in 37s without `-race`.
- The 98 `t.Skip` sites are all environmental; none hides a flake.

**The dependency graph is clean.**
- ◐ Acyclic, with leaf packages that have zero internal imports (`auth`,
  `eventlog`, `watchdog`, `pricing`, `childenv`, `transcript`, `modeltier`).
- The subagent cycle is broken properly, with an interface in `pkg/agent`
  implemented by `pkg/agent/background`.
- No internal types leak through the public API.
- 36 direct requires, with no k8s dependency (moved to k8s-lookout), providers in
  core-models and the TUI in core-tui behind a capability gate.
- The ADK v1→v2 migration touched 356 files at a net +702 lines, and only one
  "ADK doesn't…" workaround remains (`pkg/eventlog/sql.go:218`).

**The docs and the operator experience are better than average.**
- ◐ 73 of 76 CLI flags and 164 of 165 config keys appear in the site or README.
- The startup summary names the source of every effective setting, for example
  `per-session=$10.0000 [session ceiling from unattended default]`.
- A missing credential produces one line naming every env var you could set.
- An unknown top-level config key produces a warning.
- The published permissions page states the risk plainly: `acceptEdits`
  "auto-allows **every** file write, in scope or not."

**The process is self-correcting.**
- ◐ The `fix:` share fell from 42.9% (07-25 to 09-04) to 36.8% (the last seven
  weeks), and to 34.7% in the most recent 3.5 weeks.
- Zero reverts in the window.
- 98 of 120 merged PRs carry a substantive adversarial-review section (median
  1,889 characters), and 49 of them cite pre-fix or mutation verification.
- Graders ship self-tests that run on every PR.
- Run notes record their own failures: three failed A7 runs before the pass,
  `verdict.pre-fix.md` kept beside the re-scored A6 sheet, and uncountersigned
  sheets labelled as such.

---

## The Bad

Ranked by severity within each group. P0 means a release blocker; there are none.

### Security and permission defaults

**S1. P1 ✔: the default-on `read_only` bundle auto-allows commands that write
files, including in plan mode.**
- `bash:sort *`, `bash:uniq *` and `bash:tree *` sit in the default bundle
  (`pkg/permissions/builtin_allow.go:65-87`) with no verb profile.
- The policy allow (`gate.go:1445`) returns before the plan-mode refusal
  (`gate.go:1479`).
- So `sort -o ~/.bashrc /dev/null`, `uniq /dev/null ~/.bashrc` and
  `tree -o ~/.bashrc /` run unprompted in `ask` *and* `plan` mode, and bash is
  never path-scoped. The reviewer ran `sort -o` and `uniq IN OUT` and watched both
  write.
- `rg --pre sh` is in the same bundle and is blocked today only by the search
  gate's default.
- **Fix:** drop `sort`, `uniq` and `tree` from the default bundle, or give them
  verb profiles. Add a table test asserting that no default-bundle command can
  write or exec.

**S2. P1 ✔: no workspace trust; opening an untrusted checkout runs its code.**
- `config.Find` walks *up* from cwd.
- `.agents/mcp.json` stdio servers are spawned at startup on the assumption that
  "Spec is sourced from the user's own .agents/mcp.json"
  (`pkg/mcp/lifecycle.go:378`). That assumption is false for a fresh clone.
- A repo config can also set `permissions.mode: yolo` and `hooks`.
- There is no trust concept in `pkg/config`.
- This is the same class of bug as recent coding-agent CVEs.
- **Fix:** keep a trust record keyed by realpath plus a hash of `.agents/`. Until
  the directory is trusted, ignore stdio MCP, hooks, any mode above `ask`, and
  subagent roots.

**S3. P1 ◐: the control-plane tier is two filenames.**
- `controlplane.go:73` elevates `.agents/{config,mcp}.json` only.
- These are auto-approved in `acceptEdits`:
  - `.git/config` (`core.fsmonitor`, `core.hooksPath`), which runs on the
    *human's* next `git`;
  - `.git/hooks/*`;
  - a rooted subagent's `mcp.json`;
  - `.agents/env.yaml`;
  - `pricing.json` (lowering prices defeats the cost ceilings);
  - `AGENTS.md`;
  - `~/.core-agent/AGENTS.md`, which is persistent instruction injection into
    every project.
- **Fix:** a declarative list covering everything the boot path loads, and make
  `~/.core-agent` read-only to tools.

**S4. P1 ◐: multi-session tenancy isolates conversations, not execution.**
- Any authenticated identity can `POST /sessions`, own the session, set its mode
  to yolo, and answer its own prompts.
- Bash then runs as the daemon's uid and can read `~/.core-agent/sessions.db`,
  which holds every tenant's transcripts (and `cat *` is auto-allowed).
- This contradicts `docs/multi-session-design.md` ("Sessions stay isolated
  end-to-end") and the #1179 principle that an owner never gates a daemon-wide
  effect.
- **Fix:**
  - a daemon-level ceiling on the modes a non-admin may select;
  - admin-gated approval of bash;
  - until #651 lands, docs that say plainly what is isolated.

**S5. P2 ◐: smaller default-surface leaks.**
- `env` and `printenv` are in the default bundle, and provider keys are
  deliberately not withheld (`cmd/core-agent/credentials.go:40`). API keys reach
  the model's context, the eventlog and every `/events` viewer.
- The opt-in `dev_tools` bundle allows `git branch -D`, `git remote set-url`,
  `go env -w GOPROXY=…` and `go vet -vettool=./x`.
- A bash verb grant is also an env-prefix grant (`LD_PRELOAD=… cat`, after
  "allow cat for session"), because `parseSafeArgv` skips leading assignments
  (`safecmd.go:77`).
- ○ Unauthenticated loopback listeners do not validate `Host`, so DNS rebinding
  can reach reads, and writes when bash isn't registered (`csrf.go:94-108`).
- All 35 third-party GitHub Action `uses:` lines are tag-pinned, never SHA-pinned,
  including in workflows that hold `contents: write` and `id-token: write`.

### Fleet and tenancy correctness

**R1. P2 ✔: a resumed session forgets delegated spend against its cost ceiling.**
- The multi-session rebuild reads `eventlog.ForSession(..., sid)`, an exact-id
  match (`pkg/compose/multi_session.go:603`).
- Subagents and subtasks run under derived `parent:sub:…` ids, so their spend
  vanishes on rebuild.
- The rebuild also prices every model at the parent's rate
  (`func(model string) usage.Pricing { return pricingRate }`).
- Example: $8 of subagent spend against a $10 ceiling comes back after a pod roll
  as about $9 of fresh runway. `side_usage.go` says ceilings must not forget spend
  on a pod roll (#643).
- **Fix:** `eventlog.WithSessionTree` (it already exists) with per-model pricing,
  plus a kill-and-resume regression test.

**R2. P2 ✔: durable guardrail and turn-error rows are silently dropped on a DB
error.**
- `drainOutOfBandEvents` (`pkg/agent/guardrail_persist.go`) takes the batch off
  the queue, returns early if `Get` fails, and discards each `AppendEvent` error
  with `_ =`.
- The comment calls this deliberate because the rows are "audit". But they are the
  same rows `RestoreGuardrails` folds on restart, so a lost row means a halt is
  not restored. The backstop fails open, silently.
- The same pattern appears in `interrupt_audit.go`, `refusal_storm.go` and
  `approver_context.go`.
- **Fix:** requeue on failure and log once.

**R3. P2 ✔: split-brain protection is documented but not wired.**
- `SessionLock.Lost()` has zero consumers outside tests.
- Its doc (`pkg/eventlog/lock.go:213-221`) says the run loop "must select on this
  channel". `autonomous.Resume` never does.
- `beat()` treats a DB error as "not lost", so a Cloud SQL failover longer than
  30s lets a peer take the lease while this process keeps driving.
- Ordinary daemon turns take no run lock at all.
- In practice, HA means a single replica, and the docs should say so.

**R4. P2 ✔ / ○: the agentic tool wrappers race, and tenants inherit them
mis-bound.**
- ✔ The lazily resolved model is written without a lock
  (`pkg/tools/agentic/agentic.go:119-139`), and ADK v2 runs a turn's tool calls
  concurrently. The MCP twin of this code takes a mutex for exactly this case.
- ✔ The wrappers bind `func() *agent.Agent { return agentRef }` and are appended
  to `builtinTools` (`cmd/core-agent/main.go:1565-1570`), which is handed to every
  `POST /sessions` tenant (`main.go:2447`, cloned at `multi_session.go:459`).
- The `SessionFactoryDeps` doc says the opposite: tenants "see the substrate
  without them" (`multi_session.go:134-139`).
- ○ Either a tenant's subtask spend lands on the bootstrap agent's tracker and
  escapes the tenant's ceiling, or, with no bootstrap agent, the tool always
  errors. Both outcomes are wrong.

**R5. P2 ◐: liveness and blocking defaults.**
- ○ Idle eviction checks only `lastTouchedNs` (`attach/registry.go:1129-1150`).
  An unwatched, wake-driven session mid-turn can be evicted when the timeout is
  short.
- ◐ `approval_timeout` defaults to wait forever even in unattended runs; only
  `auto` mode refuses to start without one.
- ◐ Per-turn DB work grows with session lifetime. ADK loads the full session each
  turn, and core-agent adds more full-history `Get`s just to append one audit row.
  #883 and #889 (retention) are open.

**R6. P3.**
- No panic bulkhead: one session's panic in a wake loop kills a multi-session
  daemon (`multi_session.go:749` has no `recover`).
- A remote subagent whose stream closes without a terminal event is reported
  Completed (`background/remote.go:353-356`), which contradicts #1036.
- The subagent roster grows without bound when the model invents names.
- A narrow race in the out-of-band write protocol during pre-turn compaction.

### Architecture and library DX

**A1. P2 ◐: minor releases break the library API, and nothing gates it.**
- `apidiff -incompatible` from v2.8.0 to v2.9.0 shows:
  - methods added to exported interfaces (`agent.SubagentManager`,
    `attach.SessionACLStore`);
  - removed fields (`attach.Entry.ACL`, `compose.SessionFactoryDeps.NoCheckpoint`).
- None of it is marked BREAKING. For a project whose stated primary audience is
  library consumers, this is the biggest gap in release discipline.
- **Fix:** an apidiff presubmit against the latest tag that fails without a
  BREAKING bullet.

**A2. P2 ◐: the v3.0 safety defaults live in `package main`.**
- These are binary-only, so a library consumer building on `pkg/agent` and
  `pkg/compose` gets none of the defaults v3.0 advertises:
  - `resolveGuardrails`;
  - `DefaultUnattendedSessionCostUSD = 10.0` (`cmd/core-agent/guardrails.go:41`);
  - the unpriced-model refusal;
  - auto-continue resolution.
- Moving them into `pkg/compose` would also shrink `run()` (#685).

**A3. P2 ◐: duplication and sprawl.**
- 128 of `autonomous.Run`'s 204 code lines appear verbatim in `Resume`, including
  one 49-line block.
- `*Agent` has 75 exported methods and 36 `With*` options, plus combinatorial
  families: six `Inject*`/`Queue*`, three `Resume*`, and four `runner.REPL*`.
- 33 `Deprecated:` shims survived the v3 major, including ones whose comment says
  "Removed in the next major".
- `pkg/agent` imports the HTTP server package `pkg/attach` (26 files) for wire
  types.

**A4. P2 ◐: the ratchet holds the maximum, not the spread.**
- `(*RetryPolicy).Wrap` predates the 09-14 re-measure, then grew through #1206
  and #1247 to exactly the gocognit ceiling of 110.
- The line ceiling (254) is held by a UAT harness, so any `pkg/` function may
  reach it.
- `Agent.Run` regrew from 102 to 128 funlen lines after its own refactor.
- **Fix:** separate thresholds for `pkg/` and `dev/`, and a count of functions
  over N.

### Product and daily-driver

**P1. P1 ✔: every local session on the machine is one conversation, and there is
no resume.**
- The CLI never calls `agent.WithSession`; only the multi-session factory does
  (`multi_session.go:549`).
- So every `--session-db` run is session `"default"` (`pkg/agent/agent.go:273`).
  ◐ Running in project A and then project B wrote both into one row.
- There is no `--resume` or `--continue`. For goal 2 (daily driver), this is the
  single most visible gap.

**P2. P2 ✔: nested config typos are silent, including permissions and budgets.**
- `warnUnknownKeys` checks top-level keys only (`pkg/config/discovery.go:108`),
  yet its own comment names "a mistyped deny/allow" as the motivating case, and
  those keys are nested.
- `{"agent":{"max_sesion_cost_usd":1}}` leaves the ceiling at the $10 default
  with no warning.
- **Fix:** a recursive known-key walk, fatal under `permissions.*` and the cost
  fields.

**P3. P2 ◐: the cost-efficiency claim is mechanism, not measurement.**
- `north-star.md` says split-model "routinely beats single-model-everywhere by
  5–10×", and nothing in the tree measures that. The blog's 5–10× is about MCP
  payload size.
- The compaction thresholds are "starting points, not measured optima"
  (`pkg/modeltier/modeltier.go:67-69`).
- They also ignore price cliffs: a 1M-window small-tier model compacts at about
  350k tokens while its long-context rate starts at 100k.
- With no cheap tier (OpenAI, xAI), the default-on agentic subtask "cost lever"
  runs on the parent model, which adds cost instead of saving it.

**P4. P2 ◐: the CLI surface.**
- 76 flags in an ungrouped 24 KB `--help`.
- Subcommands (`attach`, `ls`, `auth`) don't appear in it.
- Six permission modes, but `--yolo` is the only switch.
- Duplicate spellings: `--plan-first` / `--plan-mode` / `mode=plan`.
- `-p` writes `asst › ` to piped stdout, and there is no `--output-format json`.
- The README's "zero outbound calls beyond the model" is false: the LiteLLM
  pricing refresh runs at startup by default.
- Missing compared with Claude Code: image input, rewind/undo of edits,
  user-defined slash commands, IDE integration, and non-Go
  `view_file_outline`.

### Tests, CI and process

**T1. P1 ◐ (✔ on A6): the v3.0 boxes are proven, but not reproducibly.**
- A7 passed once, on run 4, after rig fixes between runs.
- ✔ A6's first grading read 3 of 5; a grader fix (#1248) re-scored it to 5 of 5.
  The run note discloses this, explains it (the matcher missed Go-quoted
  denials), and keeps `verdict.pre-fix.md`. It is defensible, but it is a
  post-hoc change.
- A2's retry class is "MET (maintainer decision)".
- ✔ 14 of 42 run notes are marked not countersigned, including the 10-13 v3
  re-run's judgement rows.
- Raw artifacts live under `~/.gke-drill/` on one machine.
- A5 ("measured, not asserted") is half met: graded, but not re-gradable by
  anyone else.

**T2. P1 ◐: meta-work now outweighs the product.**
- Non-test lines added since 09-05: **29.9k** harness (Go + Python + shell/YAML)
  against **21.0k** of `pkg/` + `cmd/`.
- 42 of 92 `fix:` commits target harness scopes, so part of the falling fix ratio
  is fixes moving from the product to the rigs.
- Gate machinery alone is about 13.5k lines of Go.
- The 09-04 assessment asked for the reverse.

**T3. P2 ✔: process text has drifted from reality.**
- AGENTS.md is 4,251 words (2,019 on 08-13) and lands in every agent prompt.
- Its first convention requires `acceptance-mN.md` plans, and the only two date
  from May.
- It says "four required status checks", and the reviewer counted eight.
- Its sweep lists 12 presubmits; CI runs 16.
- It cites Anthropic adapter files that the core-models swap removed.
- `review-gate` passes if `grep -qi 'adversarial review'` matches anywhere
  (`.github/workflows/review-gate.yml:72`), so "no adversarial review done"
  passes.

**T4. P2 ✔: flakes and timing.**
- #520 (`TestSessionACLStore_ConcurrentPut`) has been open since July and recurred
  on 09-10 and 10-02. The likely lead: the test DSN lacks the `_txlock=immediate`
  that `pkg/eventlog/sql.go` calls load-bearing.
- ✔ `pkg/attach/integration_test.go:625` loops on
  `time.Now().Before(time.Now().Add(time.Second))`, which is always true. A server
  that never binds spins until the 5-minute package timeout.
- 97 `time.Sleep` calls in tests.
- ✔ #1296 landed on `main` with the subject
  `try: … (do not merge)`.

---

## The Ugly

**U1. There is no execution sandbox.** #651 is open, and there is no Landlock,
seccomp, bwrap or namespace code in the tree. Every finding in S1–S5 lands because
bash and the file tools run with the user's full authority, and the shipped GKE
recipes stay safe by setting `mode: yolo` and disabling bash ("yolo by
amputation"). The project documents this honestly. It is still the largest
distance between "proven autonomy" and "safe to point at an untrusted task".

**U2. `run()` keeps growing inside its exemption.**
- `cmd/core-agent/main.go:490-2880` is 2,391 lines with 43 positional
  parameters, including runs of adjacent bools.
- It grew 2,061 → 2,217 → 2,294 → 2,391 across the last three assessments.
- `main.go` is the most-churned file, and `cmd/core-agent` has the lowest
  coverage of any core package (59.1%).
- The `agentRef` SIGSEGV pitfall in AGENTS.md, and R4 above, are what this costs.
  The getter discipline is enforced by convention, over 17 closure sites.
- #685 has not moved since 09-14 and is filed under an unrelated milestone.

**U3. The guardrails are separate mechanisms coupled through shared latches.**
- About 40 primitive per-turn flags sit under one `a.mu` on a 98-field `Agent`
  (`agent.go:385-500`).
- Turn cleanup is 13 order-sensitive steps.
- On 2026-09-15 five fixes landed in a row (#1068 → #1074 → #1081 → #1086 →
  #1090), each moving a halt somewhere else. One is titled "the gate's cut takes
  the repetition off the watchdog's books".
- That is the signature of a missing turn-outcome state machine, not of bad luck.
  It is correct today, and every edit has to re-derive why.

**U4. The proof loop is self-referential, and the bus factor is 1.**
- Agents write the code. Agents write the graders. Grading subagents score the
  runs. A grader was corrected after its first live verdict. The evidence sits on
  one laptop.
- 231 of 239 commits in the window are the maintainer's, and 116 of 120 PRs.
- Open issues went from 77 to 109.

**U5. Weight.**
- ✔ `CHANGELOG.md` is 1.08 MB. The `[3.0.0]` section alone is 391 KB, 159
  bullets averaging 2.4 KB each, and it does not match the GitHub release body.
- ◐ 42% of production Go is comments, with 1,604 inline `#NNN` references in
  `pkg/`. Executable Go is about 64.5k lines, not 107k.
- 103 internal docs. Nine are unindexed despite the AGENTS.md rule, and only
  about 15 index entries carry a status.
- `north-star.md` still says "main is on v2.9.0-dev" and cites exports that no
  longer exist.

  The rationale in the comments is often valuable. At this density it goes stale:
  the ratchet config names a function that was renamed and excludes a directory
  that no longer exists.

**U6. Goal 2's flagship consumer is dormant.** `go-steer/cogo` was last committed
2026-05-22, is still on ADK v1.2.0, and has no core-agent dependency. The
north-star's "core-agent + core-tui + cogo" coding CLI is, in practice,
core-agent plus the `builtin:coder` persona, and no doc says so.

---

## Prior-assessment ledger

| Finding (source) | Status at v3.0.0 |
|---|---|
| Guardrails off by default (08-12) | **Fixed** (#642) |
| Trip state not durable (08-12) | **Fixed** (#643/#1258), but see R2 |
| Single evadable watchdog signal (08-12) | **Fixed** (eight signals, some read outcomes rather than calls) |
| Non-atomic eventlog dual write (08-12) | **Fixed** (#976) |
| Compaction blind when window = 0; no mechanical fallback (08-12) | **Fixed** (#974) |
| No mid-turn tool-result overflow check (08-12) | **Fixed** (#975) |
| WakeLoop has no backoff (08-12) | **Fixed** (#978) |
| No out-of-band approval channel (08-12) | **Fixed** (#647, auto mode) |
| "Safe apply MISSING" (09-04) | **Fixed** (#1105, box A6) |
| `pkg/compose` 60% coverage (08-12) | **Fixed** (86.9%) |
| No complexity linter (08-12) | **Fixed** (#662), but see A4 and U2 |
| Pin gate detects but nothing acts (09-04) | **Fixed** (the bot opens bump PRs) |
| Fix ratio: treadmill? (09-04) | **Better**: 42.9% → 36.8%, 0 reverts, though about 45% of fixes now target the rigs |
| Meta-work share (09-04) | **Worse** (T2) |
| Cross-daemon exclusion (08-12) | **Partial**: auto-continue claims (#977) fixed; general exclusion absent (R3) |
| Retention / replay cap (#883, #889) | **Persists** |
| `Agent.Run` 257 lines (08-12) | **Regrew** after #659: 354 physical |
| `run()` decomposition #685 | **Persists, worse** (U2) |
| Flake #520 | **Persists** (T4) |
| Built-in sandbox #651 | **Persists** (U1) |
| Shared-cwd multi-tenancy (08-12) | **Persists, untracked** (S4) |
| Code Mode #654 | No code exists, and the issue is still open. Close it. |

Twelve of the twenty-two rows are fixed outright. The ones that persist are the
structural items (sandbox, `run()`, retention, tenancy, meta share), not bugs.

---

## Recommendations for v3.1

Ordered by value per day of work. The first three are mostly small.

1. **Close the default-surface security gaps.** Days, not weeks.
   - Remove or profile `sort`, `uniq`, `tree`, `env` and `printenv` from the
     default bundle, and add a table test that no default-bundle command writes or
     execs (S1, S5).
   - Widen the control-plane tier to a declarative list: `.git/**`, every
     boot-loaded `mcp.json`, pricing, `env.yaml`, user-scope instructions and
     skills (S3).
   - Add workspace trust (S2).
   - SHA-pin Actions, and add a `Host` allowlist on loopback listeners (S5).
2. **Make the v3.0 safety properties hold across restarts, peers and tenants.**
   - Rebuild the cost tracker over the session tree with per-model pricing (R1).
   - Requeue dropped out-of-band rows (R2).
   - Fix the agentic race and the tenant binding (R4).
   - Make eviction turn-aware, and default `approval_timeout` when unattended
     (R5).
   - Either wire `Lost()` or document that HA means a single replica (R3).
   - Add a non-admin mode ceiling, and correct the "isolated end-to-end" claim
     (S4).
3. **Daily-driver basics.**
   - Per-project session ids with `--resume` / `--continue` (P1).
   - Recursive config validation (P2).
   - `--output-format json`, and no `asst ›` on piped stdout.
   - `--permission-mode` and grouped `--help` (P4).
   - Decide whether goal 2 runs through cogo or core-agent directly, and rewrite
     the north-star to match (U6).
4. **Make the proof reproducible by someone other than its author** (T1).
   - Archive each graded run's raw bundle with the release.
   - Freeze the grader commit before a run.
   - Require two passes per box.
   - Have a human countersign the judgement rows.
   - Do this before quoting A6 and A7 externally.
5. **Lose weight deliberately** (T2, T3, U2, U5, A1–A3).
   - Land #685 by moving guardrail, auto-continue and unpriced-model policy into
     `pkg/compose`. Library consumers get the v3.0 defaults, and `run()` shrinks
     as a side effect. Cap the exemption at today's size until then.
   - Add an apidiff presubmit, and decide the 4.0 removal list (the 33 shims, the
     `REPL*`/`Inject*` families).
   - Cut AGENTS.md to about 2,000 words of rules plus pointers, and *generate* the
     required-check and presubmit lists.
   - Cap CHANGELOG bullets at about 300 characters, and archive shipped design
     docs with a status field.
   - Set a harness budget (for example, harness:product added lines ≤ 1:2) and
     report it each release.
6. **Start #651 with the cheapest real boundary.** Landlock-restrict bash and MCP
   children to the project root plus an allowlist, falling back to today's
   posture where the kernel lacks it.
7. **Fold the guardrail latches into one turn-outcome state machine** (U3). This
   is the largest item. Do it when the next guardrail feature arrives, not as a
   standalone refactor.

---

## Confidence and caveats

- **High confidence** on everything marked ✔.
- **Medium-high** on ◐ items: the reviewers ran the commands or traced the code,
  and the synthesizer did not repeat the run.
- R4's tenant outcome, R5's eviction and S5's DNS rebinding are **○**. Treat them
  as hypotheses to reproduce, not facts.
- Line counts differ slightly between reviewers (107k vs 124k production LOC)
  because of scope (`pkg/ internal/ cmd/` vs the whole tree including `dev/`).
  The comment ratio (39–42%) holds under both.
- Nothing here re-ran a live drill or touched a cluster. Statements about A1–A7
  are about the evidence trail, not about whether the capability holds; the
  capability passed every recorded run on the v3 build.
