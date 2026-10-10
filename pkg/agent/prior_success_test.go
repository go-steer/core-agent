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
	"context"
	"errors"
	"iter"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/models"
)

// #1247. The agent marks its models.PriorSuccess after a served model
// call and puts it on every turn's context; the provider's retry policy
// reads it to decide whether an ambiguous rejection gets one retry.

var errBare400 = errors.New("Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT, Details: []")

// errCallNoop, in a servedProbeLLM script, answers with a call to the
// noop tool instead of text: a served call that keeps the run going.
var errCallNoop = errors.New("script: call the noop tool")

const noopToolName = "noop"

func newNoopTool(t *testing.T) tool.Tool {
	t.Helper()
	type empty struct{}
	tl, err := functiontool.New(
		functiontool.Config{Name: noopToolName, Description: "does nothing"},
		func(adkagent.Context, empty) (empty, error) { return empty{}, nil },
	)
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}
	return tl
}

// probeCall is what the model saw on the context of one call.
type probeCall struct {
	rec       *models.PriorSuccess
	succeeded bool
}

// servedProbeLLM answers its Nth call with the Nth script entry — an
// error, or text — through a real models.RetryPolicy wired the way the
// Gemini adapter wires it, and records the PriorSuccess each call saw.
type servedProbeLLM struct {
	mu     sync.Mutex
	script []error // nil = answer with text
	calls  int
	seen   []probeCall
	policy *models.RetryPolicy
}

func newServedProbeLLM(script ...error) *servedProbeLLM {
	return &servedProbeLLM{
		script: script,
		policy: &models.RetryPolicy{
			IsTransient:             func(error) bool { return false },
			IsTransientAfterSuccess: func(err error) bool { return errors.Is(err, errBare400) },
			Backoff:                 time.Millisecond,
			Cooldown:                -1,
		},
	}
}

func (*servedProbeLLM) Name() string { return "served-probe" }

func (l *servedProbeLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return l.policy.Wrap(ctx, func() iter.Seq2[*adkmodel.LLMResponse, error] {
		l.mu.Lock()
		rec := models.PriorSuccessFrom(ctx)
		l.seen = append(l.seen, probeCall{rec: rec, succeeded: rec.Succeeded()})
		var err error
		if l.calls < len(l.script) {
			err = l.script[l.calls]
		}
		l.calls++
		l.mu.Unlock()
		return func(yield func(*adkmodel.LLMResponse, error) bool) {
			part := &genai.Part{Text: "ok"}
			switch {
			case errors.Is(err, errCallNoop):
				part = &genai.Part{FunctionCall: &genai.FunctionCall{Name: noopToolName, Args: map[string]any{}}}
			case err != nil:
				yield(nil, err)
				return
			}
			yield(&adkmodel.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}},
				FinishReason: genai.FinishReasonStop,
				TurnComplete: true,
			}, nil)
		}
	})
}

func (l *servedProbeLLM) snapshot() (int, []probeCall) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls, append([]probeCall(nil), l.seen...)
}

