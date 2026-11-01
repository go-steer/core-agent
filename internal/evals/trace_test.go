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

package evals

import (
	"context"
	"iter"
	"reflect"
	"testing"

	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/internal/effects"
)

// eventList adapts a slice to session.Events, the same shim
// internal/effects' unit tests use.
type eventList []*adksession.Event

func (e eventList) All() iter.Seq[*adksession.Event] {
	return func(yield func(*adksession.Event) bool) {
		for _, ev := range e {
			if !yield(ev) {
				return
			}
		}
	}
}
func (e eventList) Len() int                   { return len(e) }
func (e eventList) At(i int) *adksession.Event { return e[i] }

func modelEvent(parts ...*genai.Part) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv")
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: parts}
	return ev
}

func userEvent(parts ...*genai.Part) *adksession.Event {
	ev := adksession.NewEvent(context.Background(), "inv")
	ev.Content = &genai.Content{Role: genai.RoleUser, Parts: parts}
	return ev
}

func callPart(name, id string, args map[string]any) *genai.Part {
	if args == nil {
		args = map[string]any{}
	}
	p := genai.NewPartFromFunctionCall(name, args)
	p.FunctionCall.ID = id
	return p
}

func respPart(name, id string) *genai.Part {
	p := genai.NewPartFromFunctionResponse(name, map[string]any{"ok": true})
	p.FunctionResponse.ID = id
	return p
}

// readOnly is the predicate the k8s scenarios run under: lookout's read
// tools are read-only, kubectl_rollback_deployment is not.
func readOnlyPred() effects.Predicate {
	return effects.NewPredicate(map[string]bool{
		"k8s_triage_workload": false,
		"k8s_cluster_health":  false,
		"k8s_triage_logs":     false,
		"k8s_event_timeline":  false,
	})
}

func TestTraceFromEvents_PairsAndOrders(t *testing.T) {
	pred := readOnlyPred()
	events := eventList{
		userEvent(genai.NewPartFromText("api-server is crashlooping")), // idx0
		nil, // skipped entirely, does not advance the index
		modelEvent(callPart("k8s_triage_workload", "c1", nil)),                     // idx1
		userEvent(respPart("k8s_triage_workload", "c1")),                           // idx2
		modelEvent(callPart("scale_deployment", "c2", map[string]any{"to": 3})),    // idx3
		userEvent(respPart("scale_deployment", "c2")),                              // idx4
		modelEvent(genai.NewPartFromText("CRITICAL: api-server is CrashLoopBack")), // idx5
	}

	tr := TraceFromEvents(events, pred, nil)

	if len(tr.Calls) != 2 {
		t.Fatalf("Calls = %+v, want 2", tr.Calls)
	}
	if got := tr.Calls[0]; got.Name != "k8s_triage_workload" || got.EventIndex != 1 || got.ResponseIndex != 2 || !got.Completed {
		t.Fatalf("Calls[0] = %+v, want triage@1 completed@2", got)
	}
	if tr.Calls[0].Mutating() {
		t.Fatal("k8s_triage_workload is read-only under the predicate but Mutating() is true")
	}
	if got := tr.Calls[1]; got.Name != "scale_deployment" || got.EventIndex != 3 || !got.Mutating() {
		t.Fatalf("Calls[1] = %+v, want mutating scale_deployment@3", got)
	}
	if tr.FinalText != "CRITICAL: api-server is CrashLoopBack" {
		t.Fatalf("FinalText = %q", tr.FinalText)
	}
	if got := tr.CalledTools(); !reflect.DeepEqual(got, []string{"k8s_triage_workload", "scale_deployment"}) {
		t.Fatalf("CalledTools() = %v", got)
	}
}

