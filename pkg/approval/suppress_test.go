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

package approval

import (
	"context"
	"errors"
	"iter"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

type proposingModel struct {
	mu    sync.Mutex
	round int
	calls []map[string]any
}

func (m *proposingModel) Name() string { return "refusal-loop" }

func (m *proposingModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		round := m.round
		m.round++
		m.mu.Unlock()

		var args map[string]any
		if round < len(m.calls) {
			args = m.calls[round]
		}
		resp := &model.LLMResponse{TurnComplete: true, FinishReason: genai.FinishReasonStop}
		if args == nil {
			resp.Content = genai.NewContentFromText("done", genai.RoleModel)
		} else {
			resp.Content = &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromFunctionCall("scale_deployment", args)},
			}
		}
		yield(resp, nil)
	}
}

func proposeScale(deployment string, replicas int) map[string]any {
	return map[string]any{"deployment": deployment, "replicas": replicas}
}

type refusalProbe struct {
	executions  []scaleArgs
	responses   []map[string]any
	parks       []string
	invocations []string
	forgotten   []string
	errs        []error
	stop        *probeStop
}

type probeStop struct {
	mu     sync.Mutex
	reason error
	cancel context.CancelFunc
}

func (s *probeStop) StopTurn(reason error) {
	s.mu.Lock()
	if s.reason == nil {
		s.reason = reason
	}
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *probeStop) arm(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancel = cancel
}

func (s *probeStop) why() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

type refusalProbeConfig struct {
	calls     []map[string]any
	respond   func(confID string) *genai.Content
	thirdTurn *genai.Content
	noForget  bool
	noStop    bool
}

func runRefusalProbe(t *testing.T, cfg refusalProbeConfig) *refusalProbe {
	t.Helper()
	probe := &refusalProbe{stop: &probeStop{}}

	scale, err := functiontool.New(functiontool.Config{
		Name:        "scale_deployment",
		Description: "changes a deployment's replica count",
	}, func(_ adkagent.Context, args scaleArgs) (map[string]any, error) {
		probe.executions = append(probe.executions, args)
		return map[string]any{"scaled": args.Replicas}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	root, err := llmagent.New(llmagent.Config{
		Name:        "refusal_agent",
		Description: "refusal-loop probe",
		Instruction: "act",
		Model:       &proposingModel{calls: cfg.calls},
		Tools:       []tool.Tool{scale},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	observer, err := plugin.New(plugin.Config{
		Name: "invocation-observer",
		BeforeToolCallback: func(ctx adkagent.Context, tl tool.Tool, _ map[string]any) (map[string]any, error) {
			if tl.Name() == "scale_deployment" {
				probe.invocations = append(probe.invocations, ctx.InvocationID())
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("plugin.New: %v", err)
	}

	gcfg := Config{
		Policy:   OnMutationRequireApproval,
		Mutating: alwaysMutating,
		Gate:     permissions.New(permissions.Options{}),
	}
	if !cfg.noForget {
		gcfg.ForgetToolRun = func(sessionID string) {
			probe.forgotten = append(probe.forgotten, sessionID)
		}
	}
	wg, err := New(gcfg)
	if err != nil {
		t.Fatalf("approval.New: %v", err)
	}

	svc := sqliteService(t)
	r, err := runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{observer, wg}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	run := func(msg *genai.Content) {
		runCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		probe.stop.arm(cancel)
		if !cfg.noStop {
			runCtx = WithTurnStop(runCtx, probe.stop)
		}
		for _, err := range r.Run(runCtx, testUser, sid, msg, adkagent.RunConfig{}) {
			if err != nil {
				probe.errs = append(probe.errs, err)
			}
		}
	}

	run(genai.NewContentFromText("scale api to 10", genai.RoleUser))

	ids, _, _ := confirmationRequests(t, svc)
	if cfg.respond != nil && len(ids) > 0 {
		run(cfg.respond(ids[len(ids)-1]))
	}
	if cfg.thirdTurn != nil {
		run(cfg.thirdTurn)
	}

	probe.parks, _, _ = confirmationRequests(t, svc)
	probe.responses = toolResponses(t, svc, "scale_deployment")
	return probe
}

func reject(confID string) *genai.Content {
	return verdictResponse(confID, map[string]any{
		"confirmed": false,
		"payload":   map[string]any{"verdict": "reject", "approver": "operator@example.com"},
	})
}

func TestARefusedCallIsNotPutToTheOperatorTwice(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10),
			proposeScale("api", 10),
			nil,
		},
		respond: reject,
	})

	if len(p.parks) != 1 {
		t.Fatalf("operator was asked %d times, want 1", len(p.parks))
	}
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) after a refusal, want 0: %+v", len(p.executions), p.executions)
	}
	if len(p.errs) != 0 {
		t.Fatalf("run errors = %v, want none", p.errs)
	}
	if len(p.responses) < 2 {
		t.Fatalf("responses = %v, want at least the park and the suppression", p.responses)
	}
	last := p.responses[len(p.responses)-1]
	wantField(t, last, "error", "already_refused")
	wantDetailMentions(t, last,
		"already refused",
		"not put to them a second time",
		"Do not retry",
	)
}

func TestADifferentCallStillParks(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10),
			proposeScale("api", 2),
		},
		respond: reject,
	})

	if len(p.parks) != 2 {
		t.Fatalf("operator was asked %d times, want 2", len(p.parks))
	}
	for _, resp := range p.responses {
		if code, _ := resp["error"].(string); code == "already_refused" {
			t.Fatalf("a call the operator never refused was suppressed: %v", resp)
		}
	}
}

