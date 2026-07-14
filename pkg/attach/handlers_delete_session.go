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
	"fmt"
	"net/http"

	"github.com/go-steer/core-agent/pkg/auth"
)

// DELETE /sessions/{app}/{sid} and its /sessions/{sid} shortcut —
// hard-remove a session from the registry, force-hang up any
// active SSE subscribers, and delete the persisted ACL row.
//
// Auth: ActionSessionAdmin (Admin OR Owner passes).
//
// Guardrails: the bootstrap "default" session (created at daemon
// startup) is refused with 403 — deleting it via API leaves the
// daemon in a broken state (the startup agent's wake loop keeps
// running but /events + inject can't find the session, and even
// after daemon restart there's no persisted ACL row to resume
// from since the legacy Register path never persists).
//
// Not cleaned by this endpoint:
//   - Underlying ADK/SQLite session records — pkg/eventlog owns
//     that lifecycle and doesn't expose Delete on Handle today.
//     Removed from the registry ⇒ unreachable via /sessions API,
//     which is what operators expect ("gone from the list").
//
// Returns 204 No Content on success. 404 masks not-found +
// auth-deny per the operator-endpoint convention (hides session
// existence from unauthorized callers).

// defaultBootstrapSessionID is the daemon startup session's
// SessionID. Mirrors agent.defaultSessionID; hardcoded here to
// avoid pkg/attach → pkg/agent import cycle.
const defaultBootstrapSessionID = "default"

func (h *handlers) deleteSessionQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupQualifiedAuth(w, r, auth.ActionSessionAdmin)
	if !ok {
		return
	}
	h.doDeleteSession(w, r, entry)
}

func (h *handlers) deleteSessionShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupShortcutAuth(w, r, auth.ActionSessionAdmin)
	if !ok {
		return
	}
	h.doDeleteSession(w, r, entry)
}

// doDeleteSession is the shared body for the two route handlers.
// Auth + entry lookup already ran. Order:
//  1. Guard bootstrap session (403).
//  2. Close per-entry broadcaster (SSE subscribers get channel-
//     closed signals so they can 200-then-EOF their responses).
//  3. HardDelete on the registry (removes in-memory entry, fires
//     cancelOnEvict, deletes persisted ACL row).
//  4. 204 No Content on success.
func (h *handlers) doDeleteSession(w http.ResponseWriter, r *http.Request, entry *Entry) {
	if entry.SessionID == defaultBootstrapSessionID {
		http.Error(w, "the bootstrap 'default' session is protected; restart the daemon to reset it", http.StatusForbidden)
		return
	}
	if b := h.pool.Remove(entry); b != nil {
		b.Close()
	}
	if err := h.reg.HardDelete(r.Context(), entry.AppName, entry.UserID, entry.SessionID); err != nil {
		http.Error(w, fmt.Sprintf("delete session: %v", err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