// TestTraceFromEvents_KeepsTheResult pins #169's addition. The
// arguments say what the model asked for; only the result says whether
// it learned anything, and every evaluator that wants to tell "never
// called the tool" from "called it against a scope that held nothing"
// reads it from here.
func TestTraceFromEvents_KeepsTheResult(t *testing.T) {
	failed := genai.NewPartFromFunctionResponse("k8s_triage_logs", map[string]any{"error": "namespace not found"})
	failed.FunctionResponse.ID = "c2"

	events := eventList{
		modelEvent(callPart("k8s_triage_workload", "c1", nil)),
		userEvent(respPart("k8s_triage_workload", "c1")),
		modelEvent(callPart("k8s_triage_logs", "c2", nil)),
		userEvent(failed),
		modelEvent(callPart("k8s_event_timeline", "c3", nil)),
	}

	tr := TraceFromEvents(events, readOnlyPred(), nil)
	if len(tr.Calls) != 3 {
		t.Fatalf("Calls = %+v, want 3", tr.Calls)
	}
	if got := tr.Calls[0].Response["ok"]; got != true {
		t.Errorf("Calls[0].Response = %v, want the recorded payload", tr.Calls[0].Response)
	}
	// The error key is what makes a failed call distinguishable from an
	// empty one; dropping the response would collapse the two.
	if got := tr.Calls[1].Response["error"]; got != "namespace not found" {
		t.Errorf("Calls[1].Response = %v, want the handler error", tr.Calls[1].Response)
	}
	if tr.Calls[2].Response != nil {
		t.Errorf("a call with no completion carries a response: %v", tr.Calls[2].Response)
	}
}

// TestTraceFromEvents_OrphanCompletionKeepsItsResult. An orphan is the
// one call whose response arrives without its call event; it is
// recorded, so what it answered has to be recorded with it.
func TestTraceFromEvents_OrphanCompletionKeepsItsResult(t *testing.T) {
	events := eventList{userEvent(respPart("k8s_triage_workload", "orphan"))}
	tr := TraceFromEvents(events, readOnlyPred(), nil)
	if len(tr.Calls) != 1 {
		t.Fatalf("Calls = %+v, want the orphan recorded", tr.Calls)
	}
	if got := tr.Calls[0].Response["ok"]; got != true {
		t.Errorf("the orphan's response was dropped: %v", tr.Calls[0].Response)
	}
}

