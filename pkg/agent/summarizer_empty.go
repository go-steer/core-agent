// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// The summarizer's text-less-response path (#908).
//
// `model returned no summary text` fired twice on a live GKE session and
// said nothing about why. What follows is what the code establishes
// about how that branch is reached — not a diagnosis of the live run,
// which is not settled.
//
// The Gemini adapter already has an empty-response layer (#220):
// wrapEmptyTailDetection synthesises ErrEmptyResponse when a call
// produces no USABLE response, and retryOnceOnEmpty retries the whole
// call once before surfacing it. Both would reach runSummarizer as an
// ERROR, and the message would have been `...: generate: ...`. So the
// message we actually saw means the adapter judged the response usable
// and returned it with a nil error, and the summarizer still found no
// text on it. pkg/models/gemini.isUsableResponse says when that happens:
// a response counts as usable if it has parts, OR a non-STOP finish
// reason, OR an error code.
//
// ADK's converter (internal/llminternal/converters.Genai2LLMResponse)
// supplies the shape that satisfies the second and third of those with
// no text at all: a candidate with no content parts and a finish reason
// other than STOP is converted into a SUCCESSFUL LLMResponse carrying
// ErrorCode/FinishReason and a nil Content. MAX_TOKENS, SAFETY,
// RECITATION and friends all land there. The other reachable shape is a
// response whose parts carry no `.Text` — a thought-signature-only part,
// or a server-side built-in invocation (the summarizer request declares
// no function tools, so Gemini 3 built-ins ARE injected into it).
//
// Note what this rules out. A Vertex 429 surfaces from the genai client
// as a transport error, becomes `failed to call model` inside ADK, and
// reaches runSummarizer through the `err != nil` arm with a completely
// different message. The 429s in the same live session are correlated;
// nothing here shows them to be the mechanism.
//
// Two things follow, and both are worth doing whatever the ultimate
// provider-side cause turns out to be:
//
//   - Say why. The provider's explanation is right there on the response
//     and was being discarded. /btw already extracts it for its own
//     one-shot call (emptyAnswerDetail in btw.go); the summarizer is the
//     other one-shot caller and never got the same treatment. This is
//     also the evidence that would settle the live case: with it, the
//     next occurrence names its finish reason in the log and in the
//     eventlog row.
//
//   - Retry the ones a retry can fix. An empty summary with no
//     explanation is the shape #220 documents as usually transient, and
//     one retry is cheap next to losing a compaction. An empty summary
//     the provider EXPLAINED with a terminal reason is a property of the
//     input, and a byte-identical retry buys a second bill and the same
//     answer. The split is below.
//
// The retry is capped at one extra attempt inside a single call, so the
// no-infinite-retry property the pending-drain paths rely on is
// unchanged: they still clear their pending flag on failure, and
// compaction still backs off exponentially on top.

package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/model"
)

// ErrEmptySummary is the sentinel every text-less summarizer result
// wraps. Match on it (errors.Is) to tell "the model said nothing" apart
// from a transport, auth, or persistence failure — the callers that
// swallow a failed reduction want to report the two differently, and the
// difference is invisible in a wrapped error string.
var ErrEmptySummary = errors.New("model returned no summary text")

// EmptySummaryError reports a summarizer call that completed without
// error and produced no text.
//
// Detail is the provider's own explanation, in the vocabulary /btw
// already uses: "finish_reason=MAX_TOKENS", "error=SAFETY: ...". It is
// empty when the provider offered none, which is itself a signal — that
// is the unexplained shape, and the one this code retries.
//
// Attempts is how many summarizer calls were made before giving up, so a
// reader can tell a retried-and-still-empty failure from one that was
// classified as terminal on the first response and deliberately not
// retried.
type EmptySummaryError struct {
	Operation string
	Detail    string
	Attempts  int
}

func (e *EmptySummaryError) Error() string {
	if e == nil {
		return ErrEmptySummary.Error()
	}
	var b strings.Builder
	b.WriteString("agent: ")
	b.WriteString(e.Operation)
	b.WriteString(": ")
	b.WriteString(ErrEmptySummary.Error())
	if e.Detail != "" {
		b.WriteString(" (")
		b.WriteString(e.Detail)
		b.WriteString(")")
	}
	if e.Attempts > 1 {
		fmt.Fprintf(&b, " after %d attempts", e.Attempts)
	}
	return b.String()
}

func (e *EmptySummaryError) Unwrap() error { return ErrEmptySummary }

// summarizerAttempt is what one summarizer LLM call produced: the text
// (empty when the call is the case this file is about) plus whatever the
// provider said about why there wasn't any.
type summarizerAttempt struct {
	// text is the trimmed concatenation of every text part on every
	// non-partial response in the stream.
	text string

	// detail is the last non-empty emptyAnswerDetail seen — the same
	// "finish_reason=X" / "error=CODE: msg" rendering /btw surfaces.
	detail string

	// finishReason and errorCode are the raw fields detail was rendered
	// from, kept separately because the retry decision is made on them
	// and classifying off a formatted string would be a parser.
	finishReason genai.FinishReason
	errorCode    string
}

