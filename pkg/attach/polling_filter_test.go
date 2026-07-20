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
	"net/http/httptest"
	"testing"
)

// TestShouldTraceRequest pins the otelhttp filter's behavior so an
// accidental regex tweak can't silently re-flood Cloud Trace with
// polling spans (or, worse, silently drop write traffic). The table
// includes every endpoint class we care about — the polling reads we
// want suppressed, the enumeration GETs added after Cloud Trace review
// during v2.7.0 validation, and the write / streaming / admin paths
// that MUST keep tracing.
func TestShouldTraceRequest(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		want   bool
	}{
		// --- Polling reads (should be filtered, want=false) ---
		{"sessions bare enumeration", http.MethodGet, "/sessions", false},
		{"peers bare enumeration", http.MethodGet, "/peers", false},
		{"session status shortcut", http.MethodGet, "/sessions/abc/status", false},
		{"session usage shortcut", http.MethodGet, "/sessions/abc/usage", false},
		{"session context qualified", http.MethodGet, "/sessions/app/abc/context", false},
		{"session perms qualified", http.MethodGet, "/sessions/app/abc/perms", false},
		{"session skills shortcut", http.MethodGet, "/sessions/abc/skills", false},

		// --- Write paths (want=true, real work) ---
		{"inject", http.MethodPost, "/sessions/abc/inject", true},
		{"wake", http.MethodPost, "/sessions/abc/wake", true},
		{"interrupt", http.MethodPost, "/sessions/abc/interrupt", true},
		{"session creation", http.MethodPost, "/sessions", true},
		{"peer registration", http.MethodPost, "/peers", true},
		{"peer heartbeat", http.MethodPost, "/peers/p1/heartbeat", true},
		{"perms allow", http.MethodPost, "/sessions/abc/perms/allow", true},
		{"slash command", http.MethodPost, "/sessions/abc/slash/tools", true},

		// --- Admin ops (DELETE should always trace) ---
		{"session delete", http.MethodDelete, "/sessions/abc", true},
		{"peer deregister", http.MethodDelete, "/peers/p1", true},

		// --- SSE / streams (want=true, worth seeing in traces) ---
		{"events stream", http.MethodGet, "/sessions/abc/events", true},
		{"perms stream", http.MethodGet, "/sessions/abc/perms/stream", true},

		// --- Edge cases: bare-endpoint filter must NOT swallow subpaths
		//     that don't match the hydration leaf set. Better to trace an
		//     unknown read than accidentally hide one.
		{"unknown sessions subpath", http.MethodGet, "/sessions/abc/somethingnew", true},
		{"peers subpath (non-heartbeat GET)", http.MethodGet, "/peers/p1", true},

		// --- HEAD mirrors GET on the polling filter ---
		{"head on session status", http.MethodHead, "/sessions/abc/status", false},
		{"head on peers", http.MethodHead, "/peers", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if got := shouldTraceRequest(r); got != tc.want {
				t.Errorf("shouldTraceRequest(%s %s) = %v, want %v", tc.method, tc.path, got, tc.want)
			}
		})
	}
}
