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
	"fmt"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/internal/effects"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

func alwaysMutating(string) bool { return true }

type gateProbe struct {
	executions     []scaleArgs
	responses      []map[string]any
	confirmationID string
	hintSeen       string
	gate           *permissions.Gate
	state          map[string]any
	sunkDecisions  []Decision
}

type gateProbeConfig struct {
	policy               OnMutation
	mutating             func(string) bool
	gateOptions          permissions.Options
	respond              func(confID string) *genai.Content
	seed                 func(t *testing.T, svc adksession.Service)
	extraPluginsFirst    func(t *testing.T) []*plugin.Plugin
	captures             *CaptureRules
	restartBeforeVerdict bool
	options              []Option
}

func runGateProbe(t *testing.T, cfg gateProbeConfig) *gateProbe {
	t.Helper()
	probe := &gateProbe{gate: permissions.New(cfg.gateOptions)}
	dir := t.TempDir()

	build := func() (*runner.Runner, adksession.Service) {
		t.Helper()
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

		m := &scriptedModel{name: "gate", calls: []*model.LLMResponse{{
			Content: &genai.Content{
				Role: genai.RoleModel,
				Parts: []*genai.Part{genai.NewPartFromFunctionCall("scale_deployment", map[string]any{
					"deployment": "api",
					"replicas":   10,
				})},
			},
		}}}

		root, err := llmagent.New(llmagent.Config{
			Name:        "gate_agent",
			Description: "write-gate probe",
			Instruction: "act",
			Model:       m,
			Tools:       []tool.Tool{scale},
		})
		if err != nil {
			t.Fatalf("llmagent.New: %v", err)
		}

		mutating := cfg.mutating
		if mutating == nil {
			mutating = alwaysMutating
		}
		opts := append([]Option{WithDecisionSink(func(_ context.Context, d Decision) error {
			probe.sunkDecisions = append(probe.sunkDecisions, d)
			return nil
		})}, cfg.options...)
		wg, err := NewPlugin(Config{
			Policy:   cfg.policy,
			Mutating: mutating,
			Gate:     probe.gate,
			Captures: cfg.captures,
		}, opts...)
		if err != nil {
			t.Fatalf("approval.NewPlugin: %v", err)
		}

		var plugins []*plugin.Plugin
		if cfg.extraPluginsFirst != nil {
			plugins = append(plugins, cfg.extraPluginsFirst(t)...)
		}
		plugins = append(plugins, wg)

		svc := sqliteServiceAt(t, dir)
		r, err := runner.New(runner.Config{
			AppName:           testApp,
			Agent:             root,
			SessionService:    svc,
			AutoCreateSession: true,
			PluginConfig:      runner.PluginConfig{Plugins: plugins},
		})
		if err != nil {
			t.Fatalf("runner.New: %v", err)
		}
		return r, svc
	}

	run := func(r *runner.Runner, msg *genai.Content) {
		t.Helper()
		for _, err := range r.Run(context.Background(), testUser, sid, msg, adkagent.RunConfig{}) {
			if err != nil {
				t.Fatalf("runner.Run: %v", err)
			}
		}
	}

	r, svc := build()
	if cfg.seed != nil {
		if _, err := svc.Create(context.Background(), &adksession.CreateRequest{
			AppName: testApp, UserID: testUser, SessionID: sid,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		cfg.seed(t, svc)
	}

	run(r, genai.NewContentFromText("scale api to 10", genai.RoleUser))
	probe.confirmationID, probe.hintSeen = confirmationRequest(t, svc)

	if cfg.restartBeforeVerdict {
		if got := probe.gate.Approvals(); len(got) != 0 {
			t.Fatalf("the pre-restart gate already recorded an approval: %+v", got)
		}
		probe.gate = permissions.New(cfg.gateOptions)
		r, svc = build()
	}
	if cfg.respond != nil && probe.confirmationID != "" {
		run(r, cfg.respond(probe.confirmationID))
	}
	probe.responses = toolResponses(t, svc, "scale_deployment")
	probe.state = sessionState(t, svc)
	return probe
}

func sessionState(t *testing.T, svc adksession.Service) map[string]any {
	t.Helper()
	got, err := svc.Get(context.Background(), &adksession.GetRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	out := map[string]any{}
	for k, v := range got.Session.State().All() {
		out[k] = v
	}
	return out
}

func (p *gateProbe) appliedEdits(t *testing.T) []AppliedEdit {
	t.Helper()
	var out []AppliedEdit
	for k, v := range p.state {
		if !strings.HasPrefix(k, EditStateKeyPrefix) {
			continue
		}
		e, err := DecodeAppliedEdit(v)
		if err != nil {
			t.Fatalf("state[%q] is not an applied-edit record: %v", k, err)
		}
		out = append(out, e)
	}
	return out
}

func toolResponses(t *testing.T, svc adksession.Service, name string) []map[string]any {
	t.Helper()
	got, err := svc.Get(context.Background(), &adksession.GetRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var out []map[string]any
	for ev := range got.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			if part == nil || part.FunctionResponse == nil || part.FunctionResponse.Name != name {
				continue
			}
			out = append(out, part.FunctionResponse.Response)
		}
	}
	return out
}

func (p *gateProbe) lastResponse(t *testing.T) map[string]any {
	t.Helper()
	if len(p.responses) == 0 {
		t.Fatal("no tool response recorded for scale_deployment")
	}
	return p.responses[len(p.responses)-1]
}

func wantField(t *testing.T, resp map[string]any, field, want string) {
	t.Helper()
	got, _ := resp[field].(string)
	if got != want {
		t.Errorf("response[%q] = %q, want %q (full response: %v)", field, got, want, resp)
	}
}

func wantDetailMentions(t *testing.T, resp map[string]any, substrings ...string) {
	t.Helper()
	detail, _ := resp["detail"].(string)
	for _, s := range substrings {
		if !strings.Contains(detail, s) {
			t.Errorf("detail does not mention %q: %s", s, detail)
		}
	}
}

func approve(payload map[string]any) func(string) *genai.Content {
	return func(confID string) *genai.Content {
		return verdictResponse(confID, map[string]any{"confirmed": true, "payload": payload})
	}
}

func TestNew_Validation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"empty policy", Config{Mutating: alwaysMutating}, "is not one of"},
		{"unknown policy", Config{Policy: "ask_nicely", Mutating: alwaysMutating}, "is not one of"},
		{"no predicate", Config{Policy: OnMutationApply}, "Mutating is required"},
		{
			"require_approval without a gate",
			Config{Policy: OnMutationRequireApproval, Mutating: alwaysMutating},
			"Gate is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(tt.cfg)
			if err == nil {
				t.Fatal("New = nil error, want a refusal")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestReadOnlyToolIsNotGated(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy:   OnMutationRequireApproval,
		mutating: func(string) bool { return false },
	})
	if p.confirmationID != "" {
		t.Error("a read-only tool parked for approval")
	}
	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want 1: %+v", len(p.executions), p.executions)
	}
}

