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
	"strconv"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// GET /sessions/.../agents/{name}/events — a subagent's inner turns.
//
// Subagent turns were always durable: every runner writes through the
// PARENT's session.Service, wrapped so each event carries a Branch
// label, into a derived session row "<parent>:sub:<branch>". What was
// missing (#638) was any way to read them back. The live /events SSE
// stream is deliberately scoped to the parent session row alone —
// widening it would push subagent chatter into every attached chat
// view — so a runaway loop inside a subagent was diagnosable only by
// capturing the raw event stream, which is what we ended up doing
// during the PR #622 UAT.
//
// This is the retrieval path: a plain JSON read over the persisted
// log, filtered to one subagent's branch subtree. It reads history,
// not liveness, so it works for a subagent that has already finished
// — and, because it queries the log rather than the in-memory
// manager, for one that ran before the daemon last restarted.

const (
	// subagentEventsDefaultLimit is the page size when the caller
	// doesn't ask for one. Large enough that a typical subagent run
	// comes back whole, small enough that a diagnostic curl against
	// a long-lived session doesn't dump megabytes.
	subagentEventsDefaultLimit = 500

	// subagentEventsMaxLimit bounds what a caller can ask for, for
	// the same reason maxReplayEvents bounds SSE catch-up: without
	// it, any authenticated caller can turn one GET into a
	// full-table scan-and-hydrate.
	subagentEventsMaxLimit = 5000
)

// SubagentEventsResponse is the body of
// GET /sessions/.../agents/{name}/events.
type SubagentEventsResponse struct {
	// Agent is the subagent name that was queried, echoed back.
	Agent string `json:"agent"`

	// ParentSessionID is the session whose subtree was searched.
	ParentSessionID string `json:"parent_session_id"`

	// Branches lists the branch labels this query matched against —
	// one subagent name maps to several spellings depending on how
	// it was launched. Echoed so an operator who gets an empty list
	// can see exactly what was looked for.
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

// subagentBranchPrefixes returns every branch spelling under which a
// subagent of this name could have written events. The launch path
// picks the prefix and the operator asking "what did X do?" doesn't
// know (and shouldn't need to know) which one ran:
//
//	<name>          sync subagent tool          pkg/agent/subagent.go
//	bg.<name>       background / spawn_agent    pkg/agent/background/spawn.go
//	sub.<name>      RunSubtask                  pkg/agent/subtask.go
//	remote.<name>   remote runner               pkg/agent/background/remote.go
//
// A declarative subagent is reachable through the first two at once
// (sync tool + spawn-by-reference, #626), so querying the union is
// not merely convenient — picking a single prefix would return
// nothing for whichever half the operator didn't guess.
//
// Each prefix also matches its own descendants (the eventlog filter
// treats "p" as covering "p.child"), so a nested subagent's turns
// come back under its ancestor's name too.
//
// The corollary is the one sharp edge here: prefix matching is
// anchored, so a NESTED subagent is reachable only under its
// top-level ancestor's name, not its own — ask for "cluster" to see
// what "bg.cluster.probe" did; asking for "probe" returns nothing.
// Unanchored matching would fix that at the cost of a leading-%
// LIKE (no index, full scan of the event table) and of collisions
// with any same-named subagent elsewhere in the tree — a bad trade
// for a diagnostic endpoint whose caller almost always knows the
// top-level name. Documented in the HTTP reference rather than
// worked around.
func subagentBranchPrefixes(name string) []string {
	return []string{name, "bg." + name, "sub." + name, "remote." + name}
}

// validateSubagentName rejects names that could never have produced a
// branch label. Mirrors background.validateSpawnName — which pkg/attach
// cannot call, since pkg/agent/background imports pkg/attach for
// AgentInfo and the dependency can't run both ways. Kept deliberately
// strict rather than permissive: a name is a query key here, and the
// only names that can appear in the log are ones that passed the spawn
// check.
func validateSubagentName(name string) error {
	if name == "" {
		return fmt.Errorf("subagent name is required")
	}
	if strings.TrimSpace(name) != name {
		return fmt.Errorf("subagent name must not have leading/trailing whitespace: %q", name)
	}
	if strings.ContainsAny(name, ". /") {
		return fmt.Errorf("subagent name must not contain '.', '/' or spaces: %q", name)
	}
	return nil
}

// parseSubagentEventsLimit reads ?limit=, clamping to
// [1, subagentEventsMaxLimit]. Absent, unparseable, or non-positive
// values take the default — same forgiving contract as parseSince,
// so a malformed cursor degrades to a sane page rather than a 400.
func parseSubagentEventsLimit(s string) int {
	if strings.TrimSpace(s) == "" {
		return subagentEventsDefaultLimit
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return subagentEventsDefaultLimit
	}
	if n > subagentEventsMaxLimit {
		return subagentEventsMaxLimit
	}
	return n
}

// doSubagentEvents serves the subagent turn history.
//
// Note what it does NOT do: consult the background manager to check
// that the named subagent exists. The manager only knows what is live
// in this process, and the whole point of the endpoint is post-hoc
// diagnosis — including after a restart, and including sync subagents
// (which the manager never tracks). An unknown name is therefore an
// empty list, not a 404: "no turns recorded under that name" is the
// honest answer, and Branches shows what was searched.
func (h *handlers) doSubagentEvents(w http.ResponseWriter, r *http.Request, entry *Entry) {
	name := r.PathValue("name")
	if err := validateSubagentName(name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	elog := entry.Agent.EventLog()
	if elog == nil || elog.Stream == nil {
		http.Error(w, "this session has no event log; subagent turn history requires --session-db", http.StatusPreconditionFailed)
		return
	}
	since := parseSince(r.URL.Query().Get("since"))
	limit := parseSubagentEventsLimit(r.URL.Query().Get("limit"))
	branches := subagentBranchPrefixes(name)

	out := SubagentEventsResponse{
		Agent:           name,
		ParentSessionID: entry.SessionID,
		Branches:        branches,
		Events:          []Frame{},
		NextSince:       since,
	}
	// Fetch one past the page so truncation is observed rather than
	// guessed: len(events) == limit is ambiguous when the page ends
	// exactly on the last event.
	query := []eventlog.QueryOption{
		eventlog.WithSessionTree(entry.AppName, entry.UserID, entry.SessionID),
		eventlog.WithAnyBranchPrefix(branches...),
		eventlog.WithLimit(limit + 1),
	}
	// Rows consumed, counting the ones we skip: truncation is a
	// property of the QUERY, not of how many rows happened to
	// hydrate, so an unreadable row must still consume its slot or
	// the extra row would be mistaken for "no more data".
	consumed := 0
	for e, err := range elog.Stream.Since(r.Context(), since, query...) {
		consumed++
		if consumed > limit {
			out.Truncated = true
			break
		}
		if err != nil {
			// A hydration failure on one row shouldn't discard the
			// rows that did load — a partial history is exactly what
			// the operator is here for. Skip and keep going; the
			// server log carries the detail.
			debugf("/agents/%s/events %s/%s: skipping unreadable row: %v",
				name, entry.AppName, entry.SessionID, err)
			continue
		}
		out.Events = append(out.Events, Frame{Seq: e.Seq, Event: e.Event})
		out.NextSince = e.Seq
	}
	writeJSON(w, http.StatusOK, out)
}
