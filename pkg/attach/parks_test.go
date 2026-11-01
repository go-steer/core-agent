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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/approval"
	"github.com/go-steer/core-agent/v3/pkg/auth"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

type parkTestRegistrant struct {
	stubRegistrant
	lastInjectCtx context.Context
}

func (p *parkTestRegistrant) InjectAsContext(ctx context.Context, msg string, caller auth.Caller) error {
	p.lastInjectCtx = ctx
	return p.Inject(msg)
}

func openParksTestEventLog(t *testing.T) *eventlog.Handle {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "parks.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dbPath))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestParksRoutes_EmptyAndParkedProjection(t *testing.T) {
	hLog := openParksTestEventLog(t)
	ctx := context.Background()

	s1, err := hLog.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u1", SessionID: "s-parked",
	})
	if err != nil {
		t.Fatalf("Create s-parked: %v", err)
	}
	_, err = hLog.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u1", SessionID: "s-idle",
	})
	if err != nil {
		t.Fatalf("Create s-idle: %v", err)
	}

	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "app", user: "u1", sid: "s-parked", log: hLog}); err != nil {
		t.Fatalf("Register s-parked: %v", err)
	}
	if _, err := reg.Register(&stubRegistrant{app: "app", user: "u1", sid: "s-idle", log: hLog}); err != nil {
		t.Fatalf("Register s-idle: %v", err)
	}

	mux := http.NewServeMux()
	h := newHandlers(reg, newBroadcasterPool())
	h.register(mux)

	// 1. Before any park event, GET /parks returns {"sessions": []}.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/parks", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /parks = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "null") {
		t.Errorf("empty GET /parks body contains null: %s", rec.Body.String())
	}
	var emptyOut ParksResult
	if err := json.Unmarshal(rec.Body.Bytes(), &emptyOut); err != nil {
		t.Fatalf("decode /parks: %v", err)
	}
	if emptyOut.Sessions == nil || len(emptyOut.Sessions) != 0 {
		t.Errorf("emptyOut.Sessions = %+v, want non-nil empty slice", emptyOut.Sessions)
	}

	// 2. Append a write-gate confirmation park and a prior-state capture on s-parked.
	capRaw, err := approval.EncodeCapture(approval.CaptureRecord{
		FunctionCallID: "call-0",
		Tool:           "scale_deployment",
		Arguments:      map[string]any{"deployment": "api", "replicas": float64(2)},
		Key:            "scale_deployment(deployment=api, replicas=2)",
		CapturedAt:     time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
		Read:           "get_deployment",
		Digest:         "sha256:1234",
		Prior:          map[string]any{"replicas": float64(1)},
		Revert: &approval.ProposedChange{
			Tool:      "scale_deployment",
			Arguments: map[string]any{"deployment": "api", "replicas": float64(1)},
		},
	})
	if err != nil {
		t.Fatalf("EncodeCapture: %v", err)
	}

	parkEv := session.NewEvent(ctx, "inv-1")
	parkEv.Author = "agent"
	parkEv.LongRunningToolIDs = []string{"adk-conf-1"}
	parkEv.Actions.StateDelta = map[string]any{
		approval.CaptureStateKey("call-0"): capRaw,
	}
	parkEv.Content = &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				ID:   "adk-conf-1",
				Name: toolconfirmation.FunctionCallName,
				Args: map[string]any{
					"originalFunctionCall": map[string]any{
						"id":   "call-orig-1",
						"name": "scale_deployment",
						"args": map[string]any{"deployment": "checkout", "replicas": float64(4)},
					},
					"toolConfirmation": map[string]any{
						"hint": "Approve scale_deployment?",
						"payload": map[string]any{
							"tool":    "scale_deployment",
							"args":    map[string]any{"deployment": "checkout", "replicas": float64(4)},
							"subject": "scale_deployment(deployment=checkout, replicas=4)",
						},
					},
				},
			},
		}},
	}
	if err := hLog.Service.AppendEvent(ctx, s1.Session, parkEv); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	// 3. GET /parks returns only s-parked.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/parks", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /parks = %d, want 200", rec.Code)
	}
	var parkedOut ParksResult
	if err := json.Unmarshal(rec.Body.Bytes(), &parkedOut); err != nil {
		t.Fatalf("decode /parks: %v", err)
	}
	if len(parkedOut.Sessions) != 1 {
		t.Fatalf("parkedOut.Sessions = %+v, want 1 session", parkedOut.Sessions)
	}
	sp := parkedOut.Sessions[0]
	if sp.SessionID != "s-parked" || sp.State != AgentStatePaused || sp.Awaiting != ParkKindApproval {
		t.Errorf("SessionParks = %+v, want s-parked / paused / approval", sp)
	}
	if len(sp.Parks) != 1 || sp.Parks[0].Change == nil || sp.Parks[0].Change.Tool != "scale_deployment" {
		t.Errorf("Parks = %+v, want 1 scale_deployment approval park", sp.Parks)
	}
	if len(sp.Applied) != 1 || sp.Applied[0].Revert == nil || sp.Applied[0].Revert.Tool != "scale_deployment" {
		t.Errorf("Applied = %+v, want 1 captured applied call with revert", sp.Applied)
	}

	// 4. GET /parks/s-idle and GET /sessions/s-idle/parks return 200 with empty parks.
	for _, path := range []string{"/parks/s-idle", "/sessions/s-idle/parks", "/sessions/app/s-idle/parks"} {
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, rec.Code)
		}
		var singleOut ParksResult
		if err := json.Unmarshal(rec.Body.Bytes(), &singleOut); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if len(singleOut.Sessions) != 1 || singleOut.Sessions[0].SessionID != "s-idle" || len(singleOut.Sessions[0].Parks) != 0 {
			t.Errorf("GET %s = %+v, want s-idle with 0 parks", path, singleOut)
		}
	}

	// 5. Unknown session returns 404.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/parks/no-such-session", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /parks/no-such-session = %d, want 404", rec.Code)
	}
}

