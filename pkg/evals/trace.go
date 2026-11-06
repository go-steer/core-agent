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

// Package evals provides session-trace extraction, trajectory and invariant
// evaluators, and call-schema validation for grading stored core-agent
// sessions (#1538).
package evals

import (
	"encoding/json"
	"sort"

	"google.golang.org/genai"

	adksession "google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/internal/effects"
	"github.com/go-steer/core-agent/v3/internal/modeltext"
)

// Class is a tool's effect classification under the mutation predicate.
type Class int

const (
	// ClassReadOnly tools have no external side effects.
	ClassReadOnly Class = iota
	// ClassMutating tools mutate external state (default-deny-unknown).
	ClassMutating
	// ClassSpawning tools start sub-runs whose inner tool calls are governed
	// by the sub-run's own outbox.
	ClassSpawning
)

// Predicate classifies a tool by name into ClassReadOnly, ClassMutating, or
// ClassSpawning.
type Predicate func(toolName string) Class

// ReadOnlyHint reports the `readOnlyHint` an MCP server published for toolName.
type ReadOnlyHint func(toolName string) (readOnly, annotated bool)

// NewPredicate builds a mutation predicate with optional per-tool mutating
// overrides (true = mutating, false = read-only). Unlisted built-in tools use
// their registered class; unknown tools default to ClassMutating.
func NewPredicate(overrides map[string]bool) Predicate {
	return NewPredicateWithHints(overrides, nil)
}

// NewPredicateWithHints builds a mutation predicate that also consults MCP
// readOnlyHint annotations for unclassified tools.
func NewPredicateWithHints(overrides map[string]bool, hint ReadOnlyHint) Predicate {
	var effHint effects.ReadOnlyHint
	if hint != nil {
		effHint = effects.ReadOnlyHint(hint)
	}
	effPred := effects.NewPredicateWithHints(overrides, effHint)
	return func(toolName string) Class {
		return Class(effPred(toolName))
	}
}

// Trace is the provider-free view of one recorded run that every
// evaluator scores against.
//
// It is a plain struct on purpose. Evaluators take a Trace, never an
// event log, so their unit tests construct the adversarial shapes
// directly — a duplicated effect, an orphan completion — instead of
// having to stage a session store that produces them. TraceFromEvents
// is the only place that knows about ADK.
type Trace struct {
	// Calls are the tool calls in the order the log records them,
	// engine control-flow calls and sub-agent delegations excluded (a
	// dangling adk_request_input is the normal shape of a paused
	// session, not an effect).
	Calls []Call

	// FinalText is the last non-empty model text in the log — the
	// response the severity evaluator reads.
	FinalText string

	// StructuredSeverity is set when the run produced a typed report
	// carrying an explicit severity. Empty until W1.3 lands the typed
	// report contract; the severity evaluator falls back to FinalText.
	StructuredSeverity string
}

// Call is one recorded tool call and its completion, if it has one.
type Call struct {
	Name string
	Args map[string]any
	ID   string

	// Class is the mutation predicate's verdict at scan time.
	Class Class

	// EventIndex is the call's position in the event log.
	EventIndex int

	// Completed reports whether a real completion was recorded. A call
	// without one either never ran, ran and lost its completion to a
	// crash (the ambiguous window), or was declined at a gate.
	Completed bool
	// ResponseIndex is the event index of the completion, or -1.
	ResponseIndex int

	// Response is the payload the tool answered with, nil until a
	// completion is recorded. Retained for #169: the arguments say what
	// the model asked for and only the result says whether it learned
	// anything, and the difference between "never called the tool" and
	// "called it against a scope that matched nothing" is invisible
	// without it. ADK puts a handler error under the "error" key here,
	// which is what makes a failed call distinguishable from an empty
	// one.
	Response map[string]any

	// Park is the write gate's question about this call, nil when the
	// gate did not ask. Answer is its record of how that question was
	// resolved, nil when nothing answered. Two fields because they have
	// two authors and either can be absent independently — park.go
	// enumerates the four combinations and what each one means (#295).
	Park   *Park
	Answer *Answer
}

// Mutating reports whether this call is one the outbox guards.
// Spawning calls count: they start sub-runs whose effects this process
// cannot individually attribute.
func (c Call) Mutating() bool {
	return c.Class == ClassMutating || c.Class == ClassSpawning
}

