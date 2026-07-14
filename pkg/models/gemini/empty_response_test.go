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

package gemini

import (
	"errors"
	"fmt"
	"iter"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// mkResponse builds a small LLMResponse fixture; fields default to
// empty so tests can build the "empty response" shape by passing
// zero values.
func mkResponse(parts ...string) *adkmodel.LLMResponse {
	resp := &adkmodel.LLMResponse{}
	if len(parts) > 0 {
		resp.Content = &genai.Content{}
		for _, p := range parts {
			resp.Content.Parts = append(resp.Content.Parts, &genai.Part{Text: p})
		}
	}
	return resp
}

// seqOf builds an iter.Seq2 from a slice of (resp, err) tuples so
// tests can drive wrapEmptyTailDetection deterministically.
type pair struct {
	resp *adkmodel.LLMResponse
	err  error
}

func seqOf(items []pair) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for _, p := range items {
			if !yield(p.resp, p.err) {
				return
			}
		}
	}
}

// collect exhausts an iter.Seq2 into a slice for assertion.
func collect(seq iter.Seq2[*adkmodel.LLMResponse, error]) []pair {
	var out []pair
	for r, e := range seq {
		out = append(out, pair{resp: r, err: e})
	}
	return out
}

// TestIsUsableResponse pins the classifier that decides which
// responses count as "real content." Empty-shell responses (nil,
// no parts, no signals) must NOT count; any of parts / finish
// reason / error code must count.
func TestIsUsableResponse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		resp *adkmodel.LLMResponse
		want bool
	}{
		{"nil response", nil, false},
		{"empty content, no signals", mkResponse(), false},
		{"content with parts", mkResponse("hello"), true},
		{
			// Bare STOP with no content is the exact silent-hang
			// shape observed live during the v2.6 GKE-triage drive
			// — Vertex claims completion without producing anything.
			// Must NOT count as usable so the tail detector fires
			// and the agent loop sees an error instead of going idle.
			"empty content, finish reason STOP (silent hang)",
			&adkmodel.LLMResponse{FinishReason: genai.FinishReasonStop},
			false,
		},
		{
			// Non-STOP finish reasons signal something operator-
			// visible (safety filter, budget hit, ...) and stay
			// classified as usable so we don't over-fire the tail.
			"empty content, finish reason SAFETY",
			&adkmodel.LLMResponse{FinishReason: genai.FinishReasonSafety},
			true,
		},
		{
			"empty content, finish reason RECITATION",
			&adkmodel.LLMResponse{FinishReason: genai.FinishReasonRecitation},
			true,
		},
		{
			"empty content but error signaled",
			&adkmodel.LLMResponse{ErrorCode: "SAFETY", ErrorMessage: "filtered"},
			true,
		},
		{
			"empty content but error message only",
			&adkmodel.LLMResponse{ErrorMessage: "something"},
			true,
		},
		{
			"content with zero parts (nil Parts)",
			&adkmodel.LLMResponse{Content: &genai.Content{}},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isUsableResponse(tc.resp)
			if got != tc.want {
				t.Errorf("isUsableResponse: got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWrapEmptyTailDetection_UsableResponsePasses verifies the
// happy path — real content flows through unchanged, no synthetic
// tail error appended.
func TestWrapEmptyTailDetection_UsableResponsePasses(t *testing.T) {
	t.Parallel()
	in := []pair{
		{resp: mkResponse("hello world"), err: nil},
	}
	out := collect(wrapEmptyTailDetection(seqOf(in), false, false))
	if len(out) != 1 {
		t.Fatalf("expected 1 element, got %d", len(out))
	}
	if out[0].err != nil {
		t.Errorf("expected nil error, got %v", out[0].err)
	}
}

// TestWrapEmptyTailDetection_EmptyTailSurfacesError is THE #220
// regression. Iterator yields nothing usable AND no error → the
// wrapper must synthesize ErrEmptyResponse at the tail rather than
// letting the agent loop go silently idle.
func TestWrapEmptyTailDetection_EmptyTailSurfacesError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []pair
	}{
		{
			name: "no responses at all",
			in:   nil,
		},
		{
			name: "single empty response (Content{role: model}, parts nil)",
			in:   []pair{{resp: mkResponse(), err: nil}},
		},
		{
			name: "multiple empty responses (streaming heartbeats without content)",
			in: []pair{
				{resp: mkResponse(), err: nil},
				{resp: mkResponse(), err: nil},
				{resp: mkResponse(), err: nil},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := collect(wrapEmptyTailDetection(seqOf(tc.in), false, false))
			if len(out) == 0 {
				t.Fatal("expected tail error, got empty output")
			}
			tail := out[len(out)-1]
			if !errors.Is(tail.err, ErrEmptyResponse) {
				t.Errorf("tail err = %v, want ErrEmptyResponse", tail.err)
			}
		})
	}
}

// TestWrapEmptyTailDetection_ExistingErrorSuppresesTail — if the
// iterator already surfaced an error, the wrapper must NOT ALSO
// append ErrEmptyResponse. One error signal is enough; two would
// confuse the caller.
func TestWrapEmptyTailDetection_ExistingErrorSuppresesTail(t *testing.T) {
	t.Parallel()
	upstreamErr := errors.New("some upstream failure")
	in := []pair{
		{resp: mkResponse(), err: nil},
		{resp: nil, err: upstreamErr},
	}
	out := collect(wrapEmptyTailDetection(seqOf(in), false, false))
	// Last element should be the upstream error, not ErrEmptyResponse.
	tail := out[len(out)-1]
	if !errors.Is(tail.err, upstreamErr) {
		t.Errorf("tail err = %v, want upstream error", tail.err)
	}
	if errors.Is(tail.err, ErrEmptyResponse) {
		t.Errorf("tail should NOT be ErrEmptyResponse when upstream already errored: %v", tail.err)
	}
}

// TestWrapEmptyTailDetection_NonStopFinishReasonCountsAsUsable —
// SAFETY / RECITATION / MAX_TOKENS / OTHER finish reasons signal
// operator-visible outcomes (filter fired, budget hit, ...) even
// when Content is empty. They must pass through unmodified and
// NOT trigger the empty-tail path, or we'd double-signal.
func TestWrapEmptyTailDetection_NonStopFinishReasonCountsAsUsable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		reason genai.FinishReason
	}{
		{"SAFETY", genai.FinishReasonSafety},
		{"RECITATION", genai.FinishReasonRecitation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			in := []pair{
				{resp: &adkmodel.LLMResponse{FinishReason: tc.reason}, err: nil},
			}
			out := collect(wrapEmptyTailDetection(seqOf(in), false, false))
			if len(out) != 1 {
				t.Fatalf("expected 1 element, got %d", len(out))
			}
			if out[0].err != nil {
				t.Errorf("FinishReason=%s with empty content should pass unmodified, got err=%v", tc.reason, out[0].err)
			}
		})
	}
}

