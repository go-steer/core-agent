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

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// permsACLFixture registers an owned session with a contributor and a
// viewer, behind the real route table with the ACL enforced.
func permsACLFixture(t *testing.T) (*http.ServeMux, *operatorMutationsRegistrant) {
	t.Helper()
	reg := NewSessionRegistry()
	stub := &operatorMutationsRegistrant{stubRegistrant: stubRegistrant{app: "core-agent", user: "u", sid: "s-perms"}}
	if _, err := reg.RegisterOwned(stub, "owner@example.com"); err != nil {
		t.Fatalf("RegisterOwned: %v", err)
	}
	h := &handlers{reg: reg, pool: newBroadcasterPool(), enforceACL: true}
	mux := http.NewServeMux()
	h.registerSessionACL(mux)
	h.registerOperatorState(mux)
	r, rr := aclRequest(t, http.MethodPatch, "/sessions/core-agent/s-perms/acl",
		`{"contributors":["contributor@example.com"],"viewers":["viewer@example.com"]}`, auth.Caller{Identity: "owner@example.com"})
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Fatalf("setup PATCH /acl: status = %d; body=%q", rr.Code, rr.Body.String())
	}
	return mux, stub
}

func postPerms(t *testing.T, mux *http.ServeMux, verb, who string, admin bool) int {
	t.Helper()
	r, rr := aclRequest(t, http.MethodPost, "/sessions/core-agent/s-perms/perms/"+verb,
		`{"patterns":["*"]}`, auth.Caller{Identity: who, Admin: admin})
	mux.ServeHTTP(rr, r)
	return rr.Code
}

// #1176: an allow pattern widens what runs without a prompt ("*"
// matches every non-bash call), so /perms/allow is held to the owner
// or a daemon admin, the same bar as /perms/mode. A contributor gets
// the masked 404 and nothing is added.
func TestPermsAllow_OnlyTheOwnerOrAnAdmin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		who   string
		admin bool
		want  int
	}{
		{"owner@example.com", false, http.StatusNoContent},
		{"root@example.com", true, http.StatusNoContent},
		{"contributor@example.com", false, http.StatusNotFound},
		{"viewer@example.com", false, http.StatusNotFound},
		{"stranger@example.com", false, http.StatusNotFound},
	} {
		mux, stub := permsACLFixture(t)
		if got := postPerms(t, mux, "allow", tc.who, tc.admin); got != tc.want {
			t.Errorf("%s: POST /perms/allow = %d, want %d", tc.who, got, tc.want)
		}
		stub.mu.Lock()
		n := len(stub.addedAllow)
		stub.mu.Unlock()
		if wantN := map[bool]int{true: 1, false: 0}[tc.want == http.StatusNoContent]; n != wantN {
			t.Errorf("%s: %d allow calls reached the session, want %d", tc.who, n, wantN)
		}
	}
}

// /perms/deny only narrows, so a contributor keeps it; a viewer does
// not.
func TestPermsDeny_ContributorKeepsIt(t *testing.T) {
	t.Parallel()
	for who, want := range map[string]int{
		"owner@example.com":       http.StatusNoContent,
		"contributor@example.com": http.StatusNoContent,
		"viewer@example.com":      http.StatusNotFound,
	} {
		mux, _ := permsACLFixture(t)
		if got := postPerms(t, mux, "deny", who, false); got != want {
			t.Errorf("%s: POST /perms/deny = %d, want %d", who, got, want)
		}
	}
}
