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
	"context"
	"errors"
	"fmt"
	"net/http"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// Changing a running session's permission mode (#1168, protocol
// 1.16.0). The local TUI could always do it with Shift+Tab; an attached
// operator could not, short of restarting the daemon.
//
// A mode change is the most privileged thing an operator can do to a
// session — yolo removes the gate — so the route is ActionSessionAdmin:
// the session owner or a daemon admin, the same bar as editing the ACL.
// The owner may widen as well as narrow; there is no separate opt-in,
// matching the local chip. On a daemon without --multi-session there is
// no ACL to enforce and the transport credential is the whole gate, as
// it is for every other session route.

// PermModeRequest is the body of POST /sessions/{sid}/perms/mode.
type PermModeRequest struct {
	// Mode is one of RemotePermModes.
	Mode string `json:"mode"`

	// Caller is the verified identity that asked, stamped by the
	// handler from the request context and never read off the wire.
	// Empty when the daemon verified no identity, or when the change
	// came from the in-process TUI.
	Caller string `json:"-"`
}

// PermModeResponse reports the transition. Previous == Mode when the
// session was already in the requested mode; that is a 200, not an
// error, and writes no audit row.
type PermModeResponse struct {
	Previous string `json:"previous"`
	Mode     string `json:"mode"`
}

// PermModeController is the optional capability behind POST
// /perms/mode. Implementations apply the mode to the session's own
// gate and write the audit row; see attachadapter.AttachSetPermMode.
type PermModeController interface {
	AttachSetPermMode(req PermModeRequest) (PermModeResponse, error)
}

// RemotePermModes are the modes an operator surface may switch a
// session into, in the core-tui chip's cycle order. "auto" is on the
// list but a session takes it only when it can enter it
// (permissions.Gate.AutoSelectable); GET /perms' settable_modes is the
// per-session answer. "allow" (allowlist, fail everything else, never
// prompt) is a headless posture chosen in .agents/config.json and
// stays config-only.
var RemotePermModes = []permissions.Mode{
	permissions.ModeAsk,
	permissions.ModeAuto,
	permissions.ModeAcceptEdits,
	permissions.ModePlan,
	permissions.ModeYolo,
}

// ErrPermModeNotSettable rejects a mode outside RemotePermModes.
var ErrPermModeNotSettable = errors.New(`mode must be one of "ask", "auto", "acceptEdits", "plan", "yolo"; "allow" is set in .agents/config.json only`)

// ParseRemotePermMode validates s against RemotePermModes.
func ParseRemotePermMode(s string) (permissions.Mode, error) {
	for _, m := range RemotePermModes {
		if string(m) == s {
			return m, nil
		}
	}
	return "", fmt.Errorf("perms/mode: %q: %w", s, ErrPermModeNotSettable)
}

// Durable audit row for a mode change.
const (
	// PermModeEventName is the event name of a mode-change row.
	PermModeEventName = "attach-perm-mode"
	// PermModeEventAuthor authors a mode-change row.
	PermModeEventAuthor = "attach/perm-mode"
)

const (
	permModeMetaSource = "source"
	permModeMetaFrom   = "from"
	permModeMetaTo     = "to"
	permModeMetaCaller = "caller"
)

// NewPermModeAuditEvent builds the durable row recording an operator's
// mode change. It is an audit record only: a restarted or resumed
// session comes back in its configured mode, not the last one an
// operator picked, so nothing folds these rows back into state.
// identity is omitted when empty rather than written as a placeholder,
// so the row never claims an attribution the daemon did not verify.
func NewPermModeAuditEvent(identity string, from, to permissions.Mode) *session.Event {
	ev := session.NewEventWithContext(context.Background(), PermModeEventName)
	ev.Author = PermModeEventAuthor
	meta := map[string]any{
		permModeMetaSource: "operator",
		permModeMetaFrom:   string(from),
		permModeMetaTo:     string(to),
	}
	if identity != "" {
		meta[permModeMetaCaller] = identity
	}
	ev.CustomMetadata = meta
	return ev
}

// doPermMode — POST /perms/mode.
func (h *handlers) doPermMode(w http.ResponseWriter, r *http.Request, entry *Entry) {
	p, ok := entry.Agent.(PermModeController)
	if !ok {
		http.Error(w, "perms/mode capability not registered", http.StatusNotImplemented)
		return
	}
	var body PermModeRequest
	if err := decodePOST(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := ParseRemotePermMode(body.Mode); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Attribution comes from the authenticated context, like
	// perms/respond: a bare transport with no verified identity
	// records no caller rather than the anonymous placeholder.
	body.Caller = verifiedApprover(r.Context())
	resp, err := p.AttachSetPermMode(body)
	switch {
	case errors.Is(err, ErrCapabilityNotRegistered):
		http.Error(w, "perms/mode: this session has no permission gate", http.StatusNotImplemented)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
