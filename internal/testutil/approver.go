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

package testutil

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// ApproverProbe is a permissions.Approver that records every request
// and denies it, so a tool under test reaches the approver and then
// does nothing: no command runs, no file is written, no request is
// sent. It exists to prove a call site hands the gate the call's full
// arguments (#1175 phase 2).
type ApproverProbe struct {
	mu   sync.Mutex
	reqs []permissions.ApproverRequest
}

// Judge records req and denies it.
func (p *ApproverProbe) Judge(_ context.Context, req permissions.ApproverRequest) (permissions.Verdict, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, req)
	return permissions.Verdict{Outcome: permissions.VerdictDeny, Reason: "approver probe", Model: "probe"}, nil
}

// Requests returns what the approver was asked, in order.
func (p *ApproverProbe) Requests() []permissions.ApproverRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]permissions.ApproverRequest(nil), p.reqs...)
}

// RequireArgs fails t unless the approver was asked exactly once, about
// toolName, with Args containing every one of want. A want that only
// the full arguments carry — write_file's content, a prompt past byte
// 200 — is what distinguishes a call site that passes its arguments
// from one that passes the summary alone.
func (p *ApproverProbe) RequireArgs(t testing.TB, toolName string, want ...string) {
	t.Helper()
	reqs := p.Requests()
	if len(reqs) != 1 {
		t.Fatalf("approver asked %d times, want 1 (requests: %+v)", len(reqs), reqs)
	}
	r := reqs[0]
	if r.ToolName != toolName {
		t.Errorf("approver asked about %q, want %q", r.ToolName, toolName)
	}
	for _, w := range want {
		if !strings.Contains(string(r.Args), w) {
			t.Errorf("approver Args = %s, want it to contain %q", r.Args, w)
		}
	}
}

// AutoGate returns a ModeAuto gate with no prompter whose approver is
// the returned probe and may decide calls matching eligible
// ("tool:pattern", as in permissions.allow). Pair it with
// ApproverToolContext: without an approver context every call
// escalates before the approver is asked.
func AutoGate(t testing.TB, eligible ...string) (*permissions.Gate, *ApproverProbe) {
	t.Helper()
	return AutoGateInScope(t, "", eligible...)
}

// AutoGateInScope is AutoGate with root as the declared path scope (the
// project root), for file tools: a write outside the scope is a
// path-scope prompt, which never reaches the approver.
func AutoGateInScope(t testing.TB, root string, eligible ...string) (*permissions.Gate, *ApproverProbe) {
	t.Helper()
	scope, err := permissions.NewPathScope(root, "", nil)
	if err != nil {
		t.Fatalf("AutoGateInScope: %v", err)
	}
	pol, err := permissions.NewPolicy(eligible, nil)
	if err != nil {
		t.Fatalf("AutoGate: eligible %v: %v", eligible, err)
	}
	probe := &ApproverProbe{}
	g := permissions.New(permissions.Options{
		Mode:            permissions.ModeAuto,
		Scope:           scope,
		Approver:        probe,
		AutoEligible:    pol,
		ApprovalTimeout: time.Minute,
	})
	return g, probe
}

type taskContext struct{}

func (taskContext) Task() string                                             { return "do the task" }
func (taskContext) RecentCalls() []permissions.ApproverCall                  { return nil }
func (taskContext) Bill(string, *genai.GenerateContentResponseUsageMetadata) {}
func (taskContext) CeilingReached() bool                                     { return false }
func (taskContext) Audit(permissions.ApproverAudit)                          {}

// ApproverContext returns ctx carrying an approver context with a
// non-empty task, which is what a turn context gives the gate.
func ApproverContext(ctx context.Context) context.Context {
	return permissions.WithApproverContext(ctx, taskContext{})
}

// ApproverToolContext is ApproverContext as an adkagent.Context, for calling
// a tool's handler directly. Only the context.Context methods are
// backed; anything else panics on the nil embedded interface, which is
// the right failure for a handler that needs more.
func ApproverToolContext() adkagent.Context {
	return ctxTool{ctx: ApproverContext(context.Background())}
}

type ctxTool struct {
	adkagent.Context
	ctx context.Context
}

func (c ctxTool) Deadline() (time.Time, bool) { return c.ctx.Deadline() }
func (c ctxTool) Done() <-chan struct{}       { return c.ctx.Done() }
func (c ctxTool) Err() error                  { return c.ctx.Err() }
func (c ctxTool) Value(key any) any           { return c.ctx.Value(key) }
