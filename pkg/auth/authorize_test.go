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
	"testing"

	"github.com/go-steer/core-agent/pkg/auth"
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