func TestAnApprovalDoesNotArmTheSuppression(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10),
			proposeScale("api", 10),
		},
		respond: approve(map[string]any{"verdict": "approve", "approver": "operator@example.com"}),
	})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want exactly 1: %+v", len(p.executions), p.executions)
	}
	if len(p.parks) != 2 {
		t.Fatalf("operator was asked %d times, want 2", len(p.parks))
	}
	for _, resp := range p.responses {
		if code, _ := resp["error"].(string); code == "already_refused" {
			t.Fatalf("an approval was remembered as a refusal: %v", resp)
		}
	}
}

func TestTheTurnBoundaryIsTheInvocationID(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10),
			proposeScale("api", 10),
			nil,
			proposeScale("api", 10),
		},
		respond:   reject,
		thirdTurn: genai.NewContentFromText("try scaling api again", genai.RoleUser),
	})

	if len(p.invocations) != 4 {
		t.Fatalf("saw %d tool calls %v, want 4", len(p.invocations), p.invocations)
	}
	for i, got := range p.invocations[:3] {
		if got != p.invocations[0] {
			t.Fatalf("tool call %d ran under invocation %q, want %q", i, got, p.invocations[0])
		}
	}
	if p.invocations[3] == p.invocations[0] {
		t.Fatalf("a fresh operator turn reused invocation %q", p.invocations[3])
	}

	if len(p.parks) != 2 {
		t.Fatalf("operator was asked %d times, want 2", len(p.parks))
	}
	last := p.responses[len(p.responses)-1]
	if got, _ := last["status"].(string); got != "awaiting_operator_approval" {
		t.Fatalf("the new turn's call ended as %v, want a fresh park", last)
	}
}

func TestThreeSuppressedCallsEndTheTurn(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10),
			proposeScale("api", 10),
			proposeScale("api", 10),
			proposeScale("api", 10),
			proposeScale("api", 10),
			proposeScale("api", 10),
		},
		respond: reject,
	})

	var loop *RefusalLoopError
	if !errors.As(p.stop.why(), &loop) {
		t.Fatalf("the turn ended with %v, want a *RefusalLoopError", p.stop.why())
	}
	if loop.Count != suppressedCallsEndTurn {
		t.Errorf("Count = %d, want %d", loop.Count, suppressedCallsEndTurn)
	}
	if loop.Tool != "scale_deployment" {
		t.Errorf("Tool = %q, want scale_deployment", loop.Tool)
	}

	if len(p.invocations) != 5 {
		t.Errorf("the gate saw %d calls, want 5", len(p.invocations))
	}
	if len(p.parks) != 1 {
		t.Errorf("operator was asked %d times, want 1", len(p.parks))
	}
	if len(p.executions) != 0 {
		t.Errorf("tool executed %d time(s), want 0: %+v", len(p.executions), p.executions)
	}
}

