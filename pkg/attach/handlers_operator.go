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
	"io"
	"net/http"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

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

	// Mutation endpoints (PR A2): blocked by the ReadOnly middleware
	// at the auth layer when ReadOnly=true (any non-GET is gated).
	mux.HandleFunc("GET /sessions/{app}/{sid}/perms", h.permsQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/perms/allow", h.permsAllowQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/perms/deny", h.permsDenyQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/pricing/refresh", h.pricingRefreshQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/pricing/set", h.pricingSetQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/reload", h.reloadQualified)
	mux.HandleFunc("DELETE /sessions/{app}/{sid}", h.deleteSessionQualified)

	// Single-segment shortcut.
	mux.HandleFunc("GET /sessions/{sid}/usage", h.usageShortcut)
	mux.HandleFunc("GET /sessions/{sid}/context", h.contextShortcut)
	mux.HandleFunc("GET /sessions/{sid}/memory", h.memoryShortcut)
	mux.HandleFunc("GET /sessions/{sid}/skills", h.skillsShortcut)
	mux.HandleFunc("GET /sessions/{sid}/mcp", h.mcpShortcut)
	mux.HandleFunc("GET /sessions/{sid}/pricing", h.pricingShortcut)

	mux.HandleFunc("GET /sessions/{sid}/perms", h.permsShortcut)
	mux.HandleFunc("POST /sessions/{sid}/perms/allow", h.permsAllowShortcut)
	mux.HandleFunc("POST /sessions/{sid}/perms/deny", h.permsDenyShortcut)
	mux.HandleFunc("POST /sessions/{sid}/pricing/refresh", h.pricingRefreshShortcut)
	mux.HandleFunc("POST /sessions/{sid}/pricing/set", h.pricingSetShortcut)
	mux.HandleFunc("POST /sessions/{sid}/reload", h.reloadShortcut)
	mux.HandleFunc("DELETE /sessions/{sid}", h.deleteSessionShortcut)

	// PR A3 async slash dispatchers. All synchronous on the wire —
	// the operator stares at silence until the handler returns. The
	// in-chat preamble row is the remote TUI's responsibility (it
	// renders the same preamble at dispatch as the in-process TUI's
	// AsyncSlashProviderWithPreamble path).
	mux.HandleFunc("POST /sessions/{app}/{sid}/slash/compact", h.slashCompactQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/slash/done", h.slashDoneQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/slash/btw", h.slashBtwQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/slash/subagent", h.slashSubagentQualified)
	mux.HandleFunc("POST /sessions/{app}/{sid}/slash/replan", h.slashReplanQualified)

	mux.HandleFunc("POST /sessions/{sid}/slash/compact", h.slashCompactShortcut)
	mux.HandleFunc("POST /sessions/{sid}/slash/done", h.slashDoneShortcut)
	mux.HandleFunc("POST /sessions/{sid}/slash/btw", h.slashBtwShortcut)
	mux.HandleFunc("POST /sessions/{sid}/slash/subagent", h.slashSubagentShortcut)
	mux.HandleFunc("POST /sessions/{sid}/slash/replan", h.slashReplanShortcut)
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

// resolveQualified is a thin shim over lookupQualifiedAuth that
// preserves the legacy 2-arg call sites. New code should use
// lookupQualifiedAuth directly with an explicit action; this helper
// is retained for the operator-state read handlers where the action
// is always SessionRead and the existing call sites are dense.
func (h *handlers) resolveQualified(w http.ResponseWriter, r *http.Request) (*Entry, bool) {
	return h.lookupQualifiedAuth(w, r, auth.ActionSessionRead)
}

// resolveShortcut is the single-segment-shortcut counterpart of
// resolveQualified. Returns 409 on ambiguous SessionID. Same
// SessionRead-only convention.
func (h *handlers) resolveShortcut(w http.ResponseWriter, r *http.Request) (*Entry, bool) {
	return h.lookupShortcutAuth(w, r, auth.ActionSessionRead)
}

// ===== PR A2 mutation handlers =====
//
// Reads (perms) follow the same "200 with empty data if no provider"
// convention as PR A1 reads. Writes (perms/allow, perms/deny,
// pricing/refresh, pricing/set, reload) return 501 when the
// capability isn't registered, since the operator's POST must take
// effect or fail loudly.

const operatorPostMaxBytes = 8 * 1024

// permsQualified / permsShortcut / doPerms — GET /perms.
func (h *handlers) permsQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveQualified(w, r)
	if !ok {
		return
	}
	h.doPerms(w, entry)
}

func (h *handlers) permsShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.resolveShortcut(w, r)
	if !ok {
		return
	}
	h.doPerms(w, entry)
}

func (h *handlers) doPerms(w http.ResponseWriter, entry *Entry) {
	out := PermsInfo{}
	if p, ok := entry.Agent.(PermsProvider); ok {
		out = p.AttachPerms()
	}
	writeJSON(w, http.StatusOK, out)
}

// permsAllowQualified / permsAllowShortcut / doPermsAllow — POST /perms/allow.
func (h *handlers) permsAllowQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupQualifiedAuth(w, r, auth.ActionSessionWrite)
	if !ok {
		return
	}
	h.doPermsMutation(w, r, entry, false /* deny */)
}

