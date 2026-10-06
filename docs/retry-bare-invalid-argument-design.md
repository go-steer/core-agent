# Retrying a bare 400 after a served call (#1247)

On 2026-10-06 a drill turn on `gemini-3.7-flash` died on

```
Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT, Details: []
```

straight after a turn on the same session, config and model had
succeeded. That is the shape #898 recorded on 2.9.0-dev.4. There, two
probes on the same session straight afterwards ran clean, so the
history was not poisoned and the config was not wrong. #898 changed the
hint. #935 built the retry for 429 and 503, and kept this 400 out of
it: n was 1, and on its own the error is indistinguishable from a
malformed request. It was classed `config_error`, the retry never ran,
and the operator got no answer.

This doc settles when that 400 gets one retry.

## The policy

`models.RetryPolicy` gains a second predicate,
`IsTransientAfterSuccess`. It is consulted only for a call whose
context carries a **marked** `models.PriorSuccess` and is **not a side
call**. Everywhere else an error is judged by `IsTransient` alone, as
it is today. The Gemini adapter wires it to
`gemini.IsBareInvalidArgument`, which is true when all three of these
hold:

- code 400 **and** status `INVALID_ARGUMENT`;
- no details (`len(APIError.Details) == 0`; for an untyped error, the
  rendered `Details: []`);
- the message is exactly `Request contains an invalid argument.`

Any retry it licenses is an ordinary `RetryPolicy` retry. It draws on
the same process-wide budget (burst 3, one refill per 30s). It logs the
same `transient provider error (…) — retrying once after 2s` line and
the same outcome line. It surfaces through the #1206 machinery: a
`provider_retry` stamp when it recovers, a `provider retry …` prefix
when it does not, and side-call labelling.

## Where "a prior call succeeded" lives

The policy is one process-wide instance with no session
(`provider-retry-transcript-design.md`). The evidence has to arrive
with the call, so it rides the call's context, the same way
`AsSideCall` does:

- `models.PriorSuccess` is an atomic flag. `models.WithPriorSuccess(ctx,
  rec)` puts it on a context; a nil `rec` shadows an inherited one.
- `Agent.New` creates one per Agent, and `Agent.Run` puts it on every
  turn's context. An Agent's model, persona and tool registrations
  are fixed at `New`, so for one Agent "the session was served once"
  means "a request built from this config was accepted once". That
  holds for the config, not for every byte of the request: Vertex
  context caching swaps the system instruction and tools for a cache
  reference after the first turn, and an MCP toolset re-lists its tools
  on each request. See "The cost, stated".
- The agent's event loop marks it (`markIfServed`, in
  `pkg/agent/prior_success.go`). The trigger is a model response that
  arrived whole: not partial, no error, no `ErrorCode`, model role, at
  least one part. A function call counts.
- A sync subagent delegation and a `RunSubtask` each get a **fresh**
  record per run, marked from their own loops. Each runs its own
  instruction and tools on a context derived from the parent's tool
  call, so the parent's record must not reach it. Both in-tree
  `RunSubtask` callers, the MCP digest and the agentic tools, mark
  their context as a side call, so in-tree a subtask never qualifies.
  Its record matters to a library caller that does not.
- `RunWithContents` shadows any inherited record with nil. It creates
  its session on the spot, so no served call precedes its first.

## Settled decisions (do not relitigate)

1. **A bare 400 on a run's first call is never retried.** That is what
   makes the policy safe: a request that is malformed from the start
   fails exactly as before, with one request and the bare error, no
   prefix and no log line. The record is the only door, and nothing
   but a served call opens it.