func TestRequireApprovalParksTheCall(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationRequireApproval})

	if p.confirmationID == "" {
		t.Fatal("no confirmation parked")
	}
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) while parked, want 0: %+v", len(p.executions), p.executions)
	}
	if want := "Approve mutating call scale_deployment(deployment=api, replicas=10)?"; p.hintSeen != want {
		t.Errorf("hint = %q, want %q", p.hintSeen, want)
	}
	resp := p.lastResponse(t)
	wantField(t, resp, "status", "awaiting_operator_approval")
	wantDetailMentions(t, resp, "has NOT been made", "Do not retry", "another route")
}

func TestApprovedCallRunsExactlyOnce(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy:  OnMutationRequireApproval,
		respond: approve(map[string]any{"verdict": "approve", "approver": "user:sre-oncall"}),
	})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want exactly 1: %+v", len(p.executions), p.executions)
	}
	if got := p.executions[0]; got.Deployment != "api" || got.Replicas != 10 {
		t.Errorf("executed with %+v, want {api 10}", got)
	}
	log := p.gate.Approvals()
	if len(log) != 1 {
		t.Fatalf("approval log = %+v, want exactly one entry", log)
	}
	if log[0].Tool != "scale_deployment" || log[0].Decision != permissions.DecisionAllowOnce {
		t.Errorf("approval log entry = %+v, want an allow-once for scale_deployment", log[0])
	}
	if log[0].Key != "scale_deployment(deployment=api, replicas=10)" {
		t.Errorf("approval key = %q, want the rendered call", log[0].Key)
	}
}