func TestAckEffectsRoutes_AndMast562DaemonOwnedTurnContext(t *testing.T) {
	hLog := openParksTestEventLog(t)
	ctx := context.Background()

	_, err := hLog.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u1", SessionID: "s-1",
	})
	if err != nil {
		t.Fatalf("Create s-1: %v", err)
	}

	reg := NewSessionRegistry()
	agentStub := &parkTestRegistrant{
		stubRegistrant: stubRegistrant{app: "app", user: "u1", sid: "s-1", log: hLog},
	}
	if _, err := reg.Register(agentStub); err != nil {
		t.Fatalf("Register s-1: %v", err)
	}

	daemonCtx, daemonCancel := context.WithCancel(context.Background())
	defer daemonCancel()

	mux := http.NewServeMux()
	h := newHandlers(reg, newBroadcasterPool())
	h.daemonCtx = daemonCtx
	h.register(mux)

	// 1. POST /ack-effects records the watermark.
	body := bytes.NewBufferString(`{"session_id":"s-1","reason":"checked cluster state"}`)
	req := httptest.NewRequest(http.MethodPost, "/ack-effects", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /ack-effects = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var ackResp AckEffectsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ackResp); err != nil {
		t.Fatalf("decode AckEffectsResponse: %v", err)
	}
	if ackResp.Session != "s-1" || ackResp.AckedAt.IsZero() {
		t.Errorf("AckEffectsResponse = %+v, want s-1 and non-zero AckedAt", ackResp)
	}
	if gotAt, ok := hLog.EffectsAckedAt(ctx, "app", "u1", "s-1"); !ok || !gotAt.Equal(ackResp.AckedAt) {
		t.Errorf("EffectsAckedAt = (%v, %v), want (%v, true)", gotAt, ok, ackResp.AckedAt)
	}

	// 2. POST /sessions/s-1/ack-effects also works.
	req2 := httptest.NewRequest(http.MethodPost, "/sessions/s-1/ack-effects", bytes.NewBufferString(`{"reason":"second ack"}`))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("POST /sessions/s-1/ack-effects = %d (%s), want 200", rec2.Code, rec2.Body.String())
	}

	// 3. mast #562: POST /sessions/s-1/inject passes a daemon-owned context to
	// InjectAsContext that remains uncancelled after the HTTP request context cancels
	// and after the HTTP handler returns.
	injReqCtx, injReqCancel := context.WithCancel(auth.WithCaller(context.Background(), auth.Caller{Identity: "user:alice"}))
	injReq := httptest.NewRequest(http.MethodPost, "/sessions/s-1/inject", bytes.NewBufferString(`{"message":"hello"}`)).WithContext(injReqCtx)
	injReq.Header.Set("Content-Type", "application/json")
	injRec := httptest.NewRecorder()
	mux.ServeHTTP(injRec, injReq)
	if injRec.Code != http.StatusOK {
		t.Fatalf("POST /sessions/s-1/inject = %d (%s), want 200", injRec.Code, injRec.Body.String())
	}
	// Cancel the HTTP request context (simulating client disconnect after HTTP handler returned).
	injReqCancel()
	if agentStub.lastInjectCtx == nil {
		t.Fatal("agentStub.lastInjectCtx == nil, want non-nil context from InjectAsContext")
	}
	if err := agentStub.lastInjectCtx.Err(); err != nil {
		t.Fatalf("lastInjectCtx.Err() after HTTP request cancel and handler return = %v, want nil (mast #562)", err)
	}
	if c, ok := auth.CallerFromContext(agentStub.lastInjectCtx); !ok || c.Identity != "user:alice" {
		t.Errorf("CallerFromContext(lastInjectCtx) = %+v, want user:alice preserved", c)
	}

	// Cancel the daemon context: lastInjectCtx must now cancel, and mutating routes must 503.
	daemonCancel()
	select {
	case <-agentStub.lastInjectCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("lastInjectCtx did not cancel after daemonCtx was cancelled")
	}

	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/ack-effects", bytes.NewBufferString(`{"session_id":"s-1"}`))
	req3.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /ack-effects while draining = %d, want 503", rec3.Code)
	}
}

func TestClassifyTurnError_RefusalLoopError(t *testing.T) {
	t.Parallel()
	loop := &approval.RefusalLoopError{Tool: "patch_resource", Key: "patch_resource(name=api)", Count: 3}
	if got := loop.TurnErrorKind(); got != TurnErrorRefusalLoop {
		t.Fatalf("TurnErrorKind() = %q, want %q", got, TurnErrorRefusalLoop)
	}
	te := ClassifyTurnError(loop)
	if te.Kind != TurnErrorRefusalLoop {
		t.Errorf("ClassifyTurnError kind = %q, want %q", te.Kind, TurnErrorRefusalLoop)
	}
	if te.Retryable {
		t.Error("Retryable = true — a retry re-proposes the call the operator refused")
	}
}
