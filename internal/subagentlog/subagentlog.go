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

// Package subagentlog reads one subagent's inner turns back out of the
// parent session's event log.
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
// This is the retrieval path: a filtered read over the persisted log.
// It reads history, not liveness, so it works for a subagent that has
// already finished — and, because it queries the log rather than the
// in-memory manager, for one that ran before the daemon last restarted.
//
// The logic lives here rather than in pkg/attach because it has two
// callers with nothing else in common: the HTTP endpoint
// GET /sessions/{id}/agents/{name}/events, and the in-process TUI's
// coretui.SubagentEventReader, which has no HTTP layer to go through.
// Name resolution in particular must not be duplicated — the two
// surfaces disagreeing about which spellings of a name resolve is
// precisely the failure #694 was about.
package subagentlog

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

const (
	// DefaultLimit is the page size when the caller doesn't ask for
	// one. Large enough that a typical subagent run comes back whole,
	// small enough that a diagnostic curl against a long-lived session
	// doesn't dump megabytes.
	DefaultLimit = 500

	// MaxLimit bounds what a caller can ask for, for the same reason
	// maxReplayEvents bounds SSE catch-up: without it, any
	// authenticated caller can turn one GET into a full-table
	// scan-and-hydrate.
	MaxLimit = 5000

	// branchScanLimit caps the distinct branch labels pulled while
	// resolving a name. A session with more than this many distinct
	// subagent branches is pathological; the cap keeps name resolution
	// O(1)-ish on the request path either way.
	branchScanLimit = 500
)

// Tree identifies the parent session whose subtree holds the turns.
type Tree struct {
	AppName   string
	UserID    string
	SessionID string
}

func (t Tree) queryOpt() eventlog.QueryOption {
	return eventlog.WithSessionTree(t.AppName, t.UserID, t.SessionID)
}

// Roster is what the caller knows about subagent names from outside
// the log. Both lists only ever WIDEN an answer: a name in either one
// is real even if it has written no turns, so it earns an empty page
// rather than a not-found.
type Roster struct {
	// Instances are names the live manager is tracking — a subagent
	// spawned a moment ago has a predictable branch even before its
	// first turn lands. Instance-suffixed spellings are expected here
	// ("cluster-1") and reduced to their declared name.
	Instances []string

	// Declared are configured subagent names, spawnable by reference
	// and therefore real, whether or not any has ever run.
	Declared []string
}

// LaunchPrefixes are the branch namespaces a subagent's turns can be
// written under; "" is the sync subagent tool, which tags its events
// with the bare name. Keep in sync with the launch paths listed on
// BranchPrefixes.
var LaunchPrefixes = []string{"", "bg.", "sub.", "remote."}

// BranchPrefixes returns every branch spelling under which a subagent
// of this name could have written events. The launch path picks the
// prefix and the operator asking "what did X do?" doesn't know (and
// shouldn't need to know) which one ran:
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
// them. Resolve adds the instance-suffixed spellings it finds in the
// log on top of these.
//
// The corollary is the one sharp edge here: prefix matching is
// anchored, so a NESTED subagent is reachable only under its
// top-level ancestor's name, not its own — ask for "cluster" to see
// what "bg.cluster.probe" did; asking for "probe" returns nothing.
// Unanchored matching would fix that at the cost of a leading-%
// LIKE (no index, full scan of the event table) and of collisions
// with any same-named subagent elsewhere in the tree — a bad trade
// for a diagnostic path whose caller almost always knows the
// top-level name. Documented in the HTTP reference rather than
// worked around.
func BranchPrefixes(name string) []string {
	out := make([]string, 0, len(LaunchPrefixes))
	for _, p := range LaunchPrefixes {
		out = append(out, p+name)
	}
	return out
}

