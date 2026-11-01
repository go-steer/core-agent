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

// Originally derived from go-steer/mast@d2f45c7 internal/a2a/server_test.go (#1407).

package attach

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/auth"
)

type fakeA2ABackend struct {
	mu       sync.Mutex
	tasks    map[string]A2ATaskOutcome
	lastReq  A2ATaskRequest
	sendFunc func(ctx context.Context, req A2ATaskRequest) (A2ATaskOutcome, error)
}

func newFakeA2ABackend() *fakeA2ABackend {
	return &fakeA2ABackend{
		tasks: make(map[string]A2ATaskOutcome),
	}
}

func (f *fakeA2ABackend) PreflightTask(_ context.Context, req A2ATaskRequest) error {
	if req.TaskID == "unknown-task" {
		return errors.New("task \"unknown-task\" not found")
	}
	if req.TaskID == "forbidden-task" {
		return ErrA2AForbidden
	}
	return nil
}

func (f *fakeA2ABackend) SendMessage(ctx context.Context, req A2ATaskRequest) (A2ATaskOutcome, error) {
	f.mu.Lock()
	f.lastReq = req
	fn := f.sendFunc
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx, req)
	}
	if req.OnProgress != nil {
		req.OnProgress(TaskStateWorking, "analyzing cluster signals")
	}
	taskID := req.TaskID
	if taskID == "" {
		taskID = "task-1"
	}
	ctxID := req.ContextID
	if ctxID == "" {
		ctxID = "ctx-1"
	}
	out := A2ATaskOutcome{
		TaskID:    taskID,
		ContextID: ctxID,
		State:     TaskStateCompleted,
		Output:    "resolved: " + req.Text,
		Data:      req.Data,
	}
	f.mu.Lock()
	f.tasks[taskID] = out
	f.mu.Unlock()
	return out, nil
}

func (f *fakeA2ABackend) GetTask(_ context.Context, id string) (A2ATaskOutcome, bool, error) {
	if id == "forbidden-task" {
		return A2ATaskOutcome{}, false, ErrA2AForbidden
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out, ok := f.tasks[id]
	return out, ok, nil
}

func (f *fakeA2ABackend) CancelTask(_ context.Context, id string) (A2ATaskOutcome, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out, ok := f.tasks[id]
	if !ok {
		return A2ATaskOutcome{}, false, nil
	}
	out.State = TaskStateCanceled
	f.tasks[id] = out
	return out, true, nil
}

func startTestA2AServer(t *testing.T, opts Options) (string, *Server) {
	t.Helper()
	if opts.Registry == nil {
		opts.Registry = NewSessionRegistry()
	}
	if opts.Addr == "" {
		opts.Addr = "127.0.0.1:0"
	}
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Bind(); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	go func() {
		_ = srv.Serve()
	}()
	t.Cleanup(func() {
		_ = srv.Close()
	})
	return "http://" + srv.Addr(), srv
}

func postA2A(t *testing.T, baseURL string, body any, headers map[string]string) (*http.Response, jsonrpcResponse) {
	t.Helper()
	var raw []byte
	switch v := body.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+A2APath, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	var rpcResp jsonrpcResponse
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(resp.Body).Decode(&rpcResp)
		_ = resp.Body.Close()
	}
	return resp, rpcResp
}

func TestA2A_UnmountedWhenExposeFalse(t *testing.T) {
	t.Parallel()
	baseURL, _ := startTestA2AServer(t, Options{
		A2A: A2AOptions{Expose: false, Backend: newFakeA2ABackend()},
	})
	resp, _ := postA2A(t, baseURL, map[string]any{"jsonrpc": "2.0", "id": 1, "method": A2AMethodMessageSend}, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when A2A.Expose is false", resp.StatusCode)
	}
}