// TestWrapEmptyTailDetection_BareStopTriggersEmptyTail pins the
// #220 gap surfaced by the v2.6 GKE-triage drive (session
// 019f...daf0d, turn 4). Vertex returned a FinishReason=STOP
// frame with no content; the agent loop went silently idle
// because isUsableResponse used to classify STOP-alone as usable.
// The wrapper must now synthesize ErrEmptyResponse so callers see
// an actionable error rather than a hung session.
func TestWrapEmptyTailDetection_BareStopTriggersEmptyTail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []pair
	}{
		{
			name: "FinishReason=STOP, Content nil",
			in: []pair{{
				resp: &adkmodel.LLMResponse{FinishReason: genai.FinishReasonStop},
				err:  nil,
			}},
		},
		{
			// The exact shape observed at 11:11:07.7 in the
			// stuck demo session: Content struct present with a
			// role but zero Parts, plus a STOP frame.
			name: "FinishReason=STOP, Content non-nil, Parts empty",
			in: []pair{{
				resp: &adkmodel.LLMResponse{
					Content:      &genai.Content{Role: "model"},
					FinishReason: genai.FinishReasonStop,
				},
				err: nil,
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := collect(wrapEmptyTailDetection(seqOf(tc.in), false, false))
			if len(out) == 0 {
				t.Fatal("expected at least a tail error, got empty output")
			}
			tail := out[len(out)-1]
			if !errors.Is(tail.err, ErrEmptyResponse) {
				t.Errorf("tail err = %v, want ErrEmptyResponse", tail.err)
			}
		})
	}
}

