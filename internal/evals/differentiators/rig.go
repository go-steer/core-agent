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

package differentiators

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"google.golang.org/genai"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	adksession "google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/database"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/adk/v2/tool/toolconfirmation"

	"github.com/go-steer/core-agent/v3/internal/effects"
	"github.com/go-steer/core-agent/v3/internal/evals"
	"github.com/go-steer/core-agent/v3/internal/evals/evalcompose"
	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/approval"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

// The rig runs against a real SQLite session store, the same ADK
// database service core-agent and the library root use. Scenarios about
// interrupted turns and replayed effects are exactly the ones the
// in-memory service cannot express.
const (
	appName  = "mast"
	userID   = "mast-evals"
	rigModel = "differentiator-script"
)

// Tool names. The read side is a real lookout intent from the parity
// corpus (testdata/evals/intents.yaml), so a differentiator's trace
// scores through the same intent table the 31-row corpus does. The
// write side is the change-executor shape W2.4 splits the roster on.
const (
	toolTriage  = "k8s_triage_workload"
	toolScale   = "scale_deployment"
	toolRestart = "rollout_restart"
)

// specialistName is the roster's one Task specialist. It must not
// collide with a mutating tool name — effects.CheckNameCollisions
// refuses that at construction on every real path, and the rig runs
// the same check so a fixture cannot drift into the fail-open hole.
const specialistName = "remediator"

// operator is the human in the loop.
type operator struct {
	identity string
	decide   func(req approval.Request) approval.Verdict

	mu       sync.Mutex
	consults int
}

func (o *operator) answer(req approval.Request) approval.Verdict {
	o.mu.Lock()
	o.consults++
	o.mu.Unlock()
	v := o.decide(req)
	v.Approver = o.identity
	return v
}

func (o *operator) consulted() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.consults
}

type specialistSpec struct {
	Name        string
	Description string
	Instruction string
	MaxCostUSD  float64
}

type budgetLimits = evalcompose.Limits

// rigConfig is what a scenario declares about its fixture.
type rigConfig struct {
	dir           string
	specs         []specialistSpec
	limits        budgetLimits
	tokensPerCall int32
	steps         []step
	onMutation    approval.OnMutation
	op            *operator
}

// rig is one composed runtime plus the counters a scenario checks.
type rig struct {
	cfg    rigConfig
	svc    adksession.Service
	handle *eventlog.Handle
	runner *runner.Runner
	model  *scriptModel
	meter  evalcompose.BudgetConfig
	pred   effects.Predicate
	subs   map[string]bool

	mu        sync.Mutex
	mutations []executedCall
	reads     int
}

type executedCall struct {
	Name string
	Args map[string]any
}

func newRig(ctx context.Context, cfg rigConfig) (*rig, error) {
	if cfg.dir == "" {
		return nil, fmt.Errorf("rig: no scratch dir")
	}
	if cfg.tokensPerCall == 0 {
		cfg.tokensPerCall = 15
	}
	r := &rig{cfg: cfg, model: &scriptModel{steps: cfg.steps, tokens: cfg.tokensPerCall}}
	r.meter = evalcompose.BudgetConfig{Limits: cfg.limits}

	svc, err := database.NewSessionService(
		sqlite.Open(filepath.Join(cfg.dir, "sessions.db")),
		&gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)},
	)
	if err != nil {
		return nil, fmt.Errorf("rig: open session store: %w", err)
	}
	if err := database.AutoMigrate(svc); err != nil {
		return nil, fmt.Errorf("rig: migrate session store: %w", err)
	}
	r.svc = svc
	r.handle = &eventlog.Handle{Service: svc}

	tools, err := r.buildTools()
	if err != nil {
		return nil, err
	}

	specs := cfg.specs
	if len(specs) == 0 {
		specs = []specialistSpec{{
			Name:        specialistName,
			Description: "remediates a workload",
			Instruction: "Triage the incident and remediate it.",
		}}
	}
	composeSpecs := make([]evalcompose.Spec, 0, len(specs))
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
		composeSpecs = append(composeSpecs, evalcompose.Spec{
			Name:        s.Name,
			Description: s.Description,
			Mode:        evalcompose.ModeTask,
			Instruction: s.Instruction,
			Budget:      evalcompose.SpecialistBudget{MaxCostUSD: s.MaxCostUSD},
		})
	}
	r.meter.Scopes = evalcompose.MeterScopes(composeSpecs, "", rigModel)

	root, _, err := evalcompose.BuildRoot(ctx, evalcompose.RootConfig{
		Bundle: evalcompose.Bundle{
			Name:        "differentiators",
			Description: "v0.3 differentiator eval fixture",
			Specialists: names,
		},
		Specs:     composeSpecs,
		Model:     r.model,
		ModelName: rigModel,
		Toolsets:  []tool.Toolset{&staticToolset{name: "differentiators", tools: tools}},
		Dispatch:  evalcompose.DispatchCoordinator,
	})
	if err != nil {
		return nil, fmt.Errorf("rig: compose root: %w", err)
	}

	policies := []effects.ToolPolicy{
		{Name: toolTriage, Mutating: boolPtr(false)},
	}
	r.pred = effects.NewPredicate(effects.Overrides(nil, policies))
	r.subs = effects.SubAgentNames(root)
	if hits := effects.CheckNameCollisions(r.subs, r.pred, policies); len(hits) > 0 {
		return nil, fmt.Errorf("rig: fixture names both a sub-agent and a mutating tool: %s", strings.Join(hits, ", "))
	}
	outbox, err := effects.New(effects.Config{
		Predicate:     r.pred,
		SubAgentNames: r.subs,
		AckedAt: func(ctx context.Context, sid string) (time.Time, bool) {
			return r.handle.EffectsAckedAt(ctx, appName, userID, sid)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("rig: construct effects outbox: %w", err)
	}

	plugins := []*plugin.Plugin{outbox}
	policy := onMutationOr(cfg.onMutation, approval.OnMutationApply)
	gate := permissions.New(permissions.Options{})
	gate.SetMutatingMode(permissions.MutatingMode(policy))
	writeGate, err := approval.New(approval.Config{
		Policy:   policy,
		Mutating: func(name string) bool { return r.pred(name) == effects.ClassMutating },
		Gate:     gate,
		Workload: "differentiators",
	})
	if err != nil {
		return nil, fmt.Errorf("rig: %w", err)
	}
	if writeGate != nil {
		plugins = append(plugins, writeGate)
	}
	r.runner, err = runner.New(runner.Config{
		AppName:           appName,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: plugins},
	})
	if err != nil {
		return nil, fmt.Errorf("rig: construct runner: %w", err)
	}
	return r, nil
}

