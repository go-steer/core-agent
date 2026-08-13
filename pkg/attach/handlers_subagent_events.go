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
	"fmt"
	"net/http"

	"github.com/go-steer/core-agent/v2/internal/subagentlog"
)

// GET /sessions/.../agents/{name}/events — a subagent's inner turns.
//
// The read itself (which branch spellings a name resolves to, how a
// page is cut) lives in internal/subagentlog, shared with the
// in-process TUI's coretui.SubagentEventReader. This file is the HTTP
// shim over it: request parsing, the two wire shapes, status codes.
// See that package's doc comment for why subagent turns need a
// retrieval path at all.

// SubagentEventsResponse is the body of
// GET /sessions/.../agents/{name}/events.
type SubagentEventsResponse struct {
	// Agent is the subagent name that was queried, echoed back.
	Agent string `json:"agent"`

	// ParentSessionID is the session whose subtree was searched.
	ParentSessionID string `json:"parent_session_id"`

	// Branches lists the branch prefixes this query matched against —
	// one subagent name maps to several spellings depending on how it
	// was launched, plus whatever instance-suffixed labels the log
	// turned out to hold ("bg.cluster-1" for a subagent declared as
	// "cluster"). Echoed so an operator who gets an empty list can see
	// exactly what was looked for.
	Branches []string `json:"branches"`

	// Events are the subagent's turns in seq order, in the same
	// {seq, event} shape the SSE stream's `agent` frames use.
	Events []Frame `json:"events"`

	// NextSince is the seq to pass as ?since= to fetch the next
	// page. Equals the request's since when nothing matched.
	NextSince int64 `json:"next_since"`

	// Truncated reports that the limit cut the result short and
	// there is more to fetch from NextSince.
	Truncated bool `json:"truncated"`
}

// SubagentNotFoundResponse is the body of a 404 from
// GET /sessions/.../agents/{name}/events: the name resolved to
// nothing, and here is what it would have resolved against.
//
// The empty-200 this replaced was indistinguishable from "the
// subagent ran and did nothing" — the failure mode reported in #694,
// where an operator spent a while assuming a subagent's turns weren't
// being recorded when in fact the name was spelled differently in the
// log.
type SubagentNotFoundResponse struct {
	Error           string `json:"error"`
	Agent           string `json:"agent"`
	ParentSessionID string `json:"parent_session_id"`

	// Branches is what was searched for, same as the 200 shape.
	Branches []string `json:"branches"`

	// Available lists the subagent names that DO resolve in this
	// session — every distinct branch label in the log plus the live
	// and configured rosters, reduced to the name an operator would
	// have declared. Empty means no subagent has run here at all.
	Available []string `json:"available"`
}

// subagentRoster collects the name hints this session can offer beyond
// the log itself: what the live manager is tracking, and what the
// config declares as spawnable.
func subagentRoster(entry *Entry) subagentlog.Roster {
	var r subagentlog.Roster
	if p, ok := entry.Agent.(AgentsProvider); ok {
		for _, a := range p.AttachAgents() {
			r.Instances = append(r.Instances, a.Name)
		}
	}
	if p, ok := entry.Agent.(SubagentCatalogProvider); ok {
		for _, s := range p.AttachSubagentCatalog() {
			r.Declared = append(r.Declared, s.Name)
		}
	}
	return r
}

// doSubagentEvents serves the subagent turn history.
//
// An unresolvable name is a 404 carrying the names that would have
// resolved (#694, defect 2). That reverses the endpoint's original
// answer — an empty 200, on the reasoning that the log can't tell
// "never heard of it" from "ran three restarts ago". It can now:
// subagentlog.Resolve asks the log which branches exist under this
// session tree, so absence is something this handler observes rather
// than infers from the in-memory manager. The empty 200 survives
// wherever that observation isn't available — a Stream with no
// BranchLister, a failed or capped branch scan, a name in either
// roster — so the 404 only ever means "looked, and it isn't here".
//
// The one residual lie is a log that has been pruned or rotated out
// from under a session; the 404 body says "no turns recorded", not
// "no such subagent", for exactly that reason.
func (h *handlers) doSubagentEvents(w http.ResponseWriter, r *http.Request, entry *Entry) {
	name := r.PathValue("name")
	if err := subagentlog.ValidateName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	elog := entry.Agent.EventLog()
	if elog == nil || elog.Stream == nil {
		http.Error(w, "this session has no event log; subagent turn history requires --session-db", http.StatusPreconditionFailed)
		return
	}
	since := parseSince(r.URL.Query().Get("since"))
	limit := subagentlog.ParseLimit(r.URL.Query().Get("limit"))
	tree := subagentlog.Tree{AppName: entry.AppName, UserID: entry.UserID, SessionID: entry.SessionID}

	resolved := subagentlog.Resolve(r.Context(), elog.Stream, tree, name, subagentRoster(entry))
	if resolved.ResolveErr != nil {
		debugf("/agents/%s/events %s/%s: answering without branch resolution: %v",
			name, entry.AppName, entry.SessionID, resolved.ResolveErr)
	}
	if !resolved.Known {
		writeJSON(w, http.StatusNotFound, SubagentNotFoundResponse{
			Error: fmt.Sprintf("no turns recorded for subagent %q in this session", name),
			Agent: name, ParentSessionID: entry.SessionID,
			Branches: resolved.Prefixes, Available: resolved.Available,
		})
		return
	}

	page := subagentlog.Read(r.Context(), elog.Stream, tree, resolved, since, limit)
	if page.Unreadable > 0 {
		// A hydration failure on one row shouldn't discard the rows
		// that did load — a partial history is exactly what the
		// operator is here for.
		debugf("/agents/%s/events %s/%s: skipped %d unreadable row(s)",
			name, entry.AppName, entry.SessionID, page.Unreadable)
	}
	out := SubagentEventsResponse{
		Agent:           name,
		ParentSessionID: entry.SessionID,
		Branches:        resolved.Prefixes,
		Events:          make([]Frame, 0, len(page.Events)),
		NextSince:       page.NextSince,
		Truncated:       page.Truncated,
	}
	for _, e := range page.Events {
		out.Events = append(out.Events, Frame{Seq: e.Seq, Event: e.Event})
	}
	writeJSON(w, http.StatusOK, out)
}
