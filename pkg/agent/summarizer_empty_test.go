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

package agent

import (
	"bytes"
	"context"
	"errors"
	"iter"
	"log"
	"strings"
	"sync"
	"testing"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// emptySummaryLLM reproduces the response shape that reaches the
// summarizer's text-less branch in production, which is NOT "the
// iterator yielded nothing": ADK's Genai2LLMResponse turns a candidate
// with no content parts and a non-STOP finish reason into a SUCCESSFUL
// LLMResponse carrying ErrorCode + FinishReason and a nil Content, and
// the Gemini adapter's #220 empty-response retry classifies that as
// usable and passes it straight through.
//
// scripted lets a test hand back a different response per attempt, which
// is how the retry assertions distinguish "called twice" from "called
// twice and used the second answer".
type emptySummaryLLM struct {
	mu sync.Mutex
	// scripted is consumed in order; the last entry repeats once
	// exhausted, so a one-entry script means "always this".
	scripted []*adkmodel.LLMResponse
	calls    int
}

func (l *emptySummaryLLM) Name() string { return "empty-summary" }

func (l *emptySummaryLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	idx := l.calls
	l.calls++
	if idx >= len(l.scripted) {
		idx = len(l.scripted) - 1
	}
	resp := l.scripted[idx]
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(resp, nil)
	}
}

func (l *emptySummaryLLM) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// textlessWithReason is the converter's second branch: no content, a
// finish reason, and the same string echoed as ErrorCode.
func textlessWithReason(reason genai.FinishReason) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		FinishReason: reason,
		ErrorCode:    string(reason),
		TurnComplete: true,
	}
}

// textlessUnexplained is the other reachable shape: parts present but
// none carrying text (a thought-signature-only part), with no finish
// reason to explain it. This is the one a retry can fix.
func textlessUnexplained() *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{ThoughtSignature: []byte("sig")}},
		},
		TurnComplete: true,
	}
}

func summaryResponse(text string) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

// A text-less summarizer response with no explanation is the shape #220
// documents as usually transient. It must be retried once, and the
// retry's answer must be the one that gets persisted.
//
// Fails on pre-#908 code: the summarizer gave up on the first empty
// response, so callCount is 1 and Compact returns an error.
func TestSummarizer_UnexplainedEmptyIsRetriedOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		run  func(*Agent) (string, error)
	}{
		{"compaction", func(a *Agent) (string, error) {
			res, err := a.Compact(context.Background(), "")
			return res.SummaryText, err
		}},
		{"checkpoint", func(a *Agent) (string, error) {
			res, err := a.Checkpoint(context.Background(), "shipped the thing")
			return res.SummaryText, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			llm := &emptySummaryLLM{scripted: []*adkmodel.LLMResponse{
				textlessUnexplained(),
				summaryResponse("# Current state\nrecovered on the retry"),
			}}
			a, err := New(llm,
				WithCompactor(NewDefaultCompactor()),
				WithCheckpointer(NewDefaultCheckpointer()),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			plantEvent(t, a, genai.RoleUser, "some history worth summarizing")

			text, err := tc.run(a)
			if err != nil {
				t.Fatalf("after an unexplained empty response, want recovery, got error: %v", err)
			}
			if !strings.Contains(text, "recovered on the retry") {
				t.Errorf("summary = %q, want the retry's text", text)
			}
			if got := llm.callCount(); got != 2 {
				t.Errorf("summarizer called %d time(s), want 2 (one retry)", got)
			}
		})
	}
}

// The retry is capped at one extra attempt. A summarizer that is
// persistently empty must not be called a third time, and the error it
// finally returns must say how many attempts it took — otherwise a
// retried failure and a non-retried one read identically in the log.
func TestSummarizer_UnexplainedEmptyRetriesAtMostOnce(t *testing.T) {
	t.Parallel()
	llm := &emptySummaryLLM{scripted: []*adkmodel.LLMResponse{textlessUnexplained()}}
	a, err := New(llm, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "some history worth summarizing")

	_, err = a.Compact(context.Background(), "")
	if !errors.Is(err, ErrEmptySummary) {
		t.Fatalf("Compact error = %v, want one wrapping ErrEmptySummary", err)
	}
	if got := llm.callCount(); got != 2 {
		t.Errorf("summarizer called %d time(s), want exactly 2 (one retry, no more)", got)
	}
	if !strings.Contains(err.Error(), "after 2 attempts") {
		t.Errorf("error %q does not report the attempt count", err)
	}
	// The historical text is what operators and log greps key on; a
	// reword here is a silent break of everything watching for it.
	if !strings.Contains(err.Error(), "model returned no summary text") {
		t.Errorf("error %q dropped the established message", err)
	}
}

