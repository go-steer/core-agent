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

// Package subsession holds the session-derivation plumbing shared by the
// two subagent-spawning paths: the core parallel-tool-call path in
// pkg/agent (subagent.go) and the long-lived background path in
// pkg/agent/background. Both derive a child session row from the parent's,
// tag emitted events with a hierarchical branch label, and track recursion
// depth through the context chain. Keeping these helpers in one internal
// package lets the background package spawn subagents without importing
// pkg/agent's unexported internals (which would form an import cycle).
package subsession

import (
	"context"
	"strings"

	"google.golang.org/adk/session"
)

// branchSeparator joins branch path segments, matching ADK's convention.
const branchSeparator = "."

// ComposeBranch builds the full branch path for a subagent call: the
// parent's branch (possibly empty for top-level), joined with the
// subagent's own branch label by ADK's "." separator.
func ComposeBranch(parent, this string) string {
	parent = strings.TrimSpace(parent)
	this = strings.TrimSpace(this)
	switch {
	case parent == "" && this == "":
		return ""
	case parent == "":
		return this
	case this == "":
		return parent
	default:
		return parent + branchSeparator + this
	}
}

// DeriveSessionID composes the session ID a subagent's runner uses. It
// lives in the same database as the parent's session so audit queries can
// find both (via the shared "<parent>:sub:<branch>" prefix + branch tag),
// but as a separate row per invocation so ADK's per-session
// optimistic-concurrency check doesn't trip and independent requests don't
// accumulate one another's history (#364).
//
// Format: "<parent>:sub:<branch>:<invocation>". When parent is empty the
// parent prefix is dropped. When invocation is empty the suffix is dropped
// — the parallel-tool-call path always passes a value; the background path
// intentionally passes "" because a background subagent is addressed by its
// stable name and its derived row must be deterministic.
func DeriveSessionID(parent, branch, invocation string) string {
	id := "sub:" + branch
	if parent != "" {
		id = parent + ":" + id
	}
	if invocation != "" {
		id = id + ":" + invocation
	}
	return id
}

// depthKey carries the current subagent recursion depth through the
// context chain. Top-level callers see depth 0; each nested subagent
// invocation increments by one.
type depthKey struct{}

// CurrentDepth returns the recursion depth of the current subagent
// invocation. Zero when we're not inside a subagent (i.e. the parent's
// top-level turn).
func CurrentDepth(ctx context.Context) int {
	v, _ := ctx.Value(depthKey{}).(int)
	return v
}

// WithDepth returns a context carrying subagent recursion depth n. Callers
// increment CurrentDepth(ctx)+1 when descending into a nested subagent.
func WithDepth(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, depthKey{}, n)
}

// lineageKey carries the DECLARED names of the subagents currently on
// the stack — the spec/template a subagent was built from, not its
// per-instance name. Depth answers "how deep are we"; lineage answers
// "which subagents are we inside of", which is what a self-spawn check
// needs: in the 2026-08-13 GKE UAT the cluster subagent spawned itself
// with a byte-identical goal at depth 1, comfortably inside a cap of 2
// (#732).
type lineageKey struct{}

// Lineage returns the declared names of the subagents enclosing the
// current execution, outermost first. Nil at the top level. The
// returned slice must not be modified — it is shared with every context
// derived from this one.
func Lineage(ctx context.Context) []string {
	v, _ := ctx.Value(lineageKey{}).([]string)
	return v
}

// WithLineage returns a context noting that execution has descended
// into the subagent declared as name. An empty name returns ctx
// unchanged: an unnamed subagent can't be recognized on the way back
// down, and recording "" would make every other unnamed one look like
// the same subagent.
func WithLineage(ctx context.Context, name string) context.Context {
	name = strings.TrimSpace(name)
	if name == "" {
		return ctx
	}
	prev := Lineage(ctx)
	// Copied rather than appended in place: sibling subagents derive
	// their contexts from the same parent, and append would let one of
	// them write its own name into a slice the others still read.
	next := make([]string, len(prev)+1)
	copy(next, prev)
	next[len(prev)] = name
	return context.WithValue(ctx, lineageKey{}, next)
}

// InLineage reports whether the subagent declared as name is already on
// the stack — i.e. spawning it again would be recursion. Matching is
// exact (after trimming), the same way names resolve against the
// template and predefined-spec maps.
func InLineage(ctx context.Context, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, n := range Lineage(ctx) {
		if n == name {
			return true
		}
	}
	return false
}

// BranchInjectingService wraps a session.Service so every appended event
// picks up Branch before landing in storage. The CRUD methods pass through
// unchanged. This is how a subagent's events end up tagged for the audit
// log without requiring the subagent's runner to know anything about
// branching.
type BranchInjectingService struct {
	Inner  session.Service
	Branch string
}

func (s *BranchInjectingService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	return s.Inner.Create(ctx, req)
}

func (s *BranchInjectingService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	return s.Inner.Get(ctx, req)
}

func (s *BranchInjectingService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	return s.Inner.List(ctx, req)
}

func (s *BranchInjectingService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	return s.Inner.Delete(ctx, req)
}

// AppendEvent stamps Branch on the event before delegating. We only
// override an empty Branch — events that already carry one (e.g., nested
// subagent invocations) keep their existing label so the branch hierarchy
// stays accurate.
func (s *BranchInjectingService) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if ev != nil && ev.Branch == "" {
		ev.Branch = s.Branch
	}
	return s.Inner.AppendEvent(ctx, sess, ev)
}