func (r *rig) buildTools() ([]tool.Tool, error) {
	triage, err := functiontool.New(functiontool.Config{
		Name:        toolTriage,
		Description: "Read-only workload triage snapshot.",
	}, func(_ adkagent.Context, args triageArgs) (map[string]any, error) {
		r.mu.Lock()
		r.reads++
		r.mu.Unlock()
		return map[string]any{
			"workload": args.Workload,
			"severity": "CRITICAL",
			"finding":  "container OOMKilled 4 times in 10m; memory limit 128Mi",
		}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("rig: build %s: %w", toolTriage, err)
	}
	scale, err := functiontool.New(functiontool.Config{
		Name:        toolScale,
		Description: "Scale a deployment. Mutating.",
	}, func(_ adkagent.Context, args scaleArgs) (map[string]any, error) {
		r.record(toolScale, map[string]any{"deployment": args.Deployment, "replicas": args.Replicas})
		return map[string]any{"deployment": args.Deployment, "replicas": args.Replicas}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("rig: build %s: %w", toolScale, err)
	}
	restart, err := functiontool.New(functiontool.Config{
		Name:        toolRestart,
		Description: "Restart a deployment's pods. Mutating.",
	}, func(_ adkagent.Context, args restartArgs) (map[string]any, error) {
		r.record(toolRestart, map[string]any{"deployment": args.Deployment})
		return map[string]any{"restarted": args.Deployment}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("rig: build %s: %w", toolRestart, err)
	}
	return []tool.Tool{triage, scale, restart}, nil
}

type triageArgs struct {
	Workload string `json:"workload"`
}

type scaleArgs struct {
	Deployment string `json:"deployment"`
	Replicas   int    `json:"replicas"`
}

type restartArgs struct {
	Deployment string `json:"deployment"`
}

func (r *rig) record(name string, args map[string]any) {
	r.mu.Lock()
	r.mutations = append(r.mutations, executedCall{Name: name, Args: args})
	r.mu.Unlock()
}

func (r *rig) executed() []executedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]executedCall, len(r.mutations))
	copy(out, r.mutations)
	return out
}

func (r *rig) readCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

func (r *rig) turn(ctx context.Context, sessionID, text string) (stopped error, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.model.beginTurn()
	meter := evalcompose.NewMeter(r.meter)
	ctx = agent.WithCallGate(ctx, meter)
	msg := genai.NewContentFromText(text, genai.RoleUser)
	for ev, rerr := range r.runner.Run(ctx, userID, sessionID, msg, adkagent.RunConfig{
		StreamingMode: adkagent.StreamingModeNone,
	}) {
		if rerr != nil {
			return stopped, fmt.Errorf("rig: run turn: %w", rerr)
		}
		if berr := meter.Observe(ev); berr != nil {
			cancel()
			return berr, nil
		}
	}
	return nil, nil
}