func runTurnErrs(a *Agent, prompt string) []error {
	var errs []error
	for _, err := range a.Run(context.Background(), prompt) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// The whole policy at the agent level: a bare 400 on the session's
// first call fails exactly as it did before #1247; the record is marked
// by the served call that follows, and not before it; and the same 400
// after that is retried and recovers.
func TestRun_PriorSuccessIsMarkedOnlyAfterAServedCall(t *testing.T) {
	t.Parallel()
	llm := newServedProbeLLM(errBare400, nil, errBare400, nil)
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Turn 1: the first call is rejected. Not retried, not marked.
	errs := runTurnErrs(a, "one")
	calls, seen := llm.snapshot()
	if calls != 1 {
		t.Fatalf("turn 1: %d model calls, want 1 — a bare 400 on the first call is never retried", calls)
	}
	var re *models.RetryError
	if len(errs) == 0 || !errors.Is(errs[0], errBare400) || errors.As(errs[0], &re) {
		t.Fatalf("turn 1: errs = %v, want the bare 400, unwrapped", errs)
	}
	if seen[0].rec == nil || seen[0].rec != a.priorSuccess {
		t.Fatalf("turn 1: the model saw record %p, want the agent's %p", seen[0].rec, a.priorSuccess)
	}
	if a.priorSuccess.Succeeded() {
		t.Fatal("turn 1: a rejected call marked the record")
	}

	// Turn 2: served. The call itself still sees an unmarked record —
	// the mark follows the call, it does not precede it.
	if errs := runTurnErrs(a, "two"); len(errs) != 0 {
		t.Fatalf("turn 2: %v", errs)
	}
	calls, seen = llm.snapshot()
	if calls != 2 || seen[1].succeeded {
		t.Fatalf("turn 2: calls = %d, saw succeeded = %v; want 2 and false", calls, seen[1].succeeded)
	}
	if !a.priorSuccess.Succeeded() {
		t.Fatal("turn 2: a served call did not mark the record")
	}

	// Turn 3: the #1247 turn. Retried once, recovered.
	if errs := runTurnErrs(a, "three"); len(errs) != 0 {
		t.Fatalf("turn 3: errs = %v, want the retry to recover", errs)
	}
	calls, seen = llm.snapshot()
	if calls != 4 {
		t.Errorf("turn 3: %d model calls in total, want 4 (the rejection and its retry)", calls)
	}
	if !seen[2].succeeded {
		t.Error("turn 3: the rejected call did not see the marked record")
	}
}

func TestMarkIfServed(t *testing.T) {
	t.Parallel()
	model := func(parts ...*genai.Part) *session.Event {
		ev := session.NewEvent(context.Background(), "inv")
		ev.Content = &genai.Content{Role: genai.RoleModel, Parts: parts}
		return ev
	}
	text := &genai.Part{Text: "hi"}
	for _, tc := range []struct {
		name string
		ev   func() *session.Event
		err  error
		want bool
	}{
		{"text", func() *session.Event { return model(text) }, nil, true},
		{"function call only", func() *session.Event {
			return model(&genai.Part{FunctionCall: &genai.FunctionCall{Name: "read_file"}})
		}, nil, true},
		{"nil event", func() *session.Event { return nil }, nil, false},
		{"with an error", func() *session.Event { return model(text) }, errBare400, false},
		{"partial chunk", func() *session.Event { ev := model(text); ev.Partial = true; return ev }, nil, false},
		{"error code", func() *session.Event { ev := model(text); ev.ErrorCode = "SAFETY"; return ev }, nil, false},
		{"no parts", func() *session.Event { return model() }, nil, false},
		{"no content", func() *session.Event { return session.NewEvent(context.Background(), "inv") }, nil, false},
		{"tool response (user role)", func() *session.Event {
			ev := model(&genai.Part{FunctionResponse: &genai.FunctionResponse{Name: "read_file"}})
			ev.Content.Role = genai.RoleUser
			return ev
		}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := models.NewPriorSuccess()
			markIfServed(rec, tc.ev(), tc.err)
			if rec.Succeeded() != tc.want {
				t.Errorf("marked = %v, want %v", rec.Succeeded(), tc.want)
			}
		})
	}
	markIfServed(nil, model(text), nil) // nil record: no panic
}

// A subtask runs its own instruction and tools on the parent's context;
// the parent's record must not reach its model.
func TestRunSubtask_DoesNotInheritThePriorSuccess(t *testing.T) {
	t.Parallel()
	llm := newServedProbeLLM()
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	parent := models.NewPriorSuccess()
	parent.Mark()

	if _, err := a.RunSubtask(models.WithPriorSuccess(context.Background(), parent), SubtaskSpec{
		Name:         "probe",
		SystemPrompt: "probe",
		UserMessage:  "go",
	}); err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}

	_, seen := llm.snapshot()
	if len(seen) == 0 {
		t.Fatal("no model call")
	}
	if seen[0].rec == parent || seen[0].succeeded {
		t.Errorf("the subtask's first call saw the parent's record (succeeded=%v)", seen[0].succeeded)
	}
}

