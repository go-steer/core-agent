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

	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

func fixtureChangeSet() []ProposedChange {
	return []ProposedChange{
		{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api", "replicas": float64(3)}},
		{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "worker", "replicas": float64(1)}},
	}
}

func scaleCall(deployment string, replicas int) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{
		Role: genai.RoleModel,
		Parts: []*genai.Part{genai.NewPartFromFunctionCall("scale_deployment", map[string]any{
			"deployment": deployment,
			"replicas":   replicas,
		})},
	}}
}

type readCall struct {
	tool string
	args map[string]any
}

type csProbe struct {
	executions    []scaleArgs
	reads         []readCall
	cluster       map[string]any
	readErr       error
	clock         time.Time
	gates         []*permissions.Gate
	confirmations []string
	hints         []string
	requests      []Request
	responses     []map[string]any
	state         map[string]any
}

type csTurn struct {
	process int
	send    func(p *csProbe) *genai.Content
}

type csConfig struct {
	set             []ProposedChange
	specialist      string
	noGrants        bool
	ttl             time.Duration
	preconditions   map[string]*Precondition
	preconditionErr error
	captures        *CaptureRules
	gateOptions     permissions.Options
	scripts         [][]*model.LLMResponse
	turns           []csTurn
	cluster         map[string]any
}

func runChangeSetProbe(t *testing.T, cfg csConfig) *csProbe {
	t.Helper()
	probe := &csProbe{
		cluster: cfg.cluster,
		clock:   time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC),
	}
	if probe.cluster == nil {
		probe.cluster = map[string]any{"replicas": float64(3)}
	}
	dir := t.TempDir()

	build := func(process int) (*runner.Runner, adksession.Service) {
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

		script := cfg.scripts[0]
		if process < len(cfg.scripts) {
			script = cfg.scripts[process]
		}
		root, err := llmagent.New(llmagent.Config{
			Name:        "change_executor",
			Description: "change-set probe",
			Instruction: "act",
			Model:       &scriptedModel{name: "grants", calls: script},
			Tools:       []tool.Tool{scale},
		})
		if err != nil {
			t.Fatalf("llmagent.New: %v", err)
		}

		var grants *Freshness
		if !cfg.noGrants {
			grants = &Freshness{
				TTL: cfg.ttl,
				Now: func() time.Time { return probe.clock },
				Precondition: func(name string) (*Precondition, error) {
					if cfg.preconditionErr != nil {
						return nil, cfg.preconditionErr
					}
					return cfg.preconditions[name], nil
				},
				Read: func(_ adkagent.Context, name string, args map[string]any) (map[string]any, error) {
					probe.reads = append(probe.reads, readCall{tool: name, args: args})
					if probe.readErr != nil {
						return nil, probe.readErr
					}
					out := map[string]any{}
					for k, v := range probe.cluster {
						out[k] = v
					}
					return out, nil
				},
			}
		}

		gate := permissions.New(cfg.gateOptions)
		probe.gates = append(probe.gates, gate)
		wg, err := New(Config{
			Policy:   OnMutationRequireApproval,
			Mutating: alwaysMutating,
			Gate:     gate,
			Grants:   grants,
			Captures: cfg.captures,
		})
		if err != nil {
			t.Fatalf("approval.New: %v", err)
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
		return r, svc
	}

	current := -1
	var r *runner.Runner
	var svc adksession.Service
	for i, turn := range cfg.turns {
		if turn.process != current {
			r, svc = build(turn.process)
			current = turn.process
			if i == 0 && cfg.set != nil {
				seedChangeSet(t, svc, cfg.specialist, cfg.set)
			}
		}
		msg := turn.send(probe)
		if msg == nil {
			t.Fatalf("turn %d produced no input (no confirmation to answer?)", i)
		}
		for _, err := range r.Run(context.Background(), testUser, sid, msg, adkagent.RunConfig{}) {
			if err != nil {
				t.Fatalf("turn %d: runner.Run: %v", i, err)
			}
		}
		probe.confirmations, probe.hints, probe.requests = confirmationRequests(t, svc)
	}
	probe.responses = toolResponses(t, svc, "scale_deployment")
	probe.state = sessionState(t, svc)
	return probe
}

