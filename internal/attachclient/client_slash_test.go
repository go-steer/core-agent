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

package attachclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// newTestClient points a Client at an httptest server with the given
// short RPC timeout. The slow (/slash/*) client keeps its own deadline.
func newTestClient(t *testing.T, srvURL string, timeout time.Duration) *Client {
	t.Helper()
	parsed, err := ParseURL(srvURL)
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	return NewWithCredentials(parsed, BearerCreds{}, timeout)
}

// TestSlashBtw_EmptyAnswerDecodes covers the 200-with-empty shape the
// daemon returns when the model declined to answer: the call must
// succeed and hand the caller enough to say WHY there's no text.
func TestSlashBtw_EmptyAnswerDecodes(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(attach.SideQueryResponse{
			Empty:  true,
			Detail: "finish_reason=SAFETY",
		})
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv.URL, time.Second).
		SlashBtw(context.Background(), "/sessions/s1", "why?")
	if err != nil {
		t.Fatalf("SlashBtw: %v", err)
	}
	if !got.Empty || got.Detail != "finish_reason=SAFETY" {
		t.Fatalf("response = %+v, want empty with the provider reason", got)
	}
	if want := "(no answer — finish_reason=SAFETY)"; got.AnswerText() != want {
		t.Errorf("AnswerText() = %q, want %q", got.AnswerText(), want)
	}
}

// TestSlashBtw_RateLimitIsTyped is the client half of defect 10: the
// daemon's cost limiter answers 429 + Retry-After, and the operator
// must see "retry in Ns" rather than a raw "status 429: {...}" that
// reads as a broken daemon.
func TestSlashBtw_RateLimitIsTyped(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "12")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited","retry_after_seconds":12}`))
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL, time.Second).
		SlashBtw(context.Background(), "/sessions/s1", "why?")
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v (%T), want *RateLimitError", err, err)
	}
	if rl.RetryAfter != 12*time.Second {
		t.Errorf("RetryAfter = %v, want 12s", rl.RetryAfter)
	}
	if want := "POST /sessions/s1/slash/btw: rate limited by the daemon — retry in 12s"; rl.Error() != want {
		t.Errorf("Error() = %q, want %q", rl.Error(), want)
	}
}

// TestSlashCallsUseTheSlowClient is defect 7: a slash POST blocks while
// the daemon runs a model call, so it must not be governed by the
// ordinary short RPC deadline. Same server, same delay, two calls: the
// non-slash one times out, the slash one answers.
func TestSlashCallsUseTheSlowClient(t *testing.T) {
	t.Parallel()
	const delay = 150 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer":"still working on the rollout"}`))
	}))
	defer srv.Close()

	// An RPC deadline far below the handler's delay.
	c := newTestClient(t, srv.URL, delay/5)

	if err := c.Wake(context.Background(), "/sessions/s1"); err == nil {
		t.Fatal("Wake succeeded; the short RPC deadline is not in effect, so this test proves nothing")
	}

	got, err := c.SlashBtw(context.Background(), "/sessions/s1", "status?")
	if err != nil {
		t.Fatalf("SlashBtw: %v — slash calls must use the slow client", err)
	}
	if got.Answer != "still working on the rollout" {
		t.Errorf("answer = %q, want the served text", got.Answer)
	}
}

// TestSlowClientNeverShortensAConfiguredTimeout: an operator who asked
// for an RPC deadline longer than our slow floor meant it.
func TestSlowClientNeverShortensAConfiguredTimeout(t *testing.T) {
	t.Parallel()
	long := slowRPCTimeout + time.Hour
	c := newTestClient(t, "http://127.0.0.1:7777", long)
	if c.slowHTTP.Timeout != long {
		t.Errorf("slowHTTP.Timeout = %v, want the configured %v", c.slowHTTP.Timeout, long)
	}

	c = newTestClient(t, "http://127.0.0.1:7777", 30*time.Second)
	if c.slowHTTP.Timeout != slowRPCTimeout {
		t.Errorf("slowHTTP.Timeout = %v, want the %v floor", c.slowHTTP.Timeout, slowRPCTimeout)
	}
	if c.http.Timeout != 30*time.Second {
		t.Errorf("http.Timeout = %v, want the configured 30s", c.http.Timeout)
	}
}

// TestHTTPFor_RoutesOnlySlashEndpoints pins the routing rule so a
// future endpoint rename doesn't silently move every RPC onto the
// five-minute deadline (or move /slash back off it).
func TestHTTPFor_RoutesOnlySlashEndpoints(t *testing.T) {
	t.Parallel()
	c := newTestClient(t, "http://127.0.0.1:7777", time.Second)
	cases := []struct {
		suffix   string
		wantSlow bool
	}{
		{"/sessions/s1/slash/btw", true},
		{"/sessions/s1/slash/compact", true},
		{"/sessions/s1/slash/done", true},
		{"/sessions/s1/inject", false},
		{"/sessions/s1/interrupt", false},
		{"/sessions/s1/resume", false},
		{"/sessions/s1/wake", false},
	}
	for _, tc := range cases {
		gotSlow := c.httpFor(tc.suffix) == c.slowHTTP
		if gotSlow != tc.wantSlow {
			t.Errorf("httpFor(%q) slow = %v, want %v", tc.suffix, gotSlow, tc.wantSlow)
		}
	}
}
