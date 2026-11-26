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

package agent

import (
	"strings"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/tool"
)

// isGeminiModel reports whether modelName identifies a Gemini-family model.
func isGeminiModel(modelName string) bool {
	return strings.Contains(strings.ToLower(modelName), "gemini")
}

// quirkToolSurface summarizes which built-in tool families an agent can
// actually call so provider quirks never cite disabled or absent tools
// (#1608). Derived from the agent's own tools and toolsets — never from
// a shared permissions.Gate, whose catalog describes the parent/process
// build rather than a tool-scoped subagent.
type quirkToolSurface struct {
	hasAnyTool  bool
	hasSearch   bool // grep or glob
	hasGrep     bool // grep
	hasReadFile bool // read_file
	hasBash     bool // bash
	hasEditFile bool // edit_file or write_file
}

func (s *quirkToolSurface) addToolName(name string) {
	if name == "" {
		return
	}
	s.hasAnyTool = true
	switch name {
	case "grep":
		s.hasSearch = true
		s.hasGrep = true
	case "glob":
		s.hasSearch = true
	case "read_file":
		s.hasReadFile = true
	case "bash":
		s.hasBash = true
	case "edit_file", "write_file":
		s.hasEditFile = true
	}
}

func (s *quirkToolSurface) merge(other quirkToolSurface) {
	s.hasAnyTool = s.hasAnyTool || other.hasAnyTool
	s.hasSearch = s.hasSearch || other.hasSearch
	s.hasGrep = s.hasGrep || other.hasGrep
	s.hasReadFile = s.hasReadFile || other.hasReadFile
	s.hasBash = s.hasBash || other.hasBash
	s.hasEditFile = s.hasEditFile || other.hasEditFile
}

func quirkSurfaceFromTools(ts []tool.Tool) quirkToolSurface {
	var s quirkToolSurface
	for _, t := range ts {
		if t != nil {
			s.addToolName(t.Name())
		}
	}
	return s
}

func quirkSurfaceFromNames(names ...string) quirkToolSurface {
	var s quirkToolSurface
	for _, name := range names {
		s.addToolName(name)
	}
	return s
}

// geminiParallelismQuirk renders the layer-2 Gemini parallelism guidance
// tailored to s (#1608). A tool-less agent gets no quirk at all; an agent
// with only non-builtin tools (such as MCP-only specialists) gets generic
// parallel-call wording without citing unavailable file, grep, shell, or
// edit tools.
func geminiParallelismQuirk(s quirkToolSurface) string {
	if !s.hasAnyTool {
		return ""
	}
	var examples []string
	if s.hasSearch {
		examples = append(examples, "searching")
	}
	if s.hasReadFile {
		examples = append(examples, "reading files")
	}
	if s.hasBash {
		examples = append(examples, "independent shell commands")
	}
	if s.hasEditFile {
		examples = append(examples, "editing different files")
	}

	first := "Execute multiple independent tool calls in parallel when feasible."
	if len(examples) > 0 {
		first = "Execute multiple independent tool calls in parallel when feasible — " + joinOr(examples) + "."
	}

	var batch []string
	if s.hasReadFile {
		batch = append(batch, "read multiple files")
	}
	if s.hasGrep {
		batch = append(batch, "grep multiple directories")
	}

	second := "Issue independent calls in a single response; do not execute them one by one."
	if len(batch) > 0 {
		second = "If you need to " + joinOr(batch) + ", issue all the tool calls in a single response; do not execute them one by one."
	}
	return first + " " + second
}

func joinOr(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " or " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", or " + items[len(items)-1]
	}
}

// maxTrackedInvocations bounds pending per-invocation toolset observations
// in case an ADK processor between toolProcessor and
// instructionsRequestProcessor aborts a step before consume runs.
const maxTrackedInvocations = 128

type invocationSurface struct {
	surfaces []quirkToolSurface
	seq      uint64
}

// toolsetSurfaceTracker observes tool names yielded by each tool.Toolset
// during ADK's toolProcessor pass and hands the combined quirkToolSurface
// to InstructionProvider during instructionsRequestProcessor (which runs
// immediately after in ADK's DefaultRequestProcessors).
//
// Observing on the existing ADK pass avoids calling Toolset.Tools(ctx) in
// agent.New (which has no context and must not trigger lazy MCP network
// connects) and avoids duplicating MCP ListTools RPCs per model step.
type toolsetSurfaceTracker struct {
	base    quirkToolSurface
	numSets int

	mu     sync.Mutex
	seq    uint64
	byInv  map[string]*invocationSurface
	latest []quirkToolSurface
}