2. **The predicate needs the generic message, not just empty details.**
   Vertex sends many 400s with `Details: []` that still say what is
   wrong in the message ("an empty text parameter", a function
   declaration's name). A 400 that names a field has answered the
   question, and a retry is told the same thing again. Requiring the
   exact message also keeps the predicate clear of
   `vertexcache.IsCacheGone`, whose expired-cache 400 reads
   `Cache content <id> is expired.`
3. **`IsTransient` is unchanged and still refuses the bare 400.** The
   new predicate sits beside it, not inside it, so no code path that
   lacks the record can retry the 400. `TestIsTransient` and
   `TestIsTransient_StillRefusesTheBare400` pin this.
4. **The agent marks the record and the policy only reads it.** The
   agent is the component that knows which run a call belongs to.
   Marking from the event stream works for every provider, and no side
   call can mark it, because side calls do not go through that loop. We
   rejected marking inside `Wrap`. It would put session state in the
   one object that is deliberately session-free, and a parent's
   successes would reach every nested run that inherited its context.
5. **Side calls never qualify.** The approver, the summarizer, a title,
   `/btw`, an MCP digest and an agentic subtask each send their own
   instruction and usually no tools. A session's success is no evidence
   about their requests.
6. **Nested runs get their own record, never the parent's or the inner
   agent's.** A sync subagent's inner `*Agent` is shared by every
   tenant of a multi-session daemon (#741), so nothing session-shaped
   may live on it. A fresh record per delegation still gives a
   multi-call delegation the retry from its second call on
   (`TestSubagent_DelegationMarksItsOwnRecord`). A
   background subagent is its own `Agent` and so has its own record.
7. **Same budget, same log line, same surfaces.** A retry that
   `a2_count.py` cannot count is a transcript-only retry, and box A2
   would fail on it. The log line is unchanged and its error text
   contains no parentheses, so `RETRY_RE` matches it. The selftest has
   a case.
8. **The classification is unchanged.** A bare 400 that persists
   through its retry reaches the turn-error frame as
   `provider retry persisted: Error 400, …`. One that finds the shared
   budget spent is not retried, logs `NOT retried`, and reaches the
   frame as `provider retry skipped, budget spent: Error 400, …`. Both
   are still `config_error`, code `400`, not retryable, with the same
   hint (`TestClassifyTurnError_Bare400IsClassifiedTheSameRetriedOrNot`,
   `TestClassifyTurnError_Bare400SkippedForBudgetIsClassifiedTheSame`).
9. **Anthropic is untouched on purpose.** Its adapter has no
   `RetryPolicy` and reads no `PriorSuccess`. `anthropic-sdk-go` does
   not retry a 400. An Anthropic 400 is an `invalid_request_error` that
   names what was invalid, and no generic transient 400 has been seen
   from it. A predicate written without a sample would be a guess.
   `TestGenerateContent_Bare400AfterSuccessIsNotRetried` pins one
   request and the error surfaced.

## The cost, stated

A request that is malformed only in a part that changed **after** the
session's first served call can also produce this 400, and gets the
retry. There are two such parts. One is the history: #874's stale
`thoughtSignature` is the candidate. The other is the request shape:
the first cached request names a cache instead of carrying the
instruction and tools, and an MCP tool list can change between
requests. That turn now costs one extra request, and its error leads
with `provider retry persisted: `. The classification is unchanged, and
a genuine malformation fails the same way on the retry, so the operator
learns the same thing one round-trip later. The budget caps the worst
case for the whole process at RetryBurst extra requests, then one per
30s.

The budget is process-wide, so in a multi-session daemon the cost is
shared. A session whose history went bad after its first success
spends one retry from the shared bucket on every turn that hits the
bare 400. That can leave other sessions' 429/503 retries facing an
empty budget (burst 3, one refill per 30s); the burst cap is what
bounds it.

## Out of scope

- **Surviving a restart.** A daemon that resumes a session from the
  eventlog builds a fresh Agent, so its record starts unmarked. The
  first call after a resume is treated as a first call. Persisting the
  flag would add a durable row for a single retry.
- **A retry on `RunWithContents`.** Each call creates and deletes its
  own session, so there is no earlier call in the same session to point
  to. It shadows an inherited record rather than reading it.
- **A per-request config fingerprint.** We rejected keying the record
  on a hash of model, system instruction and tool declarations. Any
  per-turn variation in the instruction would silently disable the
  retry in the case it exists for. An Agent's config is fixed at `New`,
  which makes per-Agent the honest granularity.
- **Retrying other 400s, `500 INTERNAL`, or the bare 400 on the
  Anthropic path.** Each needs a recorded sample first, as #935
  required.
- **Changing `TurnError.Retryable` or the hint.** The hint already
  tells the operator what a second failure means, and the retry now
  answers that question before the frame is written.
- **Session ids on the retry log lines.** The policy still has no
  session, which `provider-retry-transcript-design.md` already lists as
  out of scope.
