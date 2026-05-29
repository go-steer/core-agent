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
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// makeSubagentTestEvent constructs a minimal session.Event for the
// branch-injecting tests. We can't reuse the helper from
// eventlog_test.go (different package) so we redefine locally.
func makeSubagentTestEvent(id, branch string) *session.Event {
	return &session.Event{
		ID:        id,
		Author:    "tester",
		Branch:    branch,
		Timestamp: time.Now(),
		LLMResponse: adkmodel.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "x"}},
			},
		},
	}
}

func TestNewSubagentTool_RequiresInner(t *testing.T) {
	t.Parallel()
	_, err := NewSubagentTool(SubagentOptions{})
	if err == nil || !strings.Contains(err.Error(), "Inner is required") {
		t.Errorf("expected Inner-required error, got %v", err)
	}
}

func TestNewSubagentTool_RequiresInnerADKAgent(t *testing.T) {
	t.Parallel()
	// Hand-construct an Agent missing the underlying ADK agent —
	// the safety net catches this before it reaches the
	// session.Service check. Real consumers can't trip this via
	// agent.New (which always populates inner).
	a := &Agent{agentName: "research"}
	_, err := NewSubagentTool(SubagentOptions{Inner: a})
	if err == nil || !strings.Contains(err.Error(), "no underlying ADK agent") {
		t.Errorf("expected no-ADK-agent error, got %v", err)
	}
}

