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

package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	adkmodel "google.golang.org/adk/model"
)

// The #487 fixes: every stream NewStreaming opens must be Close()d —
// the SDK stream owns the HTTP response body, and no path in
// GenerateContent released it. These tests observe closure at the
// http.RoundTripper layer: each response body is wrapped in a
// tracker, and after the iterator finishes (or is abandoned early)
// every tracked body must be closed.

type trackedBody struct {
	io.ReadCloser
	mu     sync.Mutex
	closed bool
}

func (b *trackedBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return b.ReadCloser.Close()
}

func (b *trackedBody) isClosed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed
}

type trackingTransport struct {
	base   http.RoundTripper
	mu     sync.Mutex
	bodies []*trackedBody
}

func (t *trackingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if resp != nil && resp.Body != nil {
		tb := &trackedBody{ReadCloser: resp.Body}
		t.mu.Lock()
		t.bodies = append(t.bodies, tb)
		t.mu.Unlock()
		resp.Body = tb
	}
	return resp, err
}

func (t *trackingTransport) assertAllClosed(tt *testing.T, wantBodies int) {
	tt.Helper()
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.bodies) != wantBodies {
		tt.Fatalf("transport saw %d response bodies, want %d", len(t.bodies), wantBodies)
	}
	for i, b := range t.bodies {
		if !b.isClosed() {
			// For a fully-drained body this is a hygiene failure (the
			// EOF-read connection still returns to the pool); for a
			// mid-stream stop it is a live leaked connection (#487).
			tt.Errorf("response body #%d was never closed (#487)", i)
		}
	}
}

// newTrackedOfflineLLM is newOfflineLLMSeq with a body-tracking HTTP
// client so tests can assert every stream got Close()d.
func newTrackedOfflineLLM(t *testing.T, sses []string) (*llm, *trackingTransport) {
	t.Helper()
	i := 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := i
		i++
		mu.Unlock()
		if n >= len(sses) {
			n = len(sses) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, sses[n])
	}))
	t.Cleanup(srv.Close)

	tr := &trackingTransport{base: http.DefaultTransport}
	return &llm{
		client: sdk.NewClient(
			option.WithAPIKey("test-key-not-real"),
			option.WithBaseURL(srv.URL),
			option.WithHTTPClient(&http.Client{Transport: tr}),
		),
		modelID:  "claude-test",
		builtins: BuiltinTools{WebSearch: true},
	}, tr
}

func TestGenerateContent_ClosesStream_FullDrain(t *testing.T) {
	t.Parallel()
	l, tr := newTrackedOfflineLLM(t, []string{messagesSSEFixture})

	for _, err := range l.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Contents: userText("hi"),
	}, true) {
		if err != nil {
			t.Fatalf("GenerateContent yielded error: %v", err)
		}
	}
	tr.assertAllClosed(t, 1)
}

// TestGenerateContent_ClosesStream_EarlyConsumerStop is the leak the
// issue leads with: a library consumer breaking out of the iterator
// mid-stream (long-lived ctx, no per-turn cancel) must not leave the
// HTTP connection open.
func TestGenerateContent_ClosesStream_EarlyConsumerStop(t *testing.T) {
	t.Parallel()
	l, tr := newTrackedOfflineLLM(t, []string{messagesSSEFixture})

	for resp, err := range l.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Contents: userText("hi"),
	}, true) {
		if err != nil {
			t.Fatalf("GenerateContent yielded error: %v", err)
		}
		if resp.Partial {
			break // consumer stops after the first delta
		}
	}
	tr.assertAllClosed(t, 1)
}

// TestGenerateContent_ClosesStream_Continuations pins the multiplier
// the continuation loop added: every request of a pause_turn turn
// opens its own stream, and each must be closed — including when the
// consumer abandons the iterator mid-continuation.
func TestGenerateContent_ClosesStream_Continuations(t *testing.T) {
	t.Parallel()
	l, tr := newTrackedOfflineLLM(t, []string{pauseTurnSSEFixture, webSearchDoneSSEFixture})

	for _, err := range l.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Contents: userText("what's the latest Go release?"),
	}, true) {
		if err != nil {
			t.Fatalf("GenerateContent yielded error: %v", err)
		}
	}
	tr.assertAllClosed(t, 2)

	// Abandon mid-continuation: stop on the first partial of request
	// #2. Both streams must still be closed.
	l2, tr2 := newTrackedOfflineLLM(t, []string{pauseTurnSSEFixture, webSearchDoneSSEFixture})
	for resp, err := range l2.GenerateContent(context.Background(), &adkmodel.LLMRequest{
		Contents: userText("again"),
	}, true) {
		if err != nil {
			t.Fatalf("GenerateContent yielded error: %v", err)
		}
		if resp.Partial {
			break
		}
	}
	tr2.assertAllClosed(t, 2)
}
