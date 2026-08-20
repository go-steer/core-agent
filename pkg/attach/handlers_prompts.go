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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-steer/core-agent/v2/pkg/auth"
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
	h.routeSession(mux, "GET", "perms/stream", auth.ActionSessionRead, h.doPermsStream)
	h.routeSession(mux, "POST", "perms/respond", auth.ActionSessionWrite, h.doPermsRespond)
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
		case <-h.closing:
			// Server shutdown: end the stream so srv.Shutdown can
			// drain instead of waiting out its full timeout on a
			// client that would otherwise stay attached (#488). The
			// client sees EOF and re-subscribes after reconnect.
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
	approver := verifiedApprover(r.Context())
	if req.Approver != "" && req.Approver != approver {
		if approver == "" {
			http.Error(w, fmt.Sprintf("perms/respond: cannot attribute this decision to %q — this daemon verified no identity for the request, so the approval would be recorded on the strength of the body alone. Front the daemon with the asserted-caller header (X-Asserted-Caller) or enable per-caller auth, then omit the field", req.Approver), http.StatusBadRequest)
			return
		}
		http.Error(w, fmt.Sprintf("perms/respond: approver %q does not match the verified caller %q; omit the field and the server attributes the decision itself", req.Approver, approver), http.StatusBadRequest)
		return
	}
	if err := broker.RespondAs(req.ID, decision, approver); err != nil {
		if errors.Is(err, ErrPromptNotFound) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, PromptRespondResponse{Acknowledged: true, Approver: approver})
}

// verifiedApprover returns the identity to attribute a permission
// decision to, or "" when the server has nothing it verified.
//
// The identity is the caller-resolution middleware's verdict, the same
// one /whoami reports — never a header re-read here and never a name
// from the request body. An anonymous request contributes nothing even
// though it carries an identity string: the daemon's configured default
// ("anon") is a placeholder, and writing a placeholder into an audit
// line makes an unattributed approval indistinguishable from an
// attributed one, which is the failure the audit line exists to
// prevent. Empty is the honest answer, and the endpoint still works —
// it just records what it can prove.
func verifiedApprover(ctx context.Context) string {
	source, ok := authSourceFromContext(ctx)
	if !ok || source == whoAmISourceAnonymous {
		return ""
	}
	c, _ := auth.CallerFromContext(ctx)
	return c.Identity
}
