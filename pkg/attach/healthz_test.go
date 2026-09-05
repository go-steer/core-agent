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
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func decodeHealthz(t *testing.T, body []byte) healthzResponse {
	t.Helper()
	var got healthzResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("parse body %q: %v", body, err)
	}
	return got
}

// TestHealthzHandler covers the status-code contract kubelet reads and
// the body contract a human reads. The status code is the load-bearing
// half: a probe that returns 200 while a check fails is worse than no
// probe, because it launders a broken daemon as ready.
func TestHealthzHandler(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	cases := []struct {
		name       string
		checks     []HealthCheck
		method     string
		wantStatus int
		wantOK     bool
		wantChecks map[string]string
	}{
		{
			// No checks is not an error: it is the honest report of a
			// daemon with nothing dynamic to interrogate. It must not
			// emit an empty "checks" object, which reads like "we
			// looked and found nothing wrong".
			name:       "no checks reports ok and claims nothing else",
			checks:     nil,
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantOK:     true,
			wantChecks: nil,
		},
		{
			name: "passing check",
			checks: []HealthCheck{
				{Name: "session_db", Check: func(context.Context) error { return nil }},
			},
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantOK:     true,
			wantChecks: map[string]string{"session_db": healthReady},
		},
		{
			name: "failing check flips the status code, not just the body",
			checks: []HealthCheck{
				{Name: "session_db", Check: func(context.Context) error { return boom }},
			},
			method:     http.MethodGet,
			wantStatus: http.StatusServiceUnavailable,
			wantOK:     false,
			wantChecks: map[string]string{"session_db": healthFailed},
		},
		{
			// One sick subsystem makes the pod not-ready even though
			// the others are fine — that is the point of a readiness
			// gate, and the healthy names still report so the operator
			// can see which one it was.
			name: "one failure among several",
			checks: []HealthCheck{
				{Name: "a", Check: func(context.Context) error { return nil }},
				{Name: "b", Check: func(context.Context) error { return boom }},
				{Name: "c", Check: func(context.Context) error { return nil }},
			},
			method:     http.MethodGet,
			wantStatus: http.StatusServiceUnavailable,
			wantOK:     false,
			wantChecks: map[string]string{"a": healthReady, "b": healthFailed, "c": healthReady},
		},
		{
			// A nil func is a wiring bug, not a health signal. Counting
			// it as ready would be a green check that measures nothing.
			name: "nil check func is skipped rather than reported ready",
			checks: []HealthCheck{
				{Name: "unwired"},
				{Name: "real", Check: func(context.Context) error { return nil }},
			},
			method:     http.MethodGet,
			wantStatus: http.StatusOK,
			wantOK:     true,
			wantChecks: map[string]string{"real": healthReady},
		},
		{
			// The route bypasses the mux, so nothing else rejects a
			// write to it. Without this the endpoint would answer POST
			// with 200 and look like a state-changing route that had
			// been exempted from the CSRF guard.
			name: "POST is rejected",
			checks: []HealthCheck{
				{Name: "session_db", Check: func(context.Context) error { return nil }},
			},
			method:     http.MethodPost,
			wantStatus: http.StatusMethodNotAllowed,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := healthzHandler(tc.checks, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tc.method, HealthzPath, nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.Bytes())
			}
			if tc.wantStatus == http.StatusMethodNotAllowed {
				if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
					t.Errorf("Allow = %q, want %q", allow, "GET, HEAD")
				}
				return
			}
			got := decodeHealthz(t, rec.Body.Bytes())
			if got.OK != tc.wantOK {
				t.Errorf("ok = %v, want %v", got.OK, tc.wantOK)
			}
			if len(got.Checks) != len(tc.wantChecks) {
				t.Fatalf("checks = %v, want %v", got.Checks, tc.wantChecks)
			}
			for name, want := range tc.wantChecks {
				if got.Checks[name] != want {
					t.Errorf("checks[%q] = %q, want %q", name, got.Checks[name], want)
				}
			}
			if tc.wantChecks == nil && strings.Contains(rec.Body.String(), "checks") {
				t.Errorf("body %s carries a checks key with no checks registered", rec.Body.String())
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store — a cached 200 outlives the outage", cc)
			}
		})
	}
}

