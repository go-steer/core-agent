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

// GET + PATCH /sessions/{app}/{sid}/acl and their /sessions/{sid}
// shortcuts — read and amend who else may reach a session (#797).
//
// Contributors were honoured by Authorize, carried on the Entry and
// persisted on the session row from the day multi-session shipped, and
// no caller could put an identity in one: POST /sessions stamped the
// creator as Owner and nothing else, and there was no ACL verb at all.
// The capability existed everywhere except at the API boundary.
//
// What it unblocks is agent-initiated escalation. A watcher opens its
// own session when it detects a problem and pages a human; the human's
// reply comes back through a gateway that asserts *their* identity, not
// the watcher's, so it fails ActionSessionWrite against a session whose
// only ACL entry is the watcher. Contributors is the least-privilege
// answer — the creator knows who should be able to answer — and the
// failure it replaces is invisible from the client side, because a
// denial is deliberately 404 rather than 403 (see authorize) and reads
// downstream as "that session is gone".
//
// Both verbs are gated on ActionSessionAdmin, i.e. Owner or Admin.
// The read is gated as hard as the write on purpose: the ACL names
// the other people who can see an incident, and a Contributor being
// able to enumerate their co-responders is a disclosure the matrix
// doesn't otherwise grant.

// sessionACLResponse is the JSON body of GET /sessions/{sid}/acl and
// the 200 from a successful PATCH.
//
// The two lists are never omitted, even when empty: this is the
// endpoint whose entire purpose is reporting who is on the ACL, and a
// client that had to treat a missing key and an empty list alike would
// be back to guessing. PATCH echoes the stored result rather than 204
// so a caller can see what normalization did to what it sent without a
// second round trip.
type sessionACLResponse struct {
	Owner        string   `json:"owner"`
	Viewers      []string `json:"viewers"`
	Contributors []string `json:"contributors"`
}

func aclResponse(acl auth.SessionACL) sessionACLResponse {
	out := sessionACLResponse{
		Owner:        acl.Owner,
		Viewers:      acl.Viewers,
		Contributors: acl.Contributors,
	}
	// Normalized() reports an absent list as nil, which marshals to
	// `null`. Empty is what the caller means and `[]` is what a client
	// can iterate without a nil check.
	if out.Viewers == nil {
		out.Viewers = []string{}
	}
	if out.Contributors == nil {
		out.Contributors = []string{}
	}
	return out
}

// sessionACLRequest is the PATCH /sessions/{sid}/acl body, and the
// optional POST /sessions body.
//
// The list fields are *[]string rather than []string so that absent
// and empty are distinguishable, which is what makes this a PATCH: a
// field the caller didn't send is left alone, and `[]` clears the list.
// Without the pointer, `{"contributors":["responder"]}` would silently
// wipe the viewers the operator set last week.
//
// Owner is accepted only so it can be REFUSED with a reason. A caller
// that sends the wrong owner has a mistaken model of what this
// endpoint does, and dropping the field on the floor would let it go
// on believing that model — the same silent-failure shape #797 was
// filed about.
type sessionACLRequest struct {
	Owner        *string   `json:"owner,omitempty"`
	Viewers      *[]string `json:"viewers,omitempty"`
	Contributors *[]string `json:"contributors,omitempty"`
}

// apply overlays the request onto the current ACL, leaving any field
// the caller omitted untouched.
//
// Pure, and cheap enough to run under the registry lock — that is the
// point of its shape. PATCH is a read-modify-write, and doing the read
// in the handler would let two concurrent PATCHes each carry the
// other's omitted list forward from a stale snapshot, silently losing
// one edit to an authorization decision. AmendACL runs this against
// the live ACL instead.
//
// A mismatched Owner is left in the result rather than rejected here,
// so that the comparison happens against the same live ACL under the
// same lock; AmendACL turns it into ErrACLOwnerNotTransferable.
func (req sessionACLRequest) apply(current auth.SessionACL) auth.SessionACL {
	next := current
	if req.Owner != nil {
		next.Owner = *req.Owner
	}
	if req.Viewers != nil {
		next.Viewers = *req.Viewers
	}
	if req.Contributors != nil {
		next.Contributors = *req.Contributors
	}
	return next
}

// registerSessionACL wires the ACL read + amend endpoints under both
// URL forms. Both are ActionSessionAdmin; neither is cost-limited
// (a map write and one SQLite upsert) and neither is drain-gated
// (an ACL edit during shutdown persists and survives the restart —
// unlike an inject, whose queued message would be lost).
func (h *handlers) registerSessionACL(mux *http.ServeMux) {
	h.routeSession(mux, "GET", "acl", auth.ActionSessionAdmin, h.doGetSessionACL)
	h.routeSession(mux, "PATCH", "acl", auth.ActionSessionAdmin, h.doPatchSessionACL)
}

func (h *handlers) doGetSessionACL(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	writeJSON(w, http.StatusOK, aclResponse(entry.CurrentACL()))
}

func (h *handlers) doPatchSessionACL(w http.ResponseWriter, r *http.Request, entry *Entry) {
	var req sessionACLRequest
	if err := decodePOST(r, &req); err != nil {
		http.Error(w, fmt.Sprintf("acl: %v", err), http.StatusBadRequest)
		return
	}
	// An explicitly empty owner has to be caught here: AmendACL reads a
	// zero Owner as "the mutator didn't touch it", so it would silently
	// succeed as a no-op and let the caller believe it un-owned the
	// session. Same silent-failure shape #797 is about.
	if req.Owner != nil && *req.Owner == "" {
		http.Error(w, "acl: owner cannot be cleared; send the current owner or omit the field", http.StatusBadRequest)
		return
	}
	stored, err := h.reg.AmendACL(r.Context(), entry.AppName, entry.UserID, entry.SessionID, req.apply)
	if err != nil {
		if errors.Is(err, ErrACLOwnerNotTransferable) {
			http.Error(w, fmt.Sprintf("acl: %v", err), http.StatusBadRequest)
			return
		}
		if errors.Is(err, ErrSessionNotFound) {
			// Evicted between the route's lookup and here. 404 is
			// both accurate and the same answer an unauthorized
			// caller gets, so it leaks nothing new.
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("acl: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, aclResponse(stored))
}