// TestWrapEmptyTailDetection_HeartbeatDropStillTriggersTail —
// verifies the interaction of tolerateEmpty + streaming + our
// new tail detection. When Vertex heartbeats are dropped AND
// no real content ever arrives, the wrapper still surfaces
// ErrEmptyResponse (heartbeats aren't usable content).
func TestWrapEmptyTailDetection_HeartbeatDropStillTriggersTail(t *testing.T) {
	t.Parallel()
	heartbeatErr := errors.New(adkEmptyResponseError)
	in := []pair{
		{resp: nil, err: heartbeatErr}, // dropped by tolerateEmpty
		{resp: nil, err: heartbeatErr}, // dropped
		{resp: nil, err: heartbeatErr}, // dropped
	}
	// tolerateEmpty=true + stream=true → the three heartbeat
	// errors are silently dropped. Nothing usable emitted. Tail
	// detection kicks in.
	out := collect(wrapEmptyTailDetection(seqOf(in), true, true))
	if len(out) == 0 {
		t.Fatal("expected tail error, got empty output")
	}
	tail := out[len(out)-1]
	if !errors.Is(tail.err, ErrEmptyResponse) {
		t.Errorf("expected ErrEmptyResponse at tail, got %v", tail.err)
	}
}

// TestWrapEmptyTailDetection_HeartbeatDropPassesRealContent —
// heartbeats dropped, real content arrives afterward. Real
// content passes; NO synthetic tail error.
func TestWrapEmptyTailDetection_HeartbeatDropPassesRealContent(t *testing.T) {
	t.Parallel()
	heartbeatErr := errors.New(adkEmptyResponseError)
	in := []pair{
		{resp: nil, err: heartbeatErr}, // dropped
		{resp: mkResponse("real content"), err: nil},
		{resp: nil, err: heartbeatErr}, // dropped
	}
	out := collect(wrapEmptyTailDetection(seqOf(in), true, true))
	// Should see just the real-content response — heartbeats dropped,
	// tail check finds "usable was seen" so no synthetic error.
	if len(out) != 1 {
		t.Fatalf("expected 1 element (real content), got %d: %+v", len(out), out)
	}
	if out[0].err != nil {
		t.Errorf("real content should pass without error, got %v", out[0].err)
	}
	if out[0].resp == nil || len(out[0].resp.Content.Parts) == 0 {
		t.Errorf("expected real-content resp, got %+v", out[0].resp)
	}
}

