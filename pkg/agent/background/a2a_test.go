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

package background

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type fakeA2AServer struct {
	srv        *httptest.Server
	cardHits   atomic.Int32
	sendHits   atomic.Int32
	streamHits atomic.Int32
	getHits    atomic.Int32
	cancelHits atomic.Int32

	mu           sync.Mutex
	lastCanceled string
	lastVersion  string
	lastAuth     string
	lastSkillID  string
	lastGoalText string

	requireToken string
	card         A2ACard
	onSend       func(req rpcRequest, msg A2AMessage) (any, *A2ARPCError)
	onStream     func(w http.ResponseWriter, r *http.Request, req rpcRequest, msg A2AMessage)
	onGet        func(id string, n int32) (any, *A2ARPCError)
}

func newFakeA2A(t *testing.T, setup func(*fakeA2AServer)) *fakeA2AServer {
	t.Helper()
	f := &fakeA2AServer{
		card: A2ACard{
			Name:               "fake-agent",
			ProtocolVersion:    "0.3.0",
			PreferredTransport: A2ATransportJSONRPC,
			Skills: []A2ASkill{
				{ID: "investigate", Name: "Investigate"},
				{ID: "summarize", Name: "Summarize"},
			},
		},
	}
	if setup != nil {
		setup(f)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(A2AWellKnownCardPath, f.handleCard)
	mux.HandleFunc("/a2a", f.handleRPC)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	if f.card.URL == "" {
		f.card.URL = f.srv.URL + "/a2a"
	}
	return f
}

func (f *fakeA2AServer) checkAuth(w http.ResponseWriter, r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	f.mu.Lock()
	f.lastAuth = auth
	f.lastVersion = r.Header.Get(A2AVersionHeader)
	f.mu.Unlock()
	if f.requireToken != "" && auth != "Bearer "+f.requireToken {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

func (f *fakeA2AServer) handleCard(w http.ResponseWriter, r *http.Request) {
	f.cardHits.Add(1)
	if !f.checkAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(f.card)
}

func (f *fakeA2AServer) handleRPC(w http.ResponseWriter, r *http.Request) {
	if !f.checkAuth(w, r) {
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var (
		result any
		rpcErr *A2ARPCError
	)
	switch req.Method {
	case methodMessageSend:
		f.sendHits.Add(1)
		raw, _ := json.Marshal(req.Params)
		var p messageSendParams
		_ = json.Unmarshal(raw, &p)
		if p.Message != nil {
			f.recordMessage(*p.Message)
		}
		var msg A2AMessage
		if p.Message != nil {
			msg = *p.Message
		}
		result, rpcErr = f.onSend(req, msg)
	case methodMessageStream:
		f.streamHits.Add(1)
		raw, _ := json.Marshal(req.Params)
		var p messageSendParams
		_ = json.Unmarshal(raw, &p)
		var msg A2AMessage
		if p.Message != nil {
			f.recordMessage(*p.Message)
			msg = *p.Message
		}
		if f.onStream != nil {
			f.onStream(w, r, req, msg)
		}
		return
	case methodTasksGet:
		n := f.getHits.Add(1)
		raw, _ := json.Marshal(req.Params)
		var p taskQueryParams
		_ = json.Unmarshal(raw, &p)
		if f.onGet != nil {
			result, rpcErr = f.onGet(p.ID, n)
		}
	case methodTasksCancel:
		f.cancelHits.Add(1)
		raw, _ := json.Marshal(req.Params)
		var p taskIDParams
		_ = json.Unmarshal(raw, &p)
		f.mu.Lock()
		f.lastCanceled = p.ID
		f.mu.Unlock()
		result = A2ATask{Kind: "task", ID: p.ID, Status: A2ATaskStatus{State: A2ATaskStateCanceled}}
	default:
		rpcErr = &A2ARPCError{Code: -32601, Message: "method not found"}
	}

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeA2AServer) recordMessage(m A2AMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sid, ok := m.Metadata["skillId"].(string); ok {
		f.lastSkillID = sid
	}
	f.lastGoalText = textOfA2AParts(m.Parts)
}

func writeSSEFrame(w http.ResponseWriter, id int64, result any, rpcErr *A2ARPCError) {
	env := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		env["error"] = rpcErr
	} else {
		env["result"] = result
	}
	b, _ := json.Marshal(env)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}

func TestA2AConfig_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  A2AConfig
		want string
	}{
		{"missing name", A2AConfig{Endpoint: "https://x/a2a"}, "missing required field name"},
		{"invalid name", A2AConfig{Name: "bad.name", Endpoint: "https://x/a2a"}, "must not contain"},
		{"no url or endpoint", A2AConfig{Name: "x"}, "one of agent_card_url or endpoint"},
		{"unsupported auth", A2AConfig{Name: "x", Endpoint: "https://x/a2a", Auth: &A2AAuthConfig{Type: "oauth"}}, "not supported"},
		{"bearer without token_env", A2AConfig{Name: "x", Endpoint: "https://x/a2a", Auth: &A2AAuthConfig{Type: A2AAuthTypeBearer}}, "token_env is required"},
		{"negative timeout", A2AConfig{Name: "x", Endpoint: "https://x/a2a", TimeoutSeconds: -1}, "timeout_seconds must be >= 0"},
		{"default skill outside allowlist", A2AConfig{Name: "x", Endpoint: "https://x/a2a", Skill: "other", Skills: []string{"triage"}}, "not in skills allowlist"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestA2ASend_InjectsTraceparent(t *testing.T) {
	prev := otel.GetTextMapPropagator()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTextMapPropagator(prev) })

	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "parent-op")
	defer span.End()
	wantTraceID := span.SpanContext().TraceID().String()

	var gotTraceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("Traceparent")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": A2AMessage{Kind: "message", MessageID: "m1", Role: "agent", Parts: []A2APart{{Kind: "text", Text: "ok"}}},
		})
	}))
	t.Cleanup(srv.Close)

	c, err := NewA2AClient(A2AConfig{Name: "traced", Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("NewA2AClient: %v", err)
	}
	if _, err := c.Send(ctx, "s", nil, 0); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !strings.Contains(gotTraceparent, wantTraceID) {
		t.Errorf("Traceparent header = %q, want trace ID %s", gotTraceparent, wantTraceID)
	}
}