func TestA2A_JSONRPCFramingAndVersionHeader(t *testing.T) {
	t.Parallel()
	backend := newFakeA2ABackend()
	baseURL, _ := startTestA2AServer(t, Options{
		A2A: A2AOptions{
			Expose:    true,
			Backend:   backend,
			SkillName: "triage",
		},
	})

	// Unsupported A2A-Version header -> 400 + -32600.
	resp, rpc := postA2A(t, baseURL, map[string]any{"jsonrpc": "2.0", "id": 1, "method": A2AMethodMessageSend}, map[string]string{
		A2AVersionHeader: "9.9",
	})
	if resp.StatusCode != http.StatusBadRequest || rpc.Error == nil || rpc.Error.Code != JSONRPCInvalidRequest {
		t.Fatalf("bad version header: status=%d err=%+v", resp.StatusCode, rpc.Error)
	}
	if got := resp.Header.Get(A2AVersionHeader); got != A2AHeaderValue {
		t.Errorf("A2A-Version response header = %q, want %q", got, A2AHeaderValue)
	}

	// Malformed JSON -> -32700.
	_, rpc = postA2A(t, baseURL, "{not-json", nil)
	if rpc.Error == nil || rpc.Error.Code != JSONRPCParseError {
		t.Fatalf("malformed json: got %+v, want -32700", rpc.Error)
	}

	// Missing jsonrpc 2.0 -> -32600.
	_, rpc = postA2A(t, baseURL, map[string]any{"jsonrpc": "1.0", "id": 2, "method": A2AMethodMessageSend}, nil)
	if rpc.Error == nil || rpc.Error.Code != JSONRPCInvalidRequest {
		t.Fatalf("bad jsonrpc version: got %+v, want -32600", rpc.Error)
	}

	// Unknown method -> -32601.
	_, rpc = postA2A(t, baseURL, map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tasks/unknown"}, nil)
	if rpc.Error == nil || rpc.Error.Code != JSONRPCMethodNotFound {
		t.Fatalf("unknown method: got %+v, want -32601", rpc.Error)
	}
}

func TestA2A_MessageSendGetAndCancel(t *testing.T) {
	t.Parallel()
	backend := newFakeA2ABackend()
	baseURL, _ := startTestA2AServer(t, Options{
		A2A: A2AOptions{
			Expose:           true,
			Backend:          backend,
			SkillName:        "gke-triage",
			SkillDescription: "Diagnose GKE cluster issues",
		},
	})

	sendReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      "req-1",
		"method":  A2AMethodMessageSend,
		"params": map[string]any{
			"message": map[string]any{
				"messageId": "m-1",
				"role":      "user",
				"parts": []any{
					map[string]any{"kind": "text", "text": "check pod crashloop"},
				},
			},
		},
	}
	resp, rpc := postA2A(t, baseURL, sendReq, nil)
	if resp.StatusCode != http.StatusOK || rpc.Error != nil {
		t.Fatalf("message/send failed: status=%d err=%+v", resp.StatusCode, rpc.Error)
	}
	rawResult, _ := json.Marshal(rpc.Result)
	var task A2ATask
	if err := json.Unmarshal(rawResult, &task); err != nil {
		t.Fatalf("unmarshal task: %v", err)
	}
	if task.ID != "task-1" || task.Status.State != TaskStateCompleted {
		t.Fatalf("unexpected task: %+v", task)
	}
	if len(task.Artifacts) != 1 || len(task.Artifacts[0].Parts) != 1 || task.Artifacts[0].Parts[0].Text != "resolved: check pod crashloop" {
		t.Fatalf("unexpected artifacts: %+v", task.Artifacts)
	}

	// tasks/get returns the stored task.
	_, getRPC := postA2A(t, baseURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      "req-2",
		"method":  A2AMethodTasksGet,
		"params":  map[string]any{"id": "task-1"},
	}, nil)
	if getRPC.Error != nil {
		t.Fatalf("tasks/get failed: %+v", getRPC.Error)
	}

	// tasks/cancel transitions task to canceled.
	_, cancelRPC := postA2A(t, baseURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      "req-3",
		"method":  A2AMethodTasksCancel,
		"params":  map[string]any{"id": "task-1"},
	}, nil)
	if cancelRPC.Error != nil {
		t.Fatalf("tasks/cancel failed: %+v", cancelRPC.Error)
	}
	rawCanceled, _ := json.Marshal(cancelRPC.Result)
	var canceledTask A2ATask
	_ = json.Unmarshal(rawCanceled, &canceledTask)
	if canceledTask.Status.State != TaskStateCanceled {
		t.Fatalf("canceled task state = %q, want %q", canceledTask.Status.State, TaskStateCanceled)
	}
}

