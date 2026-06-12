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

package auth

// Action enumerates the authorization decisions the multi-session
// attach layer makes. The matrix is intentionally small in α.1; finer
// scoping (per-tool, per-MCP-server, per-session-sub-area) is deferred
// per docs/multi-session-design.md §"Out of scope".
//
// Handlers don't enforce these in α.1 — enforcement wiring is α.2.
// The function is here so the type surface is settled before the
// handlers wrap on top.
type Action int

const (
	// ActionSessionList is the GET /sessions endpoint. Always
	// permitted; the handler filters results to sessions the Caller
	// can actually read.
	ActionSessionList Action = iota
	// ActionSessionRead covers reading session state — events
	// stream, status, tools, memory, permissions, etc.
	ActionSessionRead
	// ActionSessionWrite covers mutating endpoints — inject,
	// wake, interrupt, slash commands.
	ActionSessionWrite
	// ActionSessionAdmin covers ACL / metadata mutations on the
	// session — modifying SessionACL, deleting the session.
	ActionSessionAdmin
	// ActionDaemonAdmin covers daemon-scoped read/write — peer
	// registry, global metrics, anything not bound to a single
	// session. Admin-only.
	ActionDaemonAdmin
)

// String returns the action name for diagnostics / audit logs.
func (a Action) String() string {
	switch a {
	case ActionSessionList:
		return "session.list"
	case ActionSessionRead:
		return "session.read"
	case ActionSessionWrite:
		return "session.write"
	case ActionSessionAdmin:
		return "session.admin"
	case ActionDaemonAdmin:
		return "daemon.admin"
	}
	return "unknown"
}

// SessionACL captures who can do what to a session. Owner is the
// creator (full access). Viewers can read; Contributors can read +
// write but not modify the ACL itself.
//
// Owner may be a synthetic identity like "channel:#incident-response"
// for shared-session deployments where the session belongs to a group
// rather than a single user.
//
// Zero value (empty Owner, nil slices) grants nothing to any
// non-Admin Caller — this is the safe default when a session was
// created before multi-session was enabled (legacy sessions become
// admin-only-accessible until the operator assigns an Owner).
type SessionACL struct {
	Owner        string
	Viewers      []string
	Contributors []string
}

// Authorize reports whether c may perform a against the given session
// ACL. The matrix follows docs/multi-session-design.md §"Authorization
// rules":
//
//	| Action          | Admin | Owner | Viewers | Contributors |
//	|-----------------|-------|-------|---------|--------------|
//	| SessionList     |   ✓   |   ✓   |    ✓    |       ✓      |
//	| SessionRead     |   ✓   |   ✓   |    ✓    |       ✓      |
//	| SessionWrite    |   ✓   |   ✓   |         |       ✓      |
//	| SessionAdmin    |   ✓   |   ✓   |         |              |
//	| DaemonAdmin     |   ✓   |       |         |              |
//
// SessionList is always permitted at the API layer; handlers filter
// results per-Caller (hiding the existence of unauthorized sessions
// prevents leaking activity patterns).
func Authorize(c Caller, a Action, acl SessionACL) bool {
	if c.Admin {
		return true
	}
	// SessionList is the one action permitted before per-identity
	// checks: handlers filter results separately, so the API can
	// safely return "always allowed" here.
	if a == ActionSessionList {
		return true
	}
	// An empty Caller identity is the zero value — a context that
	// never went through the authenticator middleware, or a misrouted
	// internal call. The matching empty Owner / Viewers / Contributors
	// entries on a half-initialized ACL would otherwise grant access
	// by accident. Safe default: deny.
	if c.Identity == "" {
		return false
	}
	switch a {
	case ActionSessionRead:
		return c.Identity == acl.Owner ||
			containsIdentity(acl.Viewers, c.Identity) ||
			containsIdentity(acl.Contributors, c.Identity)
	case ActionSessionWrite:
		return c.Identity == acl.Owner ||
			containsIdentity(acl.Contributors, c.Identity)
	case ActionSessionAdmin:
		return c.Identity == acl.Owner
	case ActionDaemonAdmin:
		return false // only Admin (handled above)
	}
	return false
}

func containsIdentity(xs []string, want string) bool {
	if want == "" {
		return false
	}
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