func TestPauseSurvivesARestart(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy:               OnMutationRequireApproval,
		restartBeforeVerdict: true,
		respond:              approve(map[string]any{"verdict": "approve", "approver": "user:sre-oncall"}),
	})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s) across the restart, want exactly 1: %+v", len(p.executions), p.executions)
	}
	if got := p.executions[0]; got.Deployment != "api" || got.Replicas != 10 {
		t.Errorf("executed with %+v, want the parked call's arguments {api 10}", got)
	}
	if log := p.gate.Approvals(); len(log) != 1 {
		t.Errorf("post-restart approval log = %+v, want the one approval this process granted", log)
	}
}

func TestRejectedCallNeverRuns(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		respond: func(confID string) *genai.Content {
			return verdictResponse(confID, map[string]any{
				"confirmed": false,
				"payload":   map[string]any{"verdict": "reject", "note": "wrong deployment", "approver": "user:sre-oncall"},
			})
		},
	})

	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) after a rejection, want 0: %+v", len(p.executions), p.executions)
	}
	resp := p.lastResponse(t)
	wantField(t, resp, "error", "denied_by_operator")
	wantField(t, resp, "operator_note", "wrong deployment")
	wantField(t, resp, "approver", "user:sre-oncall")
	wantDetailMentions(t, resp, "Do not retry", "another route")
	if log := p.gate.Approvals(); len(log) != 0 {
		t.Errorf("approval log = %+v, want empty after a rejection", log)
	}
}

func TestBroadScopeIsRefused(t *testing.T) {
	for _, scope := range []Scope{ScopeSession, ScopeSessionTool, ScopeAlways} {
		t.Run(string(scope), func(t *testing.T) {
			p := runGateProbe(t, gateProbeConfig{
				policy:      OnMutationRequireApproval,
				gateOptions: permissions.Options{Mode: permissions.ModeYolo},
				respond:     approve(map[string]any{"verdict": "approve", "scope": string(scope)}),
			})
			if len(p.executions) != 0 {
				t.Fatalf("tool executed %d time(s) under scope %s, want 0: %+v", len(p.executions), scope, p.executions)
			}
			wantField(t, p.lastResponse(t), "error", "approval_scope_refused")
			if log := p.gate.Approvals(); len(log) != 0 {
				t.Errorf("approval log = %+v, want empty after a refused scope", log)
			}
		})
	}
}

func TestUnknownScopeIsRefused(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy:  OnMutationRequireApproval,
		respond: approve(map[string]any{"verdict": "approve", "scope": "forever_and_ever"}),
	})
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) under an unknown scope, want 0: %+v", len(p.executions), p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "malformed_verdict")
}

func editVerdict(args map[string]any) map[string]any {
	return map[string]any{
		"verdict":  "edit",
		"approver": "user:sre-oncall",
		"note":     "10 replicas would exhaust the node pool",
		"args":     args,
	}
}

func TestEditedArgumentsAreWhatExecutes(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy:  OnMutationRequireApproval,
		respond: approve(editVerdict(map[string]any{"deployment": "api", "replicas": 2})),
	})

	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want exactly 1: %+v", len(p.executions), p.executions)
	}
	if got := p.executions[0]; got.Deployment != "api" || got.Replicas != 2 {
		t.Fatalf("executed with %+v, want the operator's {api 2}", got)
	}

	log := p.gate.Approvals()
	if len(log) != 1 {
		t.Fatalf("approval log = %+v, want exactly one entry", log)
	}
	if want := "scale_deployment(deployment=api, replicas=2)"; log[0].Key != want {
		t.Errorf("approval key = %q, want %q", log[0].Key, want)
	}

	edits := p.appliedEdits(t)
	if len(edits) != 1 {
		t.Fatalf("applied-edit records = %+v, want exactly one", edits)
	}
	e := edits[0]
	if e.Tool != "scale_deployment" || e.Approver != "user:sre-oncall" {
		t.Errorf("record = %+v, want scale_deployment approved by user:sre-oncall", e)
	}
	if want := "scale_deployment(deployment=api, replicas=10)"; e.ProposedKey != want {
		t.Errorf("record.ProposedKey = %q, want %q", e.ProposedKey, want)
	}
	if want := "scale_deployment(deployment=api, replicas=2)"; e.ExecutedKey != want {
		t.Errorf("record.ExecutedKey = %q, want %q", e.ExecutedKey, want)
	}
	if fmt.Sprint(e.ProposedArgs["replicas"]) != "10" || fmt.Sprint(e.ExecutedArgs["replicas"]) != "2" {
		t.Errorf("record args = proposed %v / executed %v, want 10 / 2", e.ProposedArgs, e.ExecutedArgs)
	}
}

