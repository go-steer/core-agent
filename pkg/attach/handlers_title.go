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

// POST /sessions/{app}/{sid}/title and its /sessions/{sid} shortcut —
// the manual rename half of #808.
//
// Titles ship inferred: the agent derives one from the session's first
// prompt so the picker stops being a list of IDs. Inference is right
// often enough to be worth doing and wrong often enough that a name the
// operator can see is wrong and cannot change is a worse deal than no
// name at all — go-steer/core-tui#163 asked for the override in the same
// breath as the feature. This is that override.
//
// ActionSessionWrite, not ActionSessionAdmin. A title is a display
// label, not an authorization fact: it grants nothing, reveals nothing
// the row didn't already carry, and the people who should be able to fix
// a wrong name are the people working in the session. Contributors can
// already /inject, which is a strictly larger power than renaming.
//
// Not cost-limited: it is one map read and one column write, with no
// model call anywhere on the path — unlike the automatic titling this
// overrides, which is why that one is bounded and this one isn't.

// SessionTitleRequest is the POST body of /sessions/{sid}/title.
type SessionTitleRequest struct {
	// Title is a pointer so an omitted key and an empty string stay
	// distinguishable. Omitted is a request that says nothing and is
	// refused; "" is the operator clearing a bad name, which is a real
	// instruction and is honoured. readJSON tolerates unknown fields,
	// so a typo'd key would otherwise land as a silent no-op 200.
	Title *string `json:"title"`
}

// SessionTitleResponse is the 200 body of POST /sessions/{sid}/title.
type SessionTitleResponse struct {
	Session string `json:"session"`
	// Title is what was STORED, after the registrant's normalization —
	// not what was sent. A 60-rune cap and a decorative-quote strip
	// mean the two differ, and the caller is entitled to know which
	// one the picker will show.
	Title string `json:"title,omitempty"`
	// Persisted reports whether the new name reached the durable
	// session row, i.e. whether it survives a restart.
	//
	// False is not an error and is in fact the norm for the
	// single-session --attach-listen daemon, whose sessions have no
	// ACL row at all: the title is live for as long as the process is.
	// False WITH a Detail is the other case — a store that was wired
	// and failed. Both get a 200, because the rename did take effect;
	// what would be wrong is reporting an unqualified success for a
	// name that quietly reverts at the next restart.
	Persisted bool `json:"persisted"`
	// Detail explains a Persisted=false that the caller should care
	// about. Empty when there was simply nowhere durable to write.
	Detail string `json:"detail,omitempty"`
}

func (h *handlers) registerSessionTitle(mux *http.ServeMux) {
	h.routeSession(mux, "POST", "title", auth.ActionSessionWrite, h.doSetSessionTitle)
}

func (h *handlers) doSetSessionTitle(w http.ResponseWriter, r *http.Request, entry *Entry) {
	setter, ok := entry.Agent.(SessionTitleSetter)
	if !ok {
		http.Error(w, "title capability not registered", http.StatusNotImplemented)
		return
	}
	var req SessionTitleRequest
	if err := decodePOST(r, &req); err != nil {
		http.Error(w, fmt.Sprintf("title: %v", err), http.StatusBadRequest)
		return
	}
	if req.Title == nil {
		http.Error(w, `title: title is required (send {"title":""} to clear it)`, http.StatusBadRequest)
		return
	}

	setter.SetSessionTitle(*req.Title)
	// Read back rather than echo: the registrant normalizes, and this
	// response is the client's only account of what the picker will
	// show. It also catches a setter that declined the write.
	stored := setter.SessionTitle()

	resp := SessionTitleResponse{Session: entry.SessionID, Title: stored}
	switch err := h.reg.PersistTitle(r.Context(), entry.AppName, entry.UserID, entry.SessionID, stored); {
	case err == nil:
		resp.Persisted = true
	case errors.Is(err, ErrSessionACLNotFound):
		// No durable row for this session. Expected, not a failure.
	default:
		// The rename is live and won't survive a restart. Say so in
		// the body instead of failing: rolling the in-memory title
		// back would throw away a change that did happen, and the
		// eviction write-through would try to persist it again later
		// anyway, so a rollback wouldn't even be the last word.
		resp.Detail = fmt.Sprintf("title set for this process only — persisting it failed: %v", err)
	}
	writeJSON(w, http.StatusOK, resp)
}