func TestA2A_MessageStreamSSEAndPreflightRefusal(t *testing.T) {
	t.Parallel()
	backend := newFakeA2ABackend()
	baseURL, _ := startTestA2AServer(t, Options{
		A2A: A2AOptions{
			Expose:    true,
			Backend:   backend,
			SkillName: "gke-triage",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cluster": map[string]any{"type": "string"},
				},
				"required": []any{"cluster"},
			},
		},
	})

	// 1. Schema refusal on message/stream happens BEFORE opening SSE stream.
	badStream := map[string]any{
		"jsonrpc": "2.0",
		"id":      "s-bad",
		"method":  A2AMethodMessageStream,
		"params": map[string]any{
			"message": map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"kind": "data", "data": map[string]any{"wrong": 1}}},
			},
		},
	}
	resp, rpc := postA2A(t, baseURL, badStream, nil)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("schema refusal must not open text/event-stream")
	}
	if rpc.Error == nil || rpc.Error.Code != JSONRPCInvalidParams {
		t.Fatalf("want -32602 on schema failure, got %+v", rpc.Error)
	}

	// 2. Unknown task refusal via A2APreflighter happens BEFORE opening SSE stream.
	unknownTaskStream := map[string]any{
		"jsonrpc": "2.0",
		"id":      "s-unknown",
		"method":  A2AMethodMessageStream,
		"params": map[string]any{
			"message": map[string]any{
				"taskId": "unknown-task",
				"role":   "user",
				"parts":  []any{map[string]any{"kind": "data", "data": map[string]any{"cluster": "prod"}}},
			},
		},
	}
	resp, rpc = postA2A(t, baseURL, unknownTaskStream, nil)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unknown task refusal must not open text/event-stream")
	}
	if rpc.Error == nil || rpc.Error.Code != JSONRPCServerError {
		t.Fatalf("want -32000 on unknown task preflight, got %+v", rpc.Error)
	}

	// 3. Valid message/stream emits status-update -> artifact-update (lastChunk:true) -> single final status-update.
	goodStream := map[string]any{
		"jsonrpc": "2.0",
		"id":      "s-good",
		"method":  A2AMethodMessageStream,
		"params": map[string]any{
			"message": map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"kind": "text", "text": `{"cluster":"prod"}`}},
			},
		},
	}
	resp, _ = postA2A(t, baseURL, goodStream, nil)
	defer func() { _ = resp.Body.Close() }()
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", resp.Header.Get("Content-Type"))
	}

	var kinds []string
	var finalCount int
	var sawLastChunk bool
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var env struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &env); err != nil {
			t.Fatalf("decode SSE frame: %v", err)
		}
		kind, _ := env.Result["kind"].(string)
		kinds = append(kinds, kind)
		if kind == "status-update" {
			if f, _ := env.Result["final"].(bool); f {
				finalCount++
			}
		}
		if kind == "artifact-update" {
			if lc, _ := env.Result["lastChunk"].(bool); lc {
				sawLastChunk = true
			}
		}
	}
	if finalCount != 1 {
		t.Errorf("final status-update count = %d, want 1 (kinds=%v)", finalCount, kinds)
	}
	if !sawLastChunk {
		t.Errorf("artifact-update missing lastChunk=true (kinds=%v)", kinds)
	}
}

type staticMapAuth map[string]auth.Caller

func (m staticMapAuth) Authenticate(r *http.Request) (auth.Caller, error) {
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if tok == "" {
		return auth.Caller{}, auth.ErrUnauthenticated
	}
	c, ok := m[tok]
	if !ok {
		return auth.Caller{}, auth.ErrUnauthenticated
	}
	return c, nil
}

