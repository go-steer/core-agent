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
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// permModeStub records every AttachSetPermMode call, so a test can
// assert a refused request never reached the capability.
type permModeStub struct {
	stubRegistrant
	mu    sync.Mutex
	calls []PermModeRequest
	err   error // returned by AttachSetPermMode when set
}

func (s *permModeStub) AttachSetPermMode(req PermModeRequest) (PermModeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.err != nil {
		return PermModeResponse{}, s.err
	}
	return PermModeResponse{Previous: "ask", Mode: req.Mode}, nil
}

func (s *permModeStub) seen() []PermModeRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PermModeRequest(nil), s.calls...)
}

// permModeFixture is a multi-session daemon (ACL enforced) with one
// session owned by owner@, contributor@ and viewer@ on its ACL, and
// /perms/mode routed through the real lookup + authorize chain.
func permModeFixture(t *testing.T) (*http.ServeMux, *permModeStub) {
	t.Helper()
	reg := NewSessionRegistry()
	stub := &permModeStub{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s-mode"}}
	if _, err := reg.RegisterOwned(stub, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	h := &handlers{reg: reg, pool: newBroadcasterPool(), enforceACL: true}
	mux := http.NewServeMux()
	h.registerSessionACL(mux)
	h.registerOperatorState(mux)
	r, rr := aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-mode/acl",
		`{"contributors":["contributor@example.com"],"viewers":["viewer@example.com"]}`, auth.Caller{Identity: "owner@example.com"})
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("setup PATCH /acl: status = %d; body=%q", rr.Code, rr.Body.String())
	}
	return mux, stub
}

func postPermMode(t *testing.T, mux *http.ServeMux, body string, caller auth.Caller, source string) (int, string) {
	t.Helper()
	r, rr := aclRequest(t, http.MethodPost, "/sessions/core-agent/s-mode/perms/mode", body, caller)
	if source != "" {
		r = r.WithContext(withAuthSource(r.Context(), source))
	}
	mux.ServeHTTP(rr, r)
	return rr.Code, rr.Body.String()
}

// The session owner can change the mode, widening included, and the
// capability receives the verified identity to record.
func TestPermMode_OwnerCanWiden(t *testing.T) {
	t.Parallel()
	mux, stub := permModeFixture(t)
	code, body := postPermMode(t, mux, `{"mode":"yolo"}`, auth.Caller{Identity: "owner@example.com"}, whoAmISourceBearer)
	if code != http.StatusOK {
		t.Fatalf("owner POST: status = %d, want 200; body=%q", code, body)
	}
	var resp PermModeResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if resp.Mode != "yolo" || resp.Previous != "ask" {
		t.Errorf("response = %+v, want ask → yolo", resp)
	}
	calls := stub.seen()
	if len(calls) != 1 || calls[0].Mode != "yolo" || calls[0].Caller != "owner@example.com" {
		t.Errorf("capability calls = %+v, want one yolo from owner@example.com", calls)
	}
}

// A daemon admin passes ActionSessionAdmin on any session.
func TestPermMode_AdminCanChange(t *testing.T) {
	t.Parallel()
	mux, stub := permModeFixture(t)
	code, body := postPermMode(t, mux, `{"mode":"plan"}`, auth.Caller{Identity: "root@example.com", Admin: true}, whoAmISourceBearer)
	if code != http.StatusOK || len(stub.seen()) != 1 {
		t.Errorf("admin POST: status = %d, calls = %d; want 200 and 1; body=%q", code, len(stub.seen()), body)
	}
}

// The decision this route exists to enforce: a contributor can inject
// and answer prompts on the session, but cannot change its mode — not
// even to narrow it — and neither can a viewer or a stranger. Each gets
// the masked 404 and the capability is never called.
func TestPermMode_OnlyTheOwnerOrAnAdmin(t *testing.T) {
	t.Parallel()
	for _, who := range []string{"contributor@example.com", "viewer@example.com", "stranger@example.com"} {
		for _, mode := range []string{"yolo", "plan"} {
			mux, stub := permModeFixture(t)
			code, body := postPermMode(t, mux, `{"mode":"`+mode+`"}`, auth.Caller{Identity: who}, whoAmISourceBearer)
			if code != http.StatusNotFound {
				t.Errorf("%s → %s: status = %d, want 404; body=%q", who, mode, code, body)
			}
			if n := len(stub.seen()); n != 0 {
				t.Errorf("%s → %s: capability called %d times, want 0", who, mode, n)
			}
		}
	}
}

