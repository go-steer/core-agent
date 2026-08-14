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
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent/internal/subsession"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

// openTestEventLog returns a Handle backed by a fresh on-disk SQLite
// database. Duplicated (rather than exported) so this package's tests
// stay self-contained.
func openTestEventLog(t *testing.T) (*eventlog.Handle, func()) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "session.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	return h, func() { _ = h.Close() }
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
	a, err := New(minimalLLM{}, WithName("research"))
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
	a, err := New(minimalLLM{}, WithName("research"), WithDescription("do research"))
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
	a, err := New(minimalLLM{}, WithName("research"), WithDescription("inner description"))
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

func TestWithSubagents_RegistersTools(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	r1, err := New(minimalLLM{}, WithName("research"), WithEventLog(h), WithSession("u", "s1"))
	if err != nil {
		t.Fatalf("New r1: %v", err)
	}
	r2, err := New(minimalLLM{}, WithName("planner"), WithEventLog(h), WithSession("u", "s2"))
	if err != nil {
		t.Fatalf("New r2: %v", err)
	}
	parent, err := New(minimalLLM{},
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

// callThenStopLLM asks for one tool by name, then answers when the
// result comes back. Enough to drive a parent through one subagent
// invocation.
type callThenStopLLM struct {
	tool  string
	calls int
	mu    sync.Mutex
}

func (*callThenStopLLM) Name() string { return "call-then-stop" }

func (l *callThenStopLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.calls++
	first := l.calls == 1
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "done"}}}
		if first {
			fc := &genai.FunctionCall{Name: l.tool, Args: map[string]any{"request": "go"}}
			content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: fc}}}
		}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// lineageRecordingLLM notes which subagents the context says it is
// running inside of.
type lineageRecordingLLM struct {
	mu   sync.Mutex
	seen []string
}

func (*lineageRecordingLLM) Name() string { return "lineage-recorder" }

func (l *lineageRecordingLLM) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	l.seen = append(l.seen, subsession.Lineage(ctx)...)
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "looked"}}}
		yield(&adkmodel.LLMResponse{Content: content, FinishReason: genai.FinishReasonStop, TurnComplete: true}, nil)
	}
}

// A declarative subagent is reachable two ways under one name — as a
// parent tool call and by reference from spawn_agent — so the
// synchronous half of the stack has to be recorded too, or a subagent
// invoked as a tool can turn around and spawn itself asynchronously
// (#732).
func TestSubagentTool_RecordsItsNameInTheLineage(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	inner := &lineageRecordingLLM{}
	child, err := New(inner, WithName("cluster"), WithEventLog(h), WithSession("u", "c"))
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	parent, err := New(&callThenStopLLM{tool: "cluster"},
		WithName("parent"), WithEventLog(h), WithSession("u", "p"),
		WithSubagents([]*Agent{child}))
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	for _, err := range parent.Run(context.Background(), "delegate it") {
		if err != nil {
			t.Fatalf("parent.Run: %v", err)
		}
	}
	inner.mu.Lock()
	defer inner.mu.Unlock()
	if len(inner.seen) != 1 || inner.seen[0] != "cluster" {
		t.Errorf("lineage inside the subagent = %v, want [cluster]", inner.seen)
	}
}

func TestSubagentNames_ReportsRegistered(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	r1, err := New(minimalLLM{}, WithName("research"), WithEventLog(h), WithSession("u", "s1"))
	if err != nil {
		t.Fatalf("New r1: %v", err)
	}
	r2, err := New(minimalLLM{}, WithName("planner"), WithEventLog(h), WithSession("u", "s2"))
	if err != nil {
		t.Fatalf("New r2: %v", err)
	}
	parent, err := New(minimalLLM{},
		WithName("parent"),
		WithEventLog(h),
		WithSession("u", "p"),
		WithSubagents([]*Agent{r1, r2}),
	)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	got := map[string]bool{}
	for _, n := range parent.SubagentNames() {
		got[n] = true
	}
	if len(got) != 2 || !got["research"] || !got["planner"] {
		t.Errorf("SubagentNames() = %v, want {research, planner}", parent.SubagentNames())
	}

	// An agent with no subagents reports none (not a subagent tool set
	// that mistakenly includes built-ins).
	plain, err := New(minimalLLM{}, WithName("plain"), WithEventLog(h), WithSession("u", "x"))
	if err != nil {
		t.Fatalf("New plain: %v", err)
	}
	if names := plain.SubagentNames(); len(names) != 0 {
		t.Errorf("plain.SubagentNames() = %v, want empty", names)
	}
}

