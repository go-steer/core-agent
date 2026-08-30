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

package background

import (
	"context"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// The regression tests for #825.
//
// A declarative subagent's tools are resolved ONCE at daemon startup
// against the template gate and shared across every session
// (SubagentTemplate.Tools). #825 read that as "a tenant's declarative
// subagent asks the daemon's gate", which would make plan-first
// unsatisfiable per session and route ask-mode prompts to a broker
// nobody is subscribed to.
//
// It does not, and these tests are what says so. A tool captures its
// gate at construction, but every Gate.Check* entry point begins with
// resolveSessionGate(ctx), and agent.Run stamps the per-session
// sub-gate onto the turn context (agent.go, WithSessionGate). Spawn
// carries that context into the goroutine via context.WithoutCancel,
// which drops cancellation and keeps values — so the stamp is still
// there when the subagent's tool asks.
//
// The long, easy-to-misread link is that chain, so it is what is
// tested: the probe tool holds the TEMPLATE gate, exactly as a
// startup-resolved built-in does, and the assertions are about which
// gate's per-session state decided the call.

// gateProbe is a tool that asks a gate it captured at construction
// whether it may run, and records the answer. It stands in for any
// startup-resolved built-in in a SubagentTemplate.Tools slice.
type gateProbe struct {
	mu    sync.Mutex
	calls int
	errs  []error
}

func (p *gateProbe) record(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.errs = append(p.errs, err)
}

// outcome returns the answer the gate gave the first call, and whether
// the tool ran at all.
func (p *gateProbe) outcome(t *testing.T) (err error, ran bool) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls == 0 {
		return nil, false
	}
	return p.errs[0], true
}

const probeToolName = "probe_tool"

// newGateProbe builds the tool against g — the gate a startup-resolved
// built-in would have closed over.
func newGateProbe(t *testing.T, g *permissions.Gate) (*gateProbe, tool.Tool) {
	t.Helper()
	p := &gateProbe{}
	type empty struct{}
	tl, err := functiontool.New(
		functiontool.Config{Name: probeToolName, Description: "probe the gate"},
		func(ctx tool.Context, _ empty) (empty, error) {
			err := g.CheckGeneric(ctx, probeToolName, "probe")
			p.record(err)
			return empty{}, err
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}
	return p, tl
}

// probingLLM calls the probe once, then finishes so the run can end.
type probingLLM struct {
	calls atomic.Int32
}

func (*probingLLM) Name() string { return "prober" }

func (l *probingLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	n := l.calls.Add(1)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		var content *genai.Content
		if n == 1 {
			fc := &genai.FunctionCall{Name: probeToolName, Args: map[string]any{}}
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		} else {
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "probed"}}}
		}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// spawnDeclarativeProbe wires the daemon shape #825 describes: a
// template whose tool was built against templateGate, registered on a
// manager wired to sessionGate, spawned on a turn context carrying
// sessionGate the way agent.Run stamps it.
func spawnDeclarativeProbe(t *testing.T, templateGate, sessionGate *permissions.Gate) *gateProbe {
	t.Helper()
	probe, probeTool := newGateProbe(t, templateGate)
	prov := &recordingProvider{llm: &probingLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		Tools:        []tool.Tool{probeTool},
	}}, WithGate(sessionGate), WithDefaultBudgets(Budgets{MaxTurns: 2}))
	attachEchoParent(t, mgr)
	t.Cleanup(func() { _ = mgr.Close() })

	ctx := permissions.WithSessionGate(context.Background(), sessionGate)
	h, err := mgr.SpawnTemplate(ctx, "", "cluster", RefOverrides{Goal: "triage prod"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h)
	return probe
}

// TestDeclarativeSubagent_PlanFirstFollowsTheSessionGate is #825's
// headline symptom, inverted. The template gate has no plan recorded
// and never will: record_plan in a tenant session marks THAT session's
// sub-gate. If the subagent's startup-resolved tool asked the template
// gate, plan-first would be permanently unsatisfiable for every tenant.
func TestDeclarativeSubagent_PlanFirstFollowsTheSessionGate(t *testing.T) {
	t.Parallel()
	templateGate := permissions.New(permissions.Options{
		Mode:                permissions.ModeYolo,
		RequirePlanArtifact: true,
	})
	sessionGate := templateGate.DeriveForSession("alice", nil)
	sessionGate.MarkPlanRecorded()

	// Control: the template gate really would refuse, so a passing
	// probe below means "the session gate answered" and not "plan-first
	// is inert in this configuration".
	if err := templateGate.CheckGeneric(context.Background(), probeToolName, "probe"); err == nil {
		t.Fatal("template gate allowed a gated tool with no plan recorded; the rest of this test proves nothing")
	}

	probe := spawnDeclarativeProbe(t, templateGate, sessionGate)
	err, ran := probe.outcome(t)
	if !ran {
		t.Fatal("the subagent never reached its tool")
	}
	if err != nil {
		t.Fatalf("declarative subagent tool call = %v, want allowed — the tenant recorded a plan "+
			"on its own sub-gate, and that is the gate the call has to consult", err)
	}
}

