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
	"iter"
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
			"empty content but finish reason set (STOP)",
			&adkmodel.LLMResponse{FinishReason: genai.FinishReasonStop},
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

// TestWrapEmptyTailDetection_FinishReasonCountsAsUsable — a
// "model finished cleanly, no more content" response (STOP with
// empty Content) must NOT trigger the empty-tail path. That's a
// legitimate turn-end signal, not a silent hang.
func TestWrapEmptyTailDetection_FinishReasonCountsAsUsable(t *testing.T) {
	t.Parallel()
	in := []pair{
		{resp: &adkmodel.LLMResponse{FinishReason: genai.FinishReasonStop}, err: nil},
	}
	out := collect(wrapEmptyTailDetection(seqOf(in), false, false))
	if len(out) != 1 {
		t.Fatalf("expected 1 element, got %d", len(out))
	}
	if out[0].err != nil {
		t.Errorf("FinishReason=STOP with empty content should pass unmodified, got err=%v", out[0].err)
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