// Same for a sync subagent delegation, whose parent has been served by
// the very call that delegated. Its record is per delegation, never the
// shared inner agent's (#741).
func TestSubagent_DoesNotInheritThePriorSuccess(t *testing.T) {
	t.Parallel()
	childLLM := newServedProbeLLM()
	child, err := New(childLLM, WithName("child"))
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	parent, err := New(&callThenStopLLM{tool: "child"}, WithName("parent"), WithSubagents([]*Agent{child}))
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}

	if errs := runTurnErrs(parent, "delegate"); len(errs) != 0 {
		t.Fatalf("Run: %v", errs)
	}

	_, seen := childLLM.snapshot()
	if len(seen) == 0 {
		t.Fatal("the delegation made no model call")
	}
	got := seen[0]
	if got.rec == nil || got.rec == parent.priorSuccess || got.rec == child.priorSuccess || got.succeeded {
		t.Errorf("child's first call saw record %p (succeeded=%v); want a fresh one, not the parent's %p or the inner agent's %p",
			got.rec, got.succeeded, parent.priorSuccess, child.priorSuccess)
	}
	if !parent.priorSuccess.Succeeded() {
		t.Error("the parent's served delegating call did not mark its record — the test proves nothing")
	}
}

// A delegation's own record is marked by its own served calls: a sync
// subagent whose first call was served gets the retry on its second.
func TestSubagent_DelegationMarksItsOwnRecord(t *testing.T) {
	t.Parallel()
	childLLM := newServedProbeLLM(errCallNoop, errBare400, nil)
	child, err := New(childLLM, WithName("child"), WithTools([]tool.Tool{newNoopTool(t)}))
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	parent, err := New(&callThenStopLLM{tool: "child"}, WithName("parent"), WithSubagents([]*Agent{child}))
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}

	if errs := runTurnErrs(parent, "delegate"); len(errs) != 0 {
		t.Fatalf("Run: %v", errs)
	}

	calls, seen := childLLM.snapshot()
	if calls != 3 {
		t.Fatalf("child made %d model calls, want 3 (served, rejected, retried)", calls)
	}
	if seen[0].succeeded || !seen[1].succeeded || seen[0].rec != seen[1].rec {
		t.Errorf("child saw succeeded=%v then %v on records %p, %p; want false then true on one record",
			seen[0].succeeded, seen[1].succeeded, seen[0].rec, seen[1].rec)
	}
}

// The same for a subtask run by a library caller that does not mark it
// as a side call. Both in-tree callers do, and a side call never
// qualifies (TestRetryAfterSuccess_NotRetriedWithoutAServedSession).
func TestRunSubtask_MarksItsOwnRecord(t *testing.T) {
	t.Parallel()
	llm := newServedProbeLLM(errCallNoop, errBare400, nil)
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if _, err := a.RunSubtask(context.Background(), SubtaskSpec{
		Name:         "probe",
		SystemPrompt: "probe",
		UserMessage:  "go",
		Tools:        []tool.Tool{newNoopTool(t)},
	}); err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}

	calls, seen := llm.snapshot()
	if calls != 3 || seen[0].succeeded || !seen[1].succeeded {
		t.Errorf("calls = %d, seen = %+v; want 3 calls, the rejected one seeing a marked record", calls, seen)
	}
}

// RunWithContents creates its session on the spot, so no served call
// precedes its first; a record inherited from a caller's turn must not
// license a retry there.
func TestRunWithContents_DoesNotInheritThePriorSuccess(t *testing.T) {
	t.Parallel()
	llm := newServedProbeLLM(errBare400, nil)
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	parent := models.NewPriorSuccess()
	parent.Mark()
	ctx := models.WithPriorSuccess(context.Background(), parent)

	var errs []error
	for _, err := range a.RunWithContents(ctx, []*genai.Content{genai.NewContentFromText("go", genai.RoleUser)}) {
		if err != nil {
			errs = append(errs, err)
		}
	}

	calls, seen := llm.snapshot()
	if calls != 1 || len(errs) == 0 || !errors.Is(errs[0], errBare400) {
		t.Errorf("calls = %d, errs = %v; want 1 call and the bare 400", calls, errs)
	}
	if len(seen) > 0 && seen[0].rec == parent {
		t.Error("RunWithContents' call saw the caller's record")
	}
}