func TestEditIsRefusedWhenItViolatesTheSchema(t *testing.T) {
	tests := []struct {
		name  string
		args  map[string]any
		wants []string
	}{
		{
			name:  "wrong type",
			args:  map[string]any{"deployment": "api", "replicas": "as many as it takes"},
			wants: []string{"input schema"},
		},
		{
			name:  "undeclared argument",
			args:  map[string]any{"deployment": "api", "replicas": 2, "namespace": "prod"},
			wants: []string{"namespace", "does not declare"},
		},
		{
			name:  "no arguments at all",
			args:  map[string]any{},
			wants: []string{"carries no arguments"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := runGateProbe(t, gateProbeConfig{
				policy:  OnMutationRequireApproval,
				respond: approve(editVerdict(tt.args)),
			})
			if len(p.executions) != 0 {
				t.Fatalf("tool executed %d time(s) on a refused edit, want 0: %+v", len(p.executions), p.executions)
			}
			resp := p.lastResponse(t)
			wantField(t, resp, "error", "edit_refused")
			wantDetailMentions(t, resp, append(tt.wants, "NOT made")...)
			if log := p.gate.Approvals(); len(log) != 0 {
				t.Errorf("approval log = %+v, want empty after a refused edit", log)
			}
			if edits := p.appliedEdits(t); len(edits) != 0 {
				t.Errorf("applied-edit records = %+v, want none", edits)
			}
		})
	}
}

func TestEditCannotEscapeADenyPolicy(t *testing.T) {
	policy, err := permissions.NewPolicy(nil, []string{"scale_deployment:*deployment=prod*"})
	if err != nil {
		t.Fatal(err)
	}
	p := runGateProbe(t, gateProbeConfig{
		policy:      OnMutationRequireApproval,
		gateOptions: permissions.Options{Mode: permissions.ModeYolo, Policy: policy},
		respond:     approve(editVerdict(map[string]any{"deployment": "prod", "replicas": 2})),
	})

	if p.confirmationID == "" {
		t.Fatal("the model's own call did not park, so this run never reached the edit")
	}
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) on an edit into denied territory, want 0: %+v", len(p.executions), p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "denied_by_policy")
	if log := p.gate.Approvals(); len(log) != 0 {
		t.Errorf("approval log = %+v, want empty", log)
	}
}

func TestUnattributedEditIsRefused(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		respond: approve(map[string]any{
			"verdict": "edit",
			"args":    map[string]any{"deployment": "api", "replicas": 2},
		}),
	})
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) on an unattributed edit, want 0: %+v", len(p.executions), p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "edit_unattributed")
	wantDetailMentions(t, p.lastResponse(t), "NOT made")
}

func TestEditWithBroadScopeIsRefused(t *testing.T) {
	v := editVerdict(map[string]any{"deployment": "api", "replicas": 2})
	v["scope"] = string(ScopeSessionTool)
	p := runGateProbe(t, gateProbeConfig{
		policy:  OnMutationRequireApproval,
		respond: approve(v),
	})
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) under a refused scope, want 0: %+v", len(p.executions), p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "approval_scope_refused")
	if edits := p.appliedEdits(t); len(edits) != 0 {
		t.Errorf("applied-edit records = %+v, want none", edits)
	}
}

func TestEditSurvivesARestart(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy:               OnMutationRequireApproval,
		restartBeforeVerdict: true,
		respond:              approve(editVerdict(map[string]any{"deployment": "api", "replicas": 2})),
	})
	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s) across the restart, want exactly 1: %+v", len(p.executions), p.executions)
	}
	if got := p.executions[0]; got.Replicas != 2 {
		t.Errorf("executed with %+v, want the operator's edit {api 2}", got)
	}
	if edits := p.appliedEdits(t); len(edits) != 1 {
		t.Errorf("applied-edit records = %+v, want exactly one", edits)
	}
}

func TestContradictoryVerdictIsRefused(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		respond: func(confID string) *genai.Content {
			return verdictResponse(confID, map[string]any{
				"confirmed": false,
				"payload":   map[string]any{"verdict": "approve"},
			})
		},
	})
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) on a contradictory verdict, want 0: %+v", len(p.executions), p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "malformed_verdict")
}