func seedChangeSet(t *testing.T, svc adksession.Service, specialist string, set []ProposedChange) {
	t.Helper()
	if specialist == "" {
		specialist = "diagnoser"
	}
	raw, err := EncodeChangeSet(set)
	if err != nil {
		t.Fatalf("EncodeChangeSet: %v", err)
	}
	got, err := svc.Create(context.Background(), &adksession.CreateRequest{
		AppName: testApp, UserID: testUser, SessionID: sid,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ev := &adksession.Event{
		InvocationID: "seed-invocation",
		Author:       specialist,
		Timestamp:    time.Now().Add(-time.Minute),
		Actions:      adksession.EventActions{StateDelta: map[string]any{ChangeSetStateKey(specialist): raw}},
	}
	if err := svc.AppendEvent(context.Background(), got.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
}

func confirmationRequests(t *testing.T, svc adksession.Service) (ids, hints []string, reqs []Request) {
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
			if part == nil || part.FunctionCall == nil || part.FunctionCall.Name != toolconfirmation.FunctionCallName {
				continue
			}
			p := DescribeConfirmation(part.FunctionCall.Args)
			ids = append(ids, part.FunctionCall.ID)
			hints = append(hints, p.Hint)
			req, err := DecodeRequest(p.Request)
			if err != nil {
				t.Fatalf("parked payload is not an approval request: %v", err)
			}
			reqs = append(reqs, req)
		}
	}
	return ids, hints, reqs
}

func answerLast(payload map[string]any) func(*csProbe) *genai.Content {
	return func(p *csProbe) *genai.Content {
		if len(p.confirmations) == 0 {
			return nil
		}
		return verdictResponse(p.confirmations[len(p.confirmations)-1], map[string]any{
			"confirmed": true, "payload": payload,
		})
	}
}

func modelSays(text string) *model.LLMResponse {
	return &model.LLMResponse{Content: genai.NewContentFromText(text, genai.RoleModel)}
}

func userSays(text string) func(*csProbe) *genai.Content {
	return func(*csProbe) *genai.Content { return genai.NewContentFromText(text, genai.RoleUser) }
}

func approveSet() map[string]any {
	return map[string]any{"verdict": "approve", "scope": "change_set", "approver": "user:sre-oncall"}
}

func (p *csProbe) approvals() []permissions.ApprovalLog {
	var out []permissions.ApprovalLog
	for _, g := range p.gates {
		out = append(out, g.Approvals()...)
	}
	return out
}

func (p *csProbe) grants(t *testing.T) []Grant {
	t.Helper()
	var out []Grant
	for k, v := range p.state {
		if !strings.HasPrefix(k, GrantStateKeyPrefix) {
			continue
		}
		g, err := DecodeGrant(v)
		if err != nil {
			t.Fatalf("state[%q] is not a grant record: %v", k, err)
		}
		out = append(out, g)
	}
	return out
}

func (p *csProbe) executed(deployment string, replicas int) bool {
	for _, e := range p.executions {
		if e.Deployment == deployment && e.Replicas == replicas {
			return true
		}
	}
	return false
}

func twoCallTurns(payload map[string]any, second *model.LLMResponse) ([][]*model.LLMResponse, []csTurn) {
	scripts := [][]*model.LLMResponse{{scaleCall("api", 3), second}}
	return scripts, []csTurn{
		{send: userSays("remediate the api outage")},
		{send: answerLast(payload)},
	}
}

func TestChangeSetApprovalAuthorizesTheRestOfTheSet(t *testing.T) {
	scripts, turns := twoCallTurns(approveSet(), scaleCall("worker", 1))
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), scripts: scripts, turns: turns})

	if len(p.confirmations) != 1 {
		t.Fatalf("operator was asked %d time(s), want exactly 1\nhints: %q", len(p.confirmations), p.hints)
	}
	if len(p.executions) != 2 {
		t.Fatalf("tool executed %d time(s), want 2: %+v", len(p.executions), p.executions)
	}
	if !p.executed("api", 3) || !p.executed("worker", 1) {
		t.Errorf("executions = %+v, want both calls of the approved set", p.executions)
	}

	grants := p.grants(t)
	if len(grants) != 1 {
		t.Fatalf("grant records = %+v, want exactly one", grants)
	}
	g := grants[0]
	if g.ConsumedBy == "" {
		t.Errorf("grant %+v was never marked consumed", g)
	}
	if g.Approver != "user:sre-oncall" {
		t.Errorf("grant.Approver = %q, want the operator who answered", g.Approver)
	}
	if want := `scale_deployment({"deployment":"worker","replicas":1})`; g.Signature != want {
		t.Errorf("grant.Signature = %q, want %q", g.Signature, want)
	}
	if !strings.Contains(g.Origin, "deployment=api") {
		t.Errorf("grant.Origin = %q, want the call the operator actually answered", g.Origin)
	}

	log := p.approvals()
	if len(log) != 2 {
		t.Fatalf("approval log = %+v, want one entry per executed call", log)
	}
	for _, e := range log {
		if e.Decision != permissions.DecisionAllowOnce {
			t.Errorf("approval %+v is not an allow-once", e)
		}
	}
}