func newToolsetSurfaceTracker(direct []tool.Tool, toolsets []tool.Toolset) (*toolsetSurfaceTracker, []tool.Toolset) {
	tr := &toolsetSurfaceTracker{
		base:    quirkSurfaceFromTools(direct),
		numSets: len(toolsets),
		byInv:   make(map[string]*invocationSurface),
		latest:  make([]quirkToolSurface, len(toolsets)),
	}
	wrapped := make([]tool.Toolset, len(toolsets))
	for i, ts := range toolsets {
		if ts == nil {
			continue
		}
		wrapped[i] = &observedToolset{inner: ts, tracker: tr, index: i}
	}
	return tr, wrapped
}

func (tr *toolsetSurfaceTracker) record(ctx adkagent.ReadonlyContext, index int, ts []tool.Tool) {
	surface := quirkSurfaceFromTools(ts)
	invID := invocationIDFrom(ctx)

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if invID == "" {
		tr.latest[index] = surface
		return
	}
	entry, ok := tr.byInv[invID]
	if !ok {
		tr.evictOldestIfFullLocked()
		tr.seq++
		surfaces := make([]quirkToolSurface, tr.numSets)
		if index > 0 {
			// If this invocation was evicted between earlier toolset slots and
			// this one, seed prior slots from the last committed step rather
			// than silently zeroing them out.
			copy(surfaces[:index], tr.latest[:index])
		}
		entry = &invocationSurface{
			surfaces: surfaces,
			seq:      tr.seq,
		}
		tr.byInv[invID] = entry
	}
	entry.surfaces[index] = surface
}

func (tr *toolsetSurfaceTracker) discard(ctx adkagent.ReadonlyContext) {
	invID := invocationIDFrom(ctx)
	if invID == "" {
		return
	}
	tr.mu.Lock()
	delete(tr.byInv, invID)
	tr.mu.Unlock()
}

func (tr *toolsetSurfaceTracker) consume(ctx adkagent.ReadonlyContext) quirkToolSurface {
	out := tr.base
	invID := invocationIDFrom(ctx)

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if invID != "" {
		if entry, ok := tr.byInv[invID]; ok {
			delete(tr.byInv, invID)
			copy(tr.latest, entry.surfaces)
			for _, s := range entry.surfaces {
				out.merge(s)
			}
			return out
		}
	}
	for _, s := range tr.latest {
		out.merge(s)
	}
	return out
}

func (tr *toolsetSurfaceTracker) evictOldestIfFullLocked() {
	if len(tr.byInv) < maxTrackedInvocations {
		return
	}
	var oldestKey string
	var oldestSeq uint64
	first := true
	for k, v := range tr.byInv {
		if first || v.seq < oldestSeq {
			oldestKey = k
			oldestSeq = v.seq
			first = false
		}
	}
	delete(tr.byInv, oldestKey)
}

func invocationIDFrom(ctx adkagent.ReadonlyContext) string {
	if ctx == nil {
		return ""
	}
	return ctx.InvocationID()
}

type observedToolset struct {
	inner   tool.Toolset
	tracker *toolsetSurfaceTracker
	index   int
}

func (o *observedToolset) Name() string                { return o.inner.Name() }
func (o *observedToolset) UnwrapToolset() tool.Toolset { return o.inner }

func (o *observedToolset) Tools(ctx adkagent.ReadonlyContext) ([]tool.Tool, error) {
	ts, err := o.inner.Tools(ctx)
	if err != nil {
		o.tracker.discard(ctx)
		return nil, err
	}
	o.tracker.record(ctx, o.index, ts)
	return ts, nil
}

// buildInstructionProvider constructs the ADK InstructionProvider (or nil
// when the assembled prompt is empty) and, when a Gemini agent with quirks
// enabled registers Toolsets, wraps those toolsets to observe their
// resolved tool names for layer-2 quirk gating (#1608).
func buildInstructionProvider(o *options, modelName string, toolsets []tool.Toolset) (llmagent.InstructionProvider, []tool.Toolset) {
	if o.instructionExplicit {
		tail := make([]string, 0, 1+len(o.extraInstructions))
		if o.userInstruction != "" {
			tail = append(tail, o.userInstruction)
		}
		tail = append(tail, o.extraInstructions...)
		instruction := joinLayers(append([]string{o.instruction}, tail...))
		if instruction == "" {
			return nil, toolsets
		}
		return literalInstruction(instruction), toolsets
	}

	if !o.noQuirks && isGeminiModel(modelName) && len(toolsets) > 0 {
		tracker, wrapped := newToolsetSurfaceTracker(o.tools, toolsets)
		mode := o.mode
		userInstruction := o.userInstruction
		extras := append([]string(nil), o.extraInstructions...)
		return func(ctx adkagent.ReadonlyContext) (string, error) {
			return assembleInstruction(modelName, mode, false, tracker.consume(ctx), userInstruction, extras), nil
		}, wrapped
	}

	surface := quirkSurfaceFromTools(o.tools)
	instruction := assembleInstruction(modelName, o.mode, o.noQuirks, surface, o.userInstruction, o.extraInstructions)
	if instruction == "" {
		return nil, toolsets
	}
	return literalInstruction(instruction), toolsets
}