func TestPolicyDenyIsNotPutToAnOperator(t *testing.T) {
	policy, err := permissions.NewPolicy(nil, []string{"scale_deployment:*"})
	if err != nil {
		t.Fatal(err)
	}
	p := runGateProbe(t, gateProbeConfig{
		policy:      OnMutationRequireApproval,
		gateOptions: permissions.Options{Mode: permissions.ModeYolo, Policy: policy},
	})
	if p.confirmationID != "" {
		t.Error("a denied call was parked for approval")
	}
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) despite a deny rule, want 0: %+v", len(p.executions), p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "denied_by_policy")
}

func TestApplyPolicyExecutesWithoutAsking(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationApply})
	if p.confirmationID != "" {
		t.Error("apply parked for approval")
	}
	if len(p.executions) != 1 {
		t.Fatalf("tool executed %d time(s), want 1: %+v", len(p.executions), p.executions)
	}
}

func TestDryRunPolicyNeverExecutes(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{policy: OnMutationDryRun})
	if p.confirmationID != "" {
		t.Error("dry_run parked for approval")
	}
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) under dry_run, want 0: %+v", len(p.executions), p.executions)
	}
	resp := p.lastResponse(t)
	wantField(t, resp, "status", "dry_run")
	wantField(t, resp, "tool", "scale_deployment")
	args, _ := resp["args"].(map[string]any)
	if args == nil || args["deployment"] != "api" {
		t.Errorf("response args = %v, want the proposed call's arguments", resp["args"])
	}
	wantDetailMentions(t, resp, "NOT made", "nothing changed")
}

func TestOutboxRunsBeforeTheGate(t *testing.T) {
	p := runGateProbe(t, gateProbeConfig{
		policy: OnMutationRequireApproval,
		seed:   seedDanglingMutation,
		extraPluginsFirst: func(t *testing.T) []*plugin.Plugin {
			t.Helper()
			outbox, err := effects.New(effects.Config{Predicate: effects.NewPredicate(nil)})
			if err != nil {
				t.Fatalf("effects.New: %v", err)
			}
			return []*plugin.Plugin{outbox}
		},
	})

	if p.confirmationID != "" {
		t.Error("the write gate parked a call the outbox had already refused")
	}
	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) in ambiguous-effect mode, want 0: %+v", len(p.executions), p.executions)
	}
	wantField(t, p.lastResponse(t), "error", "ambiguous_prior_effect")
}