// TestHealthzBodyWithholdsCheckErrors pins the leak boundary. The
// caller is unauthenticated, and a database error routinely carries a
// filesystem path or a DSN. The status word goes in the body; the
// error goes to the log, which only someone already on the node reads.
func TestHealthzBodyWithholdsCheckErrors(t *testing.T) {
	t.Parallel()
	secret := "open /var/secrets/tenant-42/sessions.db: permission denied"
	var log bytes.Buffer
	h := healthzHandler(
		[]HealthCheck{{Name: "session_db", Check: func(context.Context) error {
			return errors.New(secret)
		}}},
		newHealthLogger(&log),
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, HealthzPath, nil))

	if body := rec.Body.String(); strings.Contains(body, "tenant-42") || strings.Contains(body, secret) {
		t.Errorf("body leaks the check error to an unauthenticated caller:\n%s", body)
	}
	if !strings.Contains(log.String(), secret) {
		t.Errorf("log should carry the detail the body withholds; got %q", log.String())
	}
}

// TestHealthzTimesOutRatherThanHanging: a wedged dependency must make
// the probe answer "not ready". If the handler simply blocked, kubelet
// would time out on its side and the goroutine would stay parked — one
// leaked per probe interval, against the subsystem that is already ill.
func TestHealthzTimesOutRatherThanHanging(t *testing.T) {
	t.Parallel()
	h := healthzHandler([]HealthCheck{{
		Name: "wedged",
		Check: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}}, nil)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, HealthzPath, nil))
		done <- rec
	}()
	select {
	case rec := <-done:
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
		got := decodeHealthz(t, rec.Body.Bytes())
		if got.Checks["wedged"] != healthTimeout {
			t.Errorf("checks[wedged] = %q, want %q", got.Checks["wedged"], healthTimeout)
		}
	case <-time.After(healthCheckTimeout + 5*time.Second):
		t.Fatal("handler did not return: a wedged check blocked the probe forever")
	}
}

// TestHealthzHonoursClientCancellation: kubelet gives up on its own
// timeoutSeconds. The check must observe that rather than run to the
// handler's full budget.
func TestHealthzHonoursClientCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	observed := make(chan struct{})
	h := healthzHandler([]HealthCheck{{
		Name: "slow",
		Check: func(cctx context.Context) error {
			close(observed)
			<-cctx.Done()
			return cctx.Err()
		},
	}}, nil)
	req := httptest.NewRequest(http.MethodGet, HealthzPath, nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-observed
	cancel()
	select {
	case <-done:
	case <-time.After(healthCheckTimeout - 500*time.Millisecond):
		t.Fatal("handler ignored client cancellation and ran to its own budget")
	}
}

