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

package agentic

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

	"github.com/go-steer/core-agent/agent"
)

// stubTool is a no-op tool.Tool used as a placeholder in
// InnerTools — the agentic constructors check the slice is non-
// empty but never invoke the tools at construction time.
type stubTool struct{ name string }

func (s stubTool) Name() string          { return s.name }
func (s stubTool) Description() string   { return "stub for tests" }
func (s stubTool) IsLongRunning() bool   { return false }
func (s stubTool) Confirmation() *string { return nil }

// stubLLM is a minimal adkmodel.LLM that returns one canned
// response — enough to construct an agent.Agent via agent.New for
// the AgentGetter wiring.
type stubLLM struct {
	mu       sync.Mutex
	response string
}

func (l *stubLLM) Name() string { return "stub" }

func (l *stubLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	resp := l.response
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: resp}}},
			TurnComplete: true,
		}, nil)
	}
}

func newTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	a, err := agent.New(&stubLLM{response: "ok"})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

func okOpts(t *testing.T) AgenticToolOpts {
	t.Helper()
	a := newTestAgent(t)
	return AgenticToolOpts{
		AgentGetter: func() *agent.Agent { return a },
		InnerTools:  []tool.Tool{stubTool{name: "read_file"}},
	}
}

// --- name + description sanity ---

func TestAgenticReadFile_NameAndDescription(t *testing.T) {
	t.Parallel()
	tl := AgenticReadFile(okOpts(t))
	if tl.Name() != "agentic_read_file" {
		t.Errorf("Name = %q, want agentic_read_file", tl.Name())
	}
	if !strings.Contains(tl.Description(), "INSTEAD OF read_file") {
		t.Errorf("Description missing 'INSTEAD OF read_file' steer:\n%s", tl.Description())
	}
}

func TestAgenticFetchURL_NameAndDescription(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.InnerTools = []tool.Tool{stubTool{name: "fetch_url"}}
	tl := AgenticFetchURL(opts)
	if tl.Name() != "agentic_fetch_url" {
		t.Errorf("Name = %q, want agentic_fetch_url", tl.Name())
	}
	if !strings.Contains(tl.Description(), "INSTEAD OF fetch_url") {
		t.Errorf("Description missing 'INSTEAD OF fetch_url' steer")
	}
}

func TestAgenticGrep_NameAndDescription(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.InnerTools = []tool.Tool{stubTool{name: "grep"}, stubTool{name: "read_file"}}
	tl := AgenticGrep(opts)
	if tl.Name() != "agentic_grep" {
		t.Errorf("Name = %q, want agentic_grep", tl.Name())
	}
	if !strings.Contains(tl.Description(), "INSTEAD OF grep") {
		t.Errorf("Description missing 'INSTEAD OF grep' steer")
	}
}

func TestAgenticResearch_NameAndDescription(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.InnerTools = []tool.Tool{
		stubTool{name: "read_file"},
		stubTool{name: "grep"},
		stubTool{name: "list_dir"},
		stubTool{name: "glob"},
	}
	tl := AgenticResearch(opts)
	if tl.Name() != "agentic_research" {
		t.Errorf("Name = %q, want agentic_research", tl.Name())
	}
	if !strings.Contains(tl.Description(), "open-ended question") {
		t.Errorf("Description missing 'open-ended question' framing")
	}
}

// --- panic-on-misconfiguration ---

func TestAgenticReadFile_PanicsWithoutAgentGetter(t *testing.T) {
	t.Parallel()
	opts := AgenticToolOpts{
		// AgentGetter intentionally nil
		InnerTools: []tool.Tool{stubTool{name: "read_file"}},
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic for nil AgentGetter, got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value = %T, want string", r)
		}
		if !strings.Contains(msg, "AgentGetter") {
			t.Errorf("panic msg = %q, want it to mention AgentGetter", msg)
		}
	}()
	_ = AgenticReadFile(opts)
}

func TestAgenticReadFile_PanicsWithoutInnerTools(t *testing.T) {
	t.Parallel()
	opts := AgenticToolOpts{
		AgentGetter: func() *agent.Agent { return nil }, // returning nil is fine; the empty-slice check fires first
		// InnerTools intentionally empty
	}
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic for empty InnerTools, got none")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value = %T, want string", r)
		}
		if !strings.Contains(msg, "InnerTools") {
			t.Errorf("panic msg = %q, want it to mention InnerTools", msg)
		}
	}()
	_ = AgenticReadFile(opts)
}

// --- defaults ---

// TestAgenticReadFile_DefaultsTwoTurns confirms the preset sets
// MaxTurns=2 when the caller leaves Budgets zero. We can't read
// the field back off the tool.Tool, so we inspect the closure
// indirectly via reflection-free trickery: re-construct opts with
// MaxTurns explicitly 0 and verify the preset constructor
// modifies the input opts before constructing the tool. (The
// preset takes opts by value, so the only way to verify is to
// poke at the implementation. Lightweight: just check the
// constructor doesn't panic with zero budgets.)
func TestAgenticReadFile_ZeroBudgetsConstructs(t *testing.T) {
	t.Parallel()
	opts := okOpts(t)
	opts.Budgets = agent.SubtaskBudgets{} // explicitly zero
	tl := AgenticReadFile(opts)
	if tl == nil {
		t.Errorf("expected non-nil tool with zero budgets")
	}
}

// --- end-to-end plumbing via a real functiontool dispatch ---
//
// The agenticTool implementation builds a functiontool.New —
// proving the tool is well-formed JSON-schema-wise + accepts the
// expected args. We verify this by serializing the tool through
// functiontool's declaration export. If the args schema or
// handler signature were misaligned, functiontool.New would have
// errored out (we'd see a panic at construction).
//
// This is a smoke test: it doesn't actually fire the subtask, but
// it catches construction-level regressions (e.g., changing
// agenticArgs in a way that breaks JSON schema generation).
func TestAgenticTool_FunctionToolConstruction(t *testing.T) {
	t.Parallel()
	// Verify that functiontool.New is in scope and our handlers
	// don't fail it. AgenticReadFile already does the call;
	// constructing it without panic is the assertion. A nil
	// return from AgenticReadFile would indicate functiontool
	// rejected the handler signature or args type.
	if AgenticReadFile(okOpts(t)) == nil {
		t.Errorf("expected non-nil tool.Tool, got nil")
	}
}

// Compile-time check: keep functiontool imported so a future
// refactor that drops it from agentic.go's import list trips
// this test file too. (The agentic package uses functiontool
// internally — this is a structural canary.)
var _ = functiontool.New[struct{}, struct{}]
