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
	"iter"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// One declarative subagent, many tenants (#741).
//
// A multi-session daemon builds its declarative subagents ONCE at
// startup and hands the same *Agent values to every session's
// WithSubagents. That is deliberate — a rooted subagent stands up its
// own MCP servers, so rebuilding one per session would multiply server
// processes by session count — and it is only safe because nothing
// session-shaped lives on the inner agent. These tests are what says
// so: they run two tenants through ONE inner agent and assert each
// delegation carried its own tenant's identity in.
//
// The three properties, and why each is load-bearing:
//
//   - The session gate. #741's own triage read this as the blocker,
//     on the theory that a shared inner would run every tenant's call
//     against the daemon template gate. It does not, for the same
//     reason the asynchronous door does not (#825): the inner's tools
//     resolve their gate from the CONTEXT, and NewSubagentTool drives
//     the inner ADK runner directly rather than through *Agent.Run, so
//     nothing overwrites the stamp the parent's turn put there.
//   - The session row. The delegated events have to land under the
//     delegating tenant's session, not under whichever tenant was
//     constructed last.
//   - Concurrency. Two tenants delegating at once is the normal state
//     of a daemon, not an edge case.

// innerProbe records what the shared inner agent's own tools see on the
// context of each delegation: whose sub-gate is in force, and which
// session row the run was derived into.
type innerProbe struct {
	mu       sync.Mutex
	gates    []*permissions.Gate
	sessions []string
}

func (p *innerProbe) record(g *permissions.Gate, sid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gates = append(p.gates, g)
	p.sessions = append(p.sessions, sid)
}

func (p *innerProbe) snapshot() ([]*permissions.Gate, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*permissions.Gate(nil), p.gates...), append([]string(nil), p.sessions...)
}

const innerProbeTool = "inner_probe"

// newInnerProbeTool builds a tool for the SHARED inner agent, closed
// over the daemon-wide template gate exactly as a startup-resolved
// built-in is. What it records is therefore the difference between the
// gate it was built with and the gate that actually decides.
func newInnerProbeTool(t *testing.T, p *innerProbe) tool.Tool {
	t.Helper()
	type args struct {
		Request string `json:"request"`
	}
	type empty struct{}
	tl, err := functiontool.New(
		functiontool.Config{Name: innerProbeTool, Description: "record the delegation's session identity"},
		func(ctx tool.Context, _ args) (empty, error) {
			g, _ := permissions.SessionGateFromContext(ctx)
			p.record(g, ctx.SessionID())
			return empty{}, nil
		},
	)
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}
	return tl
}

// probeThenAnswerLLM calls the probe once per RUN and then answers.
// Stateless on purpose: the inner agent's model is shared across every
// tenant, so a call counter would let the first tenant's delegation
// decide what the second one does.
type probeThenAnswerLLM struct{}

func (probeThenAnswerLLM) Name() string { return "probe-then-answer" }