func (h *handlers) permsAllowShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupShortcutAuth(w, r, auth.ActionSessionWrite)
	if !ok {
		return
	}
	h.doPermsMutation(w, r, entry, false)
}

// permsDenyQualified / permsDenyShortcut — POST /perms/deny.
func (h *handlers) permsDenyQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupQualifiedAuth(w, r, auth.ActionSessionWrite)
	if !ok {
		return
	}
	h.doPermsMutation(w, r, entry, true /* deny */)
}

func (h *handlers) permsDenyShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupShortcutAuth(w, r, auth.ActionSessionWrite)
	if !ok {
		return
	}
	h.doPermsMutation(w, r, entry, true)
}

func (h *handlers) doPermsMutation(w http.ResponseWriter, r *http.Request, entry *Entry, deny bool) {
	p, ok := entry.Agent.(PermsController)
	if !ok {
		http.Error(w, "perms controller capability not registered", http.StatusNotImplemented)
		return
	}
	var body PatternsRequest
	if err := decodePOST(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(body.Patterns) == 0 {
		http.Error(w, "patterns: empty list", http.StatusBadRequest)
		return
	}
	var err error
	if deny {
		err = p.AttachAddDeny(body.Patterns)
	} else {
		err = p.AttachAddAllow(body.Patterns)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pricingRefreshQualified / Shortcut / doPricingRefresh — POST /pricing/refresh.
func (h *handlers) pricingRefreshQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupQualifiedAuth(w, r, auth.ActionSessionWrite)
	if !ok {
		return
	}
	h.doPricingRefresh(w, r, entry)
}

func (h *handlers) pricingRefreshShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupShortcutAuth(w, r, auth.ActionSessionWrite)
	if !ok {
		return
	}
	h.doPricingRefresh(w, r, entry)
}

func (h *handlers) doPricingRefresh(w http.ResponseWriter, r *http.Request, entry *Entry) {
	p, ok := entry.Agent.(PricingController)
	if !ok {
		http.Error(w, "pricing controller capability not registered", http.StatusNotImplemented)
		return
	}
	resp, err := p.AttachRefreshPricing(r.Context())
	if errors.Is(err, ErrCapabilityNotRegistered) {
		http.Error(w, "pricing refresh not registered on this OperatorView", http.StatusNotImplemented)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// pricingSetQualified / Shortcut / doPricingSet — POST /pricing/set.
func (h *handlers) pricingSetQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupQualifiedAuth(w, r, auth.ActionSessionWrite)
	if !ok {
		return
	}
	h.doPricingSet(w, r, entry)
}

func (h *handlers) pricingSetShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupShortcutAuth(w, r, auth.ActionSessionWrite)
	if !ok {
		return
	}
	h.doPricingSet(w, r, entry)
}

func (h *handlers) doPricingSet(w http.ResponseWriter, r *http.Request, entry *Entry) {
	p, ok := entry.Agent.(PricingController)
	if !ok {
		http.Error(w, "pricing controller capability not registered", http.StatusNotImplemented)
		return
	}
	var body PricingSetRequest
	if err := decodePOST(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Model == "" {
		http.Error(w, "model: required", http.StatusBadRequest)
		return
	}
	if body.InputUSDPerMTok < 0 || body.OutputUSDPerMTok < 0 {
		http.Error(w, "rates: must be non-negative", http.StatusBadRequest)
		return
	}
	err := p.AttachSetManualPricing(body)
	if errors.Is(err, ErrCapabilityNotRegistered) {
		http.Error(w, "pricing set not registered on this OperatorView", http.StatusNotImplemented)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reloadQualified / reloadShortcut / doReload — POST /reload.
func (h *handlers) reloadQualified(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupQualifiedAuth(w, r, auth.ActionSessionWrite)
	if !ok {
		return
	}
	h.doReload(w, r, entry)
}

func (h *handlers) reloadShortcut(w http.ResponseWriter, r *http.Request) {
	entry, ok := h.lookupShortcutAuth(w, r, auth.ActionSessionWrite)
	if !ok {
		return
	}
	h.doReload(w, r, entry)
}

func (h *handlers) doReload(w http.ResponseWriter, r *http.Request, entry *Entry) {
	p, ok := entry.Agent.(Reloader)
	if !ok {
		http.Error(w, "reload capability not registered", http.StatusNotImplemented)
		return
	}
	resp := p.AttachReload(r.Context())
	// Reload's sentinel-not-registered path returns a ReloadResponse
	// carrying the sentinel string in Errors. Map that to 501 so the
	// operator sees the same shape as the other mutation endpoints.
	if len(resp.Errors) == 1 && resp.Errors[0] == ErrCapabilityNotRegistered.Error() {
		http.Error(w, "reload not registered on this OperatorView", http.StatusNotImplemented)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// decodePOST reads a length-capped JSON body. Shares the 8 KiB cap
// with /inject + /wake bodies — operator nudges should be small.
func decodePOST(r *http.Request, out any) error {
	body := http.MaxBytesReader(nil, r.Body, operatorPostMaxBytes)
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return errors.New("empty request body")
	}
	return json.Unmarshal(raw, out)
}
