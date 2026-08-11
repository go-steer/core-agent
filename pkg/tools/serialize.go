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

package tools

import (
	"sync"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"
)

// Mutating-tool serialization (#460). ADK dispatches every tool call
// in a model response concurrently (one goroutine per call,
// llminternal's handleFunctionCalls); the prompt used to carry an
// edit-sequencing rule purely because the runtime didn't enforce
// anything. This file is the enforcement that retired that prompt
// text — the Claude Agent SDK pattern: read-only tools run
// concurrently, state-mutating tools run sequentially.
//
// The guarantee is MUTUAL EXCLUSION, not response-order: two
// mutating calls in one response never run concurrently (so parallel
// writes can no longer race and corrupt state), but the runtime does
// not reorder scheduler-arbitrary arrival into declaration order. A
// call that depends on another call's RESULT must still go in a
// later response — results are never visible within one response
// regardless — and CoreInstruction still states that fact.

// readOnlyBuiltins names the built-in tools that are safe to run
// concurrently with anything: they observe state without mutating
// it. Everything not listed — built-in or otherwise — defaults to
// MUTATING (fail-safe: a misclassified read-only tool merely loses
// some parallelism; a misclassified mutating tool loses the
// corruption guarantee).
//
// Deliberately excluded (i.e. classified mutating): bash (arbitrary
// commands), write_file / edit_file / delete_file, todo (list state),
// record_plan (gate state), sciontool_status (writes the sticky
// status file), spawn_agent / stop_agent (mutate the background-agent
// manager's state).
var readOnlyBuiltins = map[string]bool{
	"read_file":       true,
	"read_many_files": true,
	"stat":            true,
	"list_dir":        true,
	"glob":            true,
	"grep":            true,
	"json_query":      true,
	"fetch_url":       true, // GET-only network read; no local state
	// ask_user mutates nothing and blocks at HUMAN timescale — if it
	// held the mutation lock, one open question would stall every
	// edit in the same response for minutes (pre-#460 they ran
	// concurrently, and should keep doing so).
	"ask_user": true,
}

// ReadOnlyHinter is the optional interface a tool implements to
// declare its own dispatch class — the MCP adapter satisfies it from
// the server-declared readOnlyHint annotation, and custom tools may
// too. Takes precedence over the builtin name table.
type ReadOnlyHinter interface {
	ReadOnlyHint() bool
}

// IsReadOnlyTool reports whether t may run concurrently with other
// tools. Order of authority: the tool's own ReadOnlyHinter
// declaration, then the builtin name table, then the fail-safe
// default: mutating.
func IsReadOnlyTool(t adktool.Tool) bool {
	if h, ok := t.(ReadOnlyHinter); ok {
		return h.ReadOnlyHint()
	}
	return readOnlyBuiltins[t.Name()]
}

// IsReadOnlyToolName reports whether a built-in tool NAME is classified
// read-only. It consults only the builtin name table — the sole
// classification available when all a caller holds is a recorded tool
// name rather than a live tool object (e.g. the auto-continue classifier
// reading interrupted calls back out of the eventlog, #624). It
// therefore CANNOT see a server-declared MCP readOnlyHint (that lives on
// the live tool object, not the name); an unknown or MCP-namespaced name
// returns false, the fail-safe (mutating) default — for auto-continue
// that means the continuation note keeps its re-issue nudge, the
// conservative direction.
func IsReadOnlyToolName(name string) bool {
	return readOnlyBuiltins[name]
}

// MutationSerializer is the shared lock one agent's mutating tools
// serialize on. One per agent — cross-agent serialization would
// couple unrelated sessions.
type MutationSerializer = sync.Mutex

// SerializeMutating wraps every mutating tool in ts so its Run holds
// mu for the duration — read-only tools pass through untouched and
// keep dispatching concurrently. Wrap ALL of one agent's tools with
// ONE serializer.
func SerializeMutating(ts []adktool.Tool, mu *MutationSerializer) []adktool.Tool {
	out := make([]adktool.Tool, len(ts))
	for i, t := range ts {
		out[i] = serializeOne(t, mu)
	}
	return out
}

// SerializeMutatingToolset wraps a toolset so every mutating tool it
// yields serializes on mu. Tools resolve lazily (MCP toolsets fetch
// on demand), so classification happens per Tools() call — which is
// also when the MCP adapter's readOnlyHint is available.
func SerializeMutatingToolset(ts adktool.Toolset, mu *MutationSerializer) adktool.Toolset {
	if ts == nil {
		return nil
	}
	return &serializedToolset{inner: ts, mu: mu}
}

type serializedToolset struct {
	inner adktool.Toolset
	mu    *MutationSerializer
}

func (s *serializedToolset) Name() string { return s.inner.Name() }

func (s *serializedToolset) Tools(ctx agent.ReadonlyContext) ([]adktool.Tool, error) {
	upstream, err := s.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	return SerializeMutating(upstream, s.mu), nil
}

func serializeOne(t adktool.Tool, mu *MutationSerializer) adktool.Tool {
	if t == nil || IsReadOnlyTool(t) {
		return t
	}
	if _, ok := t.(runnableTool); !ok {
		// Not a callable tool shape we can wrap (same posture as the
		// gate wrapper): pass through rather than break registration.
		return t
	}
	return &serializedTool{inner: t, mu: mu}
}

// serializedTool holds the agent's mutation lock across Run. Same
// wrapping shape as gatedTool.
type serializedTool struct {
	inner adktool.Tool
	mu    *MutationSerializer
}

func (st *serializedTool) Name() string        { return st.inner.Name() }
func (st *serializedTool) Description() string { return st.inner.Description() }
func (st *serializedTool) IsLongRunning() bool { return st.inner.IsLongRunning() }

func (st *serializedTool) Declaration() *genai.FunctionDeclaration {
	if rn, ok := st.inner.(runnableTool); ok {
		return rn.Declaration()
	}
	return nil
}

// ProcessRequest satisfies ADK's internal toolinternal.RequestProcessor
// interface (ADK requires every tool in f.Tools to implement it) and
// packs st — the wrapper — so dispatch routes through the serializer
// instead of bypassing it. Same shape as gatedTool.ProcessRequest.
func (st *serializedTool) ProcessRequest(ctx adktool.Context, req *model.LLMRequest) error {
	return PackTool(req, st)
}

func (st *serializedTool) Run(ctx adktool.Context, args any) (map[string]any, error) {
	rn, ok := st.inner.(runnableTool)
	if !ok {
		return nil, nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return rn.Run(ctx, args)
}