func TestA2ASend_DirectMessageReply(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.onSend = func(_ rpcRequest, msg A2AMessage) (any, *A2ARPCError) {
			in := msg.Parts[0].Data["service"]
			return A2AMessage{
				Kind:      "message",
				MessageID: "reply-1",
				Role:      "agent",
				Parts: []A2APart{
					{Kind: "text", Text: fmt.Sprintf("triaged %v", in)},
					{Kind: "data", Data: map[string]any{"root_cause": "bad deploy"}},
				},
			}, nil
		}
	})

	c, err := NewA2AClient(A2AConfig{
		Name:         "triage",
		AgentCardURL: f.srv.URL,
		Skills:       []string{"investigate"},
	})
	if err != nil {
		t.Fatalf("NewA2AClient: %v", err)
	}
	res, err := c.Send(context.Background(), "investigate", map[string]any{"service": "checkout"}, 0)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.State != "completed" || res.Text != "triaged checkout" || res.Output["root_cause"] != "bad deploy" {
		t.Errorf("Result = %+v", res)
	}
	if f.lastVersion != A2AProtocolVersion {
		t.Errorf("A2A-Version header = %q, want %q", f.lastVersion, A2AProtocolVersion)
	}
	if f.lastSkillID != "investigate" {
		t.Errorf("skillId metadata = %q, want investigate", f.lastSkillID)
	}
	if f.getHits.Load() != 0 {
		t.Errorf("tasks/get called %d times on direct message reply", f.getHits.Load())
	}
}

func TestA2ASend_TaskPollsToCompletion(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.onSend = func(rpcRequest, A2AMessage) (any, *A2ARPCError) {
			return A2ATask{
				Kind:   "task",
				ID:     "task-42",
				Status: A2ATaskStatus{State: A2ATaskStateSubmitted},
			}, nil
		}
		f.onGet = func(id string, n int32) (any, *A2ARPCError) {
			if n < 3 {
				return A2ATask{Kind: "task", ID: id, Status: A2ATaskStatus{State: A2ATaskStateWorking}}, nil
			}
			return A2ATask{
				Kind: "task",
				ID:   id,
				Status: A2ATaskStatus{
					State:   A2ATaskStateCompleted,
					Message: &A2AMessage{Kind: "message", MessageID: "m", Role: "agent", Parts: []A2APart{{Kind: "text", Text: "status note"}}},
				},
				Artifacts: []A2AArtifact{{
					ArtifactID: "a1",
					Parts: []A2APart{
						{Kind: "text", Text: "artifact body"},
						{Kind: "data", Data: map[string]any{"Confidence": 0.9}},
					},
				}},
			}, nil
		}
	})

	c, err := NewA2AClient(
		A2AConfig{Name: "triage", AgentCardURL: f.srv.URL},
		WithA2APollInterval(5*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewA2AClient: %v", err)
	}
	res, err := c.Send(context.Background(), "investigate", map[string]any{"q": 1}, 0)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.RemoteID != "task-42" || res.State != "completed" {
		t.Errorf("res = %+v", res)
	}
	if res.Text != "artifact body\nstatus note" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.Output["Confidence"] != 0.9 {
		t.Errorf("Output = %v", res.Output)
	}
	if f.getHits.Load() != 3 {
		t.Errorf("tasks/get hits = %d, want 3", f.getHits.Load())
	}
}

