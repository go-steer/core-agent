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

import "testing"

// TestResolveSessionDB is the #973 gate. The rows marked "fails-first"
// are the ones that fail against pre-#973 code, where attach mode did
// not imply a session DB — it *rejected* the run at the end of boot
// unless the operator had passed --session-db by hand.
//
// The two properties worth defending are in tension, so they are both
// asserted rather than described: attach mode always ends up with a
// durable log, and an operator who named a path still gets that path.
func TestResolveSessionDB(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		sessionDB    bool
		sessionDBSet bool
		path         string
		listen       string
		unixSocket   string
		wantEnabled  bool
		wantImplied  bool
		wantConflict bool
	}{
		{
			// Pre-#973 this combination exited ExitConfigError.
			name:        "attach tcp with no flags implies a durable db (fails-first)",
			listen:      "127.0.0.1:7777",
			wantEnabled: true,
			wantImplied: true,
		},
		{
			// The unix-socket door was gated by the same check.
			name:        "attach unix socket with no flags implies a durable db (fails-first)",
			unixSocket:  "/tmp/core-agent.sock",
			wantEnabled: true,
			wantImplied: true,
		},
		{
			// The implication must never relocate a database. An
			// operator who names a path has already answered the
			// question the implication exists to answer.
			name:        "attach with an explicit path keeps the path and claims nothing",
			path:        "/var/lib/core-agent/sessions.db",
			listen:      "127.0.0.1:7777",
			wantEnabled: false, // --session-db-path alone drives the branch
			wantImplied: false,
		},
		{
			name:        "attach with an explicit --session-db is not implied",
			sessionDB:   true,
			listen:      "127.0.0.1:7777",
			wantEnabled: true,
			wantImplied: false,
		},
		{
			// Nothing is implied outside attach mode: an interactive
			// REPL that wanted durability would have asked, and
			// silently creating a database in $HOME is exactly the
			// surprise #973 objects to when it is not a precondition.
			name:        "no attach and no flags stays off",
			wantEnabled: false,
			wantImplied: false,
		},
		{
			name:        "no attach with --session-db is on but not implied",
			sessionDB:   true,
			wantEnabled: true,
			wantImplied: false,
		},
		{
			name:        "no attach with only a path leaves the bool alone",
			path:        "/var/lib/core-agent/sessions.db",
			wantEnabled: false,
			wantImplied: false,
		},
		{
			// The implication must not overrule a stated "no". A
			// plain bool reads identically whether the operator
			// never mentioned the flag or typed =false, so this row
			// is the reason the decision needs flag.Visit at all.
			name:         "attach with an explicit --session-db=false is a config error",
			sessionDBSet: true,
			listen:       "127.0.0.1:7777",
			wantEnabled:  false,
			wantImplied:  false,
			wantConflict: true,
		},
		{
			// Naming a path is itself a request for a durable log,
			// so it settles the question rather than conflicting.
			name:         "explicit --session-db=false alongside a path is not a conflict",
			sessionDBSet: true,
			path:         "/var/lib/core-agent/sessions.db",
			listen:       "127.0.0.1:7777",
			wantEnabled:  false,
			wantImplied:  false,
		},
		{
			// Nothing to conflict with: no attach shape, no
			// precondition, and "off" is a perfectly good answer.
			name:         "explicit --session-db=false without attach is just off",
			sessionDBSet: true,
			wantEnabled:  false,
			wantImplied:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			enabled, implied, conflict := resolveSessionDB(
				tc.sessionDB, tc.sessionDBSet, tc.path, tc.listen, tc.unixSocket)
			if conflict != tc.wantConflict {
				t.Errorf("conflict = %v, want %v", conflict, tc.wantConflict)
			}
			if enabled != tc.wantEnabled {
				t.Errorf("enabled = %v, want %v", enabled, tc.wantEnabled)
			}
			if implied != tc.wantImplied {
				t.Errorf("implied = %v, want %v", implied, tc.wantImplied)
			}
			// An implied DB that isn't enabled would print the
			// "[implied by attach mode]" suffix on a line that never
			// runs — assert the pairing directly so a future edit
			// cannot drift the two apart.
			if implied && !enabled {
				t.Error("implied without enabled: the startup line would lie")
			}
		})
	}
}

// TestResolveSessionDBAttachAlwaysDurable states the #973 invariant on
// its own, separately from the table, because it is the one property
// the attach HTTP surface depends on: /events replays from the event
// log, subagent history reads it, and the broadcaster pumps from it.
// If any attach shape can reach steady state without a durable log,
// those endpoints answer from nothing.
func TestResolveSessionDBAttachAlwaysDurable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		sessionDB  bool
		path       string
		listen     string
		unixSocket string
	}{
		{name: "tcp, nothing set", listen: "127.0.0.1:7777"},
		{name: "unix, nothing set", unixSocket: "/tmp/ca.sock"},
		{name: "both doors, nothing set", listen: "127.0.0.1:7777", unixSocket: "/tmp/ca.sock"},
		{name: "tcp with explicit bool", sessionDB: true, listen: "127.0.0.1:7777"},
		{name: "tcp with explicit path", path: "/tmp/x.db", listen: "127.0.0.1:7777"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			enabled, _, conflict := resolveSessionDB(
				tc.sessionDB, false, tc.path, tc.listen, tc.unixSocket)
			if conflict {
				t.Fatal("no row here disables the session db; conflict is a bug")
			}
			// This mirrors the call site's branch condition exactly:
			// the eventlog is opened when either is true.
			if !enabled && tc.path == "" {
				t.Fatal("attach mode reached steady state with no durable session db")
			}
		})
	}
}