func (r *rig) answerParks(ctx context.Context, sessionID string) error {
	const maxRounds = 8
	for round := 0; round < maxRounds; round++ {
		sessions, err := r.handle.ListParks(ctx, appName, userID, sessionID)
		if err != nil {
			return fmt.Errorf("rig: read parks: %w", err)
		}
		var parked *eventlog.ParkedCall
		for _, s := range sessions {
			for i := range s.Pending {
				if s.Pending[i].ToolName == toolconfirmation.FunctionCallName {
					parked = &s.Pending[i]
					break
				}
			}
		}
		if parked == nil {
			return nil
		}
		if r.cfg.op == nil {
			return fmt.Errorf("rig: session %q parked a mutating call but the fixture offers no operator", sessionID)
		}
		req, err := approval.DecodeRequest(parked.Payload)
		if err != nil {
			return fmt.Errorf("rig: parked call %q: %w", parked.InterruptID, err)
		}
		v := r.cfg.op.answer(req)
		part := genai.NewPartFromFunctionResponse(
			toolconfirmation.FunctionCallName, approval.ConfirmationResponse(v))
		part.FunctionResponse.ID = parked.InterruptID
		r.model.beginTurn()
		msg := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{part}}
		for _, rerr := range r.runner.Run(ctx, userID, sessionID, msg, adkagent.RunConfig{
			StreamingMode: adkagent.StreamingModeNone,
		}) {
			if rerr != nil {
				return fmt.Errorf("rig: resume turn: %w", rerr)
			}
		}
	}
	return fmt.Errorf("rig: session %q still parked after %d resume rounds", sessionID, maxRounds)
}

func (r *rig) appliedEdits(ctx context.Context, sessionID string) ([]approval.AppliedEdit, error) {
	resp, err := r.svc.Get(ctx, &adksession.GetRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("rig: read applied edits: %w", err)
	}
	var out []approval.AppliedEdit
	for ev := range resp.Session.Events().All() {
		if ev == nil || len(ev.Actions.StateDelta) == 0 {
			continue
		}
		keys := make([]string, 0, len(ev.Actions.StateDelta))
		for k := range ev.Actions.StateDelta {
			if strings.HasPrefix(k, approval.EditStateKeyPrefix) {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			e, derr := approval.DecodeAppliedEdit(ev.Actions.StateDelta[k])
			if derr == nil {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

func (r *rig) exportDecisions(ctx context.Context, sessionID string) (eventlog.ExportMeta, []approval.Decision, error) {
	var buf bytes.Buffer
	if _, err := r.handle.ExportDecisions(ctx, &buf, eventlog.ExportOptions{
		AppName:   appName,
		UserID:    userID,
		SessionID: sessionID,
		Source:    filepath.Join(r.cfg.dir, "sessions.db"),
	}); err != nil {
		return eventlog.ExportMeta{}, nil, fmt.Errorf("rig: export decisions: %w", err)
	}
	var header struct {
		Meta eventlog.ExportMeta `json:"_meta"`
	}
	var rows []approval.Decision
	sc := bufio.NewScanner(&buf)
	for line := 0; sc.Scan(); line++ {
		if line == 0 {
			if err := json.Unmarshal(sc.Bytes(), &header); err != nil {
				return eventlog.ExportMeta{}, nil, fmt.Errorf("rig: export header: %w", err)
			}
			if header.Meta.Schema == "" {
				return eventlog.ExportMeta{}, nil, fmt.Errorf("rig: export line 1 carries no _meta object: %s", sc.Text())
			}
			continue
		}
		var d approval.Decision
		if err := json.Unmarshal(sc.Bytes(), &d); err != nil {
			return eventlog.ExportMeta{}, nil, fmt.Errorf("rig: export row %d: %w", line, err)
		}
		rows = append(rows, d)
	}
	if err := sc.Err(); err != nil {
		return eventlog.ExportMeta{}, nil, fmt.Errorf("rig: read export: %w", err)
	}
	if header.Meta.Records != len(rows) {
		return eventlog.ExportMeta{}, nil, fmt.Errorf("rig: export header promises %d rows but the file holds %d",
			header.Meta.Records, len(rows))
	}
	return header.Meta, rows, nil
}

func (r *rig) roleCalls(who role) int { return r.model.roleCalls(who) }

func (r *rig) operatorConsults() int {
	if r.cfg.op == nil {
		return 0
	}
	return r.cfg.op.consulted()
}

func (r *rig) trace(ctx context.Context, sessionID string) (evals.Trace, error) {
	resp, err := r.svc.Get(ctx, &adksession.GetRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		return evals.Trace{}, fmt.Errorf("rig: read session %q: %w", sessionID, err)
	}
	return evals.TraceFromEvents(resp.Session.Events(), r.pred, r.subs), nil
}

func (r *rig) seedDangling(ctx context.Context, sessionID, toolName, callID string) error {
	created, err := r.svc.Create(ctx, &adksession.CreateRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		return fmt.Errorf("rig: create session %q: %w", sessionID, err)
	}
	ev := adksession.NewEvent(ctx, "interrupted-invocation")
	ev.Author = specialistName
	part := genai.NewPartFromFunctionCall(toolName, map[string]any{"deployment": "api", "replicas": 3})
	part.FunctionCall.ID = callID
	ev.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}}
	if err := r.svc.AppendEvent(ctx, created.Session, ev); err != nil {
		return fmt.Errorf("rig: seed dangling intent: %w", err)
	}
	time.Sleep(5 * time.Millisecond)
	return nil
}

func (r *rig) toolResponses(ctx context.Context, sessionID, toolName string) ([]map[string]any, error) {
	resp, err := r.svc.Get(ctx, &adksession.GetRequest{
		AppName: appName, UserID: userID, SessionID: sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("rig: read session %q: %w", sessionID, err)
	}
	var out []map[string]any
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Name == toolName {
				out = append(out, p.FunctionResponse.Response)
			}
		}
	}
	return out, nil
}

type staticToolset struct {
	name  string
	tools []tool.Tool
}

func (s *staticToolset) Name() string { return s.name }

func (s *staticToolset) Tools(adkagent.ReadonlyContext) ([]tool.Tool, error) { return s.tools, nil }

type role int

const (
	anyRole role = iota
	coordinatorRole
	specialistRole
)

type step struct {
	turn int
	role role
	resp *model.LLMResponse
}

func specialistStep(resp *model.LLMResponse) step {
	return step{turn: 1, role: specialistRole, resp: resp}
}

func onTurn(n int, s step) step {
	s.turn = n
	return s
}

type scriptModel struct {
	steps  []step
	tokens int32

	mu     sync.Mutex
	turnNo int
	used   []bool
	calls  int
	byRole map[role]int
}

func (m *scriptModel) Name() string { return rigModel }

func (m *scriptModel) roleCalls(who role) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byRole[who]
}

func (m *scriptModel) beginTurn() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turnNo++
}

