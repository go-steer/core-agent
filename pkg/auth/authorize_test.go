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

package auth_test

import (
	"slices"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// TestAuthorize_Matrix walks the full action × role grid documented in
// docs/multi-session-design.md §"Authorization rules". Every cell of
// the matrix must match what the design doc promises — operators read
// that table and assume the code enforces it.
func TestAuthorize_Matrix(t *testing.T) {
	t.Parallel()
	acl := auth.SessionACL{
		Owner:        "owner@example.com",
		Viewers:      []string{"viewer@example.com"},
		Contributors: []string{"contrib@example.com"},
	}

	tests := []struct {
		name   string
		caller auth.Caller
		action auth.Action
		want   bool
	}{
		// Admin passes everything.
		{"admin/list", auth.Caller{Identity: "ops@example.com", Admin: true}, auth.ActionSessionList, true},
		{"admin/read", auth.Caller{Identity: "ops@example.com", Admin: true}, auth.ActionSessionRead, true},
		{"admin/write", auth.Caller{Identity: "ops@example.com", Admin: true}, auth.ActionSessionWrite, true},
		{"admin/admin", auth.Caller{Identity: "ops@example.com", Admin: true}, auth.ActionSessionAdmin, true},
		{"admin/daemon", auth.Caller{Identity: "ops@example.com", Admin: true}, auth.ActionDaemonAdmin, true},

		// Owner can do everything on its own session except DaemonAdmin.
		{"owner/list", auth.Caller{Identity: "owner@example.com"}, auth.ActionSessionList, true},
		{"owner/read", auth.Caller{Identity: "owner@example.com"}, auth.ActionSessionRead, true},
		{"owner/write", auth.Caller{Identity: "owner@example.com"}, auth.ActionSessionWrite, true},
		{"owner/admin", auth.Caller{Identity: "owner@example.com"}, auth.ActionSessionAdmin, true},
		{"owner/daemon", auth.Caller{Identity: "owner@example.com"}, auth.ActionDaemonAdmin, false},

		// Contributor: read + write, NOT admin.
		{"contrib/list", auth.Caller{Identity: "contrib@example.com"}, auth.ActionSessionList, true},
		{"contrib/read", auth.Caller{Identity: "contrib@example.com"}, auth.ActionSessionRead, true},
		{"contrib/write", auth.Caller{Identity: "contrib@example.com"}, auth.ActionSessionWrite, true},
		{"contrib/admin", auth.Caller{Identity: "contrib@example.com"}, auth.ActionSessionAdmin, false},
		{"contrib/daemon", auth.Caller{Identity: "contrib@example.com"}, auth.ActionDaemonAdmin, false},

		// Viewer: read only.
		{"viewer/list", auth.Caller{Identity: "viewer@example.com"}, auth.ActionSessionList, true},
		{"viewer/read", auth.Caller{Identity: "viewer@example.com"}, auth.ActionSessionRead, true},
		{"viewer/write", auth.Caller{Identity: "viewer@example.com"}, auth.ActionSessionWrite, false},
		{"viewer/admin", auth.Caller{Identity: "viewer@example.com"}, auth.ActionSessionAdmin, false},
		{"viewer/daemon", auth.Caller{Identity: "viewer@example.com"}, auth.ActionDaemonAdmin, false},

		// Stranger: list only (handler filters results separately).
		{"stranger/list", auth.Caller{Identity: "stranger@example.com"}, auth.ActionSessionList, true},
		{"stranger/read", auth.Caller{Identity: "stranger@example.com"}, auth.ActionSessionRead, false},
		{"stranger/write", auth.Caller{Identity: "stranger@example.com"}, auth.ActionSessionWrite, false},
		{"stranger/admin", auth.Caller{Identity: "stranger@example.com"}, auth.ActionSessionAdmin, false},
		{"stranger/daemon", auth.Caller{Identity: "stranger@example.com"}, auth.ActionDaemonAdmin, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := auth.Authorize(tt.caller, tt.action, acl)
			if got != tt.want {
				t.Errorf("Authorize(%s, %s, acl) = %v, want %v", tt.caller.Identity, tt.action, got, tt.want)
			}
		})
	}
}

func TestAuthorize_EmptyIdentityIsNobody(t *testing.T) {
	t.Parallel()
	// The zero-value Caller must not slip past authorization just
	// because it happens to share the empty-string "Owner" of a
	// half-initialized ACL. Defense in depth against an accidental
	// SessionACL{} created somewhere in the call chain.
	acl := auth.SessionACL{Owner: ""}
	c := auth.Caller{Identity: ""}
	if auth.Authorize(c, auth.ActionSessionRead, acl) {
		t.Error("empty-identity Caller authorized against empty-Owner ACL; this is the exact case the safe default must reject")
	}
	if auth.Authorize(c, auth.ActionSessionWrite, acl) {
		t.Error("empty-identity Caller authorized for write against empty-Owner ACL")
	}
}