func TestGrantAuthorizesOnlyTheExactCall(t *testing.T) {
	scripts, turns := twoCallTurns(approveSet(), scaleCall("worker", 20))
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), scripts: scripts, turns: turns})

	if len(p.executions) != 1 || !p.executed("api", 3) {
		t.Fatalf("executions = %+v, want only the call the operator answered directly", p.executions)
	}
	if len(p.confirmations) != 2 {
		t.Fatalf("operator was asked %d time(s), want 2", len(p.confirmations))
	}
	if grants := p.grants(t); len(grants) != 1 || grants[0].ConsumedBy != "" {
		t.Errorf("grant records = %+v, want the worker/1 grant still unspent", grants)
	}
}

func TestGrantExpires(t *testing.T) {
	scripts := [][]*model.LLMResponse{
		{scaleCall("api", 3), modelSays("the first change is done")},
		{scaleCall("worker", 1)},
	}
	turns := []csTurn{
		{process: 0, send: userSays("remediate")},
		{process: 0, send: answerLast(approveSet())},
		{process: 1, send: func(p *csProbe) *genai.Content {
			p.clock = p.clock.Add(2 * time.Hour)
			return genai.NewContentFromText("carry on", genai.RoleUser)
		}},
	}
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), ttl: 10 * time.Minute, scripts: scripts, turns: turns})

	if p.executed("worker", 1) {
		t.Fatalf("an expired grant ran the call: %+v", p.executions)
	}
	if len(p.confirmations) != 2 {
		t.Fatalf("operator was asked %d time(s), want 2", len(p.confirmations))
	}
	last := p.requests[len(p.requests)-1]
	if !strings.Contains(last.Stale, "expired") {
		t.Errorf("re-parked request.Stale = %q, want it to say the approval expired", last.Stale)
	}
	if !strings.Contains(p.hints[len(p.hints)-1], "asking again") {
		t.Errorf("re-parked hint = %q, want it to say asking again", p.hints[len(p.hints)-1])
	}
	grants := p.grants(t)
	if len(grants) != 1 || grants[0].VoidedBy == "" {
		t.Fatalf("grant records = %+v, want the expired grant voided", grants)
	}
}

func TestGrantSurvivesARestart(t *testing.T) {
	scripts := [][]*model.LLMResponse{
		{scaleCall("api", 3), modelSays("the first change is done")},
		{scaleCall("worker", 1)},
	}
	turns := []csTurn{
		{process: 0, send: userSays("remediate")},
		{process: 0, send: answerLast(approveSet())},
		{process: 1, send: userSays("carry on")},
	}
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), scripts: scripts, turns: turns})

	if !p.executed("worker", 1) {
		t.Fatalf("the granted call did not run after a restart: %+v (parks: %q)", p.executions, p.hints)
	}
	if len(p.confirmations) != 1 {
		t.Fatalf("operator was asked %d time(s) across the restart, want 1", len(p.confirmations))
	}
	if got := p.gates[1].Approvals(); len(got) != 1 || !strings.Contains(got[0].Key, "worker") {
		t.Errorf("post-restart approval log = %+v, want only the granted call", got)
	}
}