func TestTraceFromEvents_Exclusions(t *testing.T) {
	pred := readOnlyPred()

	t.Run("control-and-subagent-calls-are-not-tool-calls", func(t *testing.T) {
		events := eventList{
			modelEvent(callPart("adk_request_input", "i1", nil)),
			modelEvent(callPart("transfer_to_agent", "t1", nil)),
			modelEvent(callPart("triage_bot", "d1", nil)),
			modelEvent(callPart("k8s_cluster_health", "c1", nil)),
		}
		tr := TraceFromEvents(events, pred, map[string]bool{"triage_bot": true})
		if got := tr.CalledTools(); !reflect.DeepEqual(got, []string{"k8s_cluster_health"}) {
			t.Fatalf("CalledTools() = %v, want only the real tool", got)
		}
	})

	// Regression, found by E-exactly-once (W0.3): excluding a control
	// call on the call side is not enough. Its completion has no
	// recorded call to pair with, so the orphan fallback re-materializes
	// it — classified mutating by default-deny, with EventIndex -1 — and
	// two delegations to the same specialist then score as one effect
	// applied twice. The paired shape below is the one a real run
	// produces; the call-only fixture above cannot reach it.
	t.Run("control-and-subagent-completions-are-not-orphans", func(t *testing.T) {
		events := eventList{
			modelEvent(callPart("remediator", "d1", map[string]any{"request": "scale it"})),
			userEvent(respPart("remediator", "d1")),
			modelEvent(callPart("finish_task", "f1", nil)),
			userEvent(respPart("finish_task", "f1")),
			modelEvent(callPart("remediator", "d2", map[string]any{"request": "scale it"})),
			userEvent(respPart("remediator", "d2")),
			modelEvent(callPart("k8s_cluster_health", "c1", nil)),
			userEvent(respPart("k8s_cluster_health", "c1")),
		}
		tr := TraceFromEvents(events, pred, map[string]bool{"remediator": true})
		if got := tr.CalledTools(); !reflect.DeepEqual(got, []string{"k8s_cluster_health"}) {
			t.Fatalf("CalledTools() = %v, want only the real tool", got)
		}
		if len(tr.Calls) != 1 {
			t.Fatalf("Calls = %+v, want exactly the one real call", tr.Calls)
		}
		// The consequence, asserted directly: two identical delegations
		// must not read as a repeated effect.
		if res := ExactlyOnce(tr); !res.Passed() {
			t.Fatalf("ExactlyOnce = %.2f (%s), want a pass — delegations are not effects", res.Score, res.Comment)
		}
	})

	t.Run("orphan-completion-with-an-absent-call-is-still-excluded", func(t *testing.T) {
		// A truncated log: the delegation's call event is gone, so the
		// call-side exclusion never saw the ID. The response must be
		// judged by its own name rather than defaulting to mutating.
		events := eventList{
			userEvent(respPart("remediator", "d1")),
			userEvent(respPart("finish_task", "f1")),
		}
		if tr := TraceFromEvents(events, pred, map[string]bool{"remediator": true}); len(tr.Calls) != 0 {
			t.Fatalf("Calls = %+v, want none (control-flow completions are not effects)", tr.Calls)
		}
	})

	t.Run("empty-id-calls-are-unkeyable", func(t *testing.T) {
		events := eventList{modelEvent(callPart("k8s_cluster_health", "", nil))}
		if tr := TraceFromEvents(events, pred, nil); len(tr.Calls) != 0 {
			t.Fatalf("Calls = %+v, want none (empty ID cannot be paired)", tr.Calls)
		}
	})

	t.Run("user-text-does-not-become-the-final-response", func(t *testing.T) {
		events := eventList{
			modelEvent(genai.NewPartFromText("OK: cluster is healthy")),
			userEvent(genai.NewPartFromText("thanks, and check the CRITICAL alerts too")),
		}
		if tr := TraceFromEvents(events, pred, nil); tr.FinalText != "OK: cluster is healthy" {
			t.Fatalf("FinalText = %q, want the model's report", tr.FinalText)
		}
	})

	t.Run("long-running-calls-are-kept", func(t *testing.T) {
		// internal/effects defers these at turn start; a finished run's
		// completed blocking tool is an effect like any other.
		ev := modelEvent(callPart("scale_deployment", "lr1", nil))
		ev.LongRunningToolIDs = []string{"lr1"}
		tr := TraceFromEvents(eventList{ev, userEvent(respPart("scale_deployment", "lr1"))}, pred, nil)
		if len(tr.Calls) != 1 || !tr.Calls[0].Completed {
			t.Fatalf("Calls = %+v, want one completed long-running call", tr.Calls)
		}
		// EventIndex 0, not -1: if the call were dropped, its response
		// would still land as an orphan and the assertions above would
		// pass on a trace that had lost the intent.
		if tr.Calls[0].EventIndex != 0 {
			t.Fatalf("Calls[0] = %+v, want the intent recorded at event 0, not an orphaned completion", tr.Calls[0])
		}
	})
}

func TestTraceFromEvents_ConfirmationPlaceholderIsNotCompletion(t *testing.T) {
	// An approval-gated mutation: call, placeholder response while the
	// flow pauses, then the approved re-fire under the SAME id. Counting
	// the placeholder would report one effect as two.
	pred := readOnlyPred()
	placeholder := userEvent(respPart("scale_deployment", "c1"))
	placeholder.Actions.RequestedToolConfirmations = map[string]toolconfirmation.ToolConfirmation{
		"c1": {Hint: "scale payments to 3 replicas?"},
	}

	events := eventList{
		modelEvent(callPart("scale_deployment", "c1", map[string]any{"to": 3})), // idx0
		placeholder, // idx1: not a completion
		modelEvent(callPart("scale_deployment", "c1", map[string]any{"to": 3})), // idx2: same id, re-fire
		userEvent(respPart("scale_deployment", "c1")),                           // idx3: the real completion
	}
	tr := TraceFromEvents(events, pred, nil)

	if len(tr.Calls) != 1 {
		t.Fatalf("Calls = %+v, want 1 (the re-fire shares the original id)", tr.Calls)
	}
	if c := tr.Calls[0]; !c.Completed || c.EventIndex != 0 || c.ResponseIndex != 3 {
		t.Fatalf("Calls[0] = %+v, want intent@0 completed@3", c)
	}
	if r := ExactlyOnce(tr); r.Score != 1 {
		t.Fatalf("ExactlyOnce = %v (%s), want 1 — an approved re-fire is one effect", r.Score, r.Comment)
	}
}