// TestNewHealthLogger: one line per transition, not one per probe. A
// probe runs every few seconds for the life of the pod, so a sink that
// logged every failure is how the line that mattered gets buried.
func TestNewHealthLogger(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := newHealthLogger(&buf)
	boom := errors.New("db is locked")

	log("session_db", true, nil)
	log("session_db", true, nil)
	if buf.Len() != 0 {
		t.Fatalf("a healthy daemon should be silent; got %q", buf.String())
	}
	for range 5 {
		log("session_db", false, boom)
	}
	if n := strings.Count(buf.String(), "not ready"); n != 1 {
		t.Errorf("five consecutive failures logged %d times, want 1:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "db is locked") {
		t.Errorf("failure line should carry the error: %q", buf.String())
	}
	log("session_db", true, nil)
	if n := strings.Count(buf.String(), "recovered"); n != 1 {
		t.Errorf("recovery logged %d times, want 1:\n%s", n, buf.String())
	}
	// Break again. A sink that only remembered "already reported"
	// would stay silent through the second outage.
	log("session_db", false, boom)
	if n := strings.Count(buf.String(), "not ready"); n != 2 {
		t.Errorf("re-failure after recovery logged %d total, want 2:\n%s", n, buf.String())
	}
	// A check that fails on its very first observation must log: an
	// unknown-to-failing edge is still an edge.
	log("fresh", false, boom)
	if !strings.Contains(buf.String(), "fresh is not ready") {
		t.Errorf("first-observation failure not logged:\n%s", buf.String())
	}
}

// TestNewHealthLoggerConcurrent: probes and their checks are per
// request, so the sink is called from many goroutines. Run under -race.
func TestNewHealthLoggerConcurrent(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	log := newHealthLogger(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return len(p), nil
	}))
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			log("session_db", i%2 == 0, errors.New("x"))
		}(i)
	}
	wg.Wait()
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestPublicBypassRoutesExactPathsOnly: the bypassed handlers get no
// caller identity, no ACL enforcement and no CSRF guard, so the match
// has to be exact. A prefix match would make /healthz/../sessions —
// or anything else a crafted URL can reach — unauthenticated.
func TestPublicBypassRoutesExactPathsOnly(t *testing.T) {
	t.Parallel()
	public := map[string]http.Handler{
		HealthzPath: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}),
	}
	protected := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	h := publicBypass(public, protected)

	cases := []struct {
		path string
		want int
	}{
		{"/healthz", http.StatusTeapot},
		{"/healthz/", http.StatusUnauthorized},
		{"/healthz/sessions", http.StatusUnauthorized},
		{"/healthzz", http.StatusUnauthorized},
		{"/HEALTHZ", http.StatusUnauthorized},
		{"/sessions", http.StatusUnauthorized},
		{"/", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s: status %d, want %d", tc.path, rec.Code, tc.want)
		}
	}

	// An empty map must not swallow requests.
	rec := httptest.NewRecorder()
	publicBypass(nil, protected).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("empty bypass: status %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

// TestIntegration_HealthzUnauthenticated is the test that fails on
// pre-#946 code: it drives a real listener with a bearer token
// configured and fetches /healthz with no credentials. Before this
// change that returned 401, which is precisely what kubelet read as
// "not ready" during the 2026-07-13 demo drive and why recipes fell
// back to a coarse tcpSocket probe.
func TestIntegration_HealthzUnauthenticated(t *testing.T) {
	t.Parallel()
	failing := make(chan error, 1)
	failing <- nil
	current := func() error {
		err := <-failing
		failing <- err
		return err
	}
	srv, err := NewServer(Options{
		Registry:  NewSessionRegistry(),
		Addr:      "127.0.0.1:0",
		Auth:      AuthConfig{BearerToken: "secret"},
		HealthLog: io.Discard,
		HealthChecks: []HealthCheck{
			{Name: "session_db", Check: func(context.Context) error { return current() }},
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	defer func() { _ = srv.Close(); <-errCh }()
	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	base := "http://" + srv.Addr()

	get := func(t *testing.T, path string) (int, []byte) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, body
	}

	// 1) Healthy, no credentials: 200 through the full middleware chain.
	code, body := get(t, HealthzPath)
	if code != http.StatusOK {
		t.Fatalf("unauthenticated GET /healthz: status %d, want 200 (body %s)", code, body)
	}
	if got := decodeHealthz(t, body); !got.OK || got.Checks["session_db"] != healthReady {
		t.Errorf("body = %s, want ok with session_db ready", body)
	}

	// 2) The bypass must not have disarmed the middleware for anything
	//    else. This is the guard against "made the probe work" turning
	//    into "made the daemon public".
	if code, _ := get(t, "/sessions"); code != http.StatusUnauthorized {
		t.Errorf("/sessions without a token: status %d, want 401", code)
	}

	// 3) Degrade the subsystem: the probe must go red. A probe that
	//    cannot fail is the coarse signal this issue exists to replace.
	<-failing
	failing <- errors.New("database is locked")
	code, body = get(t, HealthzPath)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("GET /healthz with a failing check: status %d, want 503 (body %s)", code, body)
	}
	if got := decodeHealthz(t, body); got.OK || got.Checks["session_db"] != healthFailed {
		t.Errorf("body = %s, want not-ok with session_db failed", body)
	}
}

// TestIntegration_HealthzWithoutChecks: the endpoint exists on every
// attach server, including ones that register nothing. It reports
// {"ok":true} and no more, which is exactly what a tcpSocket probe
// proves — the honest floor rather than a fabricated subsystem report.
func TestIntegration_HealthzWithoutChecks(t *testing.T) {
	t.Parallel()
	srv, err := NewServer(Options{Registry: NewSessionRegistry(), Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	defer func() { _ = srv.Close(); <-errCh }()
	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := http.Get("http://" + srv.Addr() + HealthzPath)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 (body %s)", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "checks") {
		t.Errorf("body %s claims subsystem knowledge it does not have", body)
	}
}

// TestHealthzIsNotTraced: kubelet re-probes for the life of the pod, so
// this is the highest-volume path on the listener. A span per probe is
// pure billing noise in Cloud Trace, and the transition log is where a
// failure actually surfaces.
func TestHealthzIsNotTraced(t *testing.T) {
	t.Parallel()
	if shouldTraceRequest(httptest.NewRequest(http.MethodGet, HealthzPath, nil)) {
		t.Error("GET /healthz should be filtered out of tracing")
	}
	// Only the reads. A POST to the path is a 405, but if one ever
	// becomes meaningful it should trace like every other write.
	if !shouldTraceRequest(httptest.NewRequest(http.MethodPost, HealthzPath, nil)) {
		t.Error("POST /healthz should still trace")
	}
	if !shouldTraceRequest(httptest.NewRequest(http.MethodGet, "/inject", nil)) {
		t.Error("filter widened beyond the polling reads")
	}
}