func TestA2A_ScopesAndCostRateLimit(t *testing.T) {
	t.Parallel()
	backend := newFakeA2ABackend()
	authn := staticMapAuth{
		"tok-noscope": {Identity: "user:bob", Labels: map[string]string{"scopes": "other:read"}},
		"tok-scoped":  {Identity: "user:alice", Labels: map[string]string{"scopes": "triage:invoke,gke:read"}},
		"tok-admin":   {Identity: "user:admin", Admin: true},
	}
	baseURL, _ := startTestA2AServer(t, Options{
		Authenticator: authn,
		CostRateLimit: CostRateLimit{PerMinute: 1, Burst: 1},
		A2A: A2AOptions{
			Expose:    true,
			Backend:   backend,
			SkillName: "gke-triage",
			Auth: A2AAuthOptions{
				Required: true,
				Scopes:   []string{"triage:invoke"},
			},
		},
	})

	msgPayload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  A2AMethodMessageSend,
		"params": map[string]any{
			"message": map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"kind": "text", "text": "ping"}},
			},
		},
	}

	// 1. Unauthenticated -> 401.
	resp, _ := postA2A(t, baseURL, msgPayload, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	// 2. Authenticated without required scope -> 403.
	resp, rpc := postA2A(t, baseURL, msgPayload, map[string]string{"Authorization": "Bearer tok-noscope"})
	if resp.StatusCode != http.StatusForbidden || rpc.Error == nil || !strings.Contains(rpc.Error.Message, "insufficient_scope") {
		t.Fatalf("missing scope: status=%d err=%+v", resp.StatusCode, rpc.Error)
	}

	// 3. Authenticated with required scope -> 200 (consumes alice's burst=1 token).
	resp, rpc = postA2A(t, baseURL, msgPayload, map[string]string{"Authorization": "Bearer tok-scoped"})
	if resp.StatusCode != http.StatusOK || rpc.Error != nil {
		t.Fatalf("scoped call failed: status=%d err=%+v", resp.StatusCode, rpc.Error)
	}

	// 4. Second immediate message/send from alice -> 429 rate-limited with Retry-After.
	resp, rpc = postA2A(t, baseURL, msgPayload, map[string]string{"Authorization": "Bearer tok-scoped"})
	if resp.StatusCode != http.StatusTooManyRequests || rpc.Error == nil {
		t.Fatalf("expected 429 on second message/send, got status=%d err=%+v", resp.StatusCode, rpc.Error)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Errorf("missing Retry-After header on 429")
	}

	// 5. tasks/get and tasks/cancel for alice are NEVER rate-limited even while alice's cost bucket is exhausted!
	resp, rpc = postA2A(t, baseURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  A2AMethodTasksGet,
		"params":  map[string]any{"id": "task-1"},
	}, map[string]string{"Authorization": "Bearer tok-scoped"})
	if resp.StatusCode != http.StatusOK || rpc.Error != nil {
		t.Fatalf("tasks/get must not be rate-limited: status=%d err=%+v", resp.StatusCode, rpc.Error)
	}

	resp, rpc = postA2A(t, baseURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  A2AMethodTasksCancel,
		"params":  map[string]any{"id": "task-1"},
	}, map[string]string{"Authorization": "Bearer tok-scoped"})
	if resp.StatusCode != http.StatusOK || rpc.Error != nil {
		t.Fatalf("tasks/cancel must not be rate-limited: status=%d err=%+v", resp.StatusCode, rpc.Error)
	}

	// 6. tasks/get and tasks/cancel still enforce required default scopes (403 on tok-noscope).
	resp, rpc = postA2A(t, baseURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  A2AMethodTasksGet,
		"params":  map[string]any{"id": "task-1"},
	}, map[string]string{"Authorization": "Bearer tok-noscope"})
	if resp.StatusCode != http.StatusForbidden || rpc.Error == nil || !strings.Contains(rpc.Error.Message, "insufficient_scope") {
		t.Fatalf("tasks/get without required scope: status=%d err=%+v, want 403 insufficient_scope", resp.StatusCode, rpc.Error)
	}

	resp, rpc = postA2A(t, baseURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  A2AMethodTasksCancel,
		"params":  map[string]any{"id": "task-1"},
	}, map[string]string{"Authorization": "Bearer tok-noscope"})
	if resp.StatusCode != http.StatusForbidden || rpc.Error == nil || !strings.Contains(rpc.Error.Message, "insufficient_scope") {
		t.Fatalf("tasks/cancel without required scope: status=%d err=%+v, want 403 insufficient_scope", resp.StatusCode, rpc.Error)
	}
}

func TestA2A_InvalidSchemaAndImpliedAuthFromScopes(t *testing.T) {
	t.Parallel()

	// 1. Direct attach.NewServer rejects invalid JSON Schema type in InputSchema.
	_, err := NewServer(Options{
		Registry: NewSessionRegistry(),
		Addr:     "127.0.0.1:0",
		A2A: A2AOptions{
			Expose:      true,
			Backend:     newFakeA2ABackend(),
			SkillName:   "bad-schema",
			InputSchema: map[string]any{"type": "not-a-valid-type"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown JSON Schema type") {
		t.Fatalf("NewServer with invalid InputSchema type = %v, want unknown JSON Schema type error", err)
	}

	// 2. Non-empty Scopes implies Auth.Required even if Required was left false.
	baseURL, _ := startTestA2AServer(t, Options{
		A2A: A2AOptions{
			Expose:    true,
			Backend:   newFakeA2ABackend(),
			SkillName: "scoped-skill",
			Auth: A2AAuthOptions{
				Required: false,
				Scopes:   []string{"triage:invoke"},
			},
		},
	})
	resp, _ := postA2A(t, baseURL, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  A2AMethodMessageSend,
		"params": map[string]any{
			"message": map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"kind": "text", "text": "ping"}},
			},
		},
	}, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated request to scoped skill got status %d, want 401", resp.StatusCode)
	}
}