func seedDanglingMutation(t *testing.T, svc adksession.Service) {
	t.Helper()
	call := genai.NewPartFromFunctionCall("scale_deployment", map[string]any{"deployment": "api", "replicas": 3})
	call.FunctionCall.ID = "dangling-call-1"
	ev := &adksession.Event{
		InvocationID: "prior-invocation",
		Author:       "gate_agent",
		Timestamp:    time.Now().Add(-time.Minute),
		LLMResponse:  model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{call}}},
	}
	got, err := svc.Get(context.Background(), &adksession.GetRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if err := svc.AppendEvent(context.Background(), got.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

// TestMast561_DecisionSinkPersistsBeforeToolRunsAndDuplicateDispatchIsRefused
// verifies both halves of the mast #561 fix inside pkg/approval:
//  1. WithDecisionSink receives the Decision BEFORE the tool body executes,
//     so an interrupted tool call still has its Decision persisted.
//  2. Re-sending the confirmation response for an already-dispatched
//     FunctionCallID is refused by the per-call dispatch guard instead of
//     executing the mutation a second time.
func TestMast561_DecisionSinkPersistsBeforeToolRunsAndDuplicateDispatchIsRefused(t *testing.T) {
	dir := t.TempDir()
	var sunk []Decision
	var seenSinkDuringTool int
	var executions int

	scale, err := functiontool.New(functiontool.Config{
		Name:        "scale_deployment",
		Description: "changes a deployment's replica count",
	}, func(_ adkagent.Context, args scaleArgs) (map[string]any, error) {
		executions++
		seenSinkDuringTool = len(sunk)
		return map[string]any{"scaled": args.Replicas}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	m := &scriptedModel{name: "gate", calls: []*model.LLMResponse{{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall("scale_deployment", map[string]any{
				"deployment": "api",
				"replicas":   10,
			})},
		},
	}}}

	root, err := llmagent.New(llmagent.Config{
		Name:        "gate_agent",
		Description: "write-gate probe",
		Instruction: "act",
		Model:       m,
		Tools:       []tool.Tool{scale},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	wg, err := NewPlugin(Config{
		Policy:   OnMutationRequireApproval,
		Mutating: alwaysMutating,
		Gate:     permissions.New(permissions.Options{}),
	}, WithDecisionSink(func(_ context.Context, d Decision) error {
		sunk = append(sunk, d)
		return nil
	}))
	if err != nil {
		t.Fatalf("NewPlugin: %v", err)
	}

	svc := sqliteServiceAt(t, dir)
	r, err := runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{wg}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	run := func(msg *genai.Content) {
		t.Helper()
		for _, err := range r.Run(context.Background(), testUser, sid, msg, adkagent.RunConfig{}) {
			if err != nil {
				t.Fatalf("runner.Run: %v", err)
			}
		}
	}

	run(genai.NewContentFromText("scale api to 10", genai.RoleUser))
	confID, _ := confirmationRequest(t, svc)
	if confID == "" {
		t.Fatal("expected parked confirmation")
	}

	approveMsg := verdictResponse(confID, map[string]any{
		"confirmed": true,
		"payload":   map[string]any{"verdict": "approve", "approver": "user:sre-oncall"},
	})
	run(approveMsg)

	if executions != 1 {
		t.Fatalf("executions = %d, want 1", executions)
	}
	if seenSinkDuringTool != 1 {
		t.Errorf("DecisionSink had %d records when tool body ran, want 1 (must persist BEFORE tool runs)", seenSinkDuringTool)
	}

	// Re-send the same confirmation response a second time: the duplicate-dispatch
	// guard must refuse it and never execute the tool a second time.
	run(approveMsg)
	if executions != 1 {
		t.Fatalf("executions after duplicate confirmation = %d, want 1 (duplicate dispatch must be refused)", executions)
	}
	resps := toolResponses(t, svc, "scale_deployment")
	last := resps[len(resps)-1]
	wantField(t, last, "error", "ambiguous_prior_effect")
}

func TestMast561_CrossRestartDecisionLookupAndFailClosedDecisionSink(t *testing.T) {
	// 1. Simulate a mid-tool crash on approval where StateDelta was never committed
	// to session.State, then construct a fresh writeGate on restart with an empty
	// in-memory dispatched map and verify DecisionLookup refuses duplicate dispatch.
	dir := t.TempDir()
	durableLedger := map[string]Decision{}
	var executions int

	scale, err := functiontool.New(functiontool.Config{
		Name:        "scale_deployment",
		Description: "Scale a deployment",
	}, func(_ adkagent.Context, in scaleArgs) (map[string]any, error) {
		executions++
		return map[string]any{"scaled": in.Deployment, "replicas": in.Replicas}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	m := &scriptedModel{name: "gate", calls: []*model.LLMResponse{{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall("scale_deployment", map[string]any{
				"deployment": "api",
				"replicas":   10,
			})},
		},
	}}}

	root, err := llmagent.New(llmagent.Config{
		Name:        "gate_agent",
		Description: "write-gate probe",
		Instruction: "act",
		Model:       m,
		Tools:       []tool.Tool{scale},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	svc := sqliteServiceAt(t, dir)
	wg1, err := NewPlugin(Config{
		Policy:   OnMutationRequireApproval,
		Mutating: alwaysMutating,
		Gate:     permissions.New(permissions.Options{}),
	})
	if err != nil {
		t.Fatalf("NewPlugin wg1: %v", err)
	}
	r1, err := runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{wg1}},
	})
	if err != nil {
		t.Fatalf("runner.New r1: %v", err)
	}

	for _, err := range r1.Run(context.Background(), testUser, sid, genai.NewContentFromText("scale api to 10", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("r1.Run: %v", err)
		}
	}
	confID, _ := confirmationRequest(t, svc)
	origCallID := parkedOriginalCallID(t, svc)
	if confID == "" || origCallID == "" {
		t.Fatalf("expected parked confirmation, got confID=%q origCallID=%q", confID, origCallID)
	}

	// Simulate that before a mid-tool crash, DecisionSink persisted a Decision
	// for origCallID into durableLedger, while session.State has NO DecisionStateKey.
	durableLedger[sid+":"+origCallID] = Decision{
		Session:        sid,
		FunctionCallID: origCallID,
		Tool:           "scale_deployment",
		Outcome:        OutcomeApprove,
		Disposition:    DispositionAuthorized,
	}

	// Construct a brand-new writeGate (simulating process restart: empty g.dispatched)
	// wired with WithDecisionLookup against durableLedger.
	wgRestarted, err := NewPlugin(Config{
		Policy:   OnMutationRequireApproval,
		Mutating: alwaysMutating,
		Gate:     permissions.New(permissions.Options{}),
	}, WithDecisionLookup(func(_ context.Context, sessionID, functionCallID string) (bool, error) {
		_, ok := durableLedger[sessionID+":"+functionCallID]
		return ok, nil
	}))
	if err != nil {
		t.Fatalf("NewPlugin wgRestarted: %v", err)
	}
	rRestarted, err := runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{wgRestarted}},
	})
	if err != nil {
		t.Fatalf("runner.New rRestarted: %v", err)
	}

	approveMsg := verdictResponse(confID, map[string]any{
		"confirmed": true,
		"payload":   map[string]any{"verdict": "approve", "approver": "user:sre-oncall"},
	})
	for _, err := range rRestarted.Run(context.Background(), testUser, sid, approveMsg, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("rRestarted.Run: %v", err)
		}
	}
	if executions != 0 {
		t.Fatalf("executions across restart when DecisionLookup found prior decision = %d, want 0", executions)
	}
	resps := toolResponses(t, svc, "scale_deployment")
	wantField(t, resps[len(resps)-1], "error", "ambiguous_prior_effect")

	// 2. Verify fail-closed when DecisionSink returns an error: the tool must NOT execute.
	dir2 := t.TempDir()
	svc2 := sqliteServiceAt(t, dir2)
	m2 := &scriptedModel{name: "gate", calls: []*model.LLMResponse{{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{genai.NewPartFromFunctionCall("scale_deployment", map[string]any{
				"deployment": "api",
				"replicas":   10,
			})},
		},
	}}}
	root2, err := llmagent.New(llmagent.Config{
		Name:        "gate_agent",
		Description: "write-gate probe",
		Instruction: "act",
		Model:       m2,
		Tools:       []tool.Tool{scale},
	})
	if err != nil {
		t.Fatalf("llmagent.New root2: %v", err)
	}
	wgSinkFail, err := NewPlugin(Config{
		Policy:   OnMutationRequireApproval,
		Mutating: alwaysMutating,
		Gate:     permissions.New(permissions.Options{}),
	}, WithDecisionSink(func(_ context.Context, _ Decision) error {
		return errors.New("sqlite disk full")
	}))
	if err != nil {
		t.Fatalf("NewPlugin wgSinkFail: %v", err)
	}
	rSinkFail, err := runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root2,
		SessionService:    svc2,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{wgSinkFail}},
	})
	if err != nil {
		t.Fatalf("runner.New rSinkFail: %v", err)
	}
	for _, err := range rSinkFail.Run(context.Background(), testUser, sid, genai.NewContentFromText("scale api to 10", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("rSinkFail.Run park: %v", err)
		}
	}
	confID2, _ := confirmationRequest(t, svc2)
	approveMsg2 := verdictResponse(confID2, map[string]any{
		"confirmed": true,
		"payload":   map[string]any{"verdict": "approve", "approver": "user:sre-oncall"},
	})
	for _, err := range rSinkFail.Run(context.Background(), testUser, sid, approveMsg2, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("rSinkFail.Run approve: %v", err)
		}
	}
	if executions != 0 {
		t.Fatalf("executions when DecisionSink failed = %d, want 0 (must fail closed)", executions)
	}
	resps2 := toolResponses(t, svc2, "scale_deployment")
	wantField(t, resps2[len(resps2)-1], "error", "decision_persist_failed")
}