func TestTheTurnStopIsOptional(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10),
			proposeScale("api", 10),
			proposeScale("api", 10),
			proposeScale("api", 10),
			nil,
		},
		respond: reject,
		noStop:  true,
	})

	if p.stop.why() != nil {
		t.Errorf("a turn with no TurnStop recorded %v", p.stop.why())
	}
	if len(p.parks) != 1 {
		t.Errorf("operator was asked %d times, want 1", len(p.parks))
	}
	if len(p.executions) != 0 {
		t.Errorf("tool executed %d time(s), want 0: %+v", len(p.executions), p.executions)
	}
	last := p.responses[len(p.responses)-1]
	wantField(t, last, "error", "already_refused")
}

func TestTheSuppressionTellsTheWatchdogItsCallsAreDisposedOf(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls: []map[string]any{
			proposeScale("api", 10),
			proposeScale("api", 10),
			proposeScale("api", 10),
			nil,
		},
		respond: reject,
	})

	if len(p.forgotten) != 2 {
		t.Fatalf("ForgetToolRun called %d time(s) %v, want 2", len(p.forgotten), p.forgotten)
	}
	for _, got := range p.forgotten {
		if got != sid {
			t.Errorf("ForgetToolRun(%q), want %q", got, sid)
		}
	}
}

func TestSuppressionWorksWithNoWatchdogToTell(t *testing.T) {
	p := runRefusalProbe(t, refusalProbeConfig{
		calls:    []map[string]any{proposeScale("api", 10), proposeScale("api", 10), nil},
		respond:  reject,
		noForget: true,
	})

	if len(p.parks) != 1 {
		t.Fatalf("operator was asked %d times, want 1", len(p.parks))
	}
	last := p.responses[len(p.responses)-1]
	wantField(t, last, "error", "already_refused")
}

func TestRefusalLoopErrorDeclaresItsTurnErrorKind(t *testing.T) {
	t.Parallel()
	loop := &RefusalLoopError{Tool: "patch_resource", Key: "patch_resource(name=api)", Count: 3}
	if got := loop.TurnErrorKind(); got != "refusal_loop" {
		t.Fatalf("TurnErrorKind() = %q, want %q", got, "refusal_loop")
	}
}

func TestRefusalsWithoutATurnIdentityDoNothing(t *testing.T) {
	t.Parallel()
	r := newRefusals()
	r.remember("", "scale_deployment(replicas=10)")
	if suppressed, _, _ := r.suppress("", "scale_deployment(replicas=10)"); suppressed {
		t.Error("suppressed a call with no turn to scope the refusal to")
	}
	if len(r.byInvocation) != 0 {
		t.Errorf("byInvocation = %v, want empty", r.byInvocation)
	}
}

func TestRefusalsCountTheTurnNotTheCall(t *testing.T) {
	t.Parallel()
	r := newRefusals()
	for _, key := range []string{"a()", "b()", "c()"} {
		r.remember("inv-1", key)
	}
	for i, key := range []string{"a()", "b()", "c()"} {
		suppressed, endTurn, count := r.suppress("inv-1", key)
		if !suppressed {
			t.Fatalf("%s was not suppressed", key)
		}
		if count != i+1 {
			t.Errorf("count = %d, want %d", count, i+1)
		}
		if want := i+1 >= suppressedCallsEndTurn; endTurn != want {
			t.Errorf("endTurn = %v after %d suppressions, want %v", endTurn, count, want)
		}
	}
}

func TestForgetDropsOnlyItsOwnTurn(t *testing.T) {
	t.Parallel()
	r := newRefusals()
	r.remember("inv-1", "a()")
	r.remember("inv-2", "a()")
	r.forget("inv-1")

	if suppressed, _, _ := r.suppress("inv-1", "a()"); suppressed {
		t.Error("a forgotten turn still suppresses")
	}
	if suppressed, _, _ := r.suppress("inv-2", "a()"); !suppressed {
		t.Error("forgetting one turn dropped another's refusal")
	}
}