// The other half of the split: when the provider EXPLAINED the empty
// answer with a terminal reason, an identical retry buys a second bill
// and the same answer. It must not fire, and the reason must reach the
// error — that reason is the entire diagnostic the live #908 occurrence
// lacked.
//
// Fails on pre-#908 code on the reason assertion: the message was a
// bare "model returned no summary text" with the provider's explanation
// discarded.
func TestSummarizer_TerminalReasonIsNotRetriedAndIsNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		reason genai.FinishReason
	}{
		{"max_tokens", genai.FinishReasonMaxTokens},
		{"safety", genai.FinishReasonSafety},
		{"recitation", genai.FinishReasonRecitation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			llm := &emptySummaryLLM{scripted: []*adkmodel.LLMResponse{textlessWithReason(tc.reason)}}
			a, err := New(llm, WithCompactor(NewDefaultCompactor()))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			plantEvent(t, a, genai.RoleUser, "some history worth summarizing")

			_, err = a.Compact(context.Background(), "")
			if !errors.Is(err, ErrEmptySummary) {
				t.Fatalf("Compact error = %v, want one wrapping ErrEmptySummary", err)
			}
			if got := llm.callCount(); got != 1 {
				t.Errorf("summarizer called %d time(s) for a terminal %s; want 1 (no retry)", got, tc.reason)
			}
			if !strings.Contains(err.Error(), string(tc.reason)) {
				t.Errorf("error %q does not name the provider's reason %s", err, tc.reason)
			}
		})
	}
}

