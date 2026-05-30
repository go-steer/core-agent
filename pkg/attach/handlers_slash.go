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

import "net/http"

// Slash dispatchers — POST /sessions/<sid>/slash/<name>. Wire to
// agent.Compact / agent.Checkpoint / agent.AskSideQuestion /
// BackgroundAgentManager.Spawn synchronously: the operator's HTTP
// POST blocks until the operation completes (5–30s typical).
//
// The remote TUI renders the in-chat preamble at dispatch (same as
// the in-process AsyncSlashProviderWithPreamble path) so the
// operator gets immediate feedback while the round-trip is in
// flight. We don't try to deliver the result via SSE for v1; the
// HTTP response body carries it and the in-flight preamble + the
// result row are enough operator UX without adding a job-correlation
// protocol.

// slashCompactQualified / Shortcut — POST /slash/compact.
func (h *handlers) slashCompactQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveQualified(w, r)
	if !ok {
		return
	}
	h.doSlashCompact(w, r, entry)
}

func (h *handlers) slashCompactShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveShortcut(w, r)
	if !ok {
		return
	}
	h.doSlashCompact(w, r, entry)
}

func (h *handlers) doSlashCompact(w http.ResponseWriter, r *http.Request, entry *Entry) {
	p, ok := entry.Agent.(CompactSlashProvider)
	if !ok {
		http.Error(w, "compact capability not registered", http.StatusNotImplemented)
		return
	}
	// Body is optional ({focus?}). Empty body is allowed — default focus.
	var req CompactRequest
	if r.ContentLength > 0 {
		if err := decodePOST(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	resp, err := p.AttachCompact(r.Context(), req.Focus)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// slashDoneQualified / Shortcut — POST /slash/done.
func (h *handlers) slashDoneQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveQualified(w, r)
	if !ok {
		return
	}
	h.doSlashDone(w, r, entry)
}

func (h *handlers) slashDoneShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveShortcut(w, r)
	if !ok {
		return
	}
	h.doSlashDone(w, r, entry)
}

func (h *handlers) doSlashDone(w http.ResponseWriter, r *http.Request, entry *Entry) {
	p, ok := entry.Agent.(CheckpointSlashProvider)
	if !ok {
		http.Error(w, "checkpoint capability not registered", http.StatusNotImplemented)
		return
	}
	var req CheckpointRequest
	if r.ContentLength > 0 {
		if err := decodePOST(r, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	resp, err := p.AttachCheckpoint(r.Context(), req.Note)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// slashBtwQualified / Shortcut — POST /slash/btw.
func (h *handlers) slashBtwQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveQualified(w, r)
	if !ok {
		return
	}
	h.doSlashBtw(w, r, entry)
}

func (h *handlers) slashBtwShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveShortcut(w, r)
	if !ok {
		return
	}
	h.doSlashBtw(w, r, entry)
}

func (h *handlers) doSlashBtw(w http.ResponseWriter, r *http.Request, entry *Entry) {
	p, ok := entry.Agent.(SideQueryProvider)
	if !ok {
		http.Error(w, "side-query capability not registered", http.StatusNotImplemented)
		return
	}
	var req SideQueryRequest
	if err := decodePOST(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Question == "" {
		http.Error(w, "question: required", http.StatusBadRequest)
		return
	}
	answer, err := p.AttachAskSideQuestion(r.Context(), req.Question)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, SideQueryResponse{Answer: answer})
}

// slashSubagentQualified / Shortcut — POST /slash/subagent.
func (h *handlers) slashSubagentQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveQualified(w, r)
	if !ok {
		return
	}
	h.doSlashSubagent(w, r, entry)
}

func (h *handlers) slashSubagentShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveShortcut(w, r)
	if !ok {
		return
	}
	h.doSlashSubagent(w, r, entry)
}

func (h *handlers) doSlashSubagent(w http.ResponseWriter, r *http.Request, entry *Entry) {
	p, ok := entry.Agent.(SubagentSpawner)
	if !ok {
		http.Error(w, "subagent spawn capability not registered", http.StatusNotImplemented)
		return
	}
	var spec SubagentSpec
	if err := decodePOST(r, &spec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if spec.Name == "" {
		http.Error(w, "name: required", http.StatusBadRequest)
		return
	}
	if spec.Goal == "" {
		http.Error(w, "goal: required", http.StatusBadRequest)
		return
	}
	resp, err := p.AttachSpawnSubagent(r.Context(), spec)
	if err != nil {
		// ErrSubagentSpawnerUnavailable from the agent side surfaces
		// as 501 (capability not actually registered at runtime even
		// though the interface check passed — e.g., no
		// WithBackgroundManager was wired). Other errors are 500s.
		if isSubagentSpawnerUnavailable(err) {
			http.Error(w, "subagent spawn not registered (no BackgroundAgentManager wired)", http.StatusNotImplemented)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// isSubagentSpawnerUnavailable matches the agent-side sentinel by
// string compare so pkg/attach doesn't import pkg/agent (would
// create a cycle). The sentinel text is stable; the agent package's
// ErrSubagentSpawnerUnavailable.Error() returns the literal we match
// here.
func isSubagentSpawnerUnavailable(err error) bool {
	return err != nil && err.Error() == "agent: subagent spawner unavailable (no BackgroundAgentManager wired)"
}
