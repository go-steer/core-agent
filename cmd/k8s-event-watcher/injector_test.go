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

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newFakeDaemon returns an httptest server that impersonates the
// core-agent daemon's POST /sessions + POST /sessions/<sid>/inject
// endpoints. Returns the URL to point --daemon-url at, plus a
// pointer to a slice of received inject bodies for assertion.
func newFakeDaemon(t *testing.T) (baseURL string, capturedInjects *[]string, capturedAuth *[]string) {
	t.Helper()
	injects := make([]string, 0, 4)
	auths := make([]string, 0, 4)
	var sessionCounter int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Asserted-Caller"))
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/sessions":
			id := atomic.AddInt32(&sessionCounter, 1)
			resp := createSessionResponse{
				AppName:   "core-agent",
				UserID:    "alice",
				SessionID: "sess-" + strings.Repeat("x", int(id)),
				URL:       "http://x",
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(resp)
			return
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/sessions/") && strings.HasSuffix(r.URL.Path, "/inject"):
			body, _ := io.ReadAll(r.Body)
			injects = append(injects, string(body))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &injects, &auths
}

func TestInjector_CreateSession_ReturnsSessionID(t *testing.T) {
	t.Parallel()
	base, _, _ := newFakeDaemon(t)
	inj, err := newInjector(injectorConfig{
		daemonURL:      base,
		bearerToken:    "tok_test",
		assertedCaller: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("newInjector: %v", err)
	}
	sid, err := inj.CreateSession(context.Background())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if !strings.HasPrefix(sid, "sess-") {
		t.Errorf("SessionID = %q, want sess-* prefix", sid)
	}
}

func TestInjector_CreateSession_SendsAssertedCaller(t *testing.T) {
	t.Parallel()
	base, _, auths := newFakeDaemon(t)
	inj, err := newInjector(injectorConfig{
		daemonURL:      base,
		bearerToken:    "tok_test",
		assertedCaller: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("newInjector: %v", err)
	}
	if _, err := inj.CreateSession(context.Background()); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(*auths) != 1 || !strings.Contains((*auths)[0], "alice@example.com") {
		t.Errorf("expected X-Asserted-Caller header carrying alice@example.com; got auth trail = %v", *auths)
	}
	if !strings.HasPrefix((*auths)[0], "Bearer tok_test|") {
		t.Errorf("expected bearer token in Authorization header; got %v", (*auths)[0])
	}
}

func TestInjector_Inject_PostsWrappedPayload(t *testing.T) {
	t.Parallel()
	base, injects, _ := newFakeDaemon(t)
	inj, err := newInjector(injectorConfig{
		daemonURL:      base,
		bearerToken:    "tok_test",
		assertedCaller: "alice@example.com",
	})
	if err != nil {
		t.Fatalf("newInjector: %v", err)
	}
	payload := InjectPayload{
		Kind:      injectKindEvent,
		Reason:    "CrashLoopBackOff",
		Namespace: "checkout",
		Name:      "checkout-svc-7b9d-x4kzq",
		UID:       "abc-123",
		Message:   "Back-off restarting failed container",
		Count:     1,
		FirstSeen: time.Now(),
		LastSeen:  time.Now(),
		Cluster:   "prod-us-central1",
	}
	if err := inj.Inject(context.Background(), "sess-test", payload); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if len(*injects) != 1 {
		t.Fatalf("expected 1 inject body captured, got %d", len(*injects))
	}
	// Wrapped envelope: {"message": "<serialized payload>"}
	var wrapper injectMessageRequest
	if err := json.Unmarshal([]byte((*injects)[0]), &wrapper); err != nil {
		t.Fatalf("captured body isn't the inject envelope: %v (body=%q)", err, (*injects)[0])
	}
	if !strings.Contains(wrapper.Message, `"kind":"k8s-event"`) {
		t.Errorf("envelope.Message should carry the JSON payload; got %q", wrapper.Message)
	}
	if !strings.Contains(wrapper.Message, `"reason":"CrashLoopBackOff"`) {
		t.Errorf("payload reason missing from wrapped body: %q", wrapper.Message)
	}
}

func TestInjector_CreateSession_NonCreatedIsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	inj, _ := newInjector(injectorConfig{daemonURL: srv.URL, bearerToken: "t", assertedCaller: "a@b"})
	_, err := inj.CreateSession(context.Background())
	if err == nil {
		t.Fatal("expected error for non-201 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should carry the status code; got %v", err)
	}
}

func TestInjector_Inject_ContextCancellationHonored(t *testing.T) {
	t.Parallel()
	// Server blocks so we can cancel mid-request and verify the
	// injector honors the context.
	block := make(chan struct{})
	defer close(block)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(srv.Close)
	inj, _ := newInjector(injectorConfig{daemonURL: srv.URL, bearerToken: "t", assertedCaller: "a@b"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	err := inj.Inject(ctx, "sess-x", InjectPayload{Kind: "k8s-event", Reason: "X"})
	if err == nil {
		t.Error("expected error on cancelled context")
	}
}

func TestInjector_TrailingSlashRejected(t *testing.T) {
	t.Parallel()
	_, err := newInjector(injectorConfig{daemonURL: "http://x/", bearerToken: "t"})
	if err == nil {
		t.Error("trailing slash in daemonURL should be rejected")
	}
}

func TestInjector_MissingTokenRejected(t *testing.T) {
	t.Parallel()
	_, err := newInjector(injectorConfig{daemonURL: "http://x", bearerToken: ""})
	if err == nil {
		t.Error("empty bearerToken should be rejected")
	}
}
