# Provider retries in the transcript (#1206)

Box A2 (#1042) asks that each failure class have equal counts in the
daemon log and in the transcripts. `pkg/models/retry.go` logged every
retry decision to stderr and nowhere else. The 2026-09-13 batch counted
13 retries in the log and 0 in 21 transcripts, and an operator reading
a session could not tell that its turn had survived a 429.

## Where a retry can surface

The policy sits inside the Gemini adapter, below the agent. It has no
session and no emitter, and one instance is shared by every model
handle in the process. It sees exactly two things it can change: the
responses it yields and the error it yields.

| What became of the retry | Log line counted by `a2_count.py` | Transcript surface |
|---|---|---|
| recovered | `— retrying once after` | `CustomMetadata.provider_retry` on the first non-partial response after recovery |
| persisted | `— retrying once after` | error text `provider retry persisted: …` |
| abandoned (ctx ended in backoff) | `— retrying once after` | error text `provider retry abandoned: …` |
| not retried, budget spent | `NOT retried` | error text `provider retry skipped, budget spent: …` |
| answered by a different error (a 400, an empty response) | `— retrying once after` | error text `provider retry failed with another error: …` |
| answered by a final response with no parts (a SAFETY or MAX_TOKENS finish) | `— retrying once after` | `provider_retry` stamp with outcome `no content` on that response, which ADK persists |
| recovered, then the stream failed before its final response | `— retrying once after` | error text `provider retry interrupted after recovering: …` |
| no outcome (consumer stopped) | `— retrying once after` | none: nothing reached the caller |

A **side call** is a model call whose retry has no transcript surface:
the approver, the compaction summarizer (which the checkpointer shares),
the session title, `/btw`, an MCP digest, or an agentic tool's subtask.
The first four make one-shot calls whose response is never a session
event and whose error is never a turn error. The last two run a subtask
whose events go to a session no capture holds. An MCP digest also
swallows its error into the raw output. An agentic tool's error does
reach the parent's function response, so a failed retry there counts
on the transcript side while its log line is a side call's. That makes
the row TRANSCRIPT-ONLY, the safe direction.
`models.AsSideCall(ctx, name)` makes its retry log as
`side call (<name>): transient provider error …`, and `a2_count.py` reports
those lines in their own NOT COUNTABLE row.

The error text then travels without any new plumbing:

- **Parent model call:** the `turn-error` frame's `message`.
- **Child call through `spawn_agent` (sync), `subagent` or `subtask`:** the
  failed function response, which carries the child's error verbatim.
- **Async `spawn_agent`:** the `[Background reports]` block, which is
  prepended to the parent's next prompt and so persisted as a user event.

## Settled decisions (do not relitigate)

1. **The stamp goes on the first non-partial response, once.** ADK
   persists only non-partial events (`runner.go`), and its stream
   aggregator builds the final response fresh, dropping any metadata
   on the chunks. Stamping every response would count one retry many
   times. The stamp is on a copy, so the provider's value is never
   mutated.
2. **A surfaced retry is a wrapped error, `*models.RetryError`, not a
   new frame field.** The attach protocol is unchanged, and the same
   text reaches all three error surfaces above. Where the turn-error
   classifier replaces the message with fixed text (a deadline, a
   cancel), `ClassifyTurnError` puts the retry's prefix back. `Unwrap` keeps
   `errors.As(…, *genai.APIError)` and `errors.Is` working.
3. **The outcome is a prefix, and the prefix uses no classifier
   keyword.** The `turn-error` frame keeps the first 240 characters,
   and a Vertex 429 body is often longer than that, so a suffix would
   be cut. `ClassifyTurnError` is a substring scan, and
   `TestClassifyTurnError_ARetryPrefixChangesNoClassification` pins
   that kind, code and retryable are the rejection's own.
4. **No claim without a log line, and no log line without a claim
   where a surface exists.** A transient error suppressed by a hard
   error in the same stream logs nothing and is not wrapped. Every
   retry that logs `retrying once` and then reaches the caller with
   anything — content, the rejection, or a different error — carries
   exactly one record: a stamp or a prefixed error, never both.
5. **A child's retry stays on the child's events.** It is not copied
   into the parent's stream. The parent sees a child retry only when
   it failed the child's call. `a2_count.py --subagent-events` reads
   the children's events from the drill's `subagents.json`, which is
   built from `GET /sessions/{id}/agents/{name}/events`.
6. **Side calls are labelled, not given a surface.** Inventing an event
   for an approver verdict or a title would put model calls in the
   transcript that no turn made. The label keeps them out of the A2
   comparison and visible in their own row.

## Out of scope (known log-only gaps)

- A retry whose consumer stopped reading: a cancelled turn, or a
  guardrail cut after the first recovered chunk. On a run it shows
  up as a FAIL delta that matches the cancellation.
- An abandoned retry inside a subagent that the parent reports only as
  a stop reason: the sync `subagent` tool's wall-clock cap, or
  `stop_agent` during the backoff.
- Exact dedup. `a2_count.py` skips the nested `calls` digest and reads
  only the `[Background reports]` block of a user prompt. An async
  child whose result is delivered both inline and as a report still
  counts twice. Overcounting is the safe direction for A2, but it can
  mask an undercount of equal size.
- A stream that yields a hard error and a transient one on the same
  retried attempt. ADK stops at the first error, so the transient one
  is never surfaced and neither is wrapped. Gemini's `generateStream`
  yields one error and returns, so this needs a future provider to happen.
- Session ids on the retry log lines. The shared policy has no session.
- A typed attach frame for retries, or core-tui rendering of the stamp.
