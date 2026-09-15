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
	"fmt"
	"iter"
	"slices"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// #1081. Run 11 of dev/uat/approval-gate/ put an operator's "no" in
// front of a model that re-issued the same call until a SESSION-scoped
// watchdog counted to five and halted the daemon for the rest of its
// life. These tests drive the real Run loop through the same shape and
// pin the two halves of the answer: the turn ends, and only the turn.

const refusalProbeTool = "delete_namespace"

// The detail string the probe puts to the gate. Constant, so every call
// is the same request — which is the state the gate suppresses and the
// agent counts.
const refusalProbeDetail = "kubectl delete ns prod"

// newRefusalProbeTool is a gated tool: it asks the gate the way a real
// destructive tool does and returns whatever the gate says. Wired with
// the agent's own gate, so a suppressed call here moves exactly the
// counter the arm under test reads.
func newRefusalProbeTool(t *testing.T, g *permissions.Gate) tool.Tool {
	t.Helper()
	type args struct {
		Namespace string `json:"namespace"`
	}
	type empty struct{}
	tl, err := functiontool.New(
		functiontool.Config{Name: refusalProbeTool, Description: "delete a namespace"},
		func(ctx tool.Context, _ args) (empty, error) {
			return empty{}, g.CheckBash(ctx, refusalProbeDetail)
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}
	return tl
}

// gatedLoopLLM re-issues one identical gated call, which is the run-11
// behaviour reduced to its essentials: the args never change, so the
// operator's answer covers every call it makes.
//
// budget bounds the loop so a test can choose exactly how many
// suppressed repeats the turn produces; a negative budget loops until
// something else stops it, which is the case the arm exists for. Honours
// ctx so a cut actually truncates the loop, as a real client would.
type gatedLoopLLM struct {
	mu     sync.Mutex
	calls  int
	budget int
}

func (*gatedLoopLLM) Name() string { return "gated-loop" }

// setBudget re-arms the fake for another turn.
func (l *gatedLoopLLM) setBudget(n int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.budget, l.calls = n, 0
}

func (l *gatedLoopLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(nil, err)
			return
		}
		l.mu.Lock()
		l.calls++
		n, budget := l.calls, l.budget
		l.mu.Unlock()

		if budget >= 0 && n > budget {
			yield(&adkmodel.LLMResponse{
				Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "fine, I'll stop"}}},
				TurnComplete: true,
			}, nil)
			return
		}
		yield(&adkmodel.LLMResponse{
			Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{
					ID:   fmt.Sprintf("refuse_%d", n),
					Name: refusalProbeTool,
					Args: map[string]any{"namespace": "prod"},
				},
			}}},
			TurnComplete: true,
		}, nil)
	}
}

// refusalStormRig builds an agent whose only tool is gated, behind a
// prompter that denies everything.
//
// No watchdog is wired, deliberately: that is the difference between
// this arm and #705's, and the reason the call site in Run sits outside
// the `a.watchdog != nil` block rather than riding its `observed` flag.
// An operator who never configured a watchdog still gets this.
func refusalStormRig(t *testing.T, sid string, opts ...Option) (*Agent, *gatedLoopLLM, *denyingPrompter) {
	t.Helper()
	p := &denyingPrompter{}
	g := permissions.New(permissions.Options{Mode: permissions.ModeAsk, Prompter: p})
	llm := &gatedLoopLLM{budget: -1}
	opts = append([]Option{
		WithSession("u-1081", sid),
		WithGate(g),
		WithTools([]tool.Tool{newRefusalProbeTool(t, g)}),
	}, opts...)
	a, err := New(llm, opts...)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a, llm, p
}

