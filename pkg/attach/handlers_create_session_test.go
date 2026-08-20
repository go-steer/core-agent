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
	return newCreateRequestBody(t, caller, "")
}

// newCreateRequestBody is newCreateRequest with an explicit body. The
// body is optional on this endpoint (#797), so every existing caller
// of newCreateRequest doubles as the no-body regression test.
func newCreateRequestBody(t *testing.T, caller auth.Caller, body string) (*http.Request, *httptest.ResponseRecorder) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
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
		pool:       newBroadcasterPool(),
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
		pool:    newBroadcasterPool(),
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
	h := &handlers{reg: reg, pool: newBroadcasterPool(), factory: fs.Factory()}

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
	if got := entries[0].CurrentACL().Owner; got != "alice@example.com" {
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
		pool:    newBroadcasterPool(),
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
		pool:    newBroadcasterPool(),
		factory: fs.Factory(),
	}
	r, rr := newCreateRequest(t, auth.Caller{Identity: "alice@example.com"})
	h.createSession(rr, r)
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("nil registrant: expected 500, got %d", rr.Code)
	}
}

// TestCreateSession_BodyStampsACL — the creator usually knows its
// audience at creation time, and stamping it here is the difference
// between the first reply landing and it 404ing until somebody
// remembers to PATCH the ACL (#797).
func TestCreateSession_BodyStampsACL(t *testing.T) {
	t.Parallel()
	fs := &factoryStub{produce: func(_ auth.Caller) Registrant {
		return &stubRegistrant{app: "core-agent", user: "u", sid: "sess-acl"}
	}}
	reg := NewSessionRegistry()
	h := &handlers{reg: reg, pool: newBroadcasterPool(), factory: fs.Factory()}

	r, rr := newCreateRequestBody(t, auth.Caller{Identity: "lookout@example.com"},
		`{"viewers":["watch@example.com"],"contributors":["  oncall@example.com  ","oncall@example.com"]}`)
	h.createSession(rr, r)
	if rr.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (body: %s)", rr.Code, rr.Body.String())
	}

	acl := reg.List()[0].CurrentACL()
	if acl.Owner != "lookout@example.com" {
		t.Errorf("Owner = %q, want the authenticated caller", acl.Owner)
	}
	if len(acl.Viewers) != 1 || acl.Viewers[0] != "watch@example.com" {
		t.Errorf("Viewers = %v", acl.Viewers)
	}
	// Normalized on the way in: the ACL is matched exactly, so an
	// untrimmed identity would read correct and deny anyway.
	if len(acl.Contributors) != 1 || acl.Contributors[0] != "oncall@example.com" {
		t.Errorf("Contributors = %v, want [oncall@example.com] trimmed and de-duplicated", acl.Contributors)
	}
}

// TestCreateSession_BodyRejectsOwner — the field exists on the wire
// shape only so it can be refused. Honouring it would let any caller
// hand a session to someone else; dropping it silently would let the
// caller believe it had.
func TestCreateSession_BodyRejectsOwner(t *testing.T) {
	t.Parallel()
	fs := &factoryStub{produce: func(_ auth.Caller) Registrant {
		return &stubRegistrant{app: "core-agent", user: "u", sid: "sess-owner"}
	}}
	reg := NewSessionRegistry()
	h := &handlers{reg: reg, pool: newBroadcasterPool(), factory: fs.Factory()}

	r, rr := newCreateRequestBody(t, auth.Caller{Identity: "alice@example.com"}, `{"owner":"bob@example.com"}`)
	h.createSession(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if len(fs.calls) != 0 {
		t.Errorf("factory ran %d times; a rejected body must not leave a constructed agent and a live wake loop behind", len(fs.calls))
	}
	if len(reg.List()) != 0 {
		t.Errorf("registry has %d entries after a rejected body, want 0", len(reg.List()))
	}
}

// TestCreateSession_MalformedBodyReturns400 — same ordering guarantee
// as the owner case, for the plainer failure.
func TestCreateSession_MalformedBodyReturns400(t *testing.T) {
	t.Parallel()
	fs := &factoryStub{produce: func(_ auth.Caller) Registrant {
		return &stubRegistrant{app: "core-agent", user: "u", sid: "sess-bad"}
	}}
	h := &handlers{reg: NewSessionRegistry(), pool: newBroadcasterPool(), factory: fs.Factory()}

	r, rr := newCreateRequestBody(t, auth.Caller{Identity: "alice@example.com"}, `{"viewers":`)
	h.createSession(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if len(fs.calls) != 0 {
		t.Errorf("factory ran %d times on a malformed body, want 0", len(fs.calls))
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
	h := &handlers{reg: reg, pool: newBroadcasterPool(), factory: fs.Factory()}

	r, rr := newCreateRequest(t, auth.Caller{Identity: "alice@example.com"})
	h.createSession(rr, r)
	if rr.Code != http.StatusConflict {
		t.Errorf("triple collision: expected 409, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}
