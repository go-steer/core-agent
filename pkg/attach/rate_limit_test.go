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

package attach

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// newTestLimiter builds a limiter with an injected, manually-advanced
// clock so the bucket math tests never sleep.
func newTestLimiter(t *testing.T, perMinute, burst int) (*costRateLimiter, *time.Time) {
	t.Helper()
	l := newCostRateLimiter(CostRateLimit{PerMinute: perMinute, Burst: burst})
	if l == nil {
		t.Fatal("newCostRateLimiter returned nil for an enabled config")
	}
	now := time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	clock := &now
	l.now = func() time.Time { return *clock }
	return l, clock
}

func TestCostRateLimiter_BurstThenDenyThenRefill(t *testing.T) {
	t.Parallel()
	// 60/min = 1 token/sec; burst 3.
	l, clock := newTestLimiter(t, 60, 3)

	for i := range 3 {
		if ok, _ := l.Allow("alice"); !ok {
			t.Fatalf("burst call %d denied, want allowed", i+1)
		}
	}
	ok, wait := l.Allow("alice")
	if ok {
		t.Fatal("call past burst allowed, want denied")
	}
	if wait != time.Second {
		t.Errorf("wait = %v, want 1s (empty bucket at 1 token/sec)", wait)
	}

	// Half a second in: still short of a full token, wait halves.
	*clock = clock.Add(500 * time.Millisecond)
	ok, wait = l.Allow("alice")
	if ok {
		t.Fatal("allowed after 0.5s, want denied (only 0.5 tokens refilled)")
	}
	if wait != 500*time.Millisecond {
		t.Errorf("wait = %v, want 500ms", wait)
	}

	// One more second: a full token has accrued.
	*clock = clock.Add(time.Second)
	if ok, _ := l.Allow("alice"); !ok {
		t.Fatal("denied after refill, want allowed")
	}

	// A long idle period refills only up to burst, never beyond.
	*clock = clock.Add(time.Hour)
	for i := range 3 {
		if ok, _ := l.Allow("alice"); !ok {
			t.Fatalf("post-idle burst call %d denied, want allowed (cap = burst)", i+1)
		}
	}
	if ok, _ := l.Allow("alice"); ok {
		t.Fatal("4th post-idle call allowed — bucket refilled past burst")
	}
}

func TestCostRateLimiter_PerIdentityIsolation(t *testing.T) {
	t.Parallel()
	l, _ := newTestLimiter(t, 60, 2)

	// Exhaust alice.
	l.Allow("alice")
	l.Allow("alice")
	if ok, _ := l.Allow("alice"); ok {
		t.Fatal("alice should be exhausted")
	}
	// Bob is untouched.
	if ok, _ := l.Allow("bob"); !ok {
		t.Fatal("bob denied, want allowed — buckets must be per-identity")
	}
}

func TestCostRateLimiter_EmptyIdentityBucketsUnderAnonymous(t *testing.T) {
	t.Parallel()
	l, _ := newTestLimiter(t, 60, 1)

	if ok, _ := l.Allow(""); !ok {
		t.Fatal("first anonymous call denied")
	}
	// The empty identity and the literal "anonymous" bucket are the
	// same bucket — empty identities must not mint fresh buckets.
	if ok, _ := l.Allow(anonymousRateIdentity); ok {
		t.Fatal(`Allow("") and Allow("anonymous") drew from different buckets`)
	}
}

func TestCostRateLimiter_PruneEvictsIdleBuckets(t *testing.T) {
	t.Parallel()
	l, clock := newTestLimiter(t, 60, 1)

	for i := range costLimiterPruneThreshold + 1 {
		l.Allow(fmt.Sprintf("id-%d", i))
	}
	if got := len(l.buckets); got != costLimiterPruneThreshold+1 {
		t.Fatalf("bucket count = %d, want %d", got, costLimiterPruneThreshold+1)
	}

	// Touch one identity at +30m so it stays fresh relative to the
	// eventual sweep cutoff.
	*clock = clock.Add(30 * time.Minute)
	l.Allow("id-0")

	// At +90m the untouched buckets are >1h idle; the next Allow
	// (map over threshold) sweeps them. id-0 (30m idle) survives.
	*clock = clock.Add(time.Hour)
	l.Allow("fresh")
	if got := len(l.buckets); got != 2 {
		t.Errorf("bucket count after prune = %d, want 2 (id-0 + fresh)", got)
	}
	if _, ok := l.buckets["id-0"]; !ok {
		t.Error("recently-seen bucket id-0 was evicted")
	}
	if _, ok := l.buckets["fresh"]; !ok {
		t.Error("freshly-created bucket missing after prune")
	}
}