// terminalEmptyReasons are the provider explanations for a text-less
// response that re-sending the SAME request cannot change.
//
// The safety family is deterministic over the input: the history that
// tripped a filter trips it again. MAX_TOKENS is a budget statement —
// the same window against the same cap overflows again, and the fix is
// a smaller window or a bigger cap, not another call. MALFORMED_
// FUNCTION_CALL and UNEXPECTED_TOOL_CALL describe a request/tool
// mismatch that a retry reproduces.
//
// Everything absent from this table — including no explanation at all,
// OTHER, and any reason a future provider adds — is treated as
// retryable. That default is deliberate: the cost of a wrong "retryable"
// is one extra summarizer call, capped at one, while the cost of a wrong
// "terminal" is a compaction that never happens on a session heading for
// the context wall.
//
// MAX_TOKENS is the debatable member. With a thinking model the output
// split between thought and answer is not fully deterministic, so a
// retry could produce text where the first call produced only thoughts.
// It is classified terminal anyway because the retry is a full second
// pass over a window that is at ~85% of the context by construction —
// the most expensive call the session makes — for a coin flip, and
// because naming MAX_TOKENS in the log and the eventlog row points at
// the actual fix. Revisit if the reason shows up in the field.
var terminalEmptyReasons = map[string]bool{
	string(genai.FinishReasonSafety):                 true,
	string(genai.FinishReasonRecitation):             true,
	string(genai.FinishReasonBlocklist):              true,
	string(genai.FinishReasonProhibitedContent):      true,
	string(genai.FinishReasonSPII):                   true,
	string(genai.FinishReasonImageSafety):            true,
	string(genai.FinishReasonImageProhibitedContent): true,
	string(genai.FinishReasonImageRecitation):        true,
	string(genai.FinishReasonLanguage):               true,
	string(genai.FinishReasonMalformedFunctionCall):  true,
	string(genai.FinishReasonUnexpectedToolCall):     true,
	string(genai.FinishReasonMaxTokens):              true,
}

// retryableEmptySummary reports whether a second identical call could
// plausibly produce text. See terminalEmptyReasons for the split.
func retryableEmptySummary(at summarizerAttempt) bool {
	for _, reason := range []string{string(at.finishReason), at.errorCode} {
		if reason == "" {
			continue
		}
		if terminalEmptyReasons[strings.ToUpper(strings.TrimSpace(reason))] {
			return false
		}
	}
	return true
}

// summarizeWithRetry runs the summarizer call, retrying ONCE when the
// first call came back with no text and no terminal explanation.
//
// Every attempt's usage is recorded, including a doomed one: a retry is
// real spend and the /stats total (and therefore the cost ceilings that
// read it) must not under-report because the call produced nothing.
//
// A cancelled context ends the loop rather than spending a retry on a
// caller that has already gone away.
func (a *Agent) summarizeWithRetry(ctx context.Context, operation string, req *adkmodel.LLMRequest) (string, error) {
	const maxAttempts = 2
	var last summarizerAttempt
	attempts := 0
	for attempts < maxAttempts {
		attempts++
		at, err := a.summarizeOnce(ctx, req)
		if err != nil {
			return "", fmt.Errorf("agent: %s: generate: %w", operation, err)
		}
		if at.text != "" {
			if attempts > 1 {
				log.Printf("agent: %s: empty summary recovered on retry (attempt %d/%d)",
					operation, attempts, maxAttempts)
			}
			return at.text, nil
		}
		last = at
		if attempts >= maxAttempts || ctx.Err() != nil || !retryableEmptySummary(at) {
			break
		}
		log.Printf("agent: %s: model returned no summary text (%s) — retrying once",
			operation, emptyDetailOrUnknown(at.detail))
	}
	return "", &EmptySummaryError{Operation: operation, Detail: last.detail, Attempts: attempts}
}

// emptyDetailOrUnknown renders a possibly-absent provider explanation
// for a log line. Absence is the interesting case, so it gets words
// rather than an empty pair of parentheses.
func emptyDetailOrUnknown(detail string) string {
	if detail == "" {
		return "no explanation from the provider"
	}
	return detail
}

// summarizeOnce drives one summarizer LLM call and accumulates its
// non-partial text, its usage, and — for the case where there is no text
// — whatever the provider said about why.
//
// Usage is committed here rather than by the caller so each attempt is
// billed exactly once. The summarizer is a single logical call per
// attempt (one TurnComplete), so the last usage-bearing response wins;
// same shape as the subtask tracker.Append in subtask.go's Run loop.
// Without it, summarizer turns escape /stats accounting (#61) and
// therefore the --max-turn-cost-usd / --max-session-cost-usd ceilings
// from #145.
func (a *Agent) summarizeOnce(ctx context.Context, req *adkmodel.LLMRequest) (summarizerAttempt, error) {
	var out summarizerAttempt
	var b strings.Builder
	var lastIn, lastOut int
	var lastMeta *genai.GenerateContentResponseUsageMetadata
	var lastCustom map[string]any
	for resp, err := range a.model.GenerateContent(ctx, req, false) {
		if err != nil {
			// Record what the failed attempt already burned before
			// bailing — a partial stream that then errors still cost
			// prompt tokens.
			a.recordInternalLLMUsage(lastIn, lastOut, lastMeta, lastCustom)
			return summarizerAttempt{}, err
		}
		if resp != nil && resp.UsageMetadata != nil {
			lastIn = int(resp.UsageMetadata.PromptTokenCount)
			lastOut = int(resp.UsageMetadata.CandidatesTokenCount)
			lastMeta = resp.UsageMetadata
			lastCustom = resp.CustomMetadata
		}
		// Capture the explanation BEFORE the content guard below: the
		// shape this whole file is about is a response with a finish
		// reason and no Content at all, which the guard skips.
		if d := emptyAnswerDetail(resp); d != "" {
			out.detail = d
		}
		if resp != nil {
			if resp.FinishReason != "" {
				out.finishReason = resp.FinishReason
			}
			if resp.ErrorCode != "" {
				out.errorCode = resp.ErrorCode
			}
		}
		if resp == nil || resp.Content == nil || resp.Partial {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p != nil && p.Text != "" {
				b.WriteString(p.Text)
			}
		}
	}
	a.recordInternalLLMUsage(lastIn, lastOut, lastMeta, lastCustom)
	out.text = strings.TrimSpace(b.String())
	return out, nil
}