// refusalStormRows returns the audit rows this agent's session carries.
func refusalStormRows(t *testing.T, a *Agent) []*session.Event {
	t.Helper()
	resp, err := a.eventLog.Service.Get(context.Background(), &session.GetRequest{
		AppName:   a.appName,
		UserID:    a.userID,
		SessionID: a.sessionID,
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	var rows []*session.Event
	for ev := range resp.Session.Events().All() {
		if ev.Author == refusalStormAuthor {
			rows = append(rows, ev)
		}
	}
	return rows
}

// drainTurn runs one turn to whatever end it comes to, failing the test
// if the loop reaches the deadline instead — which is what "nothing ends
// the turn" would look like from out here.
func drainTurn(t *testing.T, a *Agent, prompt string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for range a.Run(ctx, prompt) {
	}
	if ctx.Err() != nil {
		t.Fatal("the turn ran to the deadline: nothing ended it")
	}
}

// TestRun_RefusalStorm_1081_EndsTheTurnWithoutEndingTheSession is the
// whole of #1081 in one turn plus the turn after it.
//
// The first turn is run 11's leg 2: an operator denies a call, the model
// re-issues it, and the gate suppresses every repeat without paging
// anybody. Three suppressed repeats in, the gate's arm cuts the turn.
//
// The second turn is the part that distinguishes this from what run 11
// actually did. There, the watchdog's session-scoped guardrail tripped,
// the next turn was refused nine milliseconds later with "guardrail
// still tripped, not reset", and auto-continue stood down until a human
// came back. Here the next turn just runs. That asymmetry is the fix,
// and the assertions below are arranged around it:
//
//   - exactly one terminal frame, and it is the ordinary `canceled`;
//   - ZERO guardrail-trip frames, because nothing tripped and a client
//     told otherwise would offer the operator a reset for a guardrail
//     that does not exist;
//   - the metric still says which stop this was, so a backend can tell
//     it from an operator pressing stop;
//   - the reason is durable in the eventlog, exactly once;
//   - and the session runs the next turn to completion.
//
// Four fails-first passes were run against the code as it stood before
// each piece of the fix, and each failed differently, which is what says
// the assertions are aimed at separate things:
//
//   - Remove the enforceRefusalStormInTurn call from Run's tap: this
//     test and the watchdog one below both run to the 30-second
//     deadline, and the bounded "three is" case completes normally.
//   - Move that call back inside the `if observed` block — i.e. under
//     `a.watchdog != nil`, where it was first written. The two
//     no-watchdog tests fail exactly as above and
//     AWatchdogDoesNotChangeTheOutcome PASSES, which is the whole
//     argument for the call site being where it is.
//   - Set refusalStormThreshold to the watchdog's 5: the bounded case
//     completes, and this test still cuts but records repeats=5.
//   - Drop markGuardrailHalt: everything passes except the metric,
//     which reports `canceled`.
func TestRun_RefusalStorm_1081_EndsTheTurnWithoutEndingTheSession(t *testing.T) {
	t.Parallel()

	h, cleanup := openTestEventLog(t)
	defer cleanup()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	a, llm, p := refusalStormRig(t, "s-1081", WithEventLog(h), WithMeterProvider(mp))
	if _, err := h.Service.Create(context.Background(), &session.CreateRequest{
		AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
	}); err != nil {
		t.Fatalf("session Create: %v", err)
	}
	var rec terminalRecorder
	rec.attachTo(a)

	drainTurn(t, a, "clean up the cluster")

	// One prompt for one request, however many times it was re-issued.
	// #1074's property, restated here because it is the premise of this
	// one: the calls being counted are calls nobody was asked about.
	if p.asked != 1 {
		t.Errorf("the operator was paged %d times for one refused request, want 1", p.asked)
	}

	if got, want := rec.frames(), []string{"turn-error:" + attach.TurnErrorCanceled}; !slices.Equal(got, want) {
		t.Errorf("terminal frames = %v, want %v — the turn was cut, and `canceled` is "+
			"what happened to it", got, want)
	}
	if trips := rec.guardrailTrips(); len(trips) != 0 {
		t.Errorf("got %d guardrail-trip frames %+v, want 0 — attach.GuardrailTrip's "+
			"vocabulary is shared with the reset endpoint and the durable trip row, so "+
			"announcing a cut that needs no reset through it tells the client there is "+
			"something to reset", len(trips), trips)
	}

	// The metric is the one place the stop is named, now that no frame
	// names it. Collected before the second turn so there is one point.
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	pts := invocationPoints(t, rm)
	if len(pts) != 1 {
		t.Fatalf("got %d invocation points, want 1", len(pts))
	}
	if et, _ := invAttr(pts[0].Attributes, AttrErrorType); et != attach.TurnErrorRefusalStorm {
		t.Errorf("%s = %q, want %q — the cancel is how the cut was carried out, not why",
			AttrErrorType, et, attach.TurnErrorRefusalStorm)
	}

	rows := refusalStormRows(t, a)
	if len(rows) != 1 {
		t.Fatalf("got %d audit rows, want 1", len(rows))
	}
	if got := fmt.Sprint(rows[0].CustomMetadata["repeats"]); got != "3" {
		t.Errorf("audit row repeats = %s, want 3 (refusalStormThreshold)", got)
	}
	if rows[0].CustomMetadata["reason"] == "" {
		t.Error("audit row carries no reason; it is the only durable explanation of the cut")
	}
	// No content: a text part authored here would enter the model's
	// history as a message nobody sent.
	if rows[0].Content != nil && len(rows[0].Content.Parts) != 0 {
		t.Errorf("audit row carries content %+v; it must not enter the model's history", rows[0].Content.Parts)
	}

	// And the session is runnable. Nothing was tripped, so there is
	// nothing to reset, and the next turn opens with an empty refusal
	// map and the refusals in the model's history where they belong.
	llm.setBudget(0)
	drainTurn(t, a, "just tell me what you found")
	if got, want := rec.frames(), []string{
		"turn-error:" + attach.TurnErrorCanceled,
		"turn-complete",
	}; !slices.Equal(got, want) {
		t.Errorf("terminal frames across both turns = %v, want %v — the turn after a "+
			"refusal-storm cut must run", got, want)
	}
	if rows := refusalStormRows(t, a); len(rows) != 1 {
		t.Errorf("audit rows after a second turn = %d, want 1 (the drain must not double-write)", len(rows))
	}
}

// TestRun_RefusalStorm_ThresholdIsExactlyThree pins the boundary from
// both sides, because a threshold is only a decision if the value below
// it does nothing.
//
// Three and not five: repeated-tool-call needs five because a repeated
// tool name cannot distinguish a loop from a sweep, and a false positive
// there halts a working agent. A suppressed repeat is not an inference —
// the operator refused this exact request, the model was told so in a
// tool result, and it asked again — so the evidence does not need
// corroborating four times over.
func TestRun_RefusalStorm_ThresholdIsExactlyThree(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		toolCalls int // 1 denial + (toolCalls-1) suppressed repeats
		wantFrame string
		wantRows  int
	}{
		{
			name:      "two suppressed repeats is not a storm",
			toolCalls: 3,
			wantFrame: "turn-complete",
			wantRows:  0,
		},
		{
			name:      "three is",
			toolCalls: 4,
			wantFrame: "turn-error:" + attach.TurnErrorCanceled,
			wantRows:  1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, cleanup := openTestEventLog(t)
			defer cleanup()

			sid := fmt.Sprintf("s-1081-%d", tc.toolCalls)
			a, llm, _ := refusalStormRig(t, sid, WithEventLog(h))
			if _, err := h.Service.Create(context.Background(), &session.CreateRequest{
				AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
			}); err != nil {
				t.Fatalf("session Create: %v", err)
			}
			llm.setBudget(tc.toolCalls)
			var rec terminalRecorder
			rec.attachTo(a)

			drainTurn(t, a, "clean up the cluster")

			if got, want := rec.frames(), []string{tc.wantFrame}; !slices.Equal(got, want) {
				t.Errorf("terminal frames = %v, want %v", got, want)
			}
			if got := len(refusalStormRows(t, a)); got != tc.wantRows {
				t.Errorf("audit rows = %d, want %d", got, tc.wantRows)
			}
		})
	}
}