func TestAuthorize_UnknownAction(t *testing.T) {
	t.Parallel()
	// A bogus Action value (e.g., from a future version of the code
	// reading an old binary's audit log) must default to deny.
	got := auth.Authorize(
		auth.Caller{Identity: "owner@example.com"},
		auth.Action(99),
		auth.SessionACL{Owner: "owner@example.com"},
	)
	if got {
		t.Error("unknown Action defaulted to allow; must be deny")
	}
}

func TestAction_String(t *testing.T) {
	t.Parallel()
	tests := map[auth.Action]string{
		auth.ActionSessionList:  "session.list",
		auth.ActionSessionRead:  "session.read",
		auth.ActionSessionWrite: "session.write",
		auth.ActionSessionAdmin: "session.admin",
		auth.ActionDaemonAdmin:  "daemon.admin",
		auth.Action(42):         "unknown",
	}
	for a, want := range tests {
		if got := a.String(); got != want {
			t.Errorf("Action(%d).String() = %q, want %q", a, got, want)
		}
	}
}

// TestSessionACL_Normalized covers the cleanup applied wherever an ACL
// crosses a trust boundary. The stakes are higher than they look:
// Authorize matches identities exactly, so a trailing space on a
// pasted identity yields an ACL that reads correct and denies anyway —
// and a denial surfaces as 404 rather than 403 (deliberately), which
// is the hardest possible shape to debug from the client side.
func TestSessionACL_Normalized(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   auth.SessionACL
		want auth.SessionACL
	}{
		{
			name: "zero value round-trips",
		},
		{
			name: "trims, drops empties, de-duplicates, keeps order",
			in: auth.SessionACL{
				Owner:        "owner@example.com",
				Viewers:      []string{" b@example.com", "a@example.com ", "", "   ", "b@example.com"},
				Contributors: []string{"c@example.com"},
			},
			want: auth.SessionACL{
				Owner:        "owner@example.com",
				Viewers:      []string{"b@example.com", "a@example.com"},
				Contributors: []string{"c@example.com"},
			},
		},
		{
			// A list that is nothing but junk is not a grant, and
			// leaving "" in it would sit in the persisted row looking
			// like one.
			name: "all-empty list collapses to nil",
			in:   auth.SessionACL{Viewers: []string{"", "  "}},
		},
		{
			// nil vs. []string{} matters: the store round-trips these
			// lists through JSON, and flipping between `null` and `[]`
			// would churn the persisted row on every write.
			name: "empty list stays nil",
			in:   auth.SessionACL{Contributors: []string{}},
		},
		{
			// Owner is one value with direct caller-facing errors, and
			// may legitimately be a synthetic identity whose spelling
			// is the operator's business.
			name: "owner is left exactly as supplied",
			in:   auth.SessionACL{Owner: "  spaced@example.com  "},
			want: auth.SessionACL{Owner: "  spaced@example.com  "},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.in.Normalized()
			if got.Owner != tc.want.Owner {
				t.Errorf("Owner = %q, want %q", got.Owner, tc.want.Owner)
			}
			if !slices.Equal(got.Viewers, tc.want.Viewers) {
				t.Errorf("Viewers = %#v, want %#v", got.Viewers, tc.want.Viewers)
			}
			if !slices.Equal(got.Contributors, tc.want.Contributors) {
				t.Errorf("Contributors = %#v, want %#v", got.Contributors, tc.want.Contributors)
			}
			if (got.Viewers == nil) != (tc.want.Viewers == nil) {
				t.Errorf("Viewers nil-ness = %v, want %v", got.Viewers == nil, tc.want.Viewers == nil)
			}
			if (got.Contributors == nil) != (tc.want.Contributors == nil) {
				t.Errorf("Contributors nil-ness = %v, want %v", got.Contributors == nil, tc.want.Contributors == nil)
			}
		})
	}
}

// TestSessionACL_NormalizedDoesNotAliasReceiver — the registry stores
// the normalized value and lets readers hold it without copying, so a
// shared backing array would let a caller mutate a live ACL after the
// fact.
func TestSessionACL_NormalizedDoesNotAliasReceiver(t *testing.T) {
	t.Parallel()
	src := auth.SessionACL{Viewers: []string{"a@example.com", "b@example.com"}}
	got := src.Normalized()
	src.Viewers[0] = "mallory@example.com"
	if got.Viewers[0] != "a@example.com" {
		t.Errorf("Viewers[0] = %q; Normalized must not share a backing array with the receiver", got.Viewers[0])
	}
}

// TestSessionACL_NormalizedIsWhatAuthorizeSees ties the cleanup to the
// thing it exists to protect.
func TestSessionACL_NormalizedIsWhatAuthorizeSees(t *testing.T) {
	t.Parallel()
	raw := auth.SessionACL{Owner: "owner@example.com", Contributors: []string{" oncall@example.com "}}
	caller := auth.Caller{Identity: "oncall@example.com"}
	if auth.Authorize(caller, auth.ActionSessionWrite, raw) {
		t.Fatal("precondition: an untrimmed identity is expected to be denied — that is the bug Normalized prevents")
	}
	if !auth.Authorize(caller, auth.ActionSessionWrite, raw.Normalized()) {
		t.Error("after Normalized the contributor must be allowed")
	}
}
