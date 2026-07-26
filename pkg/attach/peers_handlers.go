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

// peersMaxBytes caps register / heartbeat bodies. Labels can be
// modest in size; 16 KiB is generous.
const peersMaxBytes = 16 * 1024

// peerHandlers bundles the registry the handler set needs plus the
// #384 authorization posture:
//
//   - requireAuth (set from Options.MultiSessionEnabled) demands an
//     authenticated, non-anonymous caller on GET /peers, POST /peers,
//     and DELETE /peers/{id} — 401 otherwise. Without multi-session
//     auth every caller shares one identity and the checks reduce to
//     the pre-#384 behavior.
//   - anonIdentity is the identity the caller middleware stamps on
//     unauthenticated requests (Options.DefaultCaller, default
//     auth.Anonymous) — the marker requireAuth screens out.
type peerHandlers struct {
	reg          *PeerRegistry
	requireAuth  bool
	anonIdentity string
}

func newPeerHandlers(reg *PeerRegistry, requireAuth bool, anonIdentity string) *peerHandlers {
	if anonIdentity == "" {
		anonIdentity = auth.Anonymous.Identity
	}
	return &peerHandlers{reg: reg, requireAuth: requireAuth, anonIdentity: anonIdentity}
}

// register wires the peer endpoints onto a mux. Called from the
// server when a PeerRegistry is configured.
func (h *peerHandlers) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /peers", h.registerPeer)
	mux.HandleFunc("GET /peers", h.listPeers)
	mux.HandleFunc("DELETE /peers/{id}", h.deregisterPeer)
	mux.HandleFunc("POST /peers/{id}/heartbeat", h.heartbeatPeer)
}

// caller resolves the request's Caller and whether it counts as
// authenticated under the handler's posture. When requireAuth is off
// the second return is always true (single-user mode — the shared
// identity is as authenticated as it gets).
func (h *peerHandlers) caller(r *http.Request) (auth.Caller, bool) {
	c, ok := auth.CallerFromContext(r.Context())
	if !h.requireAuth {
		return c, true
	}
	if !ok || c.Identity == "" || c.Identity == h.anonIdentity {
		return c, false
	}
	return c, true
}

// canManage reports whether c may see the registration ID of / delete
// the given peer: the recording owner, or an admin.
func canManage(c auth.Caller, p *Peer) bool {
	return c.Admin || c.Identity == p.Owner
}

func (h *peerHandlers) registerPeer(w http.ResponseWriter, r *http.Request) {
	c, authed := h.caller(r)
	if !authed {
		writeAttachUnauthorized(w)
		return
	}
	var req RegisterRequest
	if err := readJSON(r, &req, peersMaxBytes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := h.reg.RegisterOwned(req, c.Identity)
	if err != nil {
		switch {
		case errors.Is(err, ErrPeerNameRequired),
			errors.Is(err, ErrPeerEndpointRequired),
			errors.Is(err, ErrPeerEndpointInvalid):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, fmt.Sprintf("attach: register peer: %v", err), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *peerHandlers) listPeers(w http.ResponseWriter, r *http.Request) {
	c, authed := h.caller(r)
	if !authed {
		writeAttachUnauthorized(w)
		return
	}
	// Parse label filters: each ?label=k=v becomes a required match.
	labelMatch := parseLabelFilters(r.URL.Query()["label"])
	peers := h.reg.List(labelMatch)
	// Redact registration IDs the caller doesn't own (#384): the ID is
	// the deregistration capability, and returning it to everyone made
	// enumerate-then-delete a two-request attack. List already returns
	// defensive copies, so blanking here doesn't touch registry state.
	for _, p := range peers {
		if !canManage(c, p) {
			p.RegistrationID = ""
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": peers})
}

func (h *peerHandlers) heartbeatPeer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := h.reg.Heartbeat(id)
	if err != nil {
		if errors.Is(err, ErrPeerNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, fmt.Sprintf("heartbeat: %v", err), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (h *peerHandlers) deregisterPeer(w http.ResponseWriter, r *http.Request) {
	c, authed := h.caller(r)
	if !authed {
		writeAttachUnauthorized(w)
		return
	}
	id := r.PathValue("id")
	p, ok := h.reg.Lookup(id)
	if !ok {
		// Idempotent on unknown ids (graceful-shutdown retries). IDs
		// are unguessable and no longer enumerable by non-owners, so
		// this leaks nothing.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !canManage(c, p) {
		http.Error(w, "forbidden: peer registrations may only be deregistered by their owner or an admin", http.StatusForbidden)
		return
	}
	h.reg.Deregister(id)
	w.WriteHeader(http.StatusNoContent)
}

// parseLabelFilters turns ?label=k1=v1&label=k2=v2 query parameters
// into a map suitable for PeerRegistry.List. Entries without "=" are
// skipped silently — the registry treats an empty match as
// "match-all" so a malformed filter doesn't accidentally return
// nothing.
func parseLabelFilters(raw []string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for _, item := range raw {
		for i := 0; i < len(item); i++ {
			if item[i] == '=' {
				out[item[:i]] = item[i+1:]
				break
			}
		}
	}
	return out
}