// TestRun_RefusalStorm_AWatchdogDoesNotChangeTheOutcome covers the
// composition run 11 actually had: both arms live, the watchdog in
// enforce mode. The gate gets there first — three suppressed repeats
// against repeated-tool-call's five — and what matters is that the turn
// it cuts is not also a session halt. A watchdog that had reached its
// own threshold WOULD halt the session; the point of the low threshold
// is that it does not get the chance.
func TestRun_RefusalStorm_AWatchdogDoesNotChangeTheOutcome(t *testing.T) {
	t.Parallel()

	a, _, _ := refusalStormRig(t, "s-1081-wd",
		WithWatchdog(&fakeWatchdog{}, nil), WithWatchdogEnforce())
	var rec terminalRecorder
	rec.attachTo(a)

	drainTurn(t, a, "clean up the cluster")

	if got, want := rec.frames(), []string{"turn-error:" + attach.TurnErrorCanceled}; !slices.Equal(got, want) {
		t.Errorf("terminal frames = %v, want %v", got, want)
	}
	if tripped, reason := a.WatchdogTripped(); tripped {
		t.Errorf("the session was halted: %q — the gate's cut is turn-scoped, and the "+
			"watchdog must not have been given time to reach its own threshold", reason)
	}
	if trips := rec.guardrailTrips(); len(trips) != 0 {
		t.Errorf("got %d guardrail-trip frames %+v, want 0", len(trips), trips)
	}
}