func TestGrantIsSingleUse(t *testing.T) {
	scripts := [][]*model.LLMResponse{
		{scaleCall("api", 3), modelSays("the first change is done")},
		{scaleCall("worker", 1), scaleCall("worker", 1)},
	}
	turns := []csTurn{
		{process: 0, send: userSays("remediate")},
		{process: 0, send: answerLast(approveSet())},
		{process: 1, send: userSays("carry on")},
	}
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), scripts: scripts, turns: turns})

	worker := 0
	for _, e := range p.executions {
		if e.Deployment == "worker" {
			worker++
		}
	}
	if worker != 1 {
		t.Fatalf("the granted call ran %d time(s), want exactly 1: %+v", worker, p.executions)
	}
	if len(p.confirmations) != 2 {
		t.Fatalf("operator was asked %d time(s), want 2", len(p.confirmations))
	}
	if last := p.requests[len(p.requests)-1]; !strings.Contains(last.Stale, "already been made") {
		t.Errorf("re-parked request.Stale = %q, want it to say the call was already made once", last.Stale)
	}
}

func TestGrantIsVoidedWhenTheClusterMoves(t *testing.T) {
	pre := map[string]*Precondition{"scale_deployment": {
		Read:     "get_deployment",
		ArgsFrom: map[string]string{"name": "deployment"},
		Fields:   []string{"replicas"},
	}}
	scripts := [][]*model.LLMResponse{
		{scaleCall("api", 3), modelSays("the first change is done")},
		{scaleCall("worker", 1)},
	}
	turns := []csTurn{
		{process: 0, send: userSays("remediate")},
		{process: 0, send: answerLast(approveSet())},
		{process: 1, send: func(p *csProbe) *genai.Content {
			p.cluster = map[string]any{"replicas": float64(9)}
			return genai.NewContentFromText("carry on", genai.RoleUser)
		}},
	}
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), preconditions: pre, scripts: scripts, turns: turns})

	if p.executed("worker", 1) {
		t.Fatalf("a granted call ran against a cluster that had moved: %+v", p.executions)
	}
	if len(p.confirmations) != 2 {
		t.Fatalf("operator was asked %d time(s), want 2", len(p.confirmations))
	}
	last := p.requests[len(p.requests)-1]
	if !strings.Contains(last.Stale, "replicas") || !strings.Contains(last.Stale, "9") {
		t.Errorf("request.Stale = %q, want it to name the field that moved and its new value", last.Stale)
	}
	if len(p.reads) < 2 {
		t.Fatalf("precondition reads = %+v, want one at mint and one before the call fires", p.reads)
	}
	for _, r := range p.reads {
		if r.tool != "get_deployment" || r.args["name"] != "worker" {
			t.Errorf("precondition read %+v, want get_deployment(name=worker)", r)
		}
	}
	if grants := p.grants(t); len(grants) != 1 || grants[0].VoidedBy == "" {
		t.Errorf("grant records = %+v, want the grant voided", grants)
	}
}

func TestUnchangedPreconditionStillFires(t *testing.T) {
	pre := map[string]*Precondition{"scale_deployment": {
		Read:     "get_deployment",
		ArgsFrom: map[string]string{"name": "deployment"},
		Fields:   []string{"replicas"},
	}}
	scripts, turns := twoCallTurns(approveSet(), scaleCall("worker", 1))
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), preconditions: pre, scripts: scripts, turns: turns})

	if !p.executed("worker", 1) {
		t.Fatalf("the granted call did not run against an unchanged cluster: %+v (parks: %q)", p.executions, p.hints)
	}
	if len(p.confirmations) != 1 {
		t.Fatalf("operator was asked %d time(s), want 1", len(p.confirmations))
	}
}

