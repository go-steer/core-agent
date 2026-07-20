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
	"errors"
	"fmt"
	"net/http"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// createSessionResponse is the JSON body returned by POST /sessions
// on success. AppName + SessionID identify the new session; URL is
// the per-session base URL the client should subsequently address
// (events / inject / status / etc. live underneath it).
type createSessionResponse struct {
	AppName   string `json:"app"`
	UserID    string `json:"user"`
	SessionID string `json:"sessionID"`
	URL       string `json:"url"`
}

// createSession is the POST /sessions handler. The endpoint is the
// programmatic counterpart to the TUI's "+ New session" picker row
// (cmd/core-agent-tui) — a multi-session-aware operator (or chat-bot)
// creates a fresh session owned by the authenticated caller and
// receives back the session triple + URL to attach to.
//
// Authorization: every authenticated caller may create their own
// session. The Owner of the resulting ACL is the caller's identity,
// so SessionWrite / SessionAdmin on the new session work without
// further configuration. Anonymous callers (Caller.Identity == "")
// are rejected with 401 — an unowned session is not creatable via
// this path (legacy Register exists for the daemon's own startup).
//
// 501 when SessionFactory is not configured (older deployments that
// don't support on-demand creation). 500 when the factory itself
// errors. 409 when the factory returns a Registrant whose
// (app, user, sid) triple is already registered — a stale
// SessionID race that almost certainly means the factory's
// generator is buggy.
func (h *handlers) createSession(w http.ResponseWriter, r *http.Request) {
	if h.factory == nil {
		http.Error(w, "POST /sessions not supported: this daemon does not have a SessionFactory configured", http.StatusNotImplemented)
		return
	}
	caller, _ := auth.CallerFromContext(r.Context())
	if caller.Identity == "" {
		// Without a real identity we can't stamp an Owner, and an
		// unowned session would be admin-only-accessible (which the
		// caller couldn't even see). Better to surface the misconfig.
		http.Error(w, "POST /sessions requires an authenticated caller (none on request context)", http.StatusUnauthorized)
		return
	}

	ag, cancelOnEvict, err := h.factory(r.Context(), caller)
	if err != nil {
		http.Error(w, fmt.Sprintf("create session: %v", err), http.StatusInternalServerError)
		return
	}
	if ag == nil {
		if cancelOnEvict != nil {
			cancelOnEvict()
		}
		http.Error(w, "create session: factory returned nil Registrant", http.StatusInternalServerError)
		return
	}

	entry, err := h.reg.RegisterOwnedWithCancel(ag, caller.Identity, cancelOnEvict)
	if err != nil {
		// Registration failed — cancel the factory's wake loop so
		// it doesn't leak. Wake-loop goroutine started before this
		// handler was reached; nobody else holds the cancel.
		if cancelOnEvict != nil {
			cancelOnEvict()
		}
		if errors.Is(err, ErrSessionExists) {
			http.Error(w, fmt.Sprintf("create session: %v", err), http.StatusConflict)
			return
		}
		http.Error(w, fmt.Sprintf("create session: register: %v", err), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, createSessionResponse{
		AppName:   entry.AppName,
		UserID:    entry.UserID,
		SessionID: entry.SessionID,
		URL:       sessionURL(r, entry),
	})
}

// sessionURL returns the per-session base path for the supplied
// entry, derived from the request's Host header so the response URL
// is the one the client used to reach us (proxies, mTLS-fronted
// listeners, port-forwards all just work). Falls back to the
// qualified path when no Host is set.
func sessionURL(r *http.Request, entry *Entry) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		return fmt.Sprintf("/sessions/%s/%s", entry.AppName, entry.SessionID)
	}
	return fmt.Sprintf("%s://%s/sessions/%s/%s", scheme, host, entry.AppName, entry.SessionID)
}