// hasToolResult runs on every event of a streaming turn, so what it
// declines to match is as load-bearing as what it matches: the
// alternative to this filter is taking the gate's mutex on every text
// delta.
func TestHasToolResult(t *testing.T) {
	t.Parallel()

	ev := func(parts ...*genai.Part) *session.Event {
		e := session.NewEvent("t")
		e.Content = &genai.Content{Role: genai.RoleModel, Parts: parts}
		return e
	}

	tests := []struct {
		name string
		ev   *session.Event
		want bool
	}{
		{"nil event", nil, false},
		{"no content", session.NewEvent("t"), false},
		{"no parts", ev(), false},
		{"nil part", ev(nil), false},
		{"text delta", ev(&genai.Part{Text: "thinking about it"}), false},
		// A suppressed request produces a call and then its response.
		// Reacting to the call would read the count before the gate has
		// moved it, and then wait for the next event to notice.
		{"function call only", ev(&genai.Part{FunctionCall: &genai.FunctionCall{Name: "x"}}), false},
		{"function response", ev(&genai.Part{FunctionResponse: &genai.FunctionResponse{Name: "x"}}), true},
		{"response after text", ev(
			&genai.Part{Text: "here you go"},
			&genai.Part{FunctionResponse: &genai.FunctionResponse{Name: "x"}},
		), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := hasToolResult(tc.ev); got != tc.want {
				t.Errorf("hasToolResult = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRefusalStorm_NoGateIsANoop: the arm reads a gate. An agent wired
// without one — every ungated deployment — must run its turn out.
func TestRefusalStorm_NoGateIsANoop(t *testing.T) {
	t.Parallel()
	a, err := New(oneShotLLM{}, WithSession("u-1081-nogate", "s-1081-nogate"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	a.enforceRefusalStormInTurn(context.Background()) // must not panic or cut
	if got := a.pendingRefusalStorm.Load(); got != 0 {
		t.Errorf("pendingRefusalStorm = %d with no gate wired, want 0", got)
	}
	runTurnToCompletion(t, a)
}

// Nil-receiver tolerance, mirroring the interrupt audit's.
func TestRefusalStorm_NilSafe(t *testing.T) {
	t.Parallel()
	var a *Agent
	a.enforceRefusalStormInTurn(context.Background())
	a.clearRefusalStorm()
	a.drainRefusalStormAudit()
}

// clearRefusalStorm is belt-and-braces and says so: the drain clears
// the marker on every turn that reaches its cleanup, so Run's call at
// turn start is only reachable when one did not (a panic unwinding past
// it, an abandoned iterator). That makes the turn-start call not
// independently observable from out here, and it is still worth having,
// because the arm's first line is an early return on a non-zero marker —
// a stale one silently disables the next turn's protection rather than
// failing loudly. What is testable is that the function does its job.
func TestClearRefusalStorm(t *testing.T) {
	t.Parallel()
	a, err := New(oneShotLLM{}, WithSession("u-1081-clear", "s-1081-clear"))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	a.pendingRefusalStorm.Store(7)
	a.clearRefusalStorm()
	if got := a.pendingRefusalStorm.Load(); got != 0 {
		t.Errorf("pendingRefusalStorm = %d after clearRefusalStorm, want 0", got)
	}
}