func parkedOriginalCallID(t *testing.T, svc adksession.Service) string {
	t.Helper()
	got, err := svc.Get(context.Background(), &adksession.GetRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for ev := range got.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, part := range ev.Content.Parts {
			if part != nil && part.FunctionCall != nil && part.FunctionCall.Name != toolconfirmation.FunctionCallName {
				return part.FunctionCall.ID
			}
		}
	}
	return ""
}

func TestCallKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no args", nil, "restart_daemonset()"},
		{
			"keys are sorted",
			map[string]any{"replicas": 10, "deployment": "api", "namespace": "prod"},
			"restart_daemonset(deployment=api, namespace=prod, replicas=10)",
		},
		{"nested values are compact json", map[string]any{"patch": map[string]any{"spec": true}}, `restart_daemonset(patch={"spec":true})`},
		{"long values are elided", map[string]any{"manifest": strings.Repeat("y", 200)}, "restart_daemonset(manifest=" + strings.Repeat("y", 120) + "…)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := CallKey("restart_daemonset", tt.args); got != tt.want {
				t.Errorf("CallKey = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCallKeyIsStableAcrossRenders(t *testing.T) {
	t.Parallel()
	args := map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8}
	first := CallKey("t", args)
	for i := 0; i < 50; i++ {
		if got := CallKey("t", args); got != first {
			t.Fatalf("CallKey render %d = %q, want %q", i, got, first)
		}
	}
}

