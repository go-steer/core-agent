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
	// Reads — 200 with empty/zero data when the capability isn't
	// implemented (same convention as /tools, /agents, /status).
	h.routeSession(mux, "GET", "usage", auth.ActionSessionRead, h.doUsage)
	h.routeSession(mux, "GET", "context", auth.ActionSessionRead, h.doContext)
	h.routeSession(mux, "GET", "memory", auth.ActionSessionRead, h.doMemory)
	h.routeSession(mux, "GET", "skills", auth.ActionSessionRead, h.doSkills)
	h.routeSession(mux, "GET", "mcp", auth.ActionSessionRead, h.doMCP)
	h.routeSession(mux, "GET", "pricing", auth.ActionSessionRead, h.doPricing)
	h.routeSession(mux, "GET", "perms", auth.ActionSessionRead, h.doPerms)
	h.routeSession(mux, "GET", "guardrails", auth.ActionSessionRead, h.doGuardrails)

	// Mutation endpoints (PR A2): blocked by the ReadOnly middleware
	// at the auth layer when ReadOnly=true (any non-GET is gated).
	h.routeSession(mux, "POST", "perms/allow", auth.ActionSessionWrite, h.doPermsAllow)
	h.routeSession(mux, "POST", "perms/deny", auth.ActionSessionWrite, h.doPermsDeny)
	// guardrails/reset is ActionSessionWrite, not ActionSessionAdmin
	// (#331): clearing a trip only lets the session accept turns
	// again, and the very next thing the operator does is POST
	// /inject — which is itself ActionSessionWrite. Gating the reset
	// harder than the inject it precedes would buy no safety, only a
	// second credential to route around. Unlimited (not
	// routeSessionLimited): it's a local flag flip and cost-limiting
	// the way OUT of a cost trip is a trap.
	h.routeSession(mux, "POST", "guardrails/reset", auth.ActionSessionWrite, h.doGuardrailsReset)
	// pricing/refresh is cost-limited (#463): it does a network
	// fetch + catalog rebuild per call. pricing/set, perms/*, and
	// reload stay unlimited — they're cheap local mutations. The
	// limiter fires before entry lookup so an over-limit caller
	// can't force a lazy session resume (#484).
	h.routeSessionLimited(mux, "pricing/refresh", h.doPricingRefresh)
	h.routeSession(mux, "POST", "pricing/set", auth.ActionSessionWrite, h.doPricingSet)
	h.routeSession(mux, "POST", "reload", auth.ActionSessionWrite, h.doReload)
	h.routeSession(mux, "DELETE", "", auth.ActionSessionAdmin, h.doDeleteSession)

	// PR A3 async slash dispatchers. All synchronous on the wire —
	// the operator stares at silence until the handler returns. The
	// in-chat preamble row is the remote TUI's responsibility (it
	// renders the same preamble at dispatch as the in-process TUI's
	// AsyncSlashProviderWithPreamble path).
	//
	// All five run unbounded model work per request, so each runs
	// behind the per-caller cost limiter (#463). The limiter fires
	// before entry lookup AND before capability dispatch (#484):
	// an over-limit caller can't force a lazy session resume, and
	// even a 501 from an unwired capability consumes a token.
	h.routeSessionLimited(mux, "slash/compact", h.doSlashCompact)
	h.routeSessionLimited(mux, "slash/done", h.doSlashDone)
	h.routeSessionLimited(mux, "slash/btw", h.doSlashBtw)
	h.routeSessionLimited(mux, "slash/subagent", h.doSlashSubagent)
	h.routeSessionLimited(mux, "slash/replan", h.doSlashReplan)
}

