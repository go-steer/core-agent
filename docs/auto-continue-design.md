# Auto-continuing interrupted turns after a restart

Design doc for the last piece of the "Clean shutdown & crash resume" milestone (#539): optionally letting the daemon *finish the interrupted thought* after a restart, instead of resuming with intact-but-inert history.

**Status:** shipped 2026-07-31. PR 1 (detection + lazy-touch continuation): `pkg/agent/autocontinue.go` (classifier + note), `pkg/compose/auto_continue.go` (trigger), `agent.auto_continue` config block. PR 2 (+ folded docs): `AutoContinueBootScan` over persisted ACL rows driving the registry's normal Lookup→resume path, `agent_boot_log` table (`pkg/eventlog/bootlog.go`), crash-loop breaker (3 attempting boots / 10 min → stand down), per-session single-retry, `max_per_boot` oldest-first cap with logged remainder. Un-defers the "auto-recovering an in-progress turn at restart" non-goal in `docs/session-resume-design.md`, whose corrupting half already shipped as #537 (tail repair). Depends on: #537 (shipped), #538 (shipped — bounded teardown), lazy session resume (v2.5, #178).

*Defect B (#575, v2.8):* UAT on v2.8.0-dev.5 surfaced a residual class of failure once #576 (`_txlock=immediate`) fixed the concrete `SQLITE_BUSY` trigger. Two gaps closed here: (1) **no in-lifetime retry** — the one-shot-per-boot scan stranded a transiently-failed continuation until a reboot or human message; a bounded, default-on retry driver (`AutoContinueRetryLoop`) now self-heals it. (2) **fleet amplification of the attempt burn** — every daemon boot-scanned a shared session and all but the lock winner charged an attempt against the shared cap; outcome-aware accounting now refunds the lock-losers. Sequenced before #559. See "In-lifetime retry" and the outcome-aware-accounting note below.

*Default-on (#559, v2.8):* with the guard stack soaked (the GKE-troubleshoot restart/resume UAT plus #576/#577), auto-continue now defaults **on** when the feature can apply — a multi-session daemon or a `--no-repl` single-user daemon, with a durable eventlog — and off, silently, for interactive REPL/TUI runs and in-process library use (which the precondition excludes). `enabled` became a `*bool` tristate so an explicit `false` remains a hard opt-out. See "Config surface".

*Implementation note (PR 1):* the lazy path ships with a one-shot loop bound instead of the full breaker: the classifier refuses to continue a tail that IS a committed continuation note (recognized by marker substring), so a continuation turn that crashes after its note commits gets exactly one automatic attempt — the boot-log breaker below still lands with PR 2 for the scan path. The classifier also vetoes tails behind an operator interrupt-audit row (a deliberate kill must not be resurrected), ErrorCode finals, empty-aggregate Gemini finals, confirmation-parked turns, and SkipSummarization finals. Additionally, the run lock is held across detection + injection only, not across the continuation turn itself — the turn runs asynchronously in the session's wake loop, which has no turn-end hook this path can wait on. The residual double-continue window (two shared-DB daemons both resuming the same session after one's injection but before its turn commits events) closes with PR 2's synchronously-driven boot scan; for the lazy path it requires near-simultaneous cross-daemon touches of one session inside the freshness window, which the singleflight per daemon already makes rare.

## Motivation

With #537 + #538 merged, a K8s rolling upgrade is safe: committed events survive, histories are repaired, sessions resume lazily. What it is *not* is polite. The failure UX:

1. Alice asks the Slack-bridged session a question.
2. The pod rolls mid-turn. The turn is interrupted; her question is committed; no answer ever comes.
3. The session resumes on the next touch — which is Alice asking "hello? did you get my question?" an hour later.

From Alice's side the agent silently ignored her. Every deploy that catches a turn in flight produces one of these. The fix is a synthesized continuation turn: on resume, if the last turn was interrupted recently, run a turn whose prompt says "the previous turn was interrupted by a daemon restart — continue".

A continued turn spends tokens nobody asked for at that moment, and it is the first feature that makes restart *loops* possible — so it originally shipped **opt-in and off by default**, pending production soak. As of **#559** it defaults **on** when the feature can actually apply — a multi-session daemon or a `--no-repl` single-user daemon, with a durable eventlog (`--session-db`) — and stays off, silently, everywhere else. Interactive REPL/TUI runs and in-process library use are excluded by that precondition, so they never auto-continue by surprise; an explicit `enabled: false` is a hard opt-out that survives the flip. The guards below are therefore part of the minimum viable feature, not hardening to add later. (The precondition gate itself lives with the CLI wiring — `resolveAutoContinue` in `cmd/core-agent` — because config alone cannot see the run mode or eventlog presence.)

## Goals

- A session whose turn was interrupted by a restart gets its turn completed (or at least responded to) without waiting for the next human message.
- No write-ahead markers: detection derives entirely from state the eventlog already has.
- Bounded blast radius: freshness window, per-boot continuation cap, crash-loop breaker, and the existing cost ceilings all bound how much unattended work a boot can trigger.
- Works for both trigger paths: lazily-resumed sessions (operator touch) and untouched channel sessions (boot-time scan).

## Non-goals

- **Drain-on-shutdown.** Interrupt-immediately stays the shutdown model (see `docs/site/.../restarts-and-shutdown.md`). Repair + continue beats half-drained.
- **Autonomous runs.** Already covered end-to-end: per-turn checkpoints, `NextWakeAt` persistence, orchestrator-driven `autonomous.Resume`, run locks. This design is for *interactive/attach* sessions only.
- **Re-executing the interrupted tool call transparently.** The continuation turn tells the model what happened; the model decides whether to re-issue the tool. We do not replay tools outside a model turn.
- **Cross-daemon coordination beyond the existing run lock.** A fleet sharing one Postgres eventlog gets mutual exclusion, not work distribution.

## Detection: the eventlog already knows

An interrupted turn is visible in the committed tail of a session's branch-`""` events, without any resume_pending marker (this is the payoff of per-event persistence — Hermes-style pre-marking exists to survive SIGKILL mid-drain, which our write path already survives):

| Tail shape (last content-bearing event) | Meaning |
|---|---|
| User message with no following model event | Turn died before/during the first model call |
| Model event whose parts end in unanswered functionCall(s) | Turn died mid-tool — #537's repair target |
| A `kind: tool_tail_repair` synthesized response with no following model event | Turn died mid-tool, repair already ran, nothing continued it |
| Model text event (final response) | Turn completed normally — nothing to do |

Detection = a single SQL/eventlog read of the last few events per session. `interruptedAt` = timestamp of the last committed event of the broken turn.

One ambiguity: "user message with no model event" also matches *a user message that arrived while the daemon was down* (e.g. injected via a bridge that wrote directly to the DB). Both cases deserve a continuation turn, so the ambiguity is harmless.

## Continuation semantics

- **Prompt**: a synthesized system-note turn, not impersonated user text. Shape (final wording bikeshed deferred):
  > `[system note] The previous turn was interrupted by a daemon restart on <ts>. The last committed events are in your history; any interrupted tool call has been answered with an interruption notice. Continue the task: re-issue interrupted tool calls if their results are still needed, then answer the user's outstanding message. If nothing remains to do, reply briefly acknowledging the restart.`
- **Attribution**: the turn's inbox originator is the daemon itself (`core-agent/auto-continue`), threaded through the existing eventlog metadata extractor so audit logs distinguish continuation turns from human turns.
- **Freshness window** (`auto_continue.freshness`, default `1h`): only turns whose `interruptedAt` is within the window are continued. Staler interruptions wait for the next human message — continuing a 3-day-old thought is more likely to confuse than help. `0s` disables the window check (always continue). Omitting the block leaves the feature at its default — on for a multi-session or `--no-repl` daemon with a durable eventlog, off elsewhere (see #559 and the config surface below).
- **Permissions**: the continuation turn runs under the session's normal gate. In ask-mode sessions with no attached approver, a tool needing approval blocks and times out exactly like any unattended turn — acceptable; the model's text response still lands.

## Triggers

Two, sharing one code path (`maybeAutoContinue(sess)`):

1. **Lazy-resume touch** (already-existing path): after `sessionResumer.Resume` reconstructs the agent, if detection says interrupted-and-fresh, enqueue the continuation turn via the session's wake loop *before* the touching request's own work. Covers operator-attached sessions.
2. **Boot-time scan** (new, cheap): after the attach server is up, one background pass over `agent_session_acl` rows joined with each session's tail events — SQL only, **no agent construction** for sessions that don't need it. Sessions that pass the interrupted-and-fresh check are resumed via the normal resumer and continued. Covers channel sessions nobody re-touches. The scan is skipped entirely when auto-continue is disabled — for interactive/library callers the precondition gate keeps it off by default, and any daemon can opt out with `enabled: false` — preserving the "no eager startup cost" goal of session-resume for everyone else.

Per-boot cap: `auto_continue.max_per_boot` (default 10) bounds how many sessions the boot scan will continue, oldest-interruption first; the rest log and wait for lazy touch. A fleet-wide deploy must not turn into a token stampede.

## The crash-loop breaker (mandatory)

New failure mode: a continuation turn whose execution kills the daemon (pathological tool call, OOM on a huge history) → restart → scan → same turn → crash → loop. K8s backoff slows this but doesn't stop it, and each iteration spends tokens.

- **State**: a tiny `agent_boot_log` table in the eventlog DB (id, boot time, continuations attempted). Written once per boot by the scan. Lives in the DB rather than a file so it survives pod replacement and works identically for every daemon sharing the DB.
- **Rule**: if ≥ N boots (default 3) with attempted continuations occurred within the last window (default 10 min), the scan disables auto-continue for this boot, logs loudly, and serves traffic normally. Lazy-touch continuation is also suppressed for the sessions that were attempted in those boots (their IDs are in the boot log), but allowed for others.
- **Reset**: a boot that completes its continuations without dying ages out of the window naturally. No operator action needed; `/stats`-style surfacing can come later.
- **Write-ahead intent (implementation)**: the boot record is written *before* any resume is driven — a continuation (or resume-time agent construction) that kills the daemon mid-scan must still count on the next boot; a write-behind record would leave every guard blind in exactly the fast-kill scenarios they exist for. If the intent record can't be written, the scan aborts rather than run unguarded. The scan goroutine also recovers panics (the lazy path gets this for free from net/http).
- **Cumulative per-session cap (implementation)**: the committed-note rule and the freshness window do NOT bound a *progress-making* poisoned continuation — each attempt commits fresh events, renewing both. Only attempt counting terminates that sequence: a session attempted 3 times within the last hour (from the same boot log) is skipped until the log ages. Known corner: a multi-daemon fleet restart with legitimate continuations can trip the boot breaker for up to one window — fail-safe direction, cost one quiet boot.
- **Outcome-aware accounting / fleet refund (#575, implementation)**: the write-ahead record is *pessimistic* — it charges every candidate before any resume runs, so a daemon killed mid-scan still counts them. After the synchronous pass, the record is **narrowed** to drop sessions that skipped for a benign or contended reason: the run lock was held by a peer (`ErrSessionLocked`), or the tail went clean or stale between the unlocked candidate scan and the locked re-classification. Everything else stays charged — a queued note is a real attempt, and a *persistent* resume/inject failure is a crash-loop vector the cap must bound. This closes the fleet-amplification bug: in a fleet all sharing one eventlog, every daemon boot-scans the same session but only one wins the run lock; without the refund, each of the N−1 losers would burn an attempt against the shared cap, capping the session out after 3 boots having never once continued it. The narrowing is a single `UPDATE` of the same one-row-per-boot record, so the breaker's "distinct boots with attempts" math is unchanged. Narrowing failures are non-fatal: the pessimistic count simply stands, which fails safe.

Additionally, per-session: a session attempted inside the breaker window is skipped by the next pass — the effective per-session retry cadence is therefore ≈one attempt per window (10 min) regardless of how often a pass runs, and the cumulative cap (3 / hour) terminates a self-renewing session. This is per-session state derivable from the boot log + tail shape; no new marker.

## In-lifetime retry (#575, default-on)

The boot scan and the startup-session trigger each fire **once per boot**. That leaves a gap: a continuation that fails for a *transient* reason — a peer daemon momentarily holding the run lock, a provider blip, a residual DB hiccup — is stranded until the daemon reboots or a human sends the next message. On a stable, long-lived daemon that is exactly the audience the feature promises to serve, so v2.8 adds a bounded in-lifetime retry driver (`AutoContinueRetryLoop`) that re-runs the same guarded pass on a fixed interval (`auto_continue.retry_interval`, default `5m`).

- **Default-on wherever auto-continue is enabled.** The documented contract is no longer "one automatic attempt, then wait for a human" — it is now "self-heal up to the cumulative cap (3), minutes apart, unattended." Set `auto_continue.retry: false` to restore the one-shot-per-boot behaviour.
- **Bounded by the same guards, not a new schedule.** Every tick runs the ordinary pass, so the breaker, the per-session single-retry window, and the cumulative cap all apply unchanged. No exponential backoff is needed: a session attempted within the breaker window simply no-ops on the next tick, and the cap terminates a self-renewing session. `retry_interval <= 0` is rejected at config load.
- **Safety lever (why an in-lifetime retry is safe at all).** The driver runs off the daemon's own context, so a continuation turn that *kills* the daemon kills the driver with it. It can therefore only ever re-fire failures that did **not** take the daemon down; a true crash loop still bottoms out on the cross-boot breaker exactly as before. The driver goroutine is joined (via a `sync.WaitGroup`) before the eventlog handle closes, so a mid-tick pass never races DB teardown — consistent with the clean-shutdown milestone.

## Multi-daemon fleets

Multiple daemons sharing one Postgres/MySQL eventlog: both the boot scan and lazy touch must not double-continue one session. Reuse the existing `agent_run_lock` (5s heartbeat / 30s stale-steal): `maybeAutoContinue` acquires the session's lock for the duration of the continuation turn; `ErrSessionLocked` means another daemon got there — skip silently. This is the same primitive `autonomous.Resume` already uses, so fleet semantics stay uniform.

## Cost controls

Continuation turns run under the session's existing `CostCeiling` (per-turn and per-session caps, #145) — no new mechanism. The new knobs bound *count*, not spend: `max_per_boot`, freshness, the breaker. Recommendation in docs: deployments enabling auto-continue should also set `agent.max_turn_cost_usd`.

## Config surface

```json
{
  "agent": {
    "auto_continue": {
      "enabled": true,
      "freshness": "1h",
      "max_per_boot": 10,
      "retry": true,
      "retry_interval": "5m"
    }
  }
}
```

`enabled` is a tristate (#559): omit it (or omit the whole block) for the default — on for a multi-session or `--no-repl` daemon with a durable eventlog, off (silently) for interactive/library callers; `true` forces it on where it can apply (and warns-and-ignores where it cannot); `false` is a hard opt-out. `retry` defaults to on when auto-continue is enabled (omit or `true` to self-heal; `false` for one-shot-per-boot); `retry_interval` defaults to `5m` and must be `> 0`. Breaker thresholds are constants in v1 (3 boots / 10 min); making them knobs waits for a demonstrated need.

## Proposed PR shape

1. **PR 1 — detection + lazy-touch continuation**: tail classifier (pure function over events, heavily tested), config block, `maybeAutoContinue` on the resume path, run-lock integration, originator attribution. No boot scan yet — feature is useful immediately for operator-attached sessions.
2. **PR 2 — boot scan + breaker**: `agent_boot_log` table, scan with `max_per_boot`, crash-loop breaker, per-session single-retry rule.
3. **PR 3 — docs**: restarts-and-shutdown page section, configuration reference, multi-session concepts update.

## Open questions

1. **Should the continuation turn notify attached SSE clients distinctly** (a `turn-source: auto-continue` field on status-update) so TUIs can render "resuming interrupted work…"? Lean yes, cheap.
2. **Bridged chat sessions**: should the continuation's final text be pushed through the same outbound path the interrupted turn would have used (Slack adapter etc.)? The adapter layer lives above core-agent (AX / bridges); v1 answer: the text lands in the session and streams to whatever is attached, adapters deliver on their next poll/stream — no special casing.
3. **`default` bootstrap session**: the single-user startup agent has no ACL row and is excluded from both triggers (consistent with "ACL row ⟺ resumable"). Is lazy-only continuation for single-user REPL daemons worth a follow-up? Lean: no — an interactive REPL has a human present by definition.
   - *Resolved (#558, 2026-07-31):* the "human present" reasoning holds for REPL/TUI but NOT for headless `--no-repl` single-user daemons (the `examples/gke-deploy` shape). `AutoContinueStartupSession` now covers that shape: pre-classify unlocked (a clean boot must not burn a cumulative-cap attempt — this trigger fires every boot), write-ahead intent record, then the shared lock/classify/inject core. Interactive modes remain excluded.