func TestA2ASend_ContextCancelSendsTasksCancel(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.onSend = func(rpcRequest, A2AMessage) (any, *A2ARPCError) {
			return A2ATask{Kind: "task", ID: "task-cancel-me", Status: A2ATaskStatus{State: A2ATaskStateWorking}}, nil
		}
		f.onGet = func(id string, _ int32) (any, *A2ARPCError) {
			return A2ATask{Kind: "task", ID: id, Status: A2ATaskStatus{State: A2ATaskStateWorking}}, nil
		}
	})

	c, _ := NewA2AClient(
		A2AConfig{Name: "triage", AgentCardURL: f.srv.URL},
		WithA2APollInterval(10*time.Millisecond),
	)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(25 * time.Millisecond)
		cancel()
	}()
	_, err := c.Send(ctx, "investigate", nil, 5*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Send err = %v, want context.Canceled", err)
	}
	if f.cancelHits.Load() != 1 || f.lastCanceled != "task-cancel-me" {
		t.Errorf("cancelHits=%d lastCanceled=%q, want 1 / task-cancel-me", f.cancelHits.Load(), f.lastCanceled)
	}
}

func TestA2ASend_TimeoutSendsTasksCancel(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.onSend = func(rpcRequest, A2AMessage) (any, *A2ARPCError) {
			return A2ATask{Kind: "task", ID: "task-slow", Status: A2ATaskStatus{State: A2ATaskStateWorking}}, nil
		}
		f.onGet = func(id string, _ int32) (any, *A2ARPCError) {
			return A2ATask{Kind: "task", ID: id, Status: A2ATaskStatus{State: A2ATaskStateWorking}}, nil
		}
	})

	c, _ := NewA2AClient(
		A2AConfig{Name: "triage", AgentCardURL: f.srv.URL},
		WithA2APollInterval(10*time.Millisecond),
	)
	_, err := c.Send(context.Background(), "investigate", nil, 30*time.Millisecond)
	if !errors.Is(err, ErrRemoteTimeout) {
		t.Fatalf("Send err = %v, want ErrRemoteTimeout", err)
	}
	if f.cancelHits.Load() != 1 || f.lastCanceled != "task-slow" {
		t.Errorf("cancelHits=%d lastCanceled=%q", f.cancelHits.Load(), f.lastCanceled)
	}
}

func TestA2ASend_TaskFailed(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.onSend = func(rpcRequest, A2AMessage) (any, *A2ARPCError) {
			return A2ATask{
				Kind: "task",
				ID:   "task-boom",
				Status: A2ATaskStatus{
					State:   A2ATaskStateFailed,
					Message: &A2AMessage{Kind: "message", MessageID: "m", Role: "agent", Parts: []A2APart{{Kind: "text", Text: "upstream 500"}}},
				},
			}, nil
		}
	})
	c, _ := NewA2AClient(A2AConfig{Name: "triage", AgentCardURL: f.srv.URL})
	_, err := c.Send(context.Background(), "investigate", nil, 0)
	if !errors.Is(err, ErrRemoteFailed) || !strings.Contains(err.Error(), "upstream 500") {
		t.Fatalf("Send err = %v, want ErrRemoteFailed with detail", err)
	}
}

func TestA2ASend_InputRequiredCancelsAndFails(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.onSend = func(rpcRequest, A2AMessage) (any, *A2ARPCError) {
			return A2ATask{Kind: "task", ID: "task-hitl", Status: A2ATaskStatus{State: A2ATaskStateInputRequired}}, nil
		}
	})
	c, _ := NewA2AClient(A2AConfig{Name: "triage", AgentCardURL: f.srv.URL})
	_, err := c.Send(context.Background(), "investigate", nil, 0)
	if !errors.Is(err, ErrRemoteFailed) {
		t.Fatalf("Send err = %v, want ErrRemoteFailed", err)
	}
	if f.cancelHits.Load() != 1 {
		t.Errorf("expected tasks/cancel on input-required, got %d", f.cancelHits.Load())
	}
}

