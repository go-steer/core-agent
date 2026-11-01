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

// Originally derived from go-steer/mast.

package attach

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

const testAckBody = `{"subject":"ns/checkout/oom","reason":"known"}`

func buildMonitorAckHandler(t *testing.T, mcfg callerMiddlewareConfig, ackFn MonitorAckHandler) http.Handler {
	t.Helper()
	reg := NewSessionRegistry()
	srv, err := NewServer(Options{
		Registry:   reg,
		MonitorAck: ackFn,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return callerMiddlewareWithConfig(mcfg, srv.mux)
}

func TestMonitorAck_AttributesFromCredentialAndWritesZeroDecisions(t *testing.T) {
	t.Parallel()
	hLog := openParksTestEventLog(t)
	ctx := context.Background()

	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
	}, nil, nil)

	var seenReq *MonitorAckRequest
	ackAt := time.Date(2026, 8, 21, 9, 15, 0, 0, time.UTC)
	handler := buildMonitorAckHandler(t, callerMiddlewareConfig{
		authenticator:         authn,
		enforceAuthentication: true,
	}, func(ctx context.Context, req MonitorAckRequest) (MonitorAckResult, error) {
		seenReq = &req
		rec := eventlog.AckRecord{
			Workload:  "gke-triage",
			Subject:   req.Subject,
			By:        req.By,
			ProxyBy:   req.ProxyBy,
			Reason:    req.Reason,
			At:        ackAt,
			Forwarded: true,
		}
		if err := hLog.RecordAck(ctx, "core-agent", rec); err != nil {
			return MonitorAckResult{}, err
		}
		return MonitorAckResult{
			Workload:  rec.Workload,
			Subject:   rec.Subject,
			By:        rec.By,
			ProxyBy:   rec.ProxyBy,
			At:        rec.At,
			Forwarded: rec.Forwarded,
		}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/monitor-ack", strings.NewReader(testAckBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer alice-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /monitor-ack = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if seenReq == nil || seenReq.By != "alice@example.com" || seenReq.Subject != "ns/checkout/oom" || seenReq.Reason != "known" {
		t.Fatalf("handler saw %+v, want alice@example.com / ns/checkout/oom / known", seenReq)
	}
	var res MonitorAckResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if res.By != "alice@example.com" || res.Subject != "ns/checkout/oom" || !res.Forwarded {
		t.Errorf("result = %+v, want resolved identity and forwarded=true", res)
	}

	decisions, err := hLog.Decisions.ListDecisions(ctx, "mast-acks:mast-ops")
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 0 {
		t.Errorf("decision records = %d, want 0 (acks must never touch agent_approval_decisions)", len(decisions))
	}
}

func TestMonitorAck_BodyAckByIsRefused(t *testing.T) {
	t.Parallel()
	var called bool
	handler := buildMonitorAckHandler(t, callerMiddlewareConfig{}, func(_ context.Context, _ MonitorAckRequest) (MonitorAckResult, error) {
		called = true
		return MonitorAckResult{}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/monitor-ack", strings.NewReader(`{"subject":"ns/checkout/oom","ack_by":"bob@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /monitor-ack with ack_by in body = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	if called {
		t.Fatal("handler was called despite ack_by in body")
	}
	const wantMsg = "ack_by is not accepted in the request body; caller identity is derived from the authenticated credential (use X-Asserted-Caller from an allowlisted proxy)"
	if !strings.Contains(rec.Body.String(), wantMsg) {
		t.Errorf("body = %q, want %q", rec.Body.String(), wantMsg)
	}
}

func TestMonitorAck_ProxyAssertsHumanIdentity(t *testing.T) {
	t.Parallel()
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
		{Identity: "sa:switchboard", Token: "bot-token"},
	}, nil, []string{"sa:switchboard"})

	var seenReq *MonitorAckRequest
	handler := buildMonitorAckHandler(t, callerMiddlewareConfig{
		authenticator:         authn,
		enforceAuthentication: true,
	}, func(_ context.Context, req MonitorAckRequest) (MonitorAckResult, error) {
		seenReq = &req
		return MonitorAckResult{
			Workload:  "gke-triage",
			Subject:   req.Subject,
			By:        req.By,
			ProxyBy:   req.ProxyBy,
			Forwarded: true,
		}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/monitor-ack", strings.NewReader(testAckBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer bot-token")
	req.Header.Set(auth.HeaderAssertedCaller, "alice@example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /monitor-ack = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if seenReq == nil || seenReq.By != "alice@example.com" || seenReq.ProxyBy != "sa:switchboard" {
		t.Fatalf("seenReq = %+v, want By=alice@example.com and ProxyBy=sa:switchboard", seenReq)
	}
}

func TestMonitorAck_UnprivilegedProxyRefused(t *testing.T) {
	t.Parallel()
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
		{Identity: "bob@example.com", Token: "bob-token"},
	}, nil, nil)

	var called bool
	handler := buildMonitorAckHandler(t, callerMiddlewareConfig{
		authenticator:         authn,
		enforceAuthentication: true,
	}, func(_ context.Context, _ MonitorAckRequest) (MonitorAckResult, error) {
		called = true
		return MonitorAckResult{}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/monitor-ack", strings.NewReader(testAckBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer bob-token")
	req.Header.Set(auth.HeaderAssertedCaller, "alice@example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("POST /monitor-ack unprivileged proxy = %d, want 401 or 403", rec.Code)
	}
	if called {
		t.Fatal("handler was called for unprivileged proxy assertion")
	}
}

func TestMonitorAck_UnauthenticatedRefused(t *testing.T) {
	t.Parallel()
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
	}, nil, nil)

	var called bool
	handler := buildMonitorAckHandler(t, callerMiddlewareConfig{
		authenticator:         authn,
		enforceAuthentication: true,
	}, func(_ context.Context, _ MonitorAckRequest) (MonitorAckResult, error) {
		called = true
		return MonitorAckResult{}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/monitor-ack", strings.NewReader(testAckBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /monitor-ack unauthenticated = %d, want 401", rec.Code)
	}
	if called {
		t.Fatal("handler was called for unauthenticated request")
	}
}