func (probeThenAnswerLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	probed := false
	if req != nil {
		for _, c := range req.Contents {
			if c == nil {
				continue
			}
			for _, part := range c.Parts {
				if part != nil && part.FunctionResponse != nil && part.FunctionResponse.Name == innerProbeTool {
					probed = true
				}
			}
		}
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "triaged"}}}
		if !probed {
			fc := &genai.FunctionCall{Name: innerProbeTool, Args: map[string]any{"request": "go"}}
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// tenant is one multi-session session: its own sub-gate and session id,
// its own parent agent, and the shared inner agent behind its "cluster"
// tool.
type tenant struct {
	sid   string
	gate  *permissions.Gate
	agent *Agent
}

// twoTenantsOneSubagent wires the daemon shape: one inner agent, two
// parents built from it the way compose.ReproduceAgent builds a session
// (WithGate(sub-gate) + WithSession(user, sid) + WithSubagents(shared)).
func twoTenantsOneSubagent(t *testing.T) (*innerProbe, *tenant, *tenant) {
	t.Helper()
	h, cleanup := openTestEventLog(t)
	t.Cleanup(cleanup)

	probe := &innerProbe{}
	template := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	// The inner agent is built ONCE, against the daemon's template gate,
	// and never rebuilt. Both tenants below get this same pointer.
	shared, err := New(probeThenAnswerLLM{},
		WithName("cluster"), WithEventLog(h), WithSession("daemon", "boot"),
		WithGate(template), WithTools([]tool.Tool{newInnerProbeTool(t, probe)}))
	if err != nil {
		t.Fatalf("New shared inner: %v", err)
	}

	build := func(sid string) *tenant {
		g := template.DeriveForSession(sid, nil)
		a, err := New(&callThenStopLLM{tool: "cluster"},
			WithName("parent"), WithEventLog(h), WithSession("u-"+sid, sid),
			WithGate(g), WithSubagents([]*Agent{shared}))
		if err != nil {
			t.Fatalf("New parent %s: %v", sid, err)
		}
		return &tenant{sid: sid, gate: g, agent: a}
	}
	return probe, build("sid-a"), build("sid-b")
}

func (tn *tenant) delegate(t *testing.T) {
	t.Helper()
	for _, err := range tn.agent.Run(context.Background(), "triage it") {
		if err != nil {
			t.Errorf("%s: Run: %v", tn.sid, err)
			return
		}
	}
}

// TestSharedSubagent_EachTenantDelegatesUnderItsOwnIdentity is the
// safety proof behind compose.SessionFactoryDeps.Subagents. Sharing the
// inner agent is what makes #741 a wiring change instead of an
// architectural one, and it is only defensible if a delegation carries
// the DELEGATING tenant's gate and session row rather than the inner
// agent's own or the previous tenant's.
func TestSharedSubagent_EachTenantDelegatesUnderItsOwnIdentity(t *testing.T) {
	t.Parallel()
	probe, a, b := twoTenantsOneSubagent(t)
	a.delegate(t)
	b.delegate(t)

	gates, sessions := probe.snapshot()
	if len(gates) != 2 {
		t.Fatalf("the shared subagent's tool ran %d time(s), want 2 (once per tenant)", len(gates))
	}
	// The gate. The inner agent was built with the daemon template; each
	// delegation must nonetheless be decided by the delegating tenant's
	// sub-gate, which is where that tenant's mode, approvals, prompter
	// and plan-first state live.
	if gates[0] != a.gate || gates[1] != b.gate {
		t.Errorf("session gates inside the shared subagent = %p, %p; want each tenant's own (%p, %p) — "+
			"one tenant's approvals and plan-first state would decide the other's tool calls",
			gates[0], gates[1], a.gate, b.gate)
	}
	// The session row. DeriveSessionID prefixes the parent's, so a
	// delegation filed under the wrong tenant is visible here.
	for i, tn := range []*tenant{a, b} {
		if want := tn.sid + ":sub:cluster:"; !strings.HasPrefix(sessions[i], want) {
			t.Errorf("%s delegated into session %q, want a row derived from its own (%q…) — "+
				"delegated events would land in another tenant's audit trail", tn.sid, sessions[i], want)
		}
	}
	if sessions[0] == sessions[1] {
		t.Errorf("both tenants delegated into one session row %q", sessions[0])
	}
}

// TestSharedSubagent_ConcurrentTenantsDoNotInterfere is the same claim
// under the condition a daemon actually runs in. Worth its own test
// because the sharing this enables is new: before #741 an inner agent
// backed exactly one parent, and concurrency was bounded by one
// session's parallel tool calls. Run with -race, which is what CI does.
func TestSharedSubagent_ConcurrentTenantsDoNotInterfere(t *testing.T) {
	t.Parallel()
	probe, a, b := twoTenantsOneSubagent(t)

	var wg sync.WaitGroup
	for _, tn := range []*tenant{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tn.delegate(t)
		}()
	}
	wg.Wait()

	gates, sessions := probe.snapshot()
	if len(gates) != 2 {
		t.Fatalf("the shared subagent's tool ran %d time(s), want 2", len(gates))
	}
	// Order is nondeterministic here, so assert on the SET: each tenant
	// appears exactly once, with its own gate against its own row.
	seen := map[*permissions.Gate]string{}
	for i, g := range gates {
		seen[g] = sessions[i]
	}
	if len(seen) != 2 {
		t.Fatalf("concurrent delegations resolved to %d distinct gate(s), want 2", len(seen))
	}
	for _, tn := range []*tenant{a, b} {
		sid, ok := seen[tn.gate]
		if !ok {
			t.Errorf("no delegation ran under %s's sub-gate", tn.sid)
			continue
		}
		if want := tn.sid + ":sub:cluster:"; !strings.HasPrefix(sid, want) {
			t.Errorf("%s's gate decided a delegation filed under %q, want a row derived from %q… — "+
				"gate and session row came from different tenants", tn.sid, sid, want)
		}
	}
}
