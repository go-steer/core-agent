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
	"net/http"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// ownedPromptFixture is promptRouteFixture with the ACL enforced: an
// owned session over a live broker, with a contributor and a viewer
// added through the real PATCH /acl route.
func ownedPromptFixture(t *testing.T) (*http.ServeMux, *PromptBroker) {
	t.Helper()
	broker := NewPromptBroker()
	t.Cleanup(broker.Close)
	reg := NewSessionRegistry()
	ag := &promptRegistrant{
		stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s1"},
		broker:         broker,
	}
	if _, err := reg.RegisterOwned(ag, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	h := &handlers{reg: reg, pool: newBroadcasterPool(), enforceACL: true}
	mux := http.NewServeMux()
	h.registerSessionACL(mux)
	h.registerPrompts(mux)
	r, rr := aclRequest(t, http.MethodPatch, "/sessions/core-agent/s1/acl",
		`{"contributors":["contributor@example.com"],"viewers":["viewer@example.com"]}`, auth.Caller{Identity: "owner@example.com"})
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("setup PATCH /acl: status = %d; body=%q", rr.Code, rr.Body.String())
	}
	return mux, broker
}

func awaitApproval(t *testing.T, done <-chan askResult) permissions.Approval {
	t.Helper()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("AskApprovalAttributed: %v", res.err)
		}
		return res.approval
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not resolve")
		return permissions.Approval{}
	}
}

// #1179: an always grant lands in the policy every session shares and
// in .agents/config.json, so whoever gives one widens sessions they may
// not be able to read. Only a daemon admin keeps a true always. The
// owner is downgraded too: any authenticated caller can own a session
// by creating one, so a contributor on someone else's session could
// otherwise create their own, steer it into the same call and answer
// "always" there. A non-admin's "allow-always" is applied as
// "allow-session" — the call in front of them still runs — and the
// response says so.
func TestPermsRespond_NonAdminAlwaysIsDowngradedToSession(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		caller     auth.Caller
		want       permissions.Decision
		downgraded bool
	}{
		{"admin", auth.Caller{Identity: "root@example.com", Admin: true}, permissions.DecisionAllowAlways, false},
		{"owner", auth.Caller{Identity: "owner@example.com"}, permissions.DecisionAllowSession, true},
		{"contributor", auth.Caller{Identity: "contributor@example.com"}, permissions.DecisionAllowSession, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux, broker := ownedPromptFixture(t)
			done, id := askInBackground(t, broker)
			r, rr := respondRequest(t, `{"id":"`+id+`","decision":"allow-always"}`, tc.caller, whoAmISourceBearer)
			mux.ServeHTTP(rr, r)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%q", rr.Code, rr.Body.String())
			}
			got := decodeRespond(t, rr)
			if got.Decision != tc.want.String() || got.Downgraded != tc.downgraded {
				t.Errorf("response decision=%q downgraded=%v, want %q / %v", got.Decision, got.Downgraded, tc.want, tc.downgraded)
			}
			if a := awaitApproval(t, done); a.Decision != tc.want {
				t.Errorf("prompt resolved with %v, want %v", a.Decision, tc.want)
			}
		})
	}
}

// Only "allow-always" widens past the session, so it is the only answer
// the cap touches: a contributor's session-scoped and one-shot answers
// reach the gate as sent, with downgraded false.
func TestPermsRespond_ContributorSessionAnswersPassThrough(t *testing.T) {
	t.Parallel()
	for _, d := range []permissions.Decision{
		permissions.DecisionDeny,
		permissions.DecisionAllowOnce,
		permissions.DecisionAllowSession,
		permissions.DecisionAllowSessionVerb,
		permissions.DecisionAllowSessionTool,
	} {
		t.Run(d.String(), func(t *testing.T) {
			t.Parallel()
			mux, broker := ownedPromptFixture(t)
			done, id := askInBackground(t, broker)
			r, rr := respondRequest(t, `{"id":"`+id+`","decision":"`+d.String()+`"}`, auth.Caller{Identity: "contributor@example.com"}, whoAmISourceBearer)
			mux.ServeHTTP(rr, r)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%q", rr.Code, rr.Body.String())
			}
			if got := decodeRespond(t, rr); got.Decision != d.String() || got.Downgraded {
				t.Errorf("response decision=%q downgraded=%v, want %q / false", got.Decision, got.Downgraded, d)
			}
			if a := awaitApproval(t, done); a.Decision != d {
				t.Errorf("prompt resolved with %v, want %v", a.Decision, d)
			}
		})
	}
}

// A viewer cannot answer at all: the route is ActionSessionWrite, so
// the cap never sees them and the prompt stays pending.
func TestPermsRespond_ViewerStillGetsTheMaskedNotFound(t *testing.T) {
	t.Parallel()
	mux, broker := ownedPromptFixture(t)
	_, id := askInBackground(t, broker)
	r, rr := respondRequest(t, `{"id":"`+id+`","decision":"allow-always"}`, auth.Caller{Identity: "viewer@example.com"}, whoAmISourceBearer)
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%q", rr.Code, rr.Body.String())
	}
	if n := len(broker.Pending()); n != 1 {
		t.Errorf("%d prompts pending after the refused answer, want 1", n)
	}
}

// A daemon without ACLs (single-session) has no contributor to tell
// apart from the owner, so an always stays an always — the pre-#1179
// behaviour, now reported in the response.
func TestPermsRespond_NoACLKeepsAlways(t *testing.T) {
	t.Parallel()
	mux, broker := promptRouteFixture(t)
	done, id := askInBackground(t, broker)
	r, rr := respondRequest(t, `{"id":"`+id+`","decision":"allow-always"}`, auth.Caller{Identity: "someone@example.com"}, whoAmISourceBearer)
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%q", rr.Code, rr.Body.String())
	}
	if got := decodeRespond(t, rr); got.Decision != "allow-always" || got.Downgraded {
		t.Errorf("response decision=%q downgraded=%v, want allow-always / false", got.Decision, got.Downgraded)
	}
	if a := awaitApproval(t, done); a.Decision != permissions.DecisionAllowAlways {
		t.Errorf("prompt resolved with %v, want allow-always", a.Decision)
	}
}

// The response's decision field is Decision.String(), so every
// decision's String must be the wire name DecisionFromWire accepts.
func TestDecisionWireRoundTrip(t *testing.T) {
	t.Parallel()
	for d := permissions.DecisionDeny; d <= permissions.DecisionAllowAlways; d++ {
		if got, ok := DecisionFromWire(d.String()); !ok || got != d {
			t.Errorf("DecisionFromWire(%q) = %v, %v; want %v, true", d.String(), got, ok, d)
		}
	}
}