// "allow" is config-only; unknown and empty values are refused. All
// before the capability runs.
func TestPermMode_RefusesModesOutsideTheChip(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{"mode":"allow"}`, `{"mode":"bypassPermissions"}`, `{"mode":""}`, `{}`, ``} {
		mux, stub := permModeFixture(t)
		code, resp := postPermMode(t, mux, body, auth.Caller{Identity: "owner@example.com"}, whoAmISourceBearer)
		if code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400; resp=%q", body, code, resp)
		}
		if n := len(stub.seen()); n != 0 {
			t.Errorf("body %q: capability called %d times, want 0", body, n)
		}
	}
	mux, _ := permModeFixture(t)
	if _, resp := postPermMode(t, mux, `{"mode":"allow"}`, auth.Caller{Identity: "owner@example.com"}, whoAmISourceBearer); !strings.Contains(resp, ".agents/config.json") {
		t.Errorf("allow refusal = %q, want it to say where allow is set", resp)
	}
}

// With no verified identity the change still goes through (the
// transport credential is the gate, as on every route) but records no
// caller rather than the anonymous placeholder.
func TestPermMode_UnverifiedCallerRecordsNone(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	stub := &permModeStub{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s-mode"}}
	if _, err := reg.Register(stub); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h := &handlers{reg: reg, pool: newBroadcasterPool(), enforceACL: false}
	mux := http.NewServeMux()
	h.registerOperatorState(mux)
	code, body := postPermMode(t, mux, `{"mode":"acceptEdits"}`, auth.Anonymous, whoAmISourceAnonymous)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", code, body)
	}
	if calls := stub.seen(); len(calls) != 1 || calls[0].Caller != "" {
		t.Errorf("calls = %+v, want one with an empty caller", calls)
	}
}

func TestPermMode_NoCapabilityIs501(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(&stubRegistrant{app: "core-agent", user: "u", sid: "s-mode"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h := &handlers{reg: reg, pool: newBroadcasterPool()}
	mux := http.NewServeMux()
	h.registerOperatorState(mux)
	if code, body := postPermMode(t, mux, `{"mode":"plan"}`, auth.Anonymous, ""); code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501; body=%q", code, body)
	}
}

// The audit row's caller comes from the verified context, never the
// body: a "caller" field on the wire is ignored, for a verified owner
// and for an unverified transport alike.
func TestPermMode_CallerInTheBodyIsIgnored(t *testing.T) {
	t.Parallel()
	mux, stub := permModeFixture(t)
	code, body := postPermMode(t, mux, `{"mode":"plan","caller":"evil@example.com","Caller":"evil@example.com"}`,
		auth.Caller{Identity: "owner@example.com"}, whoAmISourceBearer)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", code, body)
	}
	if calls := stub.seen(); len(calls) != 1 || calls[0].Caller != "owner@example.com" {
		t.Errorf("calls = %+v, want one attributed to owner@example.com", calls)
	}

	reg := NewSessionRegistry()
	anon := &permModeStub{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s-mode"}}
	if _, err := reg.Register(anon); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h := &handlers{reg: reg, pool: newBroadcasterPool()}
	amux := http.NewServeMux()
	h.registerOperatorState(amux)
	if code, body := postPermMode(t, amux, `{"mode":"plan","caller":"evil@example.com"}`, auth.Anonymous, whoAmISourceAnonymous); code != http.StatusOK {
		t.Fatalf("anonymous: status = %d, want 200; body=%q", code, body)
	}
	if calls := anon.seen(); len(calls) != 1 || calls[0].Caller != "" {
		t.Errorf("anonymous calls = %+v, want one with an empty caller", calls)
	}
}

// A session that implements the capability but has no gate behind it
// is a 501 like a missing capability, not the 400 other errors get.
func TestPermMode_NoGateIs501(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	stub := &permModeStub{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s-mode"}, err: ErrCapabilityNotRegistered}
	if _, err := reg.Register(stub); err != nil {
		t.Fatalf("Register: %v", err)
	}
	h := &handlers{reg: reg, pool: newBroadcasterPool()}
	mux := http.NewServeMux()
	h.registerOperatorState(mux)
	code, body := postPermMode(t, mux, `{"mode":"plan"}`, auth.Anonymous, "")
	if code != http.StatusNotImplemented || !strings.Contains(body, "no permission gate") {
		t.Errorf("status = %d body = %q, want 501 naming the missing gate", code, body)
	}
}

// #1175 decision 13: auto is not settable remotely until both TUIs can
// display it, and the 400 says so rather than calling it unknown.
func TestParseRemotePermMode_AutoNotYetSettable(t *testing.T) {
	t.Parallel()
	_, err := ParseRemotePermMode(string(permissions.ModeAuto))
	if !errors.Is(err, ErrPermModeNotSettable) || !strings.Contains(err.Error(), "not available yet") {
		t.Fatalf("ParseRemotePermMode(auto) = %v, want ErrPermModeNotSettable naming not-available-yet", err)
	}
}