// captureLogf swaps the package logf sink for the duration of the
// test and returns a pointer to the captured messages. Cleanup
// restores the original sink so tests don't leak into stderr.
func captureLogf(t *testing.T) *[]string {
	t.Helper()
	var captured []string
	prev := logf
	logf = func(format string, args ...any) {
		captured = append(captured, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { logf = prev })
	return &captured
}

// fnSeq returns a factory for retryOnceOnEmpty that yields the
// N-th sequence on the N-th call. Extra calls beyond the fixture
// yield an empty sequence (defensive; a well-behaved wrapper
// shouldn't invoke fn() more than maxAttempts times).
func fnSeq(seqs ...[]pair) func() iter.Seq2[*adkmodel.LLMResponse, error] {
	var call int
	return func() iter.Seq2[*adkmodel.LLMResponse, error] {
		var s []pair
		if call < len(seqs) {
			s = seqs[call]
		}
		call++
		return seqOf(s)
	}
}

// TestRetryOnceOnEmpty_HappyPathNoRetry — usable first attempt
// passes through unchanged and emits no alert logs. Baseline
// regression to ensure the retry wrapper is transparent when the
// underlying model behaves.
func TestRetryOnceOnEmpty_HappyPathNoRetry(t *testing.T) {
	// No t.Parallel: retry tests swap the package-level logf sink;
	// running concurrently would race on it.
	logs := captureLogf(t)
	fn := fnSeq([]pair{{resp: mkResponse("hello"), err: nil}})
	out := collect(retryOnceOnEmpty(fn))
	if len(out) != 1 || out[0].err != nil {
		t.Fatalf("expected single usable chunk, got %+v", out)
	}
	if len(*logs) != 0 {
		t.Errorf("no retry ⇒ no logs expected, got %v", *logs)
	}
}

// TestRetryOnceOnEmpty_FirstEmptyRetrySucceeds — attempt 1 yields
// only ErrEmptyResponse; attempt 2 yields usable content. Caller
// sees ONLY the attempt-2 content (buffered empties from attempt 1
// are discarded so no bogus session events land in ADK). Both
// alert lines fire ("retrying" + "recovered").
func TestRetryOnceOnEmpty_FirstEmptyRetrySucceeds(t *testing.T) {
	logs := captureLogf(t)
	fn := fnSeq(
		[]pair{
			{resp: mkResponse(), err: nil},
			{resp: nil, err: ErrEmptyResponse},
		},
		[]pair{{resp: mkResponse("world"), err: nil}},
	)
	out := collect(retryOnceOnEmpty(fn))
	if len(out) != 1 {
		t.Fatalf("expected 1 chunk from retry, got %d: %+v", len(out), out)
	}
	if out[0].err != nil {
		t.Errorf("expected clean usable, got err=%v", out[0].err)
	}
	if len(*logs) != 2 {
		t.Fatalf("expected 2 alerts (retrying + recovered), got %d: %v", len(*logs), *logs)
	}
	if !strings.Contains((*logs)[0], "retrying") {
		t.Errorf("first alert missing 'retrying': %q", (*logs)[0])
	}
	if !strings.Contains((*logs)[1], "recovered") {
		t.Errorf("second alert missing 'recovered': %q", (*logs)[1])
	}
}

// TestRetryOnceOnEmpty_BothAttemptsEmptyPersists — both attempts
// end in ErrEmptyResponse. The wrapper surfaces ErrEmptyResponse
// to the caller and logs the persist alert so operators see the
// event in daemon stderr.
func TestRetryOnceOnEmpty_BothAttemptsEmptyPersists(t *testing.T) {
	logs := captureLogf(t)
	empty := []pair{
		{resp: mkResponse(), err: nil},
		{resp: nil, err: ErrEmptyResponse},
	}
	fn := fnSeq(empty, empty)
	out := collect(retryOnceOnEmpty(fn))
	if len(out) == 0 {
		t.Fatal("expected at least the terminal ErrEmptyResponse")
	}
	tail := out[len(out)-1]
	if !errors.Is(tail.err, ErrEmptyResponse) {
		t.Errorf("tail err = %v, want ErrEmptyResponse", tail.err)
	}
	if len(*logs) != 2 {
		t.Fatalf("expected 2 alerts (retrying + persisted), got %d: %v", len(*logs), *logs)
	}
	if !strings.Contains((*logs)[0], "retrying") {
		t.Errorf("first alert missing 'retrying': %q", (*logs)[0])
	}
	if !strings.Contains((*logs)[1], "persisted") {
		t.Errorf("second alert missing 'persisted': %q", (*logs)[1])
	}
}

// TestRetryOnceOnEmpty_RealErrorBubblesNoRetry — a non-ErrEmpty
// error from the inner iteration must NOT trigger retry (the
// underlying issue isn't a transient silent-hang) and must bubble
// through to the caller.
func TestRetryOnceOnEmpty_RealErrorBubblesNoRetry(t *testing.T) {
	logs := captureLogf(t)
	realErr := errors.New("some upstream 500")
	fn := fnSeq([]pair{
		{resp: mkResponse(), err: nil},
		{resp: nil, err: realErr},
	})
	out := collect(retryOnceOnEmpty(fn))
	var found bool
	for _, p := range out {
		if errors.Is(p.err, realErr) {
			found = true
		}
	}
	if !found {
		t.Errorf("real error should bubble through, got %+v", out)
	}
	if len(*logs) != 0 {
		t.Errorf("real errors ⇒ no retry alerts, got %v", *logs)
	}
}

// TestRetryOnceOnEmpty_EndToEndWithTailDetector — full pipeline
// with retryOnceOnEmpty wrapping wrapEmptyTailDetection. Simulates
// the exact demo failure: bare STOP on attempt 1, usable content
// on attempt 2. Caller sees a clean single-chunk stream; the empty
// STOP frame from attempt 1 is buffered + discarded so ADK never
// records an empty session event.
func TestRetryOnceOnEmpty_EndToEndWithTailDetector(t *testing.T) {
	logs := captureLogf(t)
	var call int
	fn := func() iter.Seq2[*adkmodel.LLMResponse, error] {
		var s []pair
		if call == 0 {
			s = []pair{{
				resp: &adkmodel.LLMResponse{FinishReason: genai.FinishReasonStop},
				err:  nil,
			}}
		} else {
			s = []pair{{resp: mkResponse("recovered"), err: nil}}
		}
		call++
		return wrapEmptyTailDetection(seqOf(s), false, false)
	}
	out := collect(retryOnceOnEmpty(fn))
	if len(out) != 1 {
		t.Fatalf("expected only the recovery chunk, got %d: %+v", len(out), out)
	}
	if out[0].err != nil || out[0].resp == nil || len(out[0].resp.Content.Parts) == 0 {
		t.Errorf("expected clean usable chunk, got %+v", out[0])
	}
	if len(*logs) != 2 {
		t.Fatalf("expected retry+recovered alerts, got %d: %v", len(*logs), *logs)
	}
}