func (m *scriptModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		who := roleOf(req)
		m.mu.Lock()
		m.calls++
		if m.byRole == nil {
			m.byRole = map[role]int{}
		}
		m.byRole[who]++
		if m.used == nil {
			m.used = make([]bool, len(m.steps))
		}
		var resp *model.LLMResponse
		for i, s := range m.steps {
			if m.used[i] || s.turn != m.turnNo {
				continue
			}
			if s.role != anyRole && s.role != who {
				continue
			}
			m.used[i] = true
			resp = s.resp
			break
		}
		m.mu.Unlock()

		if resp == nil {
			resp = defaultReply(who)
		}
		out := *resp
		out.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     m.tokens,
			CandidatesTokenCount: 0,
			TotalTokenCount:      m.tokens,
		}
		out.TurnComplete = true
		out.FinishReason = genai.FinishReasonStop
		yield(&out, nil)
	}
}

func defaultReply(who role) *model.LLMResponse {
	if who == specialistRole {
		return callTo("finish_task", map[string]any{"result": "remediation complete"})
	}
	return textOf("CRITICAL: workload api is OOMKilling; remediation reported above.")
}

func roleOf(req *model.LLMRequest) role {
	if req == nil || req.Config == nil {
		return anyRole
	}
	for _, t := range req.Config.Tools {
		if t == nil {
			continue
		}
		for _, d := range t.FunctionDeclarations {
			if d != nil && d.Name == "finish_task" {
				return specialistRole
			}
		}
	}
	return coordinatorRole
}

func callTo(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{Content: &genai.Content{
		Role:  genai.RoleModel,
		Parts: []*genai.Part{genai.NewPartFromFunctionCall(name, args)},
	}}
}

func callToWithID(name, id string, args map[string]any) *model.LLMResponse {
	resp := callTo(name, args)
	resp.Content.Parts[0].FunctionCall.ID = id
	return resp
}

func textOf(s string) *model.LLMResponse {
	return &model.LLMResponse{Content: genai.NewContentFromText(s, genai.RoleModel)}
}

func boolPtr(b bool) *bool { return &b }

func onMutationOr(v, fallback approval.OnMutation) approval.OnMutation {
	if v == "" {
		return fallback
	}
	return v
}

func scratch(root, id string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("differentiators: no scratch root (house rule #5: derive it from os.TempDir)")
	}
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("differentiators: scratch dir for %q: %w", id, err)
	}
	return dir, nil
}