func TestCostRateLimiter_ZeroValueDefaultsAndDisabled(t *testing.T) {
	t.Parallel()
	l := newCostRateLimiter(CostRateLimit{})
	if l == nil {
		t.Fatal("zero-value config must ENABLE the limiter with defaults")
	}
	if l.perMinute != defaultCostPerMinute || l.burst != defaultCostBurst {
		t.Errorf("defaults = %v/min burst %v, want %d/%d",
			l.perMinute, l.burst, defaultCostPerMinute, defaultCostBurst)
	}
	if newCostRateLimiter(CostRateLimit{Disabled: true}) != nil {
		t.Error("Disabled config must return a nil (no-enforcement) limiter")
	}
}

// --- handler-level coverage ----------------------------------------------

// startTestServerOpts is startTestServer with caller-controlled
// Options (Registry and Addr are filled in).
func startTestServerOpts(t *testing.T, reg *SessionRegistry, opts Options) (string, func()) {
	t.Helper()
	opts.Registry = reg
	opts.Addr = "127.0.0.1:0"
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	deadline := time.Now().Add(time.Second)
	var base string
	for time.Now().Before(deadline) {
		if a := srv.Addr(); a != "" {
			base = "http://" + a
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if base == "" {
		_ = srv.Close()
		t.Fatalf("listener never bound")
	}
	return base, func() {
		_ = srv.Close()
		select {
		case err := <-errCh:
			if err != nil {
				t.Logf("ListenAndServe returned: %v", err)
			}
		case <-time.After(time.Second):
			t.Logf("ListenAndServe did not exit promptly")
		}
	}
}

// postJSONWithToken fires a POST with an optional bearer token and
// returns the response (caller closes the body).
func postJSONWithToken(t *testing.T, url, token, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// assertRateLimited checks the full 429 contract: status, Retry-After
// header, and the JSON body with a matching retry_after_seconds.
func assertRateLimited(t *testing.T, resp *http.Response) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTooManyRequests {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 429. Body: %s", resp.StatusCode, b)
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		t.Fatal("429 response missing Retry-After header")
	}
	raSecs, err := strconv.Atoi(ra)
	if err != nil || raSecs < 1 {
		t.Fatalf("Retry-After = %q, want a positive integer second count", ra)
	}
	var body struct {
		Error             string `json:"error"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if body.Error != "rate limited" {
		t.Errorf(`body error = %q, want "rate limited"`, body.Error)
	}
	if body.RetryAfterSeconds != raSecs {
		t.Errorf("retry_after_seconds = %d, header Retry-After = %d — must match", body.RetryAfterSeconds, raSecs)
	}
}

// TestRateLimit_SlashSecondCall429 pins the enforcement-order
// contract: the limiter runs BEFORE capability dispatch, so even a
// 501 from an unwired capability consumes the token, and the second
// immediate call 429s with the full Retry-After + JSON-body shape.
func TestRateLimit_SlashSecondCall429(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	// Bare stubRegistrant — no CompactSlashProvider, so the handler
	// itself would answer 501. The token must be gone anyway.
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "s1"}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServerOpts(t, reg, Options{
		CostRateLimit: CostRateLimit{PerMinute: 1, Burst: 1},
	})
	defer cleanup()
	url := base + "/sessions/core-agent/s1/slash/compact"

	first := postJSONWithToken(t, url, "", "{}")
	_ = first.Body.Close()
	if first.StatusCode != http.StatusNotImplemented {
		t.Fatalf("first call status = %d, want 501 (unwired capability, but past the limiter)", first.StatusCode)
	}

	assertRateLimited(t, postJSONWithToken(t, url, "", "{}"))
}

// TestRateLimit_PerIdentityViaBearerTable exercises the identity key
// end-to-end: two provisioned bearer users hit the same server; alice
// exhausting her bucket leaves bob unaffected.
func TestRateLimit_PerIdentityViaBearerTable(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "s1"}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServerOpts(t, reg, Options{
		Authenticator: auth.NewBearerTokenAuth([]auth.User{
			{Identity: "alice@example.com", Token: "alice-token"},
			{Identity: "bob@example.com", Token: "bob-token"},
		}, nil, nil),
		CostRateLimit: CostRateLimit{PerMinute: 1, Burst: 1},
	})
	defer cleanup()
	url := base + "/sessions/core-agent/s1/slash/compact"

	// Alice: first consumes her only token, second 429s.
	resp := postJSONWithToken(t, url, "alice-token", "{}")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("alice first call = %d, want 501", resp.StatusCode)
	}
	assertRateLimited(t, postJSONWithToken(t, url, "alice-token", "{}"))

	// Bob presents a different verified identity — fresh bucket.
	resp = postJSONWithToken(t, url, "bob-token", "{}")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("bob first call = %d, want 501 (alice's exhaustion must not affect bob)", resp.StatusCode)
	}
}

// TestRateLimit_DisabledNever429s pins the escape hatch.
func TestRateLimit_DisabledNever429s(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "s1"}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServerOpts(t, reg, Options{
		CostRateLimit: CostRateLimit{PerMinute: 1, Burst: 1, Disabled: true},
	})
	defer cleanup()
	url := base + "/sessions/core-agent/s1/slash/compact"

	for i := range 8 {
		resp := postJSONWithToken(t, url, "", "{}")
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			t.Fatalf("call %d returned 429 with Disabled: true", i+1)
		}
	}
}

// TestRateLimit_UnlimitedEndpointsUnaffected: once the caller's cost
// bucket is exhausted, the read path (GET status) still answers 200 —
// the limiter scopes to cost-bearing endpoints only.
func TestRateLimit_UnlimitedEndpointsUnaffected(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "s1"}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServerOpts(t, reg, Options{
		CostRateLimit: CostRateLimit{PerMinute: 1, Burst: 1},
	})
	defer cleanup()
	slashURL := base + "/sessions/core-agent/s1/slash/compact"

	// Exhaust the (anonymous) bucket.
	resp := postJSONWithToken(t, slashURL, "", "{}")
	_ = resp.Body.Close()
	assertRateLimited(t, postJSONWithToken(t, slashURL, "", "{}"))

	// Reads sail through under exhaustion.
	for i := range 3 {
		getResp, err := http.Get(base + "/sessions/core-agent/s1/status")
		if err != nil {
			t.Fatalf("GET status: %v", err)
		}
		_ = getResp.Body.Close()
		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("GET status call %d = %d, want 200 (read endpoints are never limited)", i+1, getResp.StatusCode)
		}
	}
}

// TestRateLimit_CreateSessionLimited: POST /sessions constructs an
// agent per call, so it shares the cost bucket. The limiter fires
// before the factory (and even before the 501-when-no-factory path).
func TestRateLimit_CreateSessionLimited(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	base, cleanup := startTestServerOpts(t, reg, Options{
		CostRateLimit: CostRateLimit{PerMinute: 1, Burst: 1},
	})
	defer cleanup()
	url := base + "/sessions"

	// No SessionFactory wired → 501, but the token is consumed.
	first := postJSONWithToken(t, url, "", "{}")
	_ = first.Body.Close()
	if first.StatusCode != http.StatusNotImplemented {
		t.Fatalf("first POST /sessions = %d, want 501 (no factory)", first.StatusCode)
	}
	assertRateLimited(t, postJSONWithToken(t, url, "", "{}"))
}

// TestRateLimit_PricingRefreshLimited: the third cost-bearing family —
// pricing/refresh does a network fetch + catalog rebuild per call.
func TestRateLimit_PricingRefreshLimited(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "s1"}); err != nil {
		t.Fatal(err)
	}
	base, cleanup := startTestServerOpts(t, reg, Options{
		CostRateLimit: CostRateLimit{PerMinute: 1, Burst: 1},
	})
	defer cleanup()
	url := base + "/sessions/core-agent/s1/pricing/refresh"

	first := postJSONWithToken(t, url, "", "{}")
	_ = first.Body.Close()
	if first.StatusCode != http.StatusNotImplemented {
		t.Fatalf("first pricing/refresh = %d, want 501 (capability unwired on stub)", first.StatusCode)
	}
	assertRateLimited(t, postJSONWithToken(t, url, "", "{}"))
}