func (h *handlers) doUsage(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	out := UsageInfo{}
	if p, ok := entry.Agent.(UsageProvider); ok {
		out = p.AttachUsage()
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) doContext(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	out := ContextInfo{}
	if p, ok := entry.Agent.(ContextProvider); ok {
		out = p.AttachContext()
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) doMemory(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	out := []MemorySource{}
	if p, ok := entry.Agent.(MemoryProvider); ok {
		if list := p.AttachMemory(); list != nil {
			out = list
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

func (h *handlers) doSkills(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	out := []SkillInfo{}
	if p, ok := entry.Agent.(SkillsProvider); ok {
		if list := p.AttachSkills(); list != nil {
			out = list
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"skills": out})
}

func (h *handlers) doMCP(w http.ResponseWriter, _ *http.Request, entry *Entry) {
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

func (h *handlers) doPricing(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	out := PricingInfo{}
	if p, ok := entry.Agent.(PricingProvider); ok {
		out = p.AttachPricing()
	}
	writeJSON(w, http.StatusOK, out)
}

// ===== PR A2 mutation handlers =====
//
// Reads (perms) follow the same "200 with empty data if no provider"
// convention as PR A1 reads. Writes (perms/allow, perms/deny,
// pricing/refresh, pricing/set, reload) return 501 when the
// capability isn't registered, since the operator's POST must take
// effect or fail loudly.

const operatorPostMaxBytes = 8 * 1024

// doPerms — GET /perms.

func (h *handlers) doPerms(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	out := PermsInfo{}
	if p, ok := entry.Agent.(PermsProvider); ok {
		out = p.AttachPerms()
	}
	writeJSON(w, http.StatusOK, out)
}

// doPermsAllow / doPermsDeny — POST /perms/allow, /perms/deny.
func (h *handlers) doPermsAllow(w http.ResponseWriter, r *http.Request, entry *Entry) {
	h.doPermsMutation(w, r, entry, false)
}

func (h *handlers) doPermsDeny(w http.ResponseWriter, r *http.Request, entry *Entry) {
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

// doPricingRefresh — POST /pricing/refresh.

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

// doPricingSet — POST /pricing/set.

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

// doGuardrails — GET /guardrails (#666). Read-only projection, so it
// follows the 200-with-zero-value convention: a registrant with no
// guardrail capability reports everything off and nothing tripped,
// which is the truthful answer for an agent that has no backstops.
func (h *handlers) doGuardrails(w http.ResponseWriter, _ *http.Request, entry *Entry) {
	out := GuardrailInfo{}
	if p, ok := entry.Agent.(GuardrailProvider); ok {
		out = p.AttachGuardrails()
	}
	writeJSON(w, http.StatusOK, out)
}

// doGuardrailsReset — POST /guardrails/reset (#666).
//
// Status codes carry the whole contract:
//
//	200 — cleared (Reset lists what; Guardrails echoes the new state)
//	400 — malformed body, unknown guardrail name, bad budget
//	409 — the reset would provably re-trip; add budget (ErrGuardrailRetrip)
//	501 — no reset capability wired
func (h *handlers) doGuardrailsReset(w http.ResponseWriter, r *http.Request, entry *Entry) {
	p, ok := entry.Agent.(GuardrailResetter)
	if !ok {
		http.Error(w, "guardrail reset capability not registered", http.StatusNotImplemented)
		return
	}
	// An empty body is the common case — "clear whatever tripped" —
	// so unlike the other operator POSTs this one tolerates it.
	var body GuardrailResetRequest
	if err := decodePOSTOptional(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch body.Guardrail {
	case "", GuardrailAll, GuardrailWatchdog, GuardrailCostCeiling:
	default:
		http.Error(w, "guardrail: must be one of \"watchdog\", \"cost_ceiling\", \"all\"", http.StatusBadRequest)
		return
	}
	if body.AdditionalBudgetUSD < 0 {
		http.Error(w, "additional_budget_usd: must be non-negative", http.StatusBadRequest)
		return
	}
	// Attribution is stamped here, from the authenticated context —
	// never read off the wire, where it would be a claim the caller
	// makes about themselves. The capability persists it on the reset
	// row (#643), which is also the audit trail (#331); the handler
	// deliberately does NOT append out-of-band, because an out-of-band
	// Get-then-Append can bump the live session row mid-turn and trip
	// ADK's optimistic-concurrency check (the #565 lesson).
	if c, ok := auth.CallerFromContext(r.Context()); ok {
		body.Caller = c.Identity
	}
	resp, err := p.AttachResetGuardrail(body)
	switch {
	case errors.Is(err, ErrCapabilityNotRegistered):
		http.Error(w, "guardrail reset not registered on this OperatorView", http.StatusNotImplemented)
		return
	case errors.Is(err, ErrGuardrailRetrip):
		// 409 carries the post-refusal state too, so the client can
		// render "spent $X of $Y" without a follow-up GET.
		writeJSON(w, http.StatusConflict, GuardrailResetResponse{
			Reset:      []string{},
			Guardrails: resp.Guardrails,
			Message:    err.Error(),
		})
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if resp.Reset == nil {
		resp.Reset = []string{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// doReload — POST /reload.

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

// decodePOSTOptional is decodePOST for endpoints whose every field is
// optional: an absent body leaves out at its zero value rather than
// erroring. Used by guardrails/reset, where "clear whatever tripped"
// is the common request and demanding `{}` would be ceremony.
func decodePOSTOptional(r *http.Request, out any) error {
	body := http.MaxBytesReader(nil, r.Body, operatorPostMaxBytes)
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, out)
}