// TestDeclarativeSubagent_SessionWithoutAPlanIsStillRefused is the
// mirror #825 calls "worse": a plan recorded by the daemon's own
// primary agent must not unblock tenants who planned nothing.
func TestDeclarativeSubagent_SessionWithoutAPlanIsStillRefused(t *testing.T) {
	t.Parallel()
	templateGate := permissions.New(permissions.Options{
		Mode:                permissions.ModeYolo,
		RequirePlanArtifact: true,
	})
	sessionGate := templateGate.DeriveForSession("bob", nil)
	// The daemon's primary agent plans; bob does not.
	templateGate.MarkPlanRecorded()

	probe := spawnDeclarativeProbe(t, templateGate, sessionGate)
	err, ran := probe.outcome(t)
	if !ran {
		t.Fatal("the subagent never reached its tool")
	}
	if err == nil {
		t.Fatal("a session that recorded no plan ran a gated tool: the daemon's plan leaked across tenants")
	}
}

// TestDeclarativeSubagent_ApprovalsRouteToTheSessionPrompter covers
// #825's symptoms 2-4 in one pass: the prompt has to reach the tenant's
// prompter (the attach broker), not the daemon's — a template gate in a
// daemon has none, so asking it fails with ErrNoPrompter — and the
// session grant that results has to land on the tenant's sub-gate.
func TestDeclarativeSubagent_ApprovalsRouteToTheSessionPrompter(t *testing.T) {
	t.Parallel()
	// No prompter on the template gate: the daemon's startup gate in a
	// non-TTY process, which is the deployment #825 is about.
	templateGate := permissions.New(permissions.Options{Mode: permissions.ModeAsk})
	p := &countingPrompter{}
	sessionGate := templateGate.DeriveForSession("carol", p)

	probe := spawnDeclarativeProbe(t, templateGate, sessionGate)
	err, ran := probe.outcome(t)
	if !ran {
		t.Fatal("the subagent never reached its tool")
	}
	if err != nil {
		t.Fatalf("ask-mode tool call = %v, want the session prompter's approval", err)
	}
	if len(p.calls) != 1 {
		t.Fatalf("session prompter asked %d time(s), want 1 — the prompt went to a broker "+
			"the tenant is not subscribed to", len(p.calls))
	}
	if got := p.calls[0].ToolName; got != probeToolName {
		t.Errorf("prompt tool = %q, want %q", got, probeToolName)
	}
}

// TestDeclarativeSubagent_SessionGateSurvivesTheSpawnGoroutine pins the
// mechanism the three tests above depend on, so a regression names its
// own cause instead of surfacing as three unrelated permission bugs.
// Spawn detaches the run onto a goroutine with context.WithoutCancel,
// which keeps values; if that ever became context.Background() the
// stamp would vanish and every symptom in #825 would become real.
func TestDeclarativeSubagent_SessionGateSurvivesTheSpawnGoroutine(t *testing.T) {
	t.Parallel()
	templateGate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	sessionGate := templateGate.DeriveForSession("dave", nil)

	seen := make(chan *permissions.Gate, 1)
	type empty struct{}
	tl, err := functiontool.New(
		functiontool.Config{Name: probeToolName, Description: "report the session gate on ctx"},
		func(ctx tool.Context, _ empty) (empty, error) {
			g, _ := permissions.SessionGateFromContext(ctx)
			select {
			case seen <- g:
			default:
			}
			return empty{}, nil
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	prov := &recordingProvider{llm: &probingLLM{}}
	mgr := newTemplateManager(t, prov, []SubagentTemplate{{
		Name:         "cluster",
		Instruction:  "triage",
		ModelFactory: tmplFactory(prov, "cluster-model"),
		ModelID:      "cluster-model",
		Tools:        []tool.Tool{tl},
	}}, WithGate(sessionGate), WithDefaultBudgets(Budgets{MaxTurns: 2}))
	attachEchoParent(t, mgr)
	defer func() { _ = mgr.Close() }()

	ctx := permissions.WithSessionGate(context.Background(), sessionGate)
	h, err := mgr.SpawnTemplate(ctx, "", "cluster", RefOverrides{Goal: "g"}, "")
	if err != nil {
		t.Fatalf("SpawnTemplate: %v", err)
	}
	waitDone(t, h)

	select {
	case got := <-seen:
		if got != sessionGate {
			t.Fatalf("session gate on the subagent's tool context = %p, want the spawning "+
				"session's sub-gate %p", got, sessionGate)
		}
	case <-time.After(time.Second):
		t.Fatal("the subagent never reached its tool")
	}
}
