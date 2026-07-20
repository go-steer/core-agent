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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// factoryStub is a SessionFactory test double. It records every
// (ctx, caller) it sees and returns a *stubRegistrant whose
// (app, user, sid) triple the test pre-configures. err short-circuits
// the factory if set.
type factoryStub struct {
	calls   []factoryCall
	produce func(caller auth.Caller) Registrant
	err     error
}

type factoryCall struct {
	caller auth.Caller
}

func (f *factoryStub) Factory() SessionFactory {
	return func(_ context.Context, caller auth.Caller) (Registrant, context.CancelFunc, error) {
		f.calls = append(f.calls, factoryCall{caller: caller})
		if f.err != nil {
			return nil, nil, f.err
		}
		if f.produce == nil {
			return nil, nil, nil
		}
		return f.produce(caller), nil, nil
	}
}

func newCreateRequest(t *testing.T, caller auth.Caller) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(""))
	if caller.Identity != "" {
		r = r.WithContext(auth.WithCaller(r.Context(), caller))
	}
	r.Host = "core-agent.example:7777"
	return r, httptest.NewRecorder()
}

func TestCreateSession_NoFactoryReturns501(t *testing.T) {
	t.Parallel()
	h := &handlers{
		reg:        NewSessionRegistry(),
		pool:       NewBroadcasterPool(),
		enforceACL: true,
		// factory is nil — POST /sessions should refuse cleanly.
	}
	r, rr := newCreateRequest(t, auth.Caller{Identity: "alice@example.com"})
	h.createSession(rr, r)
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("nil factory: expected 501, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}

func TestCreateSession_NoCallerReturns401(t *testing.T) {
	t.Parallel()
	fs := &factoryStub{produce: func(_ auth.Caller) Registrant {
		return &stubRegistrant{app: "core-agent", user: "u", sid: "should-not-be-used"}
	}}
	h := &handlers{
		reg:     NewSessionRegistry(),
		pool:    NewBroadcasterPool(),
		factory: fs.Factory(),
	}
	r, rr := newCreateRequest(t, auth.Caller{}) // no caller on context
	h.createSession(rr, r)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("no caller: expected 401, got %d", rr.Code)
	}
	if len(fs.calls) != 0 {
		t.Errorf("factory must NOT be invoked when caller is missing; got %d calls", len(fs.calls))
	}
}

func TestCreateSession_HappyPathStampsOwner(t *testing.T) {
	t.Parallel()
	fs := &factoryStub{produce: func(_ auth.Caller) Registrant {
		return &stubRegistrant{app: "core-agent", user: "u", sid: "sess-new-1"}
	}}
	reg := NewSessionRegistry()
	h := &handlers{reg: reg, pool: NewBroadcasterPool(), factory: fs.Factory()}

	r, rr := newCreateRequest(t, auth.Caller{Identity: "alice@example.com"})
	h.createSession(rr, r)

	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var resp createSessionResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.SessionID != "sess-new-1" {
		t.Errorf("SessionID: got %q, want %q", resp.SessionID, "sess-new-1")
	}
	if resp.URL != "http://core-agent.example:7777/sessions/core-agent/sess-new-1" {
		t.Errorf("URL: got %q", resp.URL)
	}

	// Confirm the registry has the entry, owned by alice.
	entries := reg.List()
	if len(entries) != 1 {
		t.Fatalf("registry should have 1 entry, got %d", len(entries))
	}
	if got := entries[0].ACL.Owner; got != "alice@example.com" {
		t.Errorf("ACL.Owner: got %q, want %q (handler must call RegisterOwned with the caller)", got, "alice@example.com")
	}
	// And confirm the factory saw alice.
	if len(fs.calls) != 1 {
		t.Fatalf("factory call count = %d, want 1", len(fs.calls))
	}
	if fs.calls[0].caller.Identity != "alice@example.com" {
		t.Errorf("factory caller: got %q, want %q", fs.calls[0].caller.Identity, "alice@example.com")
	}
}

func TestCreateSession_FactoryErrorReturns500(t *testing.T) {
	t.Parallel()
	fs := &factoryStub{err: errors.New("model not configured")}
	h := &handlers{
		reg:     NewSessionRegistry(),
		pool:    NewBroadcasterPool(),
		factory: fs.Factory(),
	}
	r, rr := newCreateRequest(t, auth.Caller{Identity: "alice@example.com"})
	h.createSession(rr, r)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("factory error: expected 500, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "model not configured") {
		t.Errorf("error body should surface factory error; got %q", rr.Body.String())
	}
}

func TestCreateSession_FactoryNilReturnsAlso500(t *testing.T) {
	t.Parallel()
	fs := &factoryStub{produce: func(_ auth.Caller) Registrant { return nil }}
	h := &handlers{
		reg:     NewSessionRegistry(),
		pool:    NewBroadcasterPool(),
		factory: fs.Factory(),
	}
	r, rr := newCreateRequest(t, auth.Caller{Identity: "alice@example.com"})
	h.createSession(rr, r)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("nil registrant: expected 500, got %d", rr.Code)
	}
}

func TestCreateSession_DuplicateTripleReturns409(t *testing.T) {
	t.Parallel()
	// Factory returns a registrant whose triple is already in the
	// registry — should surface as 409 Conflict so the client can
	// retry with a fresh ID rather than failing opaquely.
	reg := NewSessionRegistry()
	existing := &stubRegistrant{app: "core-agent", user: "u", sid: "sess-collision"}
	if _, err := reg.Register(existing); err != nil {
		t.Fatalf("preload registry: %v", err)
	}
	fs := &factoryStub{produce: func(_ auth.Caller) Registrant {
		return &stubRegistrant{app: "core-agent", user: "u", sid: "sess-collision"}
	}}
	h := &handlers{reg: reg, pool: NewBroadcasterPool(), factory: fs.Factory()}

	r, rr := newCreateRequest(t, auth.Caller{Identity: "alice@example.com"})
	h.createSession(rr, r)
	if rr.Code != http.StatusConflict {
		t.Errorf("triple collision: expected 409, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}
