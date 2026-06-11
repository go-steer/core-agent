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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-steer/core-agent/pkg/auth"
)

// PR D — HTTP-driven permission prompts. Two endpoints:
//
//	GET  /sessions/<sid>/perms/stream    SSE stream of pending prompts
//	POST /sessions/<sid>/perms/respond   operator's decision
//
// The remote TUI's adapter subscribes to /perms/stream; each frame
// becomes a coretui.PermissionRequest displayed in the host TUI's
// modal. When the operator picks a decision, the adapter POSTs to
// /perms/respond and the daemon's blocked AskApproval call unblocks.
//
// Both routes only register when the agent satisfies
// PromptBrokerProvider. Agents without a broker get 501 for both —
// matching the "capability not registered" convention used by the
// other PR A2 mutators.

func (h *handlers) registerPrompts(mux *http.ServeMux) {
	mux.HandleFunc("GET /sessions/{app}/{sid}/perms/stream", h.permsStreamQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/perms/respond", h.permsRespondQualified)

	mux.HandleFunc("GET /sessions/{sid}/perms/stream", h.permsStreamShortcut)
	mux.HandleFunc("POST /sessions/{sid}/perms/respond", h.permsRespondShortcut)
}

func (h *handlers) permsStreamQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveQualified(w, r)
	if !ok {
		return
	}
	h.doPermsStream(w, r, entry)
}

func (h *handlers) permsStreamShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveShortcut(w, r)
	if !ok {
		return
	}
	h.doPermsStream(w, r, entry)
}

func (h *handlers) doPermsStream(w http.ResponseWriter, r *http.Request, entry *Entry) {
	provider, ok := entry.Agent.(PromptBrokerProvider)
	if !ok || provider.AttachPromptBroker() == nil {
		http.Error(w, "perms/stream capability not registered", http.StatusNotImplemented)
		return
	}
	broker := provider.AttachPromptBroker()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "server does not support streaming (no http.Flusher)", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	frames, cleanup := broker.Subscribe(r.Context())
	defer cleanup()

	for {
		select {
		case <-r.Context().Done():
			return
		case frame, ok := <-frames:
			if !ok {
				return
			}
			buf, jerr := json.Marshal(frame)
			if jerr != nil {
				continue
			}
			if _, werr := fmt.Fprintf(w, "event: prompt\ndata: %s\n\n", buf); werr != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (h *handlers) permsRespondQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupQualifiedAuth(w, r, auth.ActionSessionWrite)
	if !ok {
		return
	}
	h.doPermsRespond(w, r, entry)
}

func (h *handlers) permsRespondShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupShortcutAuth(w, r, auth.ActionSessionWrite)
	if !ok {
		return
	}
	h.doPermsRespond(w, r, entry)
}

func (h *handlers) doPermsRespond(w http.ResponseWriter, r *http.Request, entry *Entry) {
	provider, ok := entry.Agent.(PromptBrokerProvider)
	if !ok || provider.AttachPromptBroker() == nil {
		http.Error(w, "perms/respond capability not registered", http.StatusNotImplemented)
		return
	}
	broker := provider.AttachPromptBroker()

	var req PromptResponse
	if err := readJSON(r, &req, operatorPostMaxBytes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "perms/respond: id is required", http.StatusBadRequest)
		return
	}
	decision, ok := DecisionFromWire(req.Decision)
	if !ok {
		http.Error(w, fmt.Sprintf("perms/respond: unknown decision %q (want deny|allow-once|allow-session|allow-session-verb|allow-session-tool|allow-always)", req.Decision), http.StatusBadRequest)
		return
	}
	if err := broker.Respond(req.ID, decision); err != nil {
		if errors.Is(err, ErrPromptNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"acknowledged": true})
}