// Identity is the effect-equality key for exactly-once: the same tool
// with the same arguments is the same effect. Scaling two different
// deployments is two effects; scaling one deployment twice is the
// violation.
//
// Args are canonicalized through JSON with sorted keys, so map
// iteration order cannot make one effect look like two.
func (c Call) Identity() string {
	b, err := json.Marshal(canonical(c.Args))
	if err != nil {
		// Unmarshalable args cannot be compared for equality, so fall
		// back to the call ID: every such call is its own identity and
		// no false duplicate is reported.
		return c.Name + "\x00id:" + c.ID
	}
	return c.Name + "\x00" + string(b)
}

// canonical rewrites nested maps into sorted-key form for stable JSON.
func canonical(v any) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		// json.Marshal already sorts map keys, but nested slices of maps
		// need the recursion, and being explicit costs nothing.
		out := make(map[string]any, len(t))
		for _, k := range keys {
			out[k] = canonical(t[k])
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = canonical(t[i])
		}
		return out
	default:
		return v
	}
}

// CalledTools returns the distinct tool names the trace called, sorted.
func (t Trace) CalledTools() []string {
	seen := make(map[string]bool, len(t.Calls))
	var out []string
	for _, c := range t.Calls {
		if !seen[c.Name] {
			seen[c.Name] = true
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

// TraceFromEvents extracts a Trace from a recorded session event log.
//
// This is the seam between the harness and a real run: everything
// downstream is a pure function. The walk mirrors effects.pairScan's
// pairing rules deliberately — same event indexing, same empty-ID skip,
// same treatment of a confirmation placeholder as "not a completion" —
// because an evaluator that paired calls differently from the outbox
// would score a contract the runtime does not implement.
//
// If pred is nil, NewPredicate(nil) is used.
func TraceFromEvents(events adksession.Events, pred Predicate, subAgents map[string]bool) Trace {
	if events == nil {
		return Trace{}
	}
	if pred == nil {
		pred = NewPredicate(nil)
	}
	var tr Trace
	byID := make(map[string]int)     // call ID -> index into tr.Calls
	control := make(map[string]bool) // call IDs excluded as engine control flow
	evIdx := -1

	parks := map[string]Park{}
	answers := map[string]Answer{}

	for ev := range events.All() {
		if ev == nil || ev.Partial {
			continue
		}
		scanAnswers(ev, answers)
		if ev.Content == nil {
			continue
		}
		scanParks(ev, parks)
		evIdx++

		for _, part := range ev.Content.Parts {
			if part == nil {
				continue
			}
			if text, ok := modeltext.Text(part); ok && ev.Content.Role == genai.RoleModel {
				tr.FinalText = text
			}
			if fc := part.FunctionCall; fc != nil && fc.ID != "" {
				if IsControlCall(fc.Name, subAgents) {
					control[fc.ID] = true
					continue
				}
				if _, dup := byID[fc.ID]; dup {
					continue
				}
				byID[fc.ID] = len(tr.Calls)
				tr.Calls = append(tr.Calls, Call{
					Name:          fc.Name,
					Args:          fc.Args,
					ID:            fc.ID,
					Class:         pred(fc.Name),
					EventIndex:    evIdx,
					ResponseIndex: -1,
				})
			}
			if fr := part.FunctionResponse; fr != nil && fr.ID != "" {
				if control[fr.ID] {
					continue
				}
				if _, paired := byID[fr.ID]; !paired && IsControlCall(fr.Name, subAgents) {
					continue
				}
				if _, pending := ev.Actions.RequestedToolConfirmations[fr.ID]; pending {
					continue
				}
				i, ok := byID[fr.ID]
				if !ok {
					tr.Calls = append(tr.Calls, Call{
						Name:          fr.Name,
						ID:            fr.ID,
						Class:         pred(fr.Name),
						EventIndex:    -1,
						Completed:     true,
						ResponseIndex: evIdx,
						Response:      fr.Response,
					})
					byID[fr.ID] = len(tr.Calls) - 1
					continue
				}
				tr.Calls[i].Completed = true
				tr.Calls[i].ResponseIndex = evIdx
				tr.Calls[i].Response = fr.Response
			}
		}
	}

	for id, p := range parks {
		if i, ok := byID[id]; ok {
			tr.Calls[i].Park = &p
		}
	}
	for id, a := range answers {
		if i, ok := byID[id]; ok {
			tr.Calls[i].Answer = &a
		}
	}
	return tr
}

// IsControlCall reports whether name is an ADK engine control-flow call or a
// registered sub-agent delegation excluded from tool effect traces.
func IsControlCall(name string, subAgents map[string]bool) bool {
	switch name {
	case "adk_request_input",
		"adk_request_credential",
		"adk_request_confirmation",
		"finish_task",
		"mark_task_done",
		"transfer_to_agent",
		"task_completed",
		"exit_loop",
		"pause_session":
		return true
	}
	return subAgents[name]
}