func TestA2ASend_BearerAuthFromEnv(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.requireToken = "s3cr3t"
		f.onSend = func(rpcRequest, A2AMessage) (any, *A2ARPCError) {
			return A2AMessage{Kind: "message", MessageID: "m", Role: "agent", Parts: []A2APart{{Kind: "text", Text: "ok"}}}, nil
		}
	})
	cfg := A2AConfig{
		Name:         "triage",
		AgentCardURL: f.srv.URL,
		Auth:         &A2AAuthConfig{Type: "bearer", TokenEnv: "TEST_A2A_TOKEN"},
	}

	t.Setenv("TEST_A2A_TOKEN", "s3cr3t")
	c, _ := NewA2AClient(cfg)
	if _, err := c.Send(context.Background(), "investigate", nil, 0); err != nil {
		t.Fatalf("Send with valid token: %v", err)
	}

	t.Setenv("TEST_A2A_TOKEN", "wrong")
	c2, _ := NewA2AClient(cfg)
	if _, err := c2.Send(context.Background(), "investigate", nil, 0); !errors.Is(err, ErrRemoteAuthFailed) {
		t.Fatalf("Send with wrong token = %v, want ErrRemoteAuthFailed", err)
	}

	t.Setenv("TEST_A2A_TOKEN", "")
	if _, err := c2.Send(context.Background(), "investigate", nil, 0); !errors.Is(err, ErrRemoteAuthFailed) {
		t.Fatalf("Send with unset env = %v, want ErrRemoteAuthFailed", err)
	}
	if _, err := c2.Spawn(context.Background(), RemoteAgentSpec{Goal: "g"}); !errors.Is(err, ErrRemoteAuthFailed) {
		t.Fatalf("Spawn with unset env = %v, want ErrRemoteAuthFailed", err)
	}
}

func TestA2ACard_FetchedOnceAndCached(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.onSend = func(rpcRequest, A2AMessage) (any, *A2ARPCError) {
			return A2AMessage{Kind: "message", MessageID: "m", Role: "agent", Parts: []A2APart{{Kind: "text", Text: "ok"}}}, nil
		}
	})
	c, _ := NewA2AClient(A2AConfig{Name: "triage", AgentCardURL: f.srv.URL})
	for i := 0; i < 3; i++ {
		if _, err := c.Send(context.Background(), "investigate", nil, 0); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}
	if f.cardHits.Load() != 1 {
		t.Errorf("card fetched %d times across 3 sends, want 1", f.cardHits.Load())
	}
}

func TestA2ACard_SkillValidationAndAllowlist(t *testing.T) {
	f := newFakeA2A(t, nil)
	c, _ := NewA2AClient(A2AConfig{
		Name:         "triage",
		AgentCardURL: f.srv.URL,
		Skills:       []string{"investigate"},
	})
	if _, err := c.Send(context.Background(), "summarize", nil, 0); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("disallowed skill err = %v, want allowlist error", err)
	}
	c2, _ := NewA2AClient(A2AConfig{Name: "triage", AgentCardURL: f.srv.URL})
	if _, err := c2.Send(context.Background(), "nonexistent", nil, 0); err == nil || !strings.Contains(err.Error(), "not present in agent card") {
		t.Fatalf("unknown skill err = %v, want card error", err)
	}
}

func TestA2ASend_EndpointOnlySkipsCard(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.onSend = func(rpcRequest, A2AMessage) (any, *A2ARPCError) {
			return A2AMessage{Kind: "message", MessageID: "m", Role: "agent", Parts: []A2APart{{Kind: "text", Text: "direct"}}}, nil
		}
	})
	c, _ := NewA2AClient(A2AConfig{Name: "triage", Endpoint: f.srv.URL + "/a2a"})
	res, err := c.Send(context.Background(), "any-skill", nil, 0)
	if err != nil || res.Text != "direct" {
		t.Fatalf("Send = (%+v, %v)", res, err)
	}
	if f.cardHits.Load() != 0 {
		t.Errorf("card fetched %d times with explicit endpoint", f.cardHits.Load())
	}
}

