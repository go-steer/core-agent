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

// Operator-state read endpoints — feed the remote TUI's slash
// commands that mirror operator-visible agent state: /stats, /context,
// /memory, /skills, /mcp, /pricing. Each handler type-asserts on the
// corresponding capability interface; agents that don't implement it
// receive 200 with empty / zero-value data (the same convention
// /tools, /agents, /status follow — keeps client code simple by
// avoiding a separate "capability not registered" path).
//
// All read-only; safe under ReadOnly server mode (which gates POSTs
// only).

func (h *handlers) registerOperatorState(mux *http.ServeMux) {
	// Qualified two-segment form.
	mux.HandleFunc("GET /sessions/{app}/{sid}/usage", h.usageQualified)
	mux.HandleFunc("GET /sessions/{app}/{sid}/context", h.contextQualified)
	mux.HandleFunc("GET /sessions/{app}/{sid}/memory", h.memoryQualified)
	mux.HandleFunc("GET /sessions/{app}/{sid}/skills", h.skillsQualified)
	mux.HandleFunc("GET /sessions/{app}/{sid}/mcp", h.mcpQualified)
	mux.HandleFunc("GET /sessions/{app}/{sid}/pricing", h.pricingQualified)

	// Single-segment shortcut.
	mux.HandleFunc("GET /sessions/{sid}/usage", h.usageShortcut)
	mux.HandleFunc("GET /sessions/{sid}/context", h.contextShortcut)
	mux.HandleFunc("GET /sessions/{sid}/memory", h.memoryShortcut)
	mux.HandleFunc("GET /sessions/{sid}/skills", h.skillsShortcut)
	mux.HandleFunc("GET /sessions/{sid}/mcp", h.mcpShortcut)
	mux.HandleFunc("GET /sessions/{sid}/pricing", h.pricingShortcut)
}

func (h *handlers) usageQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveQualified(w, r)
	if !ok {
		return
	}
	h.doUsage(w, entry)
}

func (h *handlers) usageShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveShortcut(w, r)
	if !ok {
		return
	}
	h.doUsage(w, entry)
}

func (h *handlers) doUsage(w http.ResponseWriter, entry *Entry) {
	out := UsageInfo{}
	if p, ok := entry.Agent.(UsageProvider); ok {
		out = p.AttachUsage()
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) contextQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveQualified(w, r)
	if !ok {
		return
	}
	h.doContext(w, entry)
}

func (h *handlers) contextShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveShortcut(w, r)
	if !ok {
		return
	}
	h.doContext(w, entry)
}

func (h *handlers) doContext(w http.ResponseWriter, entry *Entry) {
	out := ContextInfo{}
	if p, ok := entry.Agent.(ContextProvider); ok {
		out = p.AttachContext()
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) memoryQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveQualified(w, r)
	if !ok {
		return
	}
	h.doMemory(w, entry)
}

func (h *handlers) memoryShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveShortcut(w, r)
	if !ok {
		return
	}
	h.doMemory(w, entry)
}

func (h *handlers) doMemory(w http.ResponseWriter, entry *Entry) {
	out := []MemorySource{}
	if p, ok := entry.Agent.(MemoryProvider); ok {
		if list := p.AttachMemory(); list != nil {
			out = list
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

func (h *handlers) skillsQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveQualified(w, r)
	if !ok {
		return
	}
	h.doSkills(w, entry)
}

func (h *handlers) skillsShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveShortcut(w, r)
	if !ok {
		return
	}
	h.doSkills(w, entry)
}

func (h *handlers) doSkills(w http.ResponseWriter, entry *Entry) {
	out := []SkillInfo{}
	if p, ok := entry.Agent.(SkillsProvider); ok {
		if list := p.AttachSkills(); list != nil {
			out = list
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

func (h *handlers) mcpQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveQualified(w, r)
	if !ok {
		return
	}
	h.doMCP(w, entry)
}

func (h *handlers) mcpShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveShortcut(w, r)
	if !ok {
		return
	}
	h.doMCP(w, entry)
}

func (h *handlers) doMCP(w http.ResponseWriter, entry *Entry) {
	out := MCPInfo{Servers: []MCPServerInfo{}}
	if p, ok := entry.Agent.(MCPProvider); ok {
		info := p.AttachMCP()
		if info.Servers == nil {
			info.Servers = []MCPServerInfo{}
		}
		out = info
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) pricingQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveQualified(w, r)
	if !ok {
		return
	}
	h.doPricing(w, entry)
}

func (h *handlers) pricingShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveShortcut(w, r)
	if !ok {
		return
	}
	h.doPricing(w, entry)
}

func (h *handlers) doPricing(w http.ResponseWriter, entry *Entry) {
	out := PricingInfo{}
	if p, ok := entry.Agent.(PricingProvider); ok {
		out = p.AttachPricing()
	}
	writeJSON(w, http.StatusOK, out)
}

// resolveQualified DRYs the {app}/{sid} lookup + error response
// shared by all qualified-form handlers. Returns the resolved entry
// and ok=true on success; on lookup failure, writes the error and
// returns ok=false (caller should just return).
func (h *handlers) resolveQualified(w http.ResponseWriter, r *http.Request) (*Entry, bool) {
	app := r.PathValue("app")
	sid := r.PathValue("sid")
	entry, err := h.reg.Lookup(app, sid)
	if err != nil {
		writeLookupError(w, err)
		return nil, false
	}
	return entry, true
}

// resolveShortcut is the single-segment-shortcut counterpart of
// resolveQualified. Returns 409 on ambiguous SessionID.
func (h *handlers) resolveShortcut(w http.ResponseWriter, r *http.Request) (*Entry, bool) {
	sid := r.PathValue("sid")
	entry, err := h.reg.LookupSingle(sid)
	if err != nil {
		writeLookupError(w, err)
		return nil, false
	}
	return entry, true
}
