# Snapshot of issue #1002, as it stood when T1 was cut

The T1 replay (`run.sh --tier t1 --replay`) hands the agent this file
instead of GitHub. The live issue is closed now and links the fix, so
reading it would give the answer away. This snapshot holds the body and
the one comment that existed when live run 2 started
(2026-09-24T12:58Z). Don't edit it: every replay grades against the same
text.

---

## #1002: spawn_agent discards an acked return_result when the subagent then errors — parent re-does the work

_Opened 2026-09-06T22:29:56Z_

## Summary

A subagent that calls `return_result`, gets `{"ack":"ok"}`, and *then* hits an error has its banked result **silently discarded**. The parent is handed the error text as `output` plus the guidance *"the subagent failed. Whatever text is here is incidental, not a result."* — so it correctly refuses to use it, and re-does the entire delegated investigation.

This is the error-path half of #641. That issue fixed the happy path (`return_result` now reaches the parent). The failure path still throws findings away.

## Ground truth — GKE drill scenario B, 2026-09-06

`std-simian-test` / `online-boutique`, run `20260906T221907Z-b`, daemon `2.9.0-dev.6`.

The `cluster` subagent ran six real `gke_*` reads, produced a complete RCA (`emailservice` OOMKilled, memory limit squeezed to `8Mi`, previously `128Mi` request `64Mi`, recovered from the `last-applied-configuration` annotation), called `return_result` at seq 1493, and was acked at seq 1494:

```json
{"functionResponse":{"name":"return_result","response":{"ack":"ok"}}}
```

It then hit a Vertex 429. What the parent received:

```json
{"name":"spawn_agent","response":{
  "branch":"bg.cluster-1","name":"cluster-1",
  "status":"failed","stop_reason":"error",
  "guidance":"the subagent failed. Whatever text is here is incidental, not a result.",
  "output":"Error 429, Message: Resource exhausted. …RESOURCE_EXHAUSTED…"}}
```

The acked RCA is nowhere in that payload.

## Cause

`pkg/agent/background/tools.go`, `completionResult`:

```go
if r != nil || runErr != nil {          // knows BOTH can be set
    ...
}
if runErr != nil {
    res.Output = runErr.Error()
    return res                          // early return — r.DoneDetail never read
}
...
res.Output = r.DoneDetail               // unreachable when runErr != nil
```

The guard at the top already contemplates `r != nil && runErr != nil`. Twenty lines later that case discards `r`.

## Impact

The parent recovered — and recovered *well*: it took the guidance at face value, refused to confabulate from the incidental 429 text, and re-investigated with seven of its own cluster reads. The return-contract work (#727–#732) did its job; the final answer was correct and fully cited.

But it cost **7 extra cluster calls**, pushing the run to 18 of G5's 25-call ceiling against 14 for the comparable scenario A. A tighter budget, a slower model, or a subagent whose work was more expensive to redo would turn a recoverable blip into a G5 failure or a worse answer. The information needed to avoid all of it had already been banked and acknowledged.

## Proposed fix

When `runErr != nil` and `r != nil && r.Returned`, surface both rather than one:

- `output` — the returned result, which is the thing the parent asked for
- a distinct error field carrying `runErr`
- `stop_reason` / `guidance` saying the subagent **returned a result and then failed**, so the parent knows the findings are real but the run was cut short and may be incomplete

The current guidance string is right for a subagent that failed with nothing banked, and wrong here. `r.Returned` already distinguishes the two — it is read at line 254 and then not used for this.

## Test

A unit test in `pkg/agent/background` driving a handle with both a `Returned` result and a non-nil `Err()`, asserting the result survives. Should fail on current `main`. The live capture is in the drill fixture set if a recorded regression is wanted.

Found by the GKE drill (#970), scenario B, seed 1.

---

### Comment, 2026-09-24T12:30:30Z

## The cause is upstream of `completionResult`

A self-development run (#1116, T1) probed this issue's exact scenario before writing a fix: a subagent calls `return_result`, and its next model call gets a 429. The run came back as:

```
err=Error 429 ... reason="retry_policy_aborted" returned=false doneDetail="" turns=1
```

So by the time `completionResult` sees the handle, `Returned` is already `false` and `DoneDetail` is empty. The result is lost in two places in `pkg/agent/autonomous/autonomous.go` before it ever reaches `completionResult`:

1. **`runOneTurn` (the `a.Run` loop, ~L555).** A stream error returns `out, err` straight away. That skips the `doneCh` drain at the bottom of the function (~L697), so `doneSignaled`/`doneDetail` are never set. The next turn's defensive drain would throw the signal away anyway.
2. **`Run` (~L248–289).** The `if turnErr != nil` branch returns on `AbortRun` before the `if turnRes.doneSignaled` block. `Returned`/`DoneDetail` stay zero even if (1) were fixed. `Resume` in `resume.go` has the same loop shape.
3. **`completionResult` (`pkg/agent/background/tools.go`).** This is the step the issue names. It is real, but on a real run it is currently unreachable: `Returned && runErr != nil` never arrives here.

A fix confined to `completionResult` would pass a unit test that builds the `Handle` by hand, and it would change nothing in the live scenario above. A regression test for this issue has to drive `autonomous.Run` (a fake LLM that returns and then errors) or go through the spawn path end to end.

The proposed shape still holds, and the two upstream hops get the same treatment:
- **Banked result:** record the banked result before the error branch.
- **Outcome and output:** keep `Reason = RetryAborted`, and have `completionResult` surface the banked result together with the run error.

This comment reflects code at `aef7dbb4`.