func TestChangeSetVerdictRefusals(t *testing.T) {
	tests := []struct {
		name    string
		cfg     csConfig
		payload map[string]any
		wants   []string
	}{
		{
			name:    "no change set was recorded",
			cfg:     csConfig{},
			payload: approveSet(),
			wants:   []string{"not one of the calls in any change set"},
		},
		{
			name:    "this deployment does not issue change-set approvals",
			cfg:     csConfig{set: fixtureChangeSet(), noGrants: true},
			payload: approveSet(),
			wants:   []string{"does not issue change-set approvals"},
		},
		{
			name: "the verdict edits the call",
			cfg:  csConfig{set: fixtureChangeSet()},
			payload: map[string]any{
				"verdict": "edit", "scope": "change_set", "approver": "user:sre-oncall",
				"args": map[string]any{"deployment": "api", "replicas": 2},
			},
			wants: []string{"speaks only for the call it edits"},
		},
		{
			name: "a call in the set is too large to read",
			cfg: csConfig{set: []ProposedChange{
				{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api", "replicas": float64(3)}},
				{Tool: "scale_deployment", Arguments: map[string]any{"deployment": strings.Repeat("x", 200), "replicas": float64(1)}},
			}},
			payload: approveSet(),
			wants:   []string{"too large to show in the approval question"},
		},
		{
			name: "a precondition declaration cannot be read",
			cfg: csConfig{
				set:             fixtureChangeSet(),
				preconditionErr: fmt.Errorf("the bundle names a read tool this daemon does not hold"),
			},
			payload: approveSet(),
			wants:   []string{"could not record what", "assumes about the cluster"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.scripts, cfg.turns = twoCallTurns(tt.payload, modelSays("mast refused the change set"))
			p := runChangeSetProbe(t, cfg)

			if len(p.executions) != 0 {
				t.Fatalf("tool executed %d time(s) on a refused change-set verdict, want 0: %+v", len(p.executions), p.executions)
			}
			if grants := p.grants(t); len(grants) != 0 {
				t.Fatalf("grant records = %+v, want none", grants)
			}
			resp := p.responses[len(p.responses)-1]
			wantField(t, resp, "error", "approval_scope_refused")
			wantDetailMentions(t, resp, append(tt.wants, "one at a time")...)
		})
	}
}

func TestPreconditionReadFailureAtMintRefusesTheSet(t *testing.T) {
	pre := map[string]*Precondition{"scale_deployment": {Read: "get_deployment"}}
	scripts, turns := twoCallTurns(approveSet(), modelSays("mast refused the change set"))
	turns[1] = csTurn{send: func(p *csProbe) *genai.Content {
		p.readErr = fmt.Errorf("connection refused")
		return answerLast(approveSet())(p)
	}}
	p := runChangeSetProbe(t, csConfig{set: fixtureChangeSet(), preconditions: pre, scripts: scripts, turns: turns})

	if len(p.executions) != 0 {
		t.Fatalf("tool executed %d time(s) when the precondition could not be snapshotted, want 0: %+v", len(p.executions), p.executions)
	}
	if grants := p.grants(t); len(grants) != 0 {
		t.Fatalf("grant records = %+v, want none", grants)
	}
	resp := p.responses[len(p.responses)-1]
	wantField(t, resp, "error", "approval_scope_refused")
	wantDetailMentions(t, resp, "connection refused", "one at a time")
}

func TestGrantDoesNotEscapeADenyPolicy(t *testing.T) {
	policy, err := permissions.NewPolicy(nil, []string{"scale_deployment:*deployment=worker*"})
	if err != nil {
		t.Fatal(err)
	}
	scripts, turns := twoCallTurns(approveSet(), scaleCall("worker", 1))
	p := runChangeSetProbe(t, csConfig{
		set:         fixtureChangeSet(),
		gateOptions: permissions.Options{Mode: permissions.ModeYolo, Policy: policy},
		scripts:     scripts,
		turns:       turns,
	})

	if p.executed("worker", 1) {
		t.Fatalf("a granted call ran into denied territory: %+v", p.executions)
	}
	if !p.executed("api", 3) {
		t.Fatalf("the approved call did not run: %+v", p.executions)
	}
	last := p.responses[len(p.responses)-1]
	wantField(t, last, "error", "denied_by_policy")
	if grants := p.grants(t); len(grants) != 1 || grants[0].ConsumedBy != "" {
		t.Errorf("grant records = %+v, want the grant unspent", grants)
	}
}

func TestParkedQuestionCarriesTheSet(t *testing.T) {
	pre := map[string]*Precondition{"scale_deployment": {Read: "get_deployment"}}
	scripts := [][]*model.LLMResponse{{scaleCall("api", 3)}}
	turns := []csTurn{{send: userSays("remediate")}}
	p := runChangeSetProbe(t, csConfig{
		set: fixtureChangeSet(), preconditions: pre, ttl: 15 * time.Minute,
		scripts: scripts, turns: turns,
	})

	if len(p.requests) != 1 {
		t.Fatalf("parked requests = %d, want 1", len(p.requests))
	}
	set := p.requests[0].ChangeSet
	if set == nil {
		t.Fatal("the parked question carries no change set")
	}
	if set.Specialist != "diagnoser" || len(set.Changes) != 2 {
		t.Errorf("change set = %+v, want the diagnoser's two calls", set)
	}
	if !set.Grantable || set.Ungrantable != "" {
		t.Errorf("change set = %+v, want it grantable", set)
	}
	if set.TTLSeconds != 900 {
		t.Errorf("TTLSeconds = %d, want 900", set.TTLSeconds)
	}
	if got := set.Preconditions["scale_deployment"]; !strings.Contains(got, "get_deployment") {
		t.Errorf("precondition description = %q, want it to name the read", got)
	}
	if !strings.Contains(p.hints[0], "scope=change_set") || !strings.Contains(p.hints[0], "2") {
		t.Errorf("hint = %q, want it to offer the whole set", p.hints[0])
	}
}

func TestUngrantableSetSaysSoInTheQuestion(t *testing.T) {
	big := []ProposedChange{
		{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api", "replicas": float64(3)}},
		{Tool: "scale_deployment", Arguments: map[string]any{"deployment": strings.Repeat("x", 200), "replicas": float64(1)}},
	}
	scripts := [][]*model.LLMResponse{{scaleCall("api", 3)}}
	p := runChangeSetProbe(t, csConfig{set: big, scripts: scripts, turns: []csTurn{{send: userSays("remediate")}}})

	set := p.requests[0].ChangeSet
	if set == nil {
		t.Fatal("no change set in the parked question")
	}
	if set.Grantable {
		t.Errorf("change set = %+v, want it marked ungrantable", set)
	}
	if !strings.Contains(set.Ungrantable, "too large") {
		t.Errorf("Ungrantable = %q, want the reason", set.Ungrantable)
	}
	if !strings.Contains(p.hints[0], "one at a time") {
		t.Errorf("hint = %q, want it to say the calls must be approved one at a time", p.hints[0])
	}
}

func TestNoChangeSetLeavesTheQuestionUnchanged(t *testing.T) {
	scripts := [][]*model.LLMResponse{{scaleCall("api", 3)}}
	p := runChangeSetProbe(t, csConfig{scripts: scripts, turns: []csTurn{{send: userSays("remediate")}}})

	if p.requests[0].ChangeSet != nil {
		t.Errorf("request.ChangeSet = %+v, want nil", p.requests[0].ChangeSet)
	}
	if want := "Approve mutating call scale_deployment(deployment=api, replicas=3)?"; p.hints[0] != want {
		t.Errorf("hint = %q, want %q", p.hints[0], want)
	}
}

func TestLegible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		changes []ProposedChange
		wantErr string
	}{
		{
			name:    "ordinary named arguments",
			changes: fixtureChangeSet(),
		},
		{
			name: "a long string argument",
			changes: []ProposedChange{
				{Tool: "apply_manifest", Arguments: map[string]any{"manifest": strings.Repeat("y", 200)}},
			},
			wantErr: `calls apply_manifest with a "manifest" argument too large`,
		},
		{
			name: "a nested object that renders long",
			changes: []ProposedChange{
				{Tool: "patch", Arguments: map[string]any{"patch": map[string]any{"spec": strings.Repeat("z", 200)}}},
			},
			wantErr: "too large",
		},
		{
			name: "exactly at the limit is still legible",
			changes: []ProposedChange{
				{Tool: "apply_manifest", Arguments: map[string]any{"manifest": strings.Repeat("y", maxValueLen)}},
			},
		},
		{
			name:    "the second entry is the offender",
			changes: []ProposedChange{{Tool: "a", Arguments: map[string]any{"x": 1}}, {Tool: "b", Arguments: map[string]any{"y": strings.Repeat("q", 300)}}},
			wantErr: "[1] calls b",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := Legible(tt.changes)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Legible = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Legible = nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestDigestResultIsStableAcrossMapOrder(t *testing.T) {
	t.Parallel()
	result := map[string]any{"a": 1, "b": 2, "c": map[string]any{"d": 3, "e": 4}, "f": 5, "g": 6, "h": 7}
	first, _, err := digestResult(result, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		got, _, err := digestResult(result, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got != first {
			t.Fatalf("digest render %d = %q, want %q", i, got, first)
		}
	}
}

func TestDigestResultFields(t *testing.T) {
	t.Parallel()
	result := map[string]any{"spec": map[string]any{"replicas": float64(3)}, "status": "Running"}

	_, fields, err := digestResult(result, []string{"spec.replicas", "status"})
	if err != nil {
		t.Fatalf("digestResult: %v", err)
	}
	if fields["spec.replicas"] != "3" || fields["status"] != `"Running"` {
		t.Errorf("fields = %v, want the rendered values at the declared paths", fields)
	}

	if _, _, err := digestResult(result, []string{"spec.image"}); err == nil {
		t.Error("digestResult accepted a declared field the result does not carry")
	}
	if _, _, err := digestResult(result, []string{"status.phase"}); err == nil {
		t.Error("digestResult walked into a non-object and called it a match")
	}
}

func TestPreconditionReadArgs(t *testing.T) {
	t.Parallel()
	ch := ProposedChange{Tool: "scale_deployment", Arguments: map[string]any{"deployment": "api", "namespace": "prod", "replicas": float64(5)}}

	p := Precondition{
		Read:     "get_deployment",
		Args:     map[string]any{"output": "json"},
		ArgsFrom: map[string]string{"name": "deployment", "namespace": "namespace"},
	}
	args, err := p.readArgs(ch)
	if err != nil {
		t.Fatalf("readArgs: %v", err)
	}
	if args["name"] != "api" || args["namespace"] != "prod" || args["output"] != "json" {
		t.Errorf("readArgs = %v, want the change's own arguments mapped onto the read's", args)
	}

	missing := Precondition{Read: "get_deployment", ArgsFrom: map[string]string{"name": "workload"}}
	if _, err := missing.readArgs(ch); err == nil {
		t.Error("readArgs accepted a declaration naming an argument the call does not carry")
	}

	if _, err := (Precondition{}).readArgs(ch); err == nil {
		t.Error("readArgs accepted a precondition with no read tool")
	}
}

func TestGrantSpent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		grant Grant
		id    string
		want  bool
	}{
		{"live", Grant{}, "call-1", false},
		{"voided", Grant{VoidedBy: "the cluster moved"}, "call-1", true},
		{"consumed by another call", Grant{ConsumedBy: "call-0"}, "call-1", true},
		{"consumed by this call", Grant{ConsumedBy: "call-1"}, "call-1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.grant.Spent(tt.id); got != tt.want {
				t.Errorf("Spent(%q) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestGrantRoundTrip(t *testing.T) {
	t.Parallel()
	want := Grant{
		Signature: `scale_deployment({"deployment":"api","replicas":3})`,
		Tool:      "scale_deployment",
		Arguments: map[string]any{"deployment": "api", "replicas": float64(3)},
		Origin:    "scale_deployment(deployment=worker, replicas=1)",
		Approver:  "user:sre-oncall",
		MintedAt:  time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC),
		ExpiresAt: time.Date(2026, 8, 15, 9, 10, 0, 0, time.UTC),
		Precondition: &PreconditionSnapshot{
			Read: "get_deployment", Args: map[string]any{"name": "api"},
			Digest: "abc123", Fields: map[string]string{"replicas": "3"},
		},
	}
	raw, err := EncodeGrant(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeGrant(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Signature != want.Signature || got.Approver != want.Approver ||
		!got.ExpiresAt.Equal(want.ExpiresAt) || got.Precondition == nil ||
		got.Precondition.Digest != "abc123" || got.Precondition.Fields["replicas"] != "3" {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if _, err := DecodeGrant(nil); err == nil {
		t.Error("DecodeGrant(nil) accepted an absent record")
	}
	if _, err := DecodeGrant("not json"); err == nil {
		t.Error("DecodeGrant accepted a value that is not a grant")
	}
}

func TestGrantStateKeyIsPerSignature(t *testing.T) {
	t.Parallel()
	a, _ := Signature("scale_deployment", map[string]any{"deployment": "api", "replicas": 3})
	b, _ := Signature("scale_deployment", map[string]any{"deployment": "api", "replicas": 4})
	if GrantStateKey(a) == GrantStateKey(b) {
		t.Fatal("two different calls share a grant key")
	}
	if !strings.HasPrefix(GrantStateKey(a), GrantStateKeyPrefix) {
		t.Errorf("GrantStateKey = %q, want the %q namespace", GrantStateKey(a), GrantStateKeyPrefix)
	}
	reordered, _ := Signature("scale_deployment", map[string]any{"replicas": 3, "deployment": "api"})
	if GrantStateKey(reordered) != GrantStateKey(a) {
		t.Error("the same call with its arguments in another order gets a different grant key")
	}
}

func TestDefaultGrantTTLApplies(t *testing.T) {
	t.Parallel()
	var f *Freshness
	if got := f.ttl(); got != DefaultGrantTTL {
		t.Errorf("nil Freshness ttl = %v, want %v", got, DefaultGrantTTL)
	}
	if got := (&Freshness{}).ttl(); got != DefaultGrantTTL {
		t.Errorf("zero TTL = %v, want %v", got, DefaultGrantTTL)
	}
	if got := (&Freshness{TTL: time.Minute}).ttl(); got != time.Minute {
		t.Errorf("TTL = %v, want a minute", got)
	}
}

func TestVerifyWithoutAReadDoesNotSayNothingChanged(t *testing.T) {
	t.Parallel()
	snap := &PreconditionSnapshot{Read: "get_deployment", Digest: "abc"}
	if got := (&Freshness{}).verify(nil, snap, "scale_deployment"); got == "" {
		t.Error("verify returned \"nothing changed\" with no way to read the cluster")
	}
	if got := (&Freshness{}).verify(nil, nil, "scale_deployment"); got != "" {
		t.Errorf("verify with no precondition = %q, want it to pass", got)
	}
}

func TestScopeChangeSetIsAdmissible(t *testing.T) {
	t.Parallel()
	d, err := Verdict{Verdict: OutcomeApprove, Scope: ScopeChangeSet}.Decision()
	if err != nil {
		t.Fatalf("Decision: %v", err)
	}
	if d != permissions.DecisionAllowOnce {
		t.Errorf("Decision = %v, want allow-once", d)
	}
	enum := VerdictSchema().Properties["scope"].Enum
	found := false
	for _, v := range enum {
		if v == string(ScopeChangeSet) {
			found = true
		}
	}
	if !found {
		t.Errorf("scope enum = %v, want it to offer %q", enum, ScopeChangeSet)
	}
}
