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
	"fmt"
	"net/http"
	"slices"
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

	// subagentBranchScanLimit caps the distinct branch labels pulled
	// while resolving a name. A session with more than this many
	// distinct subagent branches is pathological; the cap keeps name
	// resolution O(1)-ish on the request path either way.
	subagentBranchScanLimit = 500
)

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

// subagentLaunchPrefixes are the branch namespaces a subagent's turns
// can be written under; "" is the sync subagent tool, which tags its
// events with the bare name. Keep in sync with the launch paths listed
// on subagentBranchPrefixes.
var subagentLaunchPrefixes = []string{"", "bg.", "sub.", "remote."}

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
// These four are the base set, not the whole search: an instance
// counter is not a dot segment, so "bg.cluster-1" matches none of
// them. resolveSubagentQuery adds the instance-suffixed spellings it
// finds in the log on top of these.
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
	out := make([]string, 0, len(subagentLaunchPrefixes))
	for _, p := range subagentLaunchPrefixes {
		out = append(out, p+name)
	}
	return out
}

// splitBranchLabel breaks a branch label into its launch prefix and
// its top-level instance label: "bg.cluster-1.probe" is ("bg.",
// "cluster-1"). A branch with no recognised launch prefix keeps an
// empty one, which is what a sync subagent tool writes.
func splitBranchLabel(branch string) (launch, label string) {
	for _, p := range subagentLaunchPrefixes {
		if p != "" && strings.HasPrefix(branch, p) {
			launch, branch = p, branch[len(p):]
			break
		}
	}
	if i := strings.IndexAny(branch, "./"); i >= 0 {
		branch = branch[:i]
	}
	return launch, branch
}

// stripInstanceSuffix reduces a spawned instance's label back to the
// name it was declared under: "cluster-1" is instance 1 of "cluster",
// because the background manager mints instance names as
// fmt.Sprintf("%s-%d", spec, seq) (background.nextInstanceName).
//
// Only a "-" followed by digits counts. Widening this to any "-"
// suffix — or querying the log for "cluster-%", as #694 floats as one
// option — would fold genuinely distinct subagents together: ask for
// "kube" and get "kube-platform"'s turns back, silently.
func stripInstanceSuffix(label string) string {
	i := strings.LastIndex(label, "-")
	if i <= 0 || i == len(label)-1 {
		return label
	}
	for _, r := range label[i+1:] {
		if r < '0' || r > '9' {
			return label
		}
	}
	return label[:i]
}

// subagentQuery is the resolved plan for one events request: which
// branch prefixes to filter the log with, whether the name resolved to
// anything at all, and what names would have.
type subagentQuery struct {
	prefixes  []string
	known     bool
	available []string
}

// resolveSubagentQuery maps the requested name onto the branch
// prefixes to search.
//
// The base four spellings (subagentBranchPrefixes) are always queried
// — they are what a sync subagent, and any launch path that doesn't
// mint an instance counter, actually writes. On top of that the log's
// own distinct branch labels are consulted, so a subagent declared as
// "cluster" and spawned as instance "cluster-1" is reachable under the
// name the operator declared, which is the only name they know (#694,
// defect 1).
//
// Resolution deliberately leads with the log rather than the live
// manager: the manager knows only what is running in this process, and
// this endpoint exists to diagnose subagents that have finished, that
// ran before the last restart, or that were sync (never tracked by the
// manager at all). The rosters are consulted after, and only widen the
// answer — a spawned instance that hasn't written a turn yet, and a
// configured-but-never-run subagent, are both real names whose honest
// answer is an empty list rather than a 404.
func resolveSubagentQuery(ctx context.Context, entry *Entry, stream eventlog.Stream, name string) subagentQuery {
	q := subagentQuery{prefixes: subagentBranchPrefixes(name), available: []string{}}
	seen := make(map[string]bool, len(q.prefixes))
	for _, p := range q.prefixes {
		seen[p] = true
	}
	addPrefix := func(p string) {
		if !seen[p] {
			seen[p] = true
			q.prefixes = append(q.prefixes, p)
		}
	}

	bl, ok := stream.(eventlog.BranchLister)
	if !ok {
		// A Stream that can't enumerate its branches can't prove the
		// name absent, and a 404 that means "I couldn't check" is
		// worse than the empty list it replaced. Degrade to the
		// pre-#694 contract for this session.
		q.known = true
		return q
	}
	branches, err := bl.Branches(ctx,
		eventlog.WithSessionTree(entry.AppName, entry.UserID, entry.SessionID),
		eventlog.WithLimit(subagentBranchScanLimit),
	)
	if err != nil {
		debugf("/agents/%s/events %s/%s: branch resolution failed, answering without it: %v",
			name, entry.AppName, entry.SessionID, err)
		q.known = true
		return q
	}
	avail := map[string]bool{}
	for _, b := range branches {
		launch, label := splitBranchLabel(b)
		if label == "" {
			continue
		}
		declared := stripInstanceSuffix(label)
		avail[declared] = true
		if label == name || declared == name {
			q.known = true
			addPrefix(launch + label)
		}
	}

	// The live roster: an instance spawned a moment ago has a
	// predictable branch even before its first turn lands.
	if p, ok := entry.Agent.(AgentsProvider); ok {
		for _, a := range p.AttachAgents() {
			if a.Name == "" {
				continue
			}
			declared := stripInstanceSuffix(a.Name)
			avail[declared] = true
			if a.Name == name || declared == name {
				q.known = true
				for _, pre := range subagentBranchPrefixes(a.Name) {
					addPrefix(pre)
				}
			}
		}
	}
	// The configured roster: spawnable by reference, so a real name.
	if p, ok := entry.Agent.(SubagentCatalogProvider); ok {
		for _, s := range p.AttachSubagentCatalog() {
			if s.Name == "" {
				continue
			}
			avail[s.Name] = true
			if s.Name == name {
				q.known = true
			}
		}
	}
	for n := range avail {
		q.available = append(q.available, n)
	}
	slices.Sort(q.available)
	return q
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
// An unresolvable name is a 404 carrying the names that would have
// resolved (#694, defect 2). That reverses the endpoint's original
// answer — an empty 200, on the reasoning that the log can't tell
// "never heard of it" from "ran three restarts ago". It can now:
// resolveSubagentQuery asks the log which branches exist under this
// session tree, so absence is something this handler observes rather
// than infers from the in-memory manager. The empty 200 survives
// wherever that observation isn't available — a Stream with no
// BranchLister, a failed branch query, a name in either roster — so
// the 404 only ever means "looked, and it isn't here".
//
// The one residual lie is a log that has been pruned or rotated out
// from under a session; the 404 body says "no turns recorded", not
// "no such subagent", for exactly that reason.
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

	resolved := resolveSubagentQuery(r.Context(), entry, elog.Stream, name)
	branches := resolved.prefixes
	if !resolved.known {
		writeJSON(w, http.StatusNotFound, SubagentNotFoundResponse{
			Error: fmt.Sprintf("no turns recorded for subagent %q in this session", name),
			Agent: name, ParentSessionID: entry.SessionID,
			Branches: branches, Available: resolved.available,
		})
		return
	}

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
