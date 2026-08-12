# Resetting a tripped guardrail

Design doc for the operator-facing recovery path out of a halted
session: what "reset" means for the behavioral watchdog and the cost
ceiling, who may do it, and what the caller sees.

**Status:** SHIPPED (2026-08-12).

**Tracking issues:**
[#666](https://github.com/go-steer/core-agent/issues/666) (operator-facing
reset) and [#331](https://github.com/go-steer/core-agent/issues/331)
(`/reset-ceiling` semantics).

## Motivation

core-agent has two runtime halt switches:

- the **behavioral watchdog** in enforce mode
  (`pkg/agent/watchdog.go`), which stops a session that is looping on
  the same tool call, and
- the **cost ceiling** (`pkg/agent/cost_ceiling.go`), per-turn
  (`max_turn_cost_usd`) and per-session (`max_session_cost_usd`).

Both work the same way: a post-turn hook sets a flag, and the next
`Run` refuses at pre-flight with a message explaining why. Both were
resettable in-process — `Agent.ResetWatchdog()`,
`Agent.ResetCostCeiling()` — and neither had a caller. Nothing on the
attach HTTP surface, nothing in the TUI, nothing in the CLI.

So a halted daemon session was halted permanently. The only recovery
was to kill the process and start a new session, which throws away the
conversation the operator was trying to rescue. Worse, the trip
messages named the Go methods, which is advice an operator holding
only a `core-agent-tui` connection cannot act on.

This is a small instance of the v2.9 theme: we shipped a safety
property (halt a runaway) without shipping the half that makes it
usable (let a human decide the halt was wrong and continue). A
backstop nobody can clear is not a backstop, it's a crash.

## What "reset" means

#331 left this open, and it is the load-bearing question, because
"reset the cost ceiling" has three plausible readings:

1. **Zero the accumulator.** Forget that the session spent $12.
2. **Restart the window.** Keep the total, but start a new spend
   window that the ceiling is measured against.
3. **Raise the bar.** Keep the total, keep one window, move the
   ceiling up by an operator-supplied amount.

We ship **(3), bump-only**, and deliberately do not offer (1) or (2).

(1) and (2) both make `Agent.SessionCostUSD()`, the `/usage` endpoint,
and the eventlog-derived cost disagree about what the session actually
spent. The eventlog is the audit record; a reset that rewrites it
turns "how much did this session cost?" into a question with three
different answers depending on which surface you ask. Cost reporting
is a thing operators bill against and a thing #642-class safety claims
rest on — it does not get to be approximate because a recovery path
found it convenient.

(3) has none of that. Spend is append-only and monotone. The ceiling
is a policy number the operator chose, and an operator raising a
number they chose is exactly the decision they are entitled to make.
The transcript reads honestly afterwards: *spent $12, ceiling raised
from $10 to $15 by alice at T*.

The watchdog has no accumulator, so its reset is just "clear the
tripped flag." The loop-detection state resets with it; if the model
resumes the same loop, it trips again, which is the correct outcome.

### Consequence: a bare reset of a session trip is refused

If the session has spent $12 against a $10 ceiling, clearing the
tripped flag without raising the ceiling produces a session that halts
again on the very next turn. That is a 200 OK that achieves nothing —
precisely the failure mode this endpoint exists to remove.

So the adapter checks first, and refuses with `ErrGuardrailRetrip`
(HTTP 409 Conflict) when `spent >= ceiling + additional_budget_usd`,
naming the shortfall:

```
session has spent $12.0000 against a $10.0000 ceiling;
add more than $2.0000 via additional_budget_usd
```

A **per-turn** trip does not have this problem — the next turn starts
a fresh turn window — so a bare reset of a per-turn trip is allowed.

The refusal is **atomic**: the retrip check runs before any mutation,
so a rejected reset leaves the ceiling and the trip flags exactly as
they were. An operator never has to work out how much of their request
landed.

Order of operations on the success path is the mirror image: raise the
ceiling *first*, then clear the flags. The reverse leaves a window in
which the session is runnable against a ceiling it has already blown.

## Who may reset, and the audit trail

`auth.ActionSessionWrite` — not a new admin action.

The reasoning: the next thing an operator does after clearing a halt
is `POST /inject` or a new turn, both already `ActionSessionWrite`. A
caller who can steer the session can already spend its money. Gating
the reset harder buys no safety; it just adds a second credential that
deployments would route around by handing everyone the stronger one.

The endpoint is also deliberately **not** registered through
`routeSessionLimited`. Cost-limiting the way out of a cost trip is a
trap: the operator most in need of the endpoint is the one whose
per-caller budget is exhausted.

#331 asked whether a reset should generate an audit event. It does:
an `attach-guardrail-reset` event, authored `attach/guardrail-reset`,
carrying the caller identity, which guardrails were cleared, and the
budget added. It is written only when the reset actually changed
something — a defensive reset of an untripped session is a no-op and
does not clutter the log.

"Who un-halted the session that had blown its budget, and how much
runway did they hand it?" is otherwise invisible in the transcript.

## Surface

#331 asked whether this should be a slash command or a REST endpoint.
Both, routed through the same `attachadapter` methods so they cannot
drift:

| Surface | Read | Reset |
| --- | --- | --- |
| Attach HTTP | `GET /sessions/{id}/guardrails` | `POST /sessions/{id}/guardrails/reset` |
| In-process TUI | `/guardrail` | `/guardrail reset [watchdog\|cost_ceiling\|all] [+<usd>]` |
| CLI (boot) | startup line names both | — |

The REST pair is what tooling and the remote TUI use; the slash is
what a human at a terminal reaches for. The slash command is
registered unconditionally, not gated on whether a guardrail is armed
— an operator looking for the recovery command should not have to
already know which backstop caught them.

`/guardrail` is not added to `buildSlashCommands`. That list maps to
`POST /slash/<name>` routes, and there is no `slash/guardrail` route;
advertising one would be a lie of the same species this milestone is
about. Clients gate on the `guardrails` feature key instead.

### Scope

`guardrail` selects `watchdog`, `cost_ceiling`, or `all` (the
default). Two behaviors fall out of scoping that are easy to get
wrong, and both are tested:

- A watchdog-scoped reset on a cost-tripped session must **not** be
  blocked by the cost trip, and must not clear it. Otherwise a session
  that blew its budget could never have its watchdog cleared.
- That same reset must not report "nothing to reset" and leave the
  operator staring at a session that is still halted. It reports
  *"…; still halted by cost_ceiling"*.

`additional_budget_usd` on a watchdog-scoped reset is **rejected**
(400), not silently dropped. Silently dropping it would let an
operator believe they had bought runway they had not.

## What the caller sees

`GET /guardrails` returns live state, not configuration: watchdog mode
and tripped-ness, both ceilings, current session spend, and
`would_retrip` — which is what lets a client render "you must add
budget" *before* the operator tries a bare reset and eats a 409.

`Halted` is the single boolean a client needs to decide whether to
show the recovery affordance at all.

Two capability keys are reported in the attach features frame:

- `guardrails` — the endpoints are serviceable.
- `cost_ceiling` — a bound is actually **armed**. This key existed
  before #666 and was hardcoded `false`. It is now a live-state read,
  not an interface-presence probe, which matters because
  `attachadapter.Adapter` satisfies every optional interface
  unconditionally (see [#490](https://github.com/go-steer/core-agent/issues/490)).

Status codes: 200 reset, 400 malformed request or unknown guardrail or
negative budget, 409 the reset would provably re-trip, 501 the
registrant has no guardrail capability.

## Out of scope

- **Pricing overrides.** #331 wondered whether reset should compose
  with a mid-session pricing change. It does not: pricing is resolved
  where usage is recorded, and a reset that also re-priced historical
  spend would be the accumulator rewrite this design rejects.
- **A remote `core-tui` palette entry.** That lives in the `core-tui`
  repo. The REST pair is the contract it will bind to.
- **Auto-reset / operator-approval prompts.** A halt that clears
  itself on a timer is not a halt. If a session should run to a bigger
  number, configure a bigger number.
