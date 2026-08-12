# `wait_and_verify`: bounded poll-until-condition, decoupled from the shell

Design doc for the `wait_and_verify` built-in tool in `pkg/tools`: a first-class primitive that repeatedly calls a **read-only** tool until its result satisfies a condition, or a bounded budget runs out.

**Status:** implemented (2026-08-12), v2.9. **Tracking issue:** [#648](https://github.com/go-steer/core-agent/issues/648).

## Motivation

Fix-and-verify is the SRE headline: apply a change, wait for the system to converge, confirm the new state. The 2026-08-12 tri-team assessment found that the middle step was inexpressible.

- **The only wait was `bash sleep`.** The gke-troubleshoot recipe's `k8s-triage` skill literally instructed the model to "sleep the verify interval named in the reference row" — in a recipe whose `config.json` sets `"tools": {"disable": ["bash"]}` and whose image is distroless. The instruction could not execute.
- **The only poll was N model round-trips.** Absent a wait, the model's alternative is call → look → call → look, one turn per look. A three-minute verify at fifteen-second granularity is twelve extra round-trips of full context, and the model routinely gives up early instead.
- **So every `RESOLVED` was unverifiable.** With no way to observe convergence, the model asserts it. That is [#639](https://github.com/go-steer/core-agent/issues/639) (resolution confabulated without tool-verified evidence) arriving through a different door: the harness didn't make the honest path available, so the dishonest one won.

`schedule_next_turn` doesn't fill this gap — it *ends* the turn. It is the right primitive for "check back in an hour"; it is the wrong one for "wait two minutes for a rollout" (see [Composition](#composition-with-schedule_next_turn)).

## What makes it a primitive rather than a helper

Three properties, all enforced by the runtime rather than stated in a prompt — which is the whole v2.9 framing:

1. **No shell.** Pure Go, in the binary. It works in `distroless/static-debian12:nonroot` exactly as it works on a laptop.
2. **Bounded, with operator ceilings the model can't raise.** Every loop is bounded on wall clock *and* attempt count, and each individual poll runs under the wait's deadline. `tools.wait_and_verify.max_timeout_seconds` / `max_attempts` are operator ceilings; a model asking for more gets an error, not a silent clamp. Token cost is bounded *by construction*: N polls collapse into ONE tool result.
3. **Read-only by construction.** The tool refuses to poll anything the runtime doesn't classify read-only. A loop that can call `write_file` sixty times is an amplifier, not a verifier.

## Tool surface

```
wait_and_verify(
  tool:                "<name of a read-only tool>",
  args_json:           "{\"path\": \"/tmp/x\"}",   // optional; args for that tool
  expect_jq:           ".status.phase == \"Running\"",  // one of these three
  expect_contains:     "Ready",
  expect_not_contains: "CrashLoopBackOff",
  interval_seconds:    15,   // optional
  timeout_seconds:     180,  // optional
  max_attempts:        20    // optional
)
```

The result is a single structured object covering the whole loop:

```json
{
  "verified": true,
  "outcome": "verified",
  "tool": "gke__get_pod",
  "condition": "expect_contains=\"Ready\"",
  "attempts": 5,
  "interval_seconds": 15,
  "elapsed_seconds": 61.2,
  "last_result": "...",
  "observations": [{"attempt": 1, "at_seconds": 0, "matched": false}, ...]
}
```

`outcome` is one of `verified`, `timeout`, `attempts_exhausted`, `canceled`. **An unverified outcome is not an error** — it returns normally with `verified: false` and the observation trail, because "I waited three minutes and it never became Ready" is a finding, not a tool failure. The model needs the evidence to write an honest `UNRESOLVED`.

### Conditions

At least one of `expect_jq`, `expect_contains`, `expect_not_contains` is required — a poll with no condition is an unbounded sleep with extra steps. Supplying several ANDs them, and the result's `condition` field restates what was actually checked.

`expect_jq` runs against the tool's result map via [gojq](https://github.com/itchyny/gojq) (already a dependency, via `json_query`; pure Go, distroless-safe) with jq truthiness: everything except `false` and `null` matches. `expect_contains` / `expect_not_contains` match against the serialized result.

Error handling distinguishes two cases deliberately:

- **A poll error is transient.** The polled tool returning an error is recorded as an observation and the loop continues — a `get_pod` that 404s during a rollout is exactly the state we're waiting out. A persistent error surfaces in `last_error` when the budget runs out.
- **A jq runtime error aborts on attempt one.** A malformed expression will fail identically on every attempt; burning the whole budget to re-learn that wastes the operator's clock.

### Bounds

| Knob | Default | Ceiling |
|---|---|---|
| `interval_seconds` | 5s | clamped **up** to 1s minimum; reported in the result |
| `timeout_seconds` | 60s | `tools.wait_and_verify.max_timeout_seconds` (default 5m) |
| `max_attempts` | 60 | `tools.wait_and_verify.max_attempts` (default 60) |

The asymmetry is deliberate: an interval below the floor is clamped up (a faster poll than requested is a cost question, and the result reports the interval actually used), but a *timeout* above the ceiling is **rejected**, not clamped. Clamping a timeout down would have the tool report "I waited" for a budget it silently shortened — the exact class of unenforced claim this milestone is about.

The ceilings bind the *defaults* too, in the other direction: an operator ceiling below the built-in default lowers that default rather than turning every call into an error. The refusal exists to stop the model claiming a budget it wasn't granted, and a model that asked for nothing claimed nothing.

Each poll is dispatched with a context carrying the wait's deadline, so a tool call that hangs is cancelled when the budget expires rather than hanging the turn.

## Read-only enforcement and `poll_allow`

The tool resolves its target from the agent's own catalog and applies `tools.IsReadOnlyTool` — the same [#460](https://github.com/go-steer/core-agent/issues/460) classifier the mutation serializer uses: the tool's own `ReadOnlyHint()`, then the builtin name table, then the fail-safe default (mutating). Two names are refused unconditionally regardless of classification:

- **`wait_and_verify` itself** — self-recursion.
- **`ask_user`** — read-only, but it blocks on a human. Polling it sixty times is a denial-of-service on the operator.

**The MCP problem.** ADK's `mcptoolset` does not surface the server's `readOnlyHint` annotation, so *every* MCP tool classifies as mutating. Strict enforcement would make the tool useless for the exact recipe that motivated it. The escape hatch is an explicit operator assertion in config:

```json
{
  "tools": {
    "wait_and_verify": {
      "poll_allow": ["gke__get_pod", "gke__list_events"],
      "max_timeout_seconds": 300,
      "max_attempts": 60
    }
  }
}
```

Names are the ones the *model* sees, i.e. namespaced (`gke__get_pod`, not `get_pod`). This is a config-level, operator-signed statement that a named tool is safe to call repeatedly — it is not a model-reachable knob, and it is per-tool rather than per-server on purpose. When ADK grows `readOnlyHint` passthrough, `poll_allow` becomes an override rather than a requirement.

`wait_and_verify` adds **no new authority**: each poll dispatches through the same wrapper stack a direct model call takes, so the permission gate, path scope, URL scope, plan-first gating and output caps all apply unchanged. In particular, polling an MCP tool before `record_plan` is denied under `require_plan_artifact`, same as calling it directly.

## Wiring: late catalog binding

A tool that calls other tools has a construction-order problem — it must be *in* the catalog it holds a reference to. The resolution is late binding:

```go
// pkg/agent/agent.go
catalogBinders := tools.CatalogBinders(o.tools)   // BEFORE the wrappers go on
...                                                // gate / serialize / instrument
tools.BindCatalogs(catalogBinders, o.tools, o.toolsets)  // hand over the WRAPPED slices
```

Binders are collected from the *unwrapped* slice (none of `gatedTool` / `serializedTool` / `timedTool` forwards `BindCatalog`, and adding forwarding to four wrappers to support one tool is the wrong trade) but are handed the *wrapped* catalog, so a polled call traverses exactly the layers a direct model call would. `RunSubtask` does the same with the subtask's own tool subset, so a subagent's refusal to poll a mutating tool is evaluated against what that subagent can actually reach — not against the parent's catalog.

Binding inside `agent.New` rather than at each construction site is the same argument as [#643](https://github.com/go-steer/core-agent/issues/643)'s lazy guardrail restore: a wiring step an embedder can forget is a capability that silently isn't there. An unbound tool errors with "no tool catalog is bound" rather than vanishing from the model's schema.

## Composition with `schedule_next_turn`

The two are complements, not alternatives, and the boundary is roughly the model's turn budget:

- **Seconds to a few minutes** → `wait_and_verify`. Stay in the turn; the poll loop costs one tool result; the model keeps its diagnostic context.
- **Minutes to hours** → `schedule_next_turn`. End the turn, release the context, come back. A `wait_and_verify` on the *next* turn then confirms the state cheaply.

A converging rollout is the first; a "did the memory leak come back overnight" check is the second.

## Non-goals

- **Durable waits across process restarts.** The budget is in-turn wall clock. A pod restart mid-wait loses it; `schedule_next_turn` is the durable option.
- **Polling mutating tools "just once more".** No retry-with-mutation mode. That is a remediation loop, and it belongs behind `record_plan` and the permission gate, one deliberate call at a time.
- **Cross-tool composite conditions** ("A is Ready AND B has no events"). One tool, one condition. Two calls express the conjunction, and the observation trails stay separately auditable.

## Testing

`pkg/tools/wait_and_verify_test.go` drives a scriptable fake tool with a millisecond-scale minimum interval: verify-on-first-attempt, poll-until-true, timeout-with-evidence, attempt-count bounding, refusal of a mutating tool / itself / `ask_user`, `poll_allow` admitting an unclassified tool, unknown-tool errors that list the pollable names, budget-above-ceiling rejection, interval clamping, malformed and runtime jq errors, transient vs persistent poll errors, cancellation, single-hung-poll bounding, toolset resolution, and survival of an unreachable toolset.

`pkg/agent/wait_and_verify_wiring_test.go` covers the binding itself end-to-end through `agent.New` — including that the refusal still holds *through* the serializer and instrumenter wrappers, which must not mask the read-only classification.
