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

// #935 asked for a transient-error retry on both shipped providers.
// This adapter did not get one, because anthropic-sdk-go already
// retries 429 and 5xx internally and honours the server's retry-after.
// The GenerateContent doc comment says so; these tests are what make
// that a checked claim instead of a remembered one.

package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// rejectThenServe answers the first reject requests with status, then
// serves the streaming fixture. Retry-After: 0 keeps the SDK's backoff
// from making the test slow.
func rejectThenServe(t *testing.T, status, reject int) (*llm, *int32) {
	t.Helper()
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if int(atomic.AddInt32(&n, 1)) <= reject {
			w.Header().Set("Retry-After", "0")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(messagesSSEFixture))
	}))
	t.Cleanup(srv.Close)

	return &llm{
		client: sdk.NewClient(
			option.WithAPIKey("test-key-not-real"),
			option.WithBaseURL(srv.URL),
		),
		modelID:  "claude-test",
		builtins: BuiltinTools{},
	}, &n
}

func generate(t *testing.T, l *llm) (texts []string, errs []error) {
	t.Helper()
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{{Text: "hello"}},
		}},
	}
	for resp, err := range l.GenerateContent(context.Background(), req, false) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if resp != nil && resp.Content != nil {
			for _, p := range resp.Content.Parts {
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
		}
	}
	return texts, errs
}

// The load-bearing claim: a 429 does not kill the call, because the SDK
// transparently retries it. If this ever fails — someone passed
// option.WithMaxRetries(0), or the SDK changed shouldRetry — then this
// adapter DOES need the models.RetryPolicy wrapper that the Gemini side
// has, and the GenerateContent comment is wrong.
func TestSDKRetriesRateLimitWithoutOurHelp(t *testing.T) {
	l, n := rejectThenServe(t, http.StatusTooManyRequests, 1)

	texts, errs := generate(t, l)

	if len(errs) != 0 {
		t.Fatalf("429 surfaced to the caller: %v — the SDK retry is not in effect, so #935 needs to wrap this adapter after all", errs)
	}
	if got := atomic.LoadInt32(n); got != 2 {
		t.Errorf("server saw %d requests, want 2 (the rejected one and the retry)", got)
	}
	if len(texts) == 0 || !strings.Contains(strings.Join(texts, ""), "Hello world") {
		t.Errorf("texts = %v, want the fixture content after the retry", texts)
	}
}

// 529 overloaded is Anthropic's "come back in a moment" and is covered
// by the same shouldRetry branch (>= 500).
func TestSDKRetriesOverloaded(t *testing.T) {
	l, n := rejectThenServe(t, 529, 1)

	if _, errs := generate(t, l); len(errs) != 0 {
		t.Fatalf("529 surfaced to the caller: %v", errs)
	}
	if got := atomic.LoadInt32(n); got != 2 {
		t.Errorf("server saw %d requests, want 2", got)
	}
}

// And the bound: the SDK gives up after its configured retries rather
// than hammering. Three rejections exhaust the default two retries, so
// the error reaches us — which is the right moment for it to, and the
// reason stacking our own retry on top would only add a fourth and
// fifth request to a provider that has already said no three times.
func TestSDKStopsRetryingAndSurfacesTheError(t *testing.T) {
	l, n := rejectThenServe(t, http.StatusTooManyRequests, 3)

	if _, errs := generate(t, l); len(errs) == 0 {
		t.Fatal("want the 429 to surface once the SDK's retries are exhausted")
	}
	if got := atomic.LoadInt32(n); got != 3 {
		t.Errorf("server saw %d requests, want 3 (initial + two retries)", got)
	}
}