func TestDecodeVerdict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		c       *toolconfirmation.ToolConfirmation
		want    Verdict
		wantErr string
	}{
		{
			name: "bare confirmed",
			c:    &toolconfirmation.ToolConfirmation{Confirmed: true},
			want: Verdict{Verdict: OutcomeApprove},
		},
		{
			name: "bare denial",
			c:    &toolconfirmation.ToolConfirmation{Confirmed: false},
			want: Verdict{Verdict: OutcomeReject},
		},
		{
			name: "full record through the event log's map shape",
			c: &toolconfirmation.ToolConfirmation{Confirmed: true, Payload: map[string]any{
				"verdict":  "approve",
				"scope":    "once",
				"note":     "checked the HPA first",
				"approver": "user:sre-oncall",
			}},
			want: Verdict{Verdict: OutcomeApprove, Scope: ScopeOnce, Note: "checked the HPA first", Approver: "user:sre-oncall"},
		},
		{
			name: "typed payload in-process",
			c:    &toolconfirmation.ToolConfirmation{Confirmed: true, Payload: Verdict{Verdict: OutcomeApprove, Approver: "user:x"}},
			want: Verdict{Verdict: OutcomeApprove, Approver: "user:x"},
		},
		{
			name: "edit carries replacement arguments",
			c: &toolconfirmation.ToolConfirmation{Confirmed: true, Payload: map[string]any{
				"verdict": "edit",
				"args":    map[string]any{"replicas": float64(2)},
			}},
			want: Verdict{Verdict: OutcomeEdit, Args: map[string]any{"replicas": float64(2)}},
		},
		{
			name:    "unknown verdict",
			c:       &toolconfirmation.ToolConfirmation{Confirmed: true, Payload: map[string]any{"verdict": "maybe"}},
			wantErr: "unknown verdict",
		},
		{
			name:    "approve contradicting confirmed=false",
			c:       &toolconfirmation.ToolConfirmation{Confirmed: false, Payload: map[string]any{"verdict": "approve"}},
			wantErr: "contradictory",
		},
		{
			name:    "reject contradicting confirmed=true",
			c:       &toolconfirmation.ToolConfirmation{Confirmed: true, Payload: map[string]any{"verdict": "reject"}},
			wantErr: "contradictory",
		},
		{
			name:    "payload that is not a record",
			c:       &toolconfirmation.ToolConfirmation{Confirmed: true, Payload: "looks good to me"},
			wantErr: "not a verdict record",
		},
		{
			name:    "no confirmation at all",
			c:       nil,
			wantErr: "no tool confirmation",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := DecodeVerdict(tt.c)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("DecodeVerdict = %+v, want error mentioning %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeVerdict: %v", err)
			}
			if got.Verdict != tt.want.Verdict || got.Scope != tt.want.Scope ||
				got.Note != tt.want.Note || got.Approver != tt.want.Approver ||
				len(got.Args) != len(tt.want.Args) {
				t.Errorf("DecodeVerdict = %+v, want %+v", got, tt.want)
			}
			for k, want := range tt.want.Args {
				if got.Args[k] != want {
					t.Errorf("args[%q] = %v, want %v", k, got.Args[k], want)
				}
			}
		})
	}
}

func TestVerdictDecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		v    Verdict
		want permissions.Decision
	}{
		{Verdict{Verdict: OutcomeApprove}, permissions.DecisionAllowOnce},
		{Verdict{Verdict: OutcomeApprove, Scope: ScopeOnce}, permissions.DecisionAllowOnce},
		{Verdict{Verdict: OutcomeApprove, Scope: ScopeSessionTool}, permissions.DecisionAllowSessionTool},
		{Verdict{Verdict: OutcomeReject, Scope: ScopeAlways}, permissions.DecisionDeny},
	}
	for _, tt := range tests {
		got, err := tt.v.Decision()
		if err != nil {
			t.Fatalf("Decision() for %+v: %v", tt.v, err)
		}
		if got != tt.want {
			t.Errorf("Decision() for %+v = %v, want %v", tt.v, got, tt.want)
		}
	}
}