func TestTraceFromEvents_DeclinedGateDidNotRun(t *testing.T) {
	// The other half of the placeholder guard, and W0.3's
	// E-approval-rejected shape: the operator says no, so the call never
	// re-fires and the placeholder is the only response in the log.
	// Reading it as a completion would score a refused mutation as an
	// executed one.
	placeholder := userEvent(respPart("scale_deployment", "c1"))
	placeholder.Actions.RequestedToolConfirmations = map[string]toolconfirmation.ToolConfirmation{
		"c1": {Hint: "scale payments to 3 replicas?"},
	}
	events := eventList{
		modelEvent(callPart("scale_deployment", "c1", map[string]any{"to": 3})),
		placeholder,
	}

	tr := TraceFromEvents(events, readOnlyPred(), nil)
	if len(tr.Calls) != 1 {
		t.Fatalf("Calls = %+v, want 1", tr.Calls)
	}
	if tr.Calls[0].Completed {
		t.Fatal("a declined confirmation placeholder was read as a completed effect")
	}
	if r := ExactlyOnce(tr); !r.Vacuous {
		t.Fatalf("ExactlyOnce = %+v, want vacuous — nothing was executed", r)
	}
}

func TestTraceFromEvents_OrphanCompletion(t *testing.T) {
	// A completion whose call is absent: the log cannot say what ran.
	events := eventList{userEvent(respPart("scale_deployment", "ghost"))}
	tr := TraceFromEvents(events, readOnlyPred(), nil)

	if len(tr.Calls) != 1 {
		t.Fatalf("Calls = %+v, want the orphan recorded", tr.Calls)
	}
	c := tr.Calls[0]
	if c.EventIndex != -1 || !c.Completed || c.ResponseIndex != 0 {
		t.Fatalf("orphan = %+v, want EventIndex -1, completed at 0", c)
	}
	if r := EffectOrdering(tr); r.Score != 0 {
		t.Fatalf("EffectOrdering = %v (%s), want 0 for an unattributable effect", r.Score, r.Comment)
	}
}

// TestControlCallsMatchEffects cross-checks isControl against the
// runtime's own exclusion set, behaviourally: a dangling call to each
// name must land in ScanDangling's Deferred bucket, which is where
// internal/effects puts control calls. It catches a name dropped from
// effects' set. It cannot catch one added — see isControl's comment.
func TestControlCallsMatchEffects(t *testing.T) {
	names := []string{
		"adk_request_input",
		"adk_request_credential",
		"adk_request_confirmation",
		"finish_task",
		"transfer_to_agent",
		"task_completed",
		"exit_loop",
		"pause_session",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if !isControl(name, nil) {
				t.Fatalf("isControl(%q) = false", name)
			}
			ds := effects.ScanDangling(
				eventList{modelEvent(callPart(name, "c1", nil))},
				effects.NewPredicate(nil), nil)
			if len(ds.Deferred) != 1 {
				t.Fatalf("internal/effects no longer treats %q as a control call "+
					"(Deferred=%d); isControl in trace.go must be updated", name, len(ds.Deferred))
			}
		})
	}

	if isControl("k8s_cluster_health", nil) {
		t.Fatal("isControl matched an ordinary tool name")
	}
}

func TestCallIdentity_ArgOrderIndependent(t *testing.T) {
	a := Call{Name: "scale", Args: map[string]any{"ns": "prod", "replicas": 3.0}}
	b := Call{Name: "scale", Args: map[string]any{"replicas": 3.0, "ns": "prod"}}
	c := Call{Name: "scale", Args: map[string]any{"ns": "staging", "replicas": 3.0}}

	if a.identity() != b.identity() {
		t.Fatalf("key order changed identity:\n %s\n %s", a.identity(), b.identity())
	}
	if a.identity() == c.identity() {
		t.Fatal("different arguments collapsed to one identity")
	}
	if (Call{Name: "scale"}).identity() == (Call{Name: "restart"}).identity() {
		t.Fatal("different tools collapsed to one identity")
	}
}
