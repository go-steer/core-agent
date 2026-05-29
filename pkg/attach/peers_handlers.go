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
)

// peersMaxBytes caps register / heartbeat bodies. Labels can be
// modest in size; 16 KiB is generous.
const peersMaxBytes = 16 * 1024

// peerHandlers bundles the registry the handler set needs.
type peerHandlers struct {
	reg *PeerRegistry
}

func newPeerHandlers(reg *PeerRegistry) *peerHandlers {
	return &peerHandlers{reg: reg}
}

// register wires the peer endpoints onto a mux. Called from the
// server when a PeerRegistry is configured.
func (h *peerHandlers) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /peers", h.registerPeer)
	mux.HandleFunc("GET /peers", h.listPeers)
	mux.HandleFunc("DELETE /peers/{id}", h.deregisterPeer)
	mux.HandleFunc("POST /peers/{id}/heartbeat", h.heartbeatPeer)
}

func (h *peerHandlers) registerPeer(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := readJSON(r, &req, peersMaxBytes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p, err := h.reg.Register(req)
	if err != nil {
		switch {
		case errors.Is(err, ErrPeerNameRequired),
			errors.Is(err, ErrPeerEndpointRequired):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, fmt.Sprintf("attach: register peer: %v", err), http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

func (h *peerHandlers) listPeers(w http.ResponseWriter, r *http.Request) {
	// Parse label filters: each ?label=k=v becomes a required match.
	labelMatch := parseLabelFilters(r.URL.Query()["label"])
	peers := h.reg.List(labelMatch)
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
	id := r.PathValue("id")
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