func TestMonitorAck_DefaultsByToAnonymousWhenContextHasNoCaller(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	var seenBy string
	srv, err := NewServer(Options{
		Registry: reg,
		MonitorAck: func(_ context.Context, req MonitorAckRequest) (MonitorAckResult, error) {
			seenBy = req.By
			return MonitorAckResult{Subject: req.Subject, By: req.By}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/monitor-ack", strings.NewReader(testAckBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /monitor-ack = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if seenBy != "anonymous" {
		t.Errorf("seenBy = %q, want %q", seenBy, "anonymous")
	}
}

func TestMonitorAck_NilHandlerReturns404(t *testing.T) {
	t.Parallel()
	handler := buildMonitorAckHandler(t, callerMiddlewareConfig{}, nil)

	req := httptest.NewRequest(http.MethodPost, "/monitor-ack", strings.NewReader(testAckBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /monitor-ack with nil MonitorAck = %d, want 404", rec.Code)
	}
}

func TestMonitorAck_BadPayloadsReturn400(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
	}{
		{"missing subject", `{"reason":"known"}`},
		{"blank subject", `{"subject":"   "}`},
		{"invalid JSON", `{{`},
		{"unknown field", `{"subject":"s","window":"4h"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var called bool
			handler := buildMonitorAckHandler(t, callerMiddlewareConfig{}, func(_ context.Context, _ MonitorAckRequest) (MonitorAckResult, error) {
				called = true
				return MonitorAckResult{}, nil
			})
			req := httptest.NewRequest(http.MethodPost, "/monitor-ack", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("POST /monitor-ack (%s) = %d (%s), want 400", tc.name, rec.Code, rec.Body.String())
			}
			if called {
				t.Errorf("handler was called for invalid payload %s", tc.name)
			}
		})
	}
}

func TestMonitorAck_UpstreamFailureReturns502(t *testing.T) {
	t.Parallel()
	handler := buildMonitorAckHandler(t, callerMiddlewareConfig{}, func(_ context.Context, _ MonitorAckRequest) (MonitorAckResult, error) {
		return MonitorAckResult{}, errors.New("upstream MCP ack tool failed")
	})

	req := httptest.NewRequest(http.MethodPost, "/monitor-ack", strings.NewReader(testAckBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("POST /monitor-ack on upstream error = %d (%s), want 502", rec.Code, rec.Body.String())
	}
}

func TestMonitorAck_AnonymousCallerRejectedWith403(t *testing.T) {
	t.Parallel()
	authn := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "alice-token"},
	}, nil, nil)
	handler := buildMonitorAckHandler(t, callerMiddlewareConfig{
		authenticator:         authn,
		enforceAuthentication: false, // AllowAnonymous mode
	}, func(_ context.Context, req MonitorAckRequest) (MonitorAckResult, error) {
		if req.By == "" || req.By == "anonymous" || req.By == auth.Anonymous.Identity {
			return MonitorAckResult{}, ErrMonitorAckAnonymous
		}
		return MonitorAckResult{}, nil
	})

	req := httptest.NewRequest(http.MethodPost, "/monitor-ack", strings.NewReader(testAckBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /monitor-ack with anonymous caller = %d (%s), want 403", rec.Code, rec.Body.String())
	}
}