// SplitBranchLabel breaks a branch label into its launch prefix and
// its top-level instance label: "bg.cluster-1.probe" is ("bg.",
// "cluster-1"). A branch with no recognised launch prefix keeps an
// empty one, which is what a sync subagent tool writes.
func SplitBranchLabel(branch string) (launch, label string) {
	for _, p := range LaunchPrefixes {
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

// StripInstanceSuffix reduces a spawned instance's label back to the
// name it was declared under: "cluster-1" is instance 1 of "cluster",
// because the background manager mints instance names as
// fmt.Sprintf("%s-%d", spec, seq) (background.nextInstanceName).
//
// Only a "-" followed by digits counts. Widening this to any "-"
// suffix — or querying the log for "cluster-%", as #694 floats as one
// option — would fold genuinely distinct subagents together: ask for
// "kube" and get "kube-platform"'s turns back, silently.
func StripInstanceSuffix(label string) string {
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

// ValidateName rejects names that could never have produced a branch
// label. Mirrors background.validateSpawnName — which pkg/attach
// cannot call, since pkg/agent/background imports pkg/attach for
// AgentInfo and the dependency can't run both ways. Kept deliberately
// strict rather than permissive: a name is a query key here, and the
// only names that can appear in the log are ones that passed the spawn
// check.
func ValidateName(name string) error {
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

// ParseLimit reads a caller-supplied page size, clamping to
// [1, MaxLimit]. Absent, unparseable, or non-positive values take
// DefaultLimit — same forgiving contract as the since cursor, so a
// malformed request degrades to a sane page rather than an error.
func ParseLimit(s string) int {
	if strings.TrimSpace(s) == "" {
		return DefaultLimit
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return DefaultLimit
	}
	if n > MaxLimit {
		return MaxLimit
	}
	return n
}

// Query is the resolved plan for one read: which branch prefixes to
// filter the log with, whether the name resolved to anything at all,
// and what names would have.
type Query struct {
	// Prefixes are the branch spellings to filter on.
	Prefixes []string

	// Known reports that the name resolved. False means the caller
	// should report "no such subagent" rather than an empty page.
	Known bool

	// Available lists the subagent names that DO resolve in this
	// session, sorted. Empty means no subagent has run here at all.
	Available []string

	// ResolveErr carries a non-fatal degradation: the branch scan
	// failed or wasn't available, so Known was forced true rather
	// than inferred. Purely diagnostic — the Query is usable either
	// way — but worth logging, since it explains why a genuinely
	// unknown name came back as an empty page.
	ResolveErr error
}

// Resolve maps the requested name onto the branch prefixes to search.
//
// The base four spellings (BranchPrefixes) are always queried — they
// are what a sync subagent, and any launch path that doesn't mint an
// instance counter, actually writes. On top of that the log's own
// distinct branch labels are consulted, so a subagent declared as
// "cluster" and spawned as instance "cluster-1" is reachable under the
// name the operator declared, which is the only name they know (#694,
// defect 1).
//
// Resolution deliberately leads with the log rather than the live
// manager: the manager knows only what is running in this process, and
// this path exists to diagnose subagents that have finished, that ran
// before the last restart, or that were sync (never tracked by the
// manager at all). The roster is consulted after, and only widens the
// answer.
func Resolve(ctx context.Context, stream eventlog.Stream, tree Tree, name string, roster Roster) Query {
	q := Query{Prefixes: BranchPrefixes(name), Available: []string{}}
	seen := make(map[string]bool, len(q.Prefixes))
	for _, p := range q.Prefixes {
		seen[p] = true
	}
	addPrefix := func(p string) {
		if !seen[p] {
			seen[p] = true
			q.Prefixes = append(q.Prefixes, p)
		}
	}

	bl, ok := stream.(eventlog.BranchLister)
	if !ok {
		// A Stream that can't enumerate its branches can't prove the
		// name absent, and a not-found that means "I couldn't check"
		// is worse than the empty list it replaced. Degrade to the
		// pre-#694 contract for this session.
		q.Known = true
		q.ResolveErr = fmt.Errorf("event log cannot enumerate branches")
		return q
	}
	branches, err := bl.Branches(ctx, tree.queryOpt(), eventlog.WithLimit(branchScanLimit))
	if err != nil {
		q.Known = true
		q.ResolveErr = fmt.Errorf("branch resolution failed: %w", err)
		return q
	}
	// A scan that hit the cap has not seen every branch, so it cannot
	// prove this name absent either — the labels are returned in the
	// store's order, and a name sorting past the cap would look
	// missing when it is merely late. Same reasoning as the
	// no-BranchLister case: only claim absence when it was observed.
	if len(branches) >= branchScanLimit {
		q.Known = true
		q.ResolveErr = fmt.Errorf("branch scan hit the %d-label cap; absence not proven", branchScanLimit)
	}

	avail := map[string]bool{}
	for _, b := range branches {
		launch, label := SplitBranchLabel(b)
		if label == "" {
			continue
		}
		declared := StripInstanceSuffix(label)
		avail[declared] = true
		if label == name || declared == name {
			q.Known = true
			addPrefix(launch + label)
		}
	}

	// The live roster: an instance spawned a moment ago has a
	// predictable branch even before its first turn lands.
	for _, n := range roster.Instances {
		if n == "" {
			continue
		}
		declared := StripInstanceSuffix(n)
		avail[declared] = true
		if n == name || declared == name {
			q.Known = true
			for _, pre := range BranchPrefixes(n) {
				addPrefix(pre)
			}
		}
	}
	// The configured roster: spawnable by reference, so a real name.
	for _, n := range roster.Declared {
		if n == "" {
			continue
		}
		avail[n] = true
		if n == name {
			q.Known = true
		}
	}
	for n := range avail {
		q.Available = append(q.Available, n)
	}
	slices.Sort(q.Available)
	return q
}

// Page is one page of a subagent's turns.
type Page struct {
	// Events are the matching rows in seq order.
	Events []eventlog.Entry

	// NextSince is the seq to resume from. Equals the requested
	// since when nothing matched.
	NextSince int64

	// Truncated reports that the limit cut the result short and there
	// is more to fetch from NextSince.
	Truncated bool

	// Unreadable counts rows that failed to hydrate and were skipped.
	// Diagnostic only: a partial history is exactly what the caller is
	// here for, so a bad row must not discard the good ones.
	Unreadable int
}

// Read pulls one page of the branches in q, starting after since.
// limit is clamped through ParseLimit's bounds by the caller; a
// non-positive limit takes DefaultLimit.
func Read(ctx context.Context, stream eventlog.Stream, tree Tree, q Query, since int64, limit int) Page {
	if limit <= 0 {
		limit = DefaultLimit
	}
	out := Page{Events: []eventlog.Entry{}, NextSince: since}
	// Fetch one past the page so truncation is observed rather than
	// guessed: len(events) == limit is ambiguous when the page ends
	// exactly on the last event.
	opts := []eventlog.QueryOption{
		tree.queryOpt(),
		eventlog.WithAnyBranchPrefix(q.Prefixes...),
		eventlog.WithLimit(limit + 1),
	}
	// Rows consumed, counting the ones we skip: truncation is a
	// property of the QUERY, not of how many rows happened to
	// hydrate, so an unreadable row must still consume its slot or
	// the extra row would be mistaken for "no more data".
	consumed := 0
	for e, err := range stream.Since(ctx, since, opts...) {
		consumed++
		if consumed > limit {
			out.Truncated = true
			break
		}
		if err != nil {
			out.Unreadable++
			continue
		}
		out.Events = append(out.Events, e)
		out.NextSince = e.Seq
	}
	return out
}
