# Auto-continuing interrupted turns after a restart

Design doc for the last piece of the "Clean shutdown & crash resume" milestone (#539): optionally letting the daemon *finish the interrupted thought* after a restart, instead of resuming with intact-but-inert history.

**Status:** proposed (2026-07-30). Un-defers the "auto-recovering an in-progress turn at restart" non-goal in `docs/session-resume-design.md`, whose corrupting half already shipped as #537 (tail repair). Depends on: #537 (shipped), #538 (shipped — bounded teardown), lazy session resume (v2.5, #178).

## Motivation

With #537 + #538 merged, a K8s rolling upgrade is safe: committed events survive, histories are repaired, sessions resume lazily. What it is *not* is polite. The failure UX:

1. Alice asks the Slack-bridged session a question.
2. The pod rolls mid-turn. The turn is interrupted; her question is committed; no answer ever comes.
3. The session resumes on the next touch — which is Alice asking "hello? did you get my question?" an hour later.

From Alice's side the agent silently ignored her. Every deploy that catches a turn in flight produces one of these. The fix is a synthesized continuation turn: on resume, if the last turn was interrupted recently, run a turn whose prompt says "the previous turn was interrupted by a daemon restart — continue".

This is deliberately **opt-in and off by default**: a continued turn spends tokens nobody asked for at that moment, and it is the first feature that makes restart *loops* possible. The guards below are therefore part of the minimum viable feature, not hardening to add later.

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
- **Freshness window** (`auto_continue.freshness`, default `1h`): only turns whose `interruptedAt` is within the window are continued. Staler interruptions wait for the next human message — continuing a 3-day-old thought is more likely to confuse than help. `0s` disables the window check (always continue); omitting the block disables the feature entirely.
- **Permissions**: the continuation turn runs under the session's normal gate. In ask-mode sessions with no attached approver, a tool needing approval blocks and times out exactly like any unattended turn — acceptable; the model's text response still lands.

## Triggers

Two, sharing one code path (`maybeAutoContinue(sess)`):

1. **Lazy-resume touch** (already-existing path): after `sessionResumer.Resume` reconstructs the agent, if detection says interrupted-and-fresh, enqueue the continuation turn via the session's wake loop *before* the touching request's own work. Covers operator-attached sessions.
2. **Boot-time scan** (new, cheap): after the attach server is up, one background pass over `agent_session_acl` rows joined with each session's tail events — SQL only, **no agent construction** for sessions that don't need it. Sessions that pass the interrupted-and-fresh check are resumed via the normal resumer and continued. Covers channel sessions nobody re-touches. The scan is skipped entirely when auto-continue is disabled (the default), preserving the "no eager startup cost" goal of session-resume for everyone else.

Per-boot cap: `auto_continue.max_per_boot` (default 10) bounds how many sessions the boot scan will continue, oldest-interruption first; the rest log and wait for lazy touch. A fleet-wide deploy must not turn into a token stampede.

## The crash-loop breaker (mandatory)

New failure mode: a continuation turn whose execution kills the daemon (pathological tool call, OOM on a huge history) → restart → scan → same turn → crash → loop. K8s backoff slows this but doesn't stop it, and each iteration spends tokens.

- **State**: a tiny `agent_boot_log` table in the eventlog DB (id, boot time, continuations attempted). Written once per boot by the scan. Lives in the DB rather than a file so it survives pod replacement and works identically for every daemon sharing the DB.
- **Rule**: if ≥ N boots (default 3) with attempted continuations occurred within the last window (default 10 min), the scan disables auto-continue for this boot, logs loudly, and serves traffic normally. Lazy-touch continuation is also suppressed for the sessions that were attempted in those boots (their IDs are in the boot log), but allowed for others.
- **Reset**: a boot that completes its continuations without dying ages out of the window naturally. No operator action needed; `/stats`-style surfacing can come later.

Additionally, per-session: a session whose continuation turn was attempted in the previous boot and is *still* interrupted (i.e., the continuation itself died) is skipped by the next boot's scan — one automatic retry, then wait for a human. This is per-session state derivable from the boot log + tail shape; no new marker.

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
      "max_per_boot": 10
    }
  }
}
```

Absent block = disabled = today's behavior. Breaker thresholds are constants in v1 (3 boots / 10 min); making them knobs waits for a demonstrated need.

## Proposed PR shape

1. **PR 1 — detection + lazy-touch continuation**: tail classifier (pure function over events, heavily tested), config block, `maybeAutoContinue` on the resume path, run-lock integration, originator attribution. No boot scan yet — feature is useful immediately for operator-attached sessions.
2. **PR 2 — boot scan + breaker**: `agent_boot_log` table, scan with `max_per_boot`, crash-loop breaker, per-session single-retry rule.
3. **PR 3 — docs**: restarts-and-shutdown page section, configuration reference, multi-session concepts update.

## Open questions

1. **Should the continuation turn notify attached SSE clients distinctly** (a `turn-source: auto-continue` field on status-update) so TUIs can render "resuming interrupted work…"? Lean yes, cheap.
2. **Bridged chat sessions**: should the continuation's final text be pushed through the same outbound path the interrupted turn would have used (Slack adapter etc.)? The adapter layer lives above core-agent (AX / bridges); v1 answer: the text lands in the session and streams to whatever is attached, adapters deliver on their next poll/stream — no special casing.
3. **`default` bootstrap session**: the single-user startup agent has no ACL row and is excluded from both triggers (consistent with "ACL row ⟺ resumable"). Is lazy-only continuation for single-user REPL daemons worth a follow-up? Lean: no — an interactive REPL has a human present by definition.