func TestWithSubagentMaxDepth_SetsField(t *testing.T) {
	t.Parallel()
	// Default: unset → 0, which NewSubagentTool reads as "substrate
	// default" (defaultSubagentMaxDepth).
	def, err := New(minimalLLM{}, WithName("plain"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if def.subagentMaxDepth != 0 {
		t.Errorf("default subagentMaxDepth = %d, want 0", def.subagentMaxDepth)
	}
	// Explicit value is retained on the agent so the parent's
	// WithSubagents resolution can forward it to SubagentOptions.MaxDepth.
	a, err := New(minimalLLM{}, WithName("deep"), WithSubagentMaxDepth(5))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.subagentMaxDepth != 5 {
		t.Errorf("subagentMaxDepth = %d, want 5", a.subagentMaxDepth)
	}
}

func TestWithSubagents_HonorsPerSubagentMaxDepth(t *testing.T) {
	t.Parallel()
	// A subagent that declares its own depth cap keeps it when wrapped
	// as a tool by the parent — the value must survive the WithSubagents
	// resolution into SubagentOptions.MaxDepth. We can't read maxDepth
	// off the opaque tool.Tool, so assert the observable proxy: parent
	// construction succeeds and the tool registers under its name. The
	// field-forwarding one-liner is covered by TestWithSubagentMaxDepth_SetsField
	// plus this integration check that the depth-carrying subagent is a
	// legal WithSubagents input.
	h, cleanup := openTestEventLog(t)
	defer cleanup()
	child, err := New(minimalLLM{}, WithName("capped"), WithSubagentMaxDepth(1), WithEventLog(h), WithSession("u", "c"))
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	parent, err := New(minimalLLM{}, WithName("parent"), WithEventLog(h), WithSession("u", "p"), WithSubagents([]*Agent{child}))
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	var found bool
	for _, tl := range parent.Tools() {
		if tl.Name() == "capped" {
			found = true
		}
	}
	if !found {
		t.Error("depth-capped subagent should register as a tool named \"capped\"")
	}
}

func TestWithSubagents_NilEntryIgnored(t *testing.T) {
	t.Parallel()
	a, err := New(minimalLLM{}, WithSubagents([]*Agent{nil}))
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
	research, err := New(minimalLLM{}, WithName("research"), WithEventLog(h), WithSession("u", "r"))
	if err != nil {
		t.Fatalf("New research: %v", err)
	}
	parent, err := New(minimalLLM{},
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

// TestDeriveSubagentSessionID_UniquePerInvocation is the #364
// regression: two invocations of the same subagent (identical parent
// + branch) must land in distinct session rows so concurrent calls
// don't interleave history and race ADK's optimistic-concurrency
// check, and sequential calls don't silently accumulate history
// across independent requests. The derived ID must still carry the
// shared "<parent>:sub:<branch>" prefix so audit queries find both.
// This exercises core's subagentInvocationID feeding subsession's
// DeriveSessionID — the two halves that together guarantee #364.
func TestDeriveSubagentSessionID_UniquePerInvocation(t *testing.T) {
	t.Parallel()

	const (
		parent = "sess-1"
		branch = "research"
	)
	a := subsession.DeriveSessionID(parent, branch, subagentInvocationID("fc-A"))
	b := subsession.DeriveSessionID(parent, branch, subagentInvocationID("fc-B"))

	if a == b {
		t.Fatalf("distinct invocations produced the same session row %q (would interleave; #364)", a)
	}
	prefix := parent + ":sub:" + branch
	for _, id := range []string{a, b} {
		if !strings.HasPrefix(id, prefix) {
			t.Errorf("derived id %q lost the audit prefix %q", id, prefix)
		}
	}
}

// TestSubagentInvocationID prefers the FunctionCallID and falls back
// to a fresh unique value when it's empty/blank — otherwise an empty
// component would collapse concurrent invocations back onto one shared
// row (#364).
func TestSubagentInvocationID(t *testing.T) {
	t.Parallel()
	if got := subagentInvocationID("fc-123"); got != "fc-123" {
		t.Errorf("subagentInvocationID(%q)=%q, want passthrough", "fc-123", got)
	}
	// Blank IDs must not be treated as a usable component.
	a := subagentInvocationID("")
	b := subagentInvocationID("   ")
	if a == "" || b == "" {
		t.Fatalf("fallback produced an empty invocation id (a=%q b=%q)", a, b)
	}
	if a == b {
		t.Errorf("fallback ids collided (%q); concurrent invocations would share a row", a)
	}
}
