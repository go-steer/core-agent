# Subagent return contract

Status: accepted, 2026-08-13. Tracks [#727](https://github.com/go-steer/core-agent/issues/727),
[#728](https://github.com/go-steer/core-agent/issues/728),
[#729](https://github.com/go-steer/core-agent/issues/729),
[#730](https://github.com/go-steer/core-agent/issues/730),
[#731](https://github.com/go-steer/core-agent/issues/731),
[#732](https://github.com/go-steer/core-agent/issues/732).
Milestone: v2.9. Part of the [#589](https://github.com/go-steer/core-agent/issues/589)
Hermes-replacement epic.

## The problem in one sentence

A subagent has no reliable way to hand a value back to the agent that
delegated to it, so the parent receives whatever the subagent happened
to say last — which the continuation loop guarantees is the *worst*
thing it said, not the best.

## What the 2026-08-13 UAT showed

Live GKE, `core-agent-demo-3` in namespace `kube-platform-native`, a
`bad-image` incident on `emailservice`. Subagent session
`019ffcf5-…:sub:bg.cluster-1`, 48 events:

1. **21:11:52** — the `cluster` subagent produced a correct root-cause
   analysis, recovered the good image tag from
   `last-applied-configuration`, and wrote a valid manifest patch.
2. The driver injected `"continue"`. Six times.
3. The model reached for `mark_task_done` — not registered on
   subagents — and got a tool-not-found error whose "available tools"
   list included `report_completed` but which the model never acted on.
4. It scope-crept across four namespaces.
5. It ended at **"standing by in a healthy, inactive state"**.
6. That sentence is what `spawn_agent` returned to the parent.

Total: ~190 events, $1.05.

**The control.** The *identical* subagent content, run standalone with
`core-agent -p`, produced the correct RCA, the right image, and the
patch in **7 turns / $0.1010**. Content is exonerated; the runtime is
implicated. Everything below follows from that one comparison.

## Root cause

`agent.Run` is **already a terminating loop**. The ADK runner drives
model → tool → model until the model emits a turn with no tool calls,
bounded by `max_steps`. That is exactly what `runOneTurn` wraps.

The autonomous driver's entire contribution on top of it is: *after*
that loop terminated naturally, inject `continuationPrompt` (`"continue"`)
and run it again, up to `maxTurns` (50).

For a **standing worker** — "monitor this fleet" — that is correct.
"No tool calls this turn" means *idle*, not *finished*, so an explicit
done signal is genuinely required.

For a **bounded delegation** — "diagnose this incident" — natural
termination already *is* completion, and the re-drive is pure harm.
Bounded delegations are running on the standing-worker loop and paying
all of its costs for none of its benefits.

Three secondary defects compound it, each independently sufficient to
lose the result:

- The return is split across **two tools** (`report_completed` sends,
  `report_done` terminates) with nothing enforcing the second.
- `FinalText` is **overwritten every turn**, so it degrades monotonically
  as the run continues.
- The subagent's cost budget is checked **between turns only**, so it
  never preempts the expensive part.

## Settled decisions (do not relitigate)

1. **The discriminator is bounded-vs-standing, not sync-vs-async.**
   The UAT's `cluster` subagent was spawned *async* and had a terminal
   deliverable. Keying the behavior off `wait:true` would have missed
   the exact case that failed.

2. **Natural termination is the completion signal for bounded
   delegations, and they register no done tool at all.** Not "a done
   tool the model is more likely to call" — no done tool. One
   termination path admits no ambiguity about which signal to use, and
   the observed failure was precisely ambiguity between three
   near-synonymous names.

3. **A stop condition on the existing driver, not a second driver.**
   `stopOnNaturalEnd` in `autoConfig`, checked beside
   `turnRes.doneSignaled`. This inherits retry policy,
   checkpoint/resume, the pause hook, per-turn timeout, budget
   wrappers, and the terminal-alert machinery unchanged. A parallel
   delegation driver would fork all of that and drift.

4. **Standing workers keep today's behavior verbatim.** The re-drive
   loop is correct for them. This work narrows where it applies; it
   does not remove it.

5. **A premature stop returns a partial, and recovery moves to the
   parent.** The parent holds the goal and can re-ask with specifics;
   `"continue"` injected blind inside the subagent knows nothing about
   what is missing, and in the UAT produced namespace scope-creep
   rather than completion.

6. **The stop reason is a typed field, not prose.** `natural` /
   `max_steps` / `budget` / `error`. A parent cannot distinguish
   "finished" from "ran out of room" by reading a text blob, and
   decision 5 makes that distinction load-bearing.

7. **One return tool, named for what it does.** `return_result(result)`
   sets `DoneDetail`, pushes the `completed` alert, and stops the loop.
   `report_done` / `report_completed` / `mark_task_done` become aliases
   onto the same handler so a model reaching for any of them succeeds.

   The old names are lifecycle-status names, and that is *why* they
   behaved this way. `docs/autonomous-plan.md:42` introduces
   `report_done` as one of the "Built-in `report_done` / `set_status`
   lifecycle tools", and `pkg/tools/lifecycle.go:68` shows the shape it
   was cut from — "Emit a lifecycle status … **detail is an optional
   short human-readable note**". The payload field is called `detail`
   because it was designed as a status annotation, and it did exactly
   what its name asked. `WithDoneToolName` being configurable is the
   other tell: you make a status label configurable, not a return
   statement.

8. **The return contract lives in the subagent's instruction, not in a
   tool description.** [#641](https://github.com/go-steer/core-agent/issues/641)
   put it in `report_done`'s description, which only reaches a model
   that reads and calls that specific tool. It has to hold on every
   termination path — natural stop, budget cap, watchdog halt — which
   means the system prompt.

9. **`FinalText` is redefined, not supplemented.** Every current
   consumer wants "what did it find"; none wants "what did it say
   last". A parallel `BestText` would leave the wrong field as the
   obvious one. Observable behavior changes for library users, so it
   ships with a `**BREAKING:**`-adjacent CHANGELOG note and an
   `examples/autonomous*` sweep.

10. **In-turn budget enforcement lands before the terminating loop.**
    Once a bounded delegation runs a single turn, every between-turn
    budget check is unreachable by construction, leaving `max_steps` —
    a step count, not a cost — as the only bound. Shipping #730 first
    would trade a runaway loop for a runaway turn.

## Out of scope

- **Removing the autonomous driver's re-drive loop entirely.** Standing
  workers need it (decision 4).
- **Lowering the subagent depth cap.** Depth capping already exists and
  works (`background/manager.go:39` `defaultMaxDepth = 2`;
  `agent.WithSubagentMaxDepth`, default 2). The UAT's self-recursion
  happened at depth 1, inside the cap. Lowering it would break
  legitimate two-level fan-out without touching the observed failure.
- **Duplicate-goal suppression.** Goal similarity is fuzzy; the
  structural self-spawn guard covers the observed failure at zero
  false-positive risk.
- **A schema-validated structured return** (`result_schema` on
  `spawn_agent`, forced-tool-call validation with retry). Genuinely
  wanted, but it is a new user-facing surface that should follow the
  contract being correct first. Deferred to v3.0.
- **Per-subagent budget configuration generally** — that is
  [#713](https://github.com/go-steer/core-agent/issues/713). #729 is
  narrower: making the ceiling that already exists actually fire.
- **The `structural_json` digest total-loss defect** observed in the
  same UAT (71900 raw bytes → 65 bytes of digest). Real, unrelated,
  filed separately.

## Sequencing

| PR | Issues | Depends on | Change |
|---|---|---|---|
| 1 | #727, #728 | — | Return contract in the subagent instruction; `return_result` + aliases |
| 2 | #729 | — | Arm the subagent Agent's in-turn cost ceiling against a spawn-time baseline |
| 3 | #730 | 2 | `madeToolCalls` + `stopOnNaturalEnd`; bounded spawns drop the done tool; typed stop reason |
| 4 | #731 | 3 | Retain the last *substantive* turn's text |
| 5 | #732 | — | Self-spawn guard |

PR 2 has no code dependency on PR 1 but is sequenced after it so the
train lands in one rebase order.

## Verification

**Unit.** Each PR ships a regression test that fails on the parent
commit, per the repo's bug-fix gate. The decisive fixtures are scripted
against the real failure shape recorded in the UAT eventlog: a subagent
that produces a good answer on turn 3 and narrates itself for six turns
afterwards.

**End-to-end.** Re-run the `bad-image` scenario in
`core-agent-demo-3` / `kube-platform-native`. Acceptance is the parent
receiving the RCA and the manifest patch, in place of
`"standing by in a healthy, inactive state"`, at a cost within an order
of magnitude of the $0.1010 standalone control.

## What this does not fix

The parent emitting no operator-visible text when a cost ceiling halts
its turn — the incident dies silently. Observed in the same UAT,
adjacent to this work, tracked separately.