// The no-infinite-retry property the pending drains depend on is
// unchanged by the in-call retry: a pending checkpoint that fails is
// still cleared, so the next turn does not re-attempt it.
func TestRunPendingCheckpoint_EmptySummaryClearsPendingAnyway(t *testing.T) {
	t.Parallel()
	llm := &emptySummaryLLM{scripted: []*adkmodel.LLMResponse{textlessWithReason(genai.FinishReasonMaxTokens)}}
	a, err := New(llm, WithCheckpointer(NewDefaultCheckpointer()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "history to checkpoint")

	a.mu.Lock()
	a.checkpointPending = true
	a.mu.Unlock()
	a.runPendingCheckpoint(context.Background())

	a.mu.Lock()
	pending := a.checkpointPending
	a.mu.Unlock()
	if pending {
		t.Error("checkpointPending still set after an empty-summary failure; the next turn would retry-loop")
	}
	// Drive three more turns' worth of drains: none should call the
	// model, because nothing re-flagged pending.
	for i := 0; i < 3; i++ {
		a.runPendingCheckpoint(context.Background())
	}
	if got := llm.callCount(); got != 1 {
		t.Errorf("summarizer called %d time(s) across four drains; want 1", got)
	}
}

// An empty summary on the AUTOMATIC compaction path must reach an
// attached client, not only the daemon log. The mechanism is a durable
// eventlog row, which the attach broadcaster tails and GET /events
// serves.
//
// Fails on pre-#908 code: no row is written at all.
func TestRunPendingCompaction_EmptySummaryWritesTheFailureRow(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-908-compact")

	llm := &emptySummaryLLM{scripted: []*adkmodel.LLMResponse{textlessWithReason(genai.FinishReasonMaxTokens)}}
	a, err := New(llm,
		WithEventLog(h),
		WithSession("u", "s-908-compact"),
		WithCompactor(NewDefaultCompactor()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "history over the threshold")

	a.mu.Lock()
	a.compactionPending = true
	a.mu.Unlock()
	a.runPendingCompaction(context.Background())

	op, reason := findContextReductionRow(t, a)
	if op != attach.ContextReductionCompaction {
		t.Errorf("failure row operation = %q, want %q", op, attach.ContextReductionCompaction)
	}
	if !strings.Contains(reason, "model returned no summary text") {
		t.Errorf("failure row reason = %q, want the summarizer's message", reason)
	}
	if !strings.Contains(reason, string(genai.FinishReasonMaxTokens)) {
		t.Errorf("failure row reason = %q, want the provider's explanation carried verbatim", reason)
	}
}

// Same row on the checkpoint path. Both operations share one summarizer
// and fail the same way; a row for only one of them would read as "the
// other is fine" during an incident where the shared call is broken.
func TestRunPendingCheckpoint_EmptySummaryWritesTheFailureRow(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-908-checkpoint")

	llm := &emptySummaryLLM{scripted: []*adkmodel.LLMResponse{textlessWithReason(genai.FinishReasonSafety)}}
	a, err := New(llm,
		WithEventLog(h),
		WithSession("u", "s-908-checkpoint"),
		WithCheckpointer(NewDefaultCheckpointer()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "history to checkpoint")

	a.mu.Lock()
	a.checkpointPending = true
	a.mu.Unlock()
	a.runPendingCheckpoint(context.Background())

	op, reason := findContextReductionRow(t, a)
	if op != attach.ContextReductionCheckpoint {
		t.Errorf("failure row operation = %q, want %q", op, attach.ContextReductionCheckpoint)
	}
	if !strings.Contains(reason, string(genai.FinishReasonSafety)) {
		t.Errorf("failure row reason = %q, want the provider's explanation", reason)
	}
}

// A successful reduction writes no row. A notice channel that fires on
// the happy path is one operators learn to ignore.
func TestRunPendingCompaction_SuccessWritesNoFailureRow(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	createTestSession(t, h, "core-agent", "u", "s-908-ok")

	llm := &emptySummaryLLM{scripted: []*adkmodel.LLMResponse{summaryResponse("# Current state\nfine")}}
	a, err := New(llm,
		WithEventLog(h),
		WithSession("u", "s-908-ok"),
		WithCompactor(NewDefaultCompactor()),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "history over the threshold")

	a.mu.Lock()
	a.compactionPending = true
	a.mu.Unlock()
	a.runPendingCompaction(context.Background())

	if n := countContextReductionRows(t, a); n != 0 {
		t.Errorf("%d failure row(s) written for a successful compaction; want 0", n)
	}
}

// The daemon log stays authoritative too — the row is an addition, not a
// replacement. Not parallel: it captures the global logger.
func TestRunPendingCompaction_EmptySummaryStillLogs(t *testing.T) {
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	llm := &emptySummaryLLM{scripted: []*adkmodel.LLMResponse{textlessWithReason(genai.FinishReasonMaxTokens)}}
	a, err := New(llm, WithCompactor(NewDefaultCompactor()))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	plantEvent(t, a, genai.RoleUser, "history over the threshold")

	a.mu.Lock()
	a.compactionPending = true
	a.mu.Unlock()
	a.runPendingCompaction(context.Background())

	if !strings.Contains(buf.String(), "auto-compaction failed") {
		t.Errorf("compaction failure not surfaced to log; got %q", buf.String())
	}
}

func TestRetryableEmptySummary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		at   summarizerAttempt
		want bool
	}{
		{"no explanation at all", summarizerAttempt{}, true},
		{"bare STOP", summarizerAttempt{finishReason: genai.FinishReasonStop}, true},
		{"OTHER", summarizerAttempt{finishReason: genai.FinishReasonOther}, true},
		{"a reason we don't know", summarizerAttempt{finishReason: "SOME_FUTURE_REASON"}, true},
		{"quota code on the response", summarizerAttempt{errorCode: "RESOURCE_EXHAUSTED"}, true},
		{"MAX_TOKENS", summarizerAttempt{finishReason: genai.FinishReasonMaxTokens}, false},
		{"SAFETY", summarizerAttempt{finishReason: genai.FinishReasonSafety}, false},
		{"terminal on ErrorCode only", summarizerAttempt{errorCode: "RECITATION"}, false},
		{"lowercased terminal reason", summarizerAttempt{errorCode: "safety"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := retryableEmptySummary(tc.at); got != tc.want {
				t.Errorf("retryableEmptySummary(%+v) = %v, want %v", tc.at, got, tc.want)
			}
		})
	}
}

func findContextReductionRow(t *testing.T, a *Agent) (operation, reason string) {
	t.Helper()
	for _, ev := range allEventLogEvents(t, a) {
		if op, r, ok := attach.ContextReductionFailure(ev); ok {
			return op, r
		}
	}
	t.Fatal("no context-reduction failure row in the event log")
	return "", ""
}

func countContextReductionRows(t *testing.T, a *Agent) int {
	t.Helper()
	n := 0
	for _, ev := range allEventLogEvents(t, a) {
		if _, _, ok := attach.ContextReductionFailure(ev); ok {
			n++
		}
	}
	return n
}

func allEventLogEvents(t *testing.T, a *Agent) []*session.Event {
	t.Helper()
	resp, err := a.eventLog.Service.Get(context.Background(), &session.GetRequest{
		AppName:   a.appName,
		UserID:    a.userID,
		SessionID: a.sessionID,
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	var out []*session.Event
	for ev := range resp.Session.Events().All() {
		out = append(out, ev)
	}
	return out
}