func TestNewSubagentTool_DefaultsNameToInnerAgentName(t *testing.T) {
	t.Parallel()
	a, err := New(&stubLLM{}, WithName("research"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tl, err := NewSubagentTool(SubagentOptions{Inner: a})
	if err != nil {
		t.Fatalf("NewSubagentTool: %v", err)
	}
	if tl.Name() != "research" {
		t.Errorf("default tool name = %q, want %q (Inner.AgentName())", tl.Name(), "research")
	}
}

func TestNewSubagentTool_NameAndDescriptionOverrides(t *testing.T) {
	t.Parallel()
	a, err := New(&stubLLM{}, WithName("research"), WithDescription("do research"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tl, err := NewSubagentTool(SubagentOptions{
		Inner:       a,
		Name:        "lookup",
		Description: "look it up",
	})
	if err != nil {
		t.Fatalf("NewSubagentTool: %v", err)
	}
	if tl.Name() != "lookup" {
		t.Errorf("Name override didn't take: got %q want %q", tl.Name(), "lookup")
	}
	if tl.Description() != "look it up" {
		t.Errorf("Description override didn't take: got %q", tl.Description())
	}
}

func TestNewSubagentTool_FallsBackToInnerDescription(t *testing.T) {
	t.Parallel()
	a, err := New(&stubLLM{}, WithName("research"), WithDescription("inner description"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tl, err := NewSubagentTool(SubagentOptions{Inner: a})
	if err != nil {
		t.Fatalf("NewSubagentTool: %v", err)
	}
	if tl.Description() != "inner description" {
		t.Errorf("Description = %q, want %q (Inner's)", tl.Description(), "inner description")
	}
}

func TestBranchInjectingService_StampsEmptyBranch(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u", SessionID: "branch-test",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: "app", UserID: "u", SessionID: "branch-test",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	wrapped := &branchInjectingService{inner: h.Service, branch: "research"}
	ev := makeSubagentTestEvent("ev-1", "")
	if err := wrapped.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if ev.Branch != "research" {
		t.Errorf("Branch should be stamped on the event; got %q", ev.Branch)
	}
}

func TestBranchInjectingService_PreservesPresetBranch(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := h.Service.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u", SessionID: "preset",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	resp, err := h.Service.Get(ctx, &session.GetRequest{
		AppName: "app", UserID: "u", SessionID: "preset",
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	wrapped := &branchInjectingService{inner: h.Service, branch: "research"}
	// A nested subagent at "research.deep" — the wrapper must not
	// overwrite the deeper branch label with its own.
	ev := makeSubagentTestEvent("ev-2", "research.deep")
	if err := wrapped.AppendEvent(ctx, resp.Session, ev); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if ev.Branch != "research.deep" {
		t.Errorf("preset Branch should not be overwritten; got %q", ev.Branch)
	}
}

func TestBranchInjectingService_DelegatesCRUD(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	ctx := context.Background()
	wrapped := &branchInjectingService{inner: h.Service, branch: "research"}
	if _, err := wrapped.Create(ctx, &session.CreateRequest{
		AppName: "app", UserID: "u", SessionID: "delegated",
	}); err != nil {
		t.Fatalf("Create through wrapper: %v", err)
	}
	resp, err := wrapped.Get(ctx, &session.GetRequest{
		AppName: "app", UserID: "u", SessionID: "delegated",
	})
	if err != nil {
		t.Fatalf("Get through wrapper: %v", err)
	}
	if resp == nil || resp.Session == nil || resp.Session.ID() != "delegated" {
		t.Errorf("Get returned %+v, want session with ID=delegated", resp)
	}
	listResp, err := wrapped.List(ctx, &session.ListRequest{AppName: "app", UserID: "u"})
	if err != nil {
		t.Fatalf("List through wrapper: %v", err)
	}
	if listResp == nil || len(listResp.Sessions) == 0 {
		t.Errorf("List returned no sessions: %+v", listResp)
	}
	if err := wrapped.Delete(ctx, &session.DeleteRequest{
		AppName: "app", UserID: "u", SessionID: "delegated",
	}); err != nil {
		t.Fatalf("Delete through wrapper: %v", err)
	}
}

func TestComposeBranch(t *testing.T) {
	t.Parallel()
	cases := []struct{ parent, this, want string }{
		{"", "", ""},
		{"", "research", "research"},
		{"parent", "", "parent"},
		{"parent", "research", "parent.research"},
		{"a.b", "c", "a.b.c"},
	}
	for _, c := range cases {
		if got := composeBranch(c.parent, c.this); got != c.want {
			t.Errorf("composeBranch(%q,%q)=%q, want %q", c.parent, c.this, got, c.want)
		}
	}
}

func TestCurrentSubagentDepth_DefaultsZeroAndReadsContext(t *testing.T) {
	t.Parallel()
	if d := CurrentSubagentDepth(context.Background()); d != 0 {
		t.Errorf("default depth = %d, want 0", d)
	}
	ctx := context.WithValue(context.Background(), subagentDepthKey{}, 7)
	if d := CurrentSubagentDepth(ctx); d != 7 {
		t.Errorf("depth from context = %d, want 7", d)
	}
}

func TestWithSubagents_RegistersTools(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	r1, err := New(&stubLLM{}, WithName("research"), WithEventLog(h), WithSession("u", "s1"))
	if err != nil {
		t.Fatalf("New r1: %v", err)
	}
	r2, err := New(&stubLLM{}, WithName("planner"), WithEventLog(h), WithSession("u", "s2"))
	if err != nil {
		t.Fatalf("New r2: %v", err)
	}
	parent, err := New(&stubLLM{},
		WithName("parent"),
		WithEventLog(h),
		WithSession("u", "p"),
		WithSubagents([]*Agent{r1, r2}),
	)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range parent.Tools() {
		names[tl.Name()] = true
	}
	for _, want := range []string{"research", "planner"} {
		if !names[want] {
			t.Errorf("WithSubagents should have added %q tool; have %v", want, names)
		}
	}
}

func TestWithSubagents_NilEntryIgnored(t *testing.T) {
	t.Parallel()
	a, err := New(&stubLLM{}, WithSubagents([]*Agent{nil}))
	if err != nil {
		t.Fatalf("nil subagent should not error: %v", err)
	}
	if a == nil {
		t.Fatalf("New returned nil agent")
	}
}

func TestWithSubagents_OrderIndependent(t *testing.T) {
	t.Parallel()
	// WithSubagents should resolve to the right session.Service
	// regardless of where it appears in the option list — even
	// before WithEventLog. We verify by introspecting Inner.
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	research, err := New(&stubLLM{}, WithName("research"), WithEventLog(h), WithSession("u", "r"))
	if err != nil {
		t.Fatalf("New research: %v", err)
	}
	parent, err := New(&stubLLM{},
		WithSubagents([]*Agent{research}), // appears BEFORE WithEventLog
		WithName("parent"),
		WithEventLog(h),
		WithSession("u", "p"),
	)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	if parent.SessionService() != h.Service {
		t.Errorf("parent's SessionService should be h.Service")
	}
	var found bool
	for _, tl := range parent.Tools() {
		if tl.Name() == "research" {
			found = true
		}
	}
	if !found {
		t.Errorf("research subagent tool missing from parent")
	}
}