func TestA2ASend_NonJSONRPCTransportRejected(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.card.PreferredTransport = "GRPC"
	})
	c, _ := NewA2AClient(A2AConfig{Name: "triage", AgentCardURL: f.srv.URL})
	_, err := c.Send(context.Background(), "investigate", nil, 0)
	if !errors.Is(err, ErrRemoteProtocolMismatch) {
		t.Fatalf("err = %v, want ErrRemoteProtocolMismatch", err)
	}
}

func TestA2ASend_Unreachable(t *testing.T) {
	c, _ := NewA2AClient(A2AConfig{Name: "dead", Endpoint: "http://127.0.0.1:1/a2a"})
	_, err := c.Send(context.Background(), "s", nil, time.Second)
	if !errors.Is(err, ErrRemoteUnreachable) {
		t.Fatalf("err = %v, want ErrRemoteUnreachable", err)
	}
}

func TestA2ASpawner_StreamingCardUsesMessageStream(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.card.Capabilities.Streaming = true
		f.onStream = func(w http.ResponseWriter, _ *http.Request, req rpcRequest, _ A2AMessage) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			writeSSEFrame(w, req.ID, A2ATaskStatusUpdateEvent{
				Kind:   "status-update",
				TaskID: "stream-task-1",
				Status: A2ATaskStatus{
					State:   A2ATaskStateWorking,
					Message: &A2AMessage{Kind: "message", MessageID: "m1", Role: "agent", Parts: []A2APart{{Kind: "text", Text: "scanning logs"}}},
				},
			}, nil)
			writeSSEFrame(w, req.ID, A2ATaskArtifactUpdateEvent{
				Kind:   "artifact-update",
				TaskID: "stream-task-1",
				Artifact: A2AArtifact{
					ArtifactID: "art-1",
					Parts:      []A2APart{{Kind: "text", Text: "RCA: OOMKilled in payment-svc"}},
				},
				LastChunk: true,
			}, nil)
			writeSSEFrame(w, req.ID, A2ATaskStatusUpdateEvent{
				Kind:   "status-update",
				TaskID: "stream-task-1",
				Final:  true,
				Status: A2ATaskStatus{
					State: A2ATaskStateCompleted,
				},
			}, nil)
		}
	})

	spawner, err := NewA2AClient(A2AConfig{
		Name:         "incident-triage",
		AgentCardURL: f.srv.URL,
		Skill:        "investigate",
	})
	if err != nil {
		t.Fatalf("NewA2AClient: %v", err)
	}
	h, err := spawner.Spawn(context.Background(), RemoteAgentSpec{
		ID:   "incident-triage-1",
		Name: "incident-triage-1",
		Goal: "investigate checkout 5xx spike",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	var events []RemoteAgentEvent
	for ev := range h.Events() {
		events = append(events, ev)
	}
	if f.streamHits.Load() != 1 || f.sendHits.Load() != 0 {
		t.Fatalf("streamHits=%d sendHits=%d, want 1 / 0", f.streamHits.Load(), f.sendHits.Load())
	}
	if f.lastSkillID != "investigate" || f.lastGoalText != "investigate checkout 5xx spike" {
		t.Errorf("lastSkillID=%q lastGoalText=%q", f.lastSkillID, f.lastGoalText)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v, want 2 (1 alert + 1 completed)", events)
	}
	if events[0].Kind != "alert" || events[0].Text != "scanning logs" {
		t.Errorf("events[0] = %+v, want alert 'scanning logs'", events[0])
	}
	if events[1].Kind != "completed" || events[1].Text != "RCA: OOMKilled in payment-svc" {
		t.Errorf("events[1] = %+v, want completed 'RCA: OOMKilled in payment-svc'", events[1])
	}
	if st, _ := h.Status(context.Background()); st != RemoteStatusCompleted {
		t.Errorf("Status = %v, want RemoteStatusCompleted", st)
	}
}

func TestA2ASpawner_StopCancelsRemoteStreamTask(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.card.Capabilities.Streaming = true
		f.onStream = func(w http.ResponseWriter, r *http.Request, req rpcRequest, _ A2AMessage) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			writeSSEFrame(w, req.ID, A2ATaskStatusUpdateEvent{
				Kind:   "status-update",
				TaskID: "stream-stop-1",
				Status: A2ATaskStatus{
					State:   A2ATaskStateWorking,
					Message: &A2AMessage{Kind: "message", MessageID: "m1", Role: "agent", Parts: []A2APart{{Kind: "text", Text: "working"}}},
				},
			}, nil)
			<-r.Context().Done()
		}
	})

	spawner, err := NewA2AClient(A2AConfig{Name: "triage", AgentCardURL: f.srv.URL})
	if err != nil {
		t.Fatalf("NewA2AClient: %v", err)
	}
	h, err := spawner.Spawn(context.Background(), RemoteAgentSpec{ID: "triage-1", Name: "triage-1", Goal: "work"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	first := <-h.Events()
	if first.Kind != "alert" || first.Text != "working" {
		t.Fatalf("first event = %+v, want alert 'working'", first)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	var last RemoteAgentEvent
	for ev := range h.Events() {
		last = ev
	}
	if last.Kind != "stopped" {
		t.Errorf("last event = %+v, want Kind=stopped", last)
	}
	if f.cancelHits.Load() != 1 || f.lastCanceled != "stream-stop-1" {
		t.Errorf("cancelHits=%d lastCanceled=%q, want 1 / stream-stop-1", f.cancelHits.Load(), f.lastCanceled)
	}
}

func TestA2ASpawner_WallclockBudgetCancelsRemoteTask(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.onSend = func(rpcRequest, A2AMessage) (any, *A2ARPCError) {
			return A2ATask{Kind: "task", ID: "budget-task-1", Status: A2ATaskStatus{State: A2ATaskStateWorking}}, nil
		}
		f.onGet = func(id string, _ int32) (any, *A2ARPCError) {
			return A2ATask{Kind: "task", ID: id, Status: A2ATaskStatus{State: A2ATaskStateWorking}}, nil
		}
	})

	spawner, err := NewA2AClient(
		A2AConfig{Name: "triage", AgentCardURL: f.srv.URL},
		WithA2APollInterval(10*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("NewA2AClient: %v", err)
	}
	h, err := spawner.Spawn(context.Background(), RemoteAgentSpec{
		ID:      "triage-1",
		Name:    "triage-1",
		Goal:    "long task",
		Budgets: Budgets{MaxWallclock: 35 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	var last RemoteAgentEvent
	for ev := range h.Events() {
		last = ev
	}
	if last.Kind != "failed" || !strings.Contains(last.Text, ErrRemoteTimeout.Error()) {
		t.Errorf("last event = %+v, want failed with timeout", last)
	}
	if f.cancelHits.Load() != 1 || f.lastCanceled != "budget-task-1" {
		t.Errorf("cancelHits=%d lastCanceled=%q, want 1 / budget-task-1", f.cancelHits.Load(), f.lastCanceled)
	}
}

func TestSpawnAgentTool_RemoteA2ATemplate_SyncAndAsync(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.card.Capabilities.Streaming = true
		f.onStream = func(w http.ResponseWriter, _ *http.Request, req rpcRequest, msg A2AMessage) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			goal := textOfA2AParts(msg.Parts)
			writeSSEFrame(w, req.ID, A2ATaskArtifactUpdateEvent{
				Kind:   "artifact-update",
				TaskID: "remote-task",
				Artifact: A2AArtifact{
					ArtifactID: "a1",
					Parts:      []A2APart{{Kind: "text", Text: "remote diagnosis for: " + goal}},
				},
				LastChunk: true,
			}, nil)
			writeSSEFrame(w, req.ID, A2ATaskStatusUpdateEvent{
				Kind:   "status-update",
				TaskID: "remote-task",
				Final:  true,
				Status: A2ATaskStatus{State: A2ATaskStateCompleted},
			}, nil)
		}
	})

	spawner, err := NewA2AClient(A2AConfig{
		Name:         "incident-triage",
		AgentCardURL: f.srv.URL,
		Skill:        "investigate",
	})
	if err != nil {
		t.Fatalf("NewA2AClient: %v", err)
	}

	prov := &recordingProvider{llm: &stopRaceLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:        "incident-triage",
		Description: "Remote A2A incident triage specialist",
		Remote:      spawner,
	}})
	attachEchoParent(t, mgr)
	defer mgr.Close()

	// Verify Catalog reports the remote template.
	cat := mgr.Catalog()
	if len(cat) != 1 || cat[0].Name != "incident-triage" || cat[0].Model != "remote (a2a)" {
		t.Fatalf("Catalog() = %+v", cat)
	}

	// 1. Synchronous delegation via SpawnTemplate + awaitResult (wait: true).
	hSync, err := mgr.SpawnTemplate(context.Background(), "", "incident-triage", RefOverrides{Goal: "checkout latency"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate (sync): %v", err)
	}
	resSync := mgr.awaitResult(context.Background(), hSync)
	if resSync.Status != "completed" || resSync.Output != "remote diagnosis for: checkout latency" || resSync.StopReason != StopNatural {
		t.Errorf("awaitResult = %+v", resSync)
	}

	// 2. Asynchronous delegation via SpawnTemplate (wait: false) pushes completion alert.
	hAsync, err := mgr.SpawnTemplate(context.Background(), "", "incident-triage", RefOverrides{Goal: "cart errors"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate (async): %v", err)
	}
	waitDone(t, hAsync)
	select {
	case a := <-mgr.Alerts():
		if a.From != "incident-triage-2" || a.Kind != "completed" || a.Text != "remote diagnosis for: cart errors" {
			t.Errorf("unexpected alert: %+v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("expected completion alert from async remote subagent")
	}
	if got := hAsync.LastReport(); got != "remote diagnosis for: cart errors" {
		t.Errorf("LastReport() = %q", got)
	}
}

func TestA2ASpawner_PrematureStreamEOFFails(t *testing.T) {
	f := newFakeA2A(t, func(f *fakeA2AServer) {
		f.card.Capabilities.Streaming = true
		f.onStream = func(w http.ResponseWriter, _ *http.Request, req rpcRequest, _ A2AMessage) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			writeSSEFrame(w, req.ID, A2ATaskStatusUpdateEvent{
				Kind:   "status-update",
				TaskID: "stream-eof-1",
				Status: A2ATaskStatus{State: A2ATaskStateWorking},
			}, nil)
			// Close stream without a terminal event.
		}
	})

	spawner, err := NewA2AClient(A2AConfig{Name: "triage", AgentCardURL: f.srv.URL})
	if err != nil {
		t.Fatalf("NewA2AClient: %v", err)
	}
	h, err := spawner.Spawn(context.Background(), RemoteAgentSpec{ID: "triage-1", Name: "triage-1", Goal: "work"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	var last RemoteAgentEvent
	for ev := range h.Events() {
		last = ev
	}
	if last.Kind != "failed" || !strings.Contains(last.Text, "stream closed before task") {
		t.Errorf("last event = %+v, want failed with premature stream close", last)
	}
	if st, _ := h.Status(context.Background()); st != RemoteStatusFailed {
		t.Errorf("Status = %v, want RemoteStatusFailed", st)
	}
}

// A card that names a JSON-RPC endpoint on a different host is refused
// by default: the bearer token goes to whichever endpoint is chosen,
// and the operator configured the card's host, not the card's content
// (#1435). The opt-in permits it; the second server must then see the
// token, which is exactly why it is opt-in.
func TestA2ACard_EndpointOnOtherHostRefusedUnlessAllowed(t *testing.T) {
	t.Setenv("A2A_TEST_TOKEN", "s3cret")
	other := newFakeA2A(t, func(f *fakeA2AServer) {
		f.onSend = func(_ rpcRequest, _ A2AMessage) (any, *A2ARPCError) {
			return A2AMessage{Kind: "message", MessageID: "r", Role: "agent",
				Parts: []A2APart{{Kind: "text", Text: "from other host"}}}, nil
		}
	})
	// The card server hands out the other server's endpoint. Both are
	// 127.0.0.1 but on different ports, which is a different host:port.
	cardOnly := newFakeA2A(t, func(f *fakeA2AServer) {
		f.card.URL = other.srv.URL + "/a2a"
	})
	auth := &A2AAuthConfig{Type: A2AAuthTypeBearer, TokenEnv: "A2A_TEST_TOKEN"}

	c, err := NewA2AClient(A2AConfig{Name: "triage", AgentCardURL: cardOnly.srv.URL, Auth: auth})
	if err != nil {
		t.Fatalf("NewA2AClient: %v", err)
	}
	_, err = c.Send(context.Background(), "investigate", nil, 0)
	if !errors.Is(err, ErrRemoteEndpointUntrusted) {
		t.Fatalf("Send with a card pointing at another host: err = %v, want ErrRemoteEndpointUntrusted", err)
	}
	if other.sendHits.Load() != 0 {
		t.Fatalf("the other host received %d message/send calls; none may be sent", other.sendHits.Load())
	}
	other.mu.Lock()
	leaked := other.lastAuth
	other.mu.Unlock()
	if leaked != "" {
		t.Fatalf("the other host saw Authorization %q; the token must not leave the configured host", leaked)
	}

	allowed, err := NewA2AClient(A2AConfig{Name: "triage", AgentCardURL: cardOnly.srv.URL, Auth: auth, AllowCardHostChange: true})
	if err != nil {
		t.Fatalf("NewA2AClient: %v", err)
	}
	res, err := allowed.Send(context.Background(), "investigate", nil, 0)
	if err != nil {
		t.Fatalf("Send with allow_card_host_change: %v", err)
	}
	if res.Text != "from other host" {
		t.Fatalf("Result.Text = %q, want the opted-in host's reply", res.Text)
	}
}

// Redirects are not followed, on the card GET or on a JSON-RPC POST.
// Go keeps Authorization across same-host and subdomain redirects, so
// following one would carry the token to a URL the operator never
// configured (#1435).
func TestA2AClient_DoesNotFollowRedirects(t *testing.T) {
	t.Setenv("A2A_TEST_TOKEN", "s3cret")
	target := newFakeA2A(t, func(f *fakeA2AServer) {
		f.onSend = func(_ rpcRequest, _ A2AMessage) (any, *A2ARPCError) {
			return A2AMessage{Kind: "message", MessageID: "r", Role: "agent",
				Parts: []A2APart{{Kind: "text", Text: "redirected"}}}, nil
		}
	})
	var cardHits, rpcHits atomic.Int32
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case A2AWellKnownCardPath:
			cardHits.Add(1)
			http.Redirect(w, r, target.srv.URL+A2AWellKnownCardPath, http.StatusFound)
		case "/a2a":
			rpcHits.Add(1)
			http.Redirect(w, r, target.srv.URL+"/a2a", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(redirector.Close)
	auth := &A2AAuthConfig{Type: A2AAuthTypeBearer, TokenEnv: "A2A_TEST_TOKEN"}

	// Card GET redirected: surfaces as a non-200, nothing fetched from target.
	c, err := NewA2AClient(A2AConfig{Name: "triage", AgentCardURL: redirector.URL, Auth: auth})
	if err != nil {
		t.Fatalf("NewA2AClient: %v", err)
	}
	if _, err := c.Card(context.Background()); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("Card through a 302: err = %v, want an HTTP 302 error, not a followed redirect", err)
	}
	if target.cardHits.Load() != 0 {
		t.Fatalf("redirect target served %d card fetches; the redirect must not be followed", target.cardHits.Load())
	}

	// RPC POST redirected: same.
	ep, err := NewA2AClient(A2AConfig{Name: "triage", Endpoint: redirector.URL + "/a2a", Auth: auth})
	if err != nil {
		t.Fatalf("NewA2AClient: %v", err)
	}
	if _, err := ep.Send(context.Background(), "investigate", nil, 0); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("Send through a 302: err = %v, want an HTTP 302 error", err)
	}
	if target.sendHits.Load() != 0 {
		t.Fatalf("redirect target served %d message/send calls", target.sendHits.Load())
	}
	target.mu.Lock()
	leaked := target.lastAuth
	target.mu.Unlock()
	if leaked != "" {
		t.Fatalf("redirect target saw Authorization %q", leaked)
	}
	if cardHits.Load() == 0 || rpcHits.Load() == 0 {
		t.Fatalf("redirector was not reached (card=%d rpc=%d); the test did not exercise the redirect", cardHits.Load(), rpcHits.Load())
	}
}

func TestTrustCardEndpoint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, card, ep string
		allow          bool
		wantErr        bool
	}{
		{"same host", "https://a.example/.well-known/agent-card.json", "https://a.example/rpc", false, false},
		{"same host, case", "https://A.Example", "https://a.example/rpc", false, false},
		{"other host refused", "https://a.example", "https://b.example/rpc", false, true},
		{"other host allowed", "https://a.example", "https://b.example/rpc", true, false},
		{"other port is other host", "http://127.0.0.1:1000", "http://127.0.0.1:2000/rpc", false, true},
		{"downgrade refused even when allowed", "https://a.example", "http://a.example/rpc", true, true},
		{"http card may name http", "http://a.example", "http://a.example/rpc", false, false},
		{"http card may upgrade", "http://a.example", "https://a.example/rpc", false, false},
		{"bad scheme", "https://a.example", "ftp://a.example/rpc", true, true},
		{"no host", "https://a.example", "https:///rpc", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := trustCardEndpoint(tc.card, tc.ep, tc.allow)
			if (err != nil) != tc.wantErr {
				t.Fatalf("trustCardEndpoint(%q, %q, %v) = %v, wantErr %v", tc.card, tc.ep, tc.allow, err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrRemoteEndpointUntrusted) {
				t.Fatalf("err %v is not ErrRemoteEndpointUntrusted", err)
			}
		})
	}
}
