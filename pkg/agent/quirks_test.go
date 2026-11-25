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
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	"github.com/go-steer/core-agent/v3/pkg/tools"
)

type namedRecordingLLM struct {
	name     string
	mu       sync.Mutex
	requests []*adkmodel.LLMRequest
}

func (r *namedRecordingLLM) Name() string { return r.name }

func (r *namedRecordingLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content: &genai.Content{
				Role:  genai.RoleModel,
				Parts: []*genai.Part{{Text: "ok"}},
			},
			TurnComplete: true,
		}, nil)
	}
}

func (r *namedRecordingLLM) firstSystemInstruction(t *testing.T) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		t.Fatal("model was never invoked")
	}
	return extractSystemInstruction(r.requests[0])
}

func extractSystemInstruction(req *adkmodel.LLMRequest) string {
	if req == nil || req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range req.Config.SystemInstruction.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func newQuirkNoopTool(t *testing.T, name string) tool.Tool {
	t.Helper()
	type emptyArgs struct{}
	type emptyResult struct {
		OK bool `json:"ok"`
	}
	tl, err := functiontool.New(functiontool.Config{
		Name:        name,
		Description: "noop test tool " + name,
	}, func(_ adkagent.Context, _ emptyArgs) (emptyResult, error) {
		return emptyResult{OK: true}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New(%q): %v", name, err)
	}
	return tl
}

type quirkStubToolset struct {
	name  string
	tools []tool.Tool
}

func (s *quirkStubToolset) Name() string { return s.name }
func (s *quirkStubToolset) Tools(adkagent.ReadonlyContext) ([]tool.Tool, error) {
	return s.tools, nil
}

type quirkErrToolset struct {
	name string
	err  error
}

func (e *quirkErrToolset) Name() string { return e.name }
func (e *quirkErrToolset) Tools(adkagent.ReadonlyContext) ([]tool.Tool, error) {
	return nil, e.err
}

type quirkDynamicToolset struct {
	name string
	fn   func(adkagent.ReadonlyContext) ([]tool.Tool, error)
}

func (d *quirkDynamicToolset) Name() string { return d.name }
func (d *quirkDynamicToolset) Tools(ctx adkagent.ReadonlyContext) ([]tool.Tool, error) {
	return d.fn(ctx)
}

func geminiInstructionSeen(t *testing.T, modelName string, opts ...Option) string {
	t.Helper()
	rec := &namedRecordingLLM{name: modelName}
	a, err := New(rec, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, err := range a.Run(context.Background(), "hi") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	return rec.firstSystemInstruction(t)
}

func assertSubstrings(t *testing.T, text string, wants, forbids []string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("expected substring %q not found in:\n%s", want, text)
		}
	}
	for _, forbid := range forbids {
		if strings.Contains(lower, strings.ToLower(forbid)) {
			t.Errorf("forbidden substring %q leaked into:\n%s", forbid, text)
		}
	}
}

func TestGeminiParallelismQuirk_ToolSurfaceMatrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tools   []string
		empty   bool
		wants   []string
		forbids []string
	}{
		{
			name:  "zero tools omits quirk",
			tools: nil,
			empty: true,
		},
		{
			name:  "mcp-only tools get generic guidance only",
			tools: []string{"gke_list_pods", "gke_get_events"},
			wants: []string{
				"Execute multiple independent tool calls in parallel when feasible.",
				"Issue independent calls in a single response; do not execute them one by one.",
			},
			forbids: []string{
				"read_file", "reading files", "read multiple files",
				"grep", "searching", "shell", "bash", "editing", "code",
			},
		},
		{
			name:  "read_file only",
			tools: []string{"read_file"},
			wants: []string{
				"reading files",
				"read multiple files",
				"do not execute them one by one",
			},
			forbids: []string{"grep", "searching", "shell", "bash", "editing"},
		},
		{
			name:  "grep only",
			tools: []string{"grep"},
			wants: []string{
				"searching",
				"grep multiple directories",
				"do not execute them one by one",
			},
			forbids: []string{"read_file", "reading files", "read multiple files", "shell", "bash", "editing"},
		},
		{
			name:  "glob only mentions searching without grep",
			tools: []string{"glob"},
			wants: []string{
				"searching",
				"Issue independent calls in a single response; do not execute them one by one.",
			},
			forbids: []string{"grep", "read_file", "reading files", "shell", "bash", "editing"},
		},
		{
			name:  "bash only",
			tools: []string{"bash"},
			wants: []string{
				"independent shell commands",
				"Issue independent calls in a single response; do not execute them one by one.",
			},
			forbids: []string{"read_file", "reading files", "grep", "searching", "editing"},
		},
		{
			name:  "edit_file only",
			tools: []string{"edit_file"},
			wants: []string{
				"editing different files",
				"Issue independent calls in a single response; do not execute them one by one.",
			},
			forbids: []string{"read_file", "reading files", "grep", "searching", "shell", "bash"},
		},
		{
			name:  "read_file and grep without bash or edit",
			tools: []string{"read_file", "grep"},
			wants: []string{
				"searching or reading files",
				"read multiple files or grep multiple directories",
			},
			forbids: []string{"shell", "bash", "editing"},
		},
		{
			name:  "read_file and bash without grep or edit",
			tools: []string{"read_file", "bash"},
			wants: []string{
				"reading files or independent shell commands",
				"If you need to read multiple files, issue all the tool calls",
			},
			forbids: []string{"grep", "searching", "editing"},
		},
		{
			name:  "full built-in set",
			tools: []string{"read_file", "grep", "glob", "bash", "edit_file", "write_file"},
			wants: []string{
				"searching, reading files, independent shell commands, or editing different files",
				"read multiple files or grep multiple directories",
				"do not execute them one by one",
			},
			forbids: []string{"investigating code"},
		},
		{
			name:  "read_many_files without read_file omits reading files",
			tools: []string{"read_many_files", "view_file_outline", "todo"},
			wants: []string{
				"Execute multiple independent tool calls in parallel when feasible.",
				"do not execute them one by one",
			},
			forbids: []string{"read_file", "reading files", "read multiple files", "grep", "shell", "bash"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := geminiParallelismQuirk(quirkSurfaceFromNames(tc.tools...))
			if tc.empty {
				if got != "" {
					t.Fatalf("geminiParallelismQuirk(%v) = %q, want empty", tc.tools, got)
				}
				return
			}
			assertSubstrings(t, got, tc.wants, tc.forbids)
		})
	}
}

func TestNew_GeminiQuirkGatedOnDirectTools(t *testing.T) {
	t.Parallel()
	const model = "gemini-3.8-flash"

	t.Run("tools empty slice omits quirk", func(t *testing.T) {
		t.Parallel()
		instr := geminiInstructionSeen(t, model, WithTools([]tool.Tool{}))
		assertSubstrings(t, instr,
			[]string{"execute concurrently"},
			[]string{"read_file", "reading files", "grep", "shell", "do not execute them one by one"},
		)
	})

	t.Run("builtin suite with read_file grep bash disabled", func(t *testing.T) {
		t.Parallel()
		b := tools.Default()
		b.ReadFile = false
		b.Grep = false
		b.Bash = false
		gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
		reg, err := tools.Build(config.DefaultConfig(), gate, t.TempDir(), b)
		if err != nil {
			t.Fatalf("tools.Build: %v", err)
		}
		instr := geminiInstructionSeen(t, model, WithTools(reg.Tools), WithGate(gate))
		assertSubstrings(t, instr,
			[]string{"in parallel", "do not execute them one by one"},
			[]string{"read_file", "reading files", "read multiple files", "grep", "shell commands"},
		)
	})

	t.Run("builtin suite fully enabled keeps all examples", func(t *testing.T) {
		t.Parallel()
		gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
		reg, err := tools.Build(config.DefaultConfig(), gate, t.TempDir(), tools.Default())
		if err != nil {
			t.Fatalf("tools.Build: %v", err)
		}
		instr := geminiInstructionSeen(t, model, WithTools(reg.Tools), WithGate(gate))
		assertSubstrings(t, instr,
			[]string{
				"searching, reading files, independent shell commands, or editing different files",
				"read multiple files or grep multiple directories",
				"do not execute them one by one",
			},
			nil,
		)
	})

	t.Run("partial subset read_file without bash or grep", func(t *testing.T) {
		t.Parallel()
		instr := geminiInstructionSeen(t, model, WithTools([]tool.Tool{newQuirkNoopTool(t, "read_file")}))
		assertSubstrings(t, instr,
			[]string{"reading files", "read multiple files"},
			[]string{"grep", "shell", "bash", "editing different files"},
		)
	})

	t.Run("WithoutProviderQuirks suppresses even with full builtins", func(t *testing.T) {
		t.Parallel()
		instr := geminiInstructionSeen(t, model,
			WithTools([]tool.Tool{
				newQuirkNoopTool(t, "read_file"),
				newQuirkNoopTool(t, "grep"),
				newQuirkNoopTool(t, "bash"),
			}),
			WithoutProviderQuirks(),
		)
		assertSubstrings(t, instr,
			[]string{"execute concurrently"},
			[]string{"reading files", "grep", "shell commands", "do not execute them one by one"},
		)
	})

	t.Run("non-Gemini model gets no quirk even with full builtins", func(t *testing.T) {
		t.Parallel()
		instr := geminiInstructionSeen(t, "claude-sonnet-4-6",
			WithTools([]tool.Tool{
				newQuirkNoopTool(t, "read_file"),
				newQuirkNoopTool(t, "grep"),
				newQuirkNoopTool(t, "bash"),
			}),
		)
		assertSubstrings(t, instr,
			[]string{"execute concurrently"},
			[]string{"reading files", "grep", "shell commands", "do not execute them one by one"},
		)
	})
}

func TestNew_GeminiQuirkGatedOnToolsets(t *testing.T) {
	t.Parallel()
	const model = "gemini-3.8-flash"

	// Parent gate has all built-ins registered, mirroring a declarative
	// subagent with tools: [] sharing the parent's permission gate (#1608).
	parentGate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	if _, err := tools.Build(config.DefaultConfig(), parentGate, t.TempDir(), tools.Default()); err != nil {
		t.Fatalf("tools.Build(parent): %v", err)
	}

	t.Run("mcp-only gated toolset with tools empty omits file grep shell", func(t *testing.T) {
		t.Parallel()
		mcpTS := tools.GateToolset(&quirkStubToolset{
			name: "gke",
			tools: []tool.Tool{
				newQuirkNoopTool(t, "gke_list_pods"),
				newQuirkNoopTool(t, "gke_get_events"),
			},
		}, parentGate, "mcp")

		instr := geminiInstructionSeen(t, model,
			WithMode(ModeAutonomous),
			WithSubagentMode(SubagentModeTask),
			WithTools([]tool.Tool{}),
			WithToolsets([]tool.Toolset{mcpTS}),
			WithGate(parentGate),
		)
		assertSubstrings(t, instr,
			[]string{
				"Execute multiple independent tool calls in parallel when feasible.",
				"Issue independent calls in a single response; do not execute them one by one.",
			},
			[]string{"read_file", "reading files", "read multiple files", "grep", "searching", "shell", "bash", "editing"},
		)
	})

	t.Run("builtins inside GateToolset enable matching quirk families", func(t *testing.T) {
		t.Parallel()
		wrapped := tools.GateToolset(&quirkStubToolset{
			name: "custom_builtins",
			tools: []tool.Tool{
				newQuirkNoopTool(t, "read_file"),
				newQuirkNoopTool(t, "bash"),
			},
		}, parentGate, "custom")

		instr := geminiInstructionSeen(t, model, WithToolsets([]tool.Toolset{wrapped}))
		assertSubstrings(t, instr,
			[]string{
				"reading files or independent shell commands",
				"If you need to read multiple files",
			},
			[]string{"grep", "searching", "editing different files"},
		)
	})

	t.Run("empty toolset with no direct tools omits quirk", func(t *testing.T) {
		t.Parallel()
		emptyTS := tools.GateToolset(&quirkStubToolset{name: "empty"}, parentGate, "mcp")
		instr := geminiInstructionSeen(t, model, WithToolsets([]tool.Toolset{emptyTS}))
		assertSubstrings(t, instr,
			[]string{"execute concurrently"},
			[]string{"reading files", "grep", "shell", "do not execute them one by one"},
		)
	})
}

func TestRunSubtask_GeminiQuirkGatedOnSubtaskTools(t *testing.T) {
	t.Parallel()

	t.Run("tool-less subtask omits quirk", func(t *testing.T) {
		t.Parallel()
		parentRec := &namedRecordingLLM{name: "gemini-3.8-flash"}
		a, err := New(parentRec, WithTools([]tool.Tool{
			newQuirkNoopTool(t, "read_file"),
			newQuirkNoopTool(t, "grep"),
			newQuirkNoopTool(t, "bash"),
		}))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		subRec := &namedRecordingLLM{name: "gemini-3.5-flash-lite"}
		if _, err := a.RunSubtask(context.Background(), SubtaskSpec{
			Name:         "digest_only",
			SystemPrompt: "Digest the payload.",
			UserMessage:  "go",
			Model:        subRec,
		}); err != nil {
			t.Fatalf("RunSubtask: %v", err)
		}
		assertSubstrings(t, subRec.firstSystemInstruction(t),
			[]string{"Digest the payload."},
			[]string{"read_file", "reading files", "grep", "shell", "do not execute them one by one"},
		)
	})

	t.Run("read_file-only subtask omits grep and shell", func(t *testing.T) {
		t.Parallel()
		parentRec := &namedRecordingLLM{name: "gemini-3.8-flash"}
		a, err := New(parentRec)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		subRec := &namedRecordingLLM{name: "gemini-3.5-flash-lite"}
		if _, err := a.RunSubtask(context.Background(), SubtaskSpec{
			Name:         "read_only",
			SystemPrompt: "Extract the target lines.",
			UserMessage:  "go",
			Tools:        []tool.Tool{newQuirkNoopTool(t, "read_file")},
			Model:        subRec,
		}); err != nil {
			t.Fatalf("RunSubtask: %v", err)
		}
		assertSubstrings(t, subRec.firstSystemInstruction(t),
			[]string{"reading files", "read multiple files"},
			[]string{"grep", "shell", "bash", "editing different files"},
		)
	})

	t.Run("fetch_url-only subtask gets generic guidance", func(t *testing.T) {
		t.Parallel()
		parentRec := &namedRecordingLLM{name: "gemini-3.8-flash"}
		a, err := New(parentRec)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		subRec := &namedRecordingLLM{name: "gemini-3.5-flash-lite"}
		if _, err := a.RunSubtask(context.Background(), SubtaskSpec{
			Name:         "fetch_only",
			SystemPrompt: "Fetch and extract.",
			UserMessage:  "go",
			Tools:        []tool.Tool{newQuirkNoopTool(t, "fetch_url")},
			Model:        subRec,
		}); err != nil {
			t.Fatalf("RunSubtask: %v", err)
		}
		assertSubstrings(t, subRec.firstSystemInstruction(t),
			[]string{"Execute multiple independent tool calls in parallel when feasible.", "do not execute them one by one"},
			[]string{"read_file", "reading files", "grep", "shell", "bash", "editing"},
		)
	})
}

func TestNew_GeminiToolsetObserverConcurrentRunWithContents(t *testing.T) {
	t.Parallel()
	rec := &namedRecordingLLM{name: "gemini-3.8-flash"}
	readTool := newQuirkNoopTool(t, "read_file")
	mcpTool := newQuirkNoopTool(t, "gke_list_pods")

	dyn := &quirkDynamicToolset{
		name: "dynamic",
		fn: func(ctx adkagent.ReadonlyContext) ([]tool.Tool, error) {
			if ctx != nil && ctx.UserContent() != nil && strings.Contains(flattenText([]*genai.Content{ctx.UserContent()}), "with-read") {
				return []tool.Tool{readTool}, nil
			}
			return []tool.Tool{mcpTool}, nil
		},
	}

	a, err := New(rec, WithToolsets([]tool.Toolset{dyn}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			prompt := fmt.Sprintf("mcp-only-%d", idx)
			if idx%2 == 0 {
				prompt = fmt.Sprintf("with-read-%d", idx)
			}
			contents := []*genai.Content{
				{Role: genai.RoleUser, Parts: []*genai.Part{{Text: prompt}}},
			}
			for _, runErr := range a.RunWithContents(context.Background(), contents) {
				if runErr != nil {
					errs <- runErr
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for runErr := range errs {
		t.Fatalf("RunWithContents: %v", runErr)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) != 8 {
		t.Fatalf("recorded %d requests, want 8", len(rec.requests))
	}
	for _, req := range rec.requests {
		userText := flattenText(req.Contents)
		sys := extractSystemInstruction(req)
		if strings.Contains(userText, "with-read-") {
			if !strings.Contains(sys, "reading files") {
				t.Errorf("with-read request missed 'reading files' in system instruction:\n%s", sys)
			}
		} else {
			if strings.Contains(sys, "reading files") {
				t.Errorf("mcp-only request leaked 'reading files' across concurrent calls:\n%s", sys)
			}
		}
	}
}

func TestNew_GeminiToolsetErrorDiscardsPendingTracking(t *testing.T) {
	t.Parallel()
	boom := errors.New("mcp server unreachable")
	okTS := &quirkStubToolset{name: "ok", tools: []tool.Tool{newQuirkNoopTool(t, "read_file")}}
	badTS := &quirkErrToolset{name: "bad", err: boom}

	tracker, wrapped := newToolsetSurfaceTracker(nil, []tool.Toolset{okTS, badTS})
	ctx := &fakeInvCtx{invID: "inv-err-1"}
	if _, err := wrapped[0].Tools(ctx); err != nil {
		t.Fatalf("wrapped[0].Tools: %v", err)
	}
	if _, err := wrapped[1].Tools(ctx); !errors.Is(err, boom) {
		t.Fatalf("wrapped[1].Tools err = %v, want %v", err, boom)
	}
	tracker.mu.Lock()
	remaining := len(tracker.byInv)
	latest0 := tracker.latest[0]
	tracker.mu.Unlock()
	if remaining != 0 {
		t.Errorf("byInv has %d entries after toolset error, want 0", remaining)
	}
	if latest0.hasAnyTool {
		t.Errorf("aborted step polluted tr.latest[0] = %+v, want zero value", latest0)
	}
}

func TestNew_GeminiToolsetMultiStepDynamicShrink(t *testing.T) {
	t.Parallel()
	readTool := newQuirkNoopTool(t, "read_file")

	var mu sync.Mutex
	var step int
	var reqs []*adkmodel.LLMRequest
	multiLLM := &multiStepQuirkLLM{
		name: "gemini-3.8-flash",
		onReq: func(req *adkmodel.LLMRequest) *adkmodel.LLMResponse {
			mu.Lock()
			reqs = append(reqs, req)
			cur := len(reqs)
			mu.Unlock()
			if cur == 1 {
				return &adkmodel.LLMResponse{
					Content: &genai.Content{
						Role: genai.RoleModel,
						Parts: []*genai.Part{{
							FunctionCall: &genai.FunctionCall{
								ID:   "call-1",
								Name: "read_file",
								Args: map[string]any{},
							},
						}},
					},
					TurnComplete: true,
				}
			}
			return &adkmodel.LLMResponse{
				Content: &genai.Content{
					Role:  genai.RoleModel,
					Parts: []*genai.Part{{Text: "done"}},
				},
				TurnComplete: true,
			}
		},
	}

	dyn := &quirkDynamicToolset{
		name: "shrinking",
		fn: func(adkagent.ReadonlyContext) ([]tool.Tool, error) {
			mu.Lock()
			step++
			cur := step
			mu.Unlock()
			if cur == 1 {
				return []tool.Tool{readTool}, nil
			}
			return nil, nil
		},
	}

	a, err := New(multiLLM, WithToolsets([]tool.Toolset{dyn}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, err := range a.Run(context.Background(), "go") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("recorded %d requests, want 2", len(reqs))
	}
	assertSubstrings(t, extractSystemInstruction(reqs[0]),
		[]string{"reading files", "read multiple files"},
		[]string{"grep", "shell"},
	)
	assertSubstrings(t, extractSystemInstruction(reqs[1]),
		[]string{"execute concurrently"},
		[]string{"reading files", "grep", "shell", "do not execute them one by one"},
	)
}

func TestToolsetSurfaceTracker_EvictionRecoveryPreservesEarlierSlots(t *testing.T) {
	t.Parallel()
	ts0 := &quirkStubToolset{name: "files", tools: []tool.Tool{newQuirkNoopTool(t, "read_file")}}
	ts1 := &quirkStubToolset{name: "shell", tools: []tool.Tool{newQuirkNoopTool(t, "bash")}}

	tracker, wrapped := newToolsetSurfaceTracker(nil, []tool.Toolset{ts0, ts1})

	// Seed tr.latest via one completed invocation.
	seedCtx := &fakeInvCtx{invID: "seed"}
	if _, err := wrapped[0].Tools(seedCtx); err != nil {
		t.Fatalf("seed wrapped[0]: %v", err)
	}
	if _, err := wrapped[1].Tools(seedCtx); err != nil {
		t.Fatalf("seed wrapped[1]: %v", err)
	}
	_ = tracker.consume(seedCtx)

	// Start target invocation at slot 0, then burst past maxTrackedInvocations
	// so target is evicted before it reaches slot 1.
	targetCtx := &fakeInvCtx{invID: "target"}
	if _, err := wrapped[0].Tools(targetCtx); err != nil {
		t.Fatalf("target wrapped[0]: %v", err)
	}
	for i := range maxTrackedInvocations + 5 {
		burstCtx := &fakeInvCtx{invID: fmt.Sprintf("burst-%d", i)}
		if _, err := wrapped[0].Tools(burstCtx); err != nil {
			t.Fatalf("burst %d: %v", i, err)
		}
	}
	if _, err := wrapped[1].Tools(targetCtx); err != nil {
		t.Fatalf("target wrapped[1]: %v", err)
	}

	got := tracker.consume(targetCtx)
	if !got.hasReadFile || !got.hasBash {
		t.Errorf("consume after eviction = %+v, want both hasReadFile=true and hasBash=true", got)
	}
}

type multiStepQuirkLLM struct {
	name  string
	onReq func(*adkmodel.LLMRequest) *adkmodel.LLMResponse
}

func (m *multiStepQuirkLLM) Name() string { return m.name }

func (m *multiStepQuirkLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	resp := m.onReq(req)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(resp, nil)
	}
}

type fakeInvCtx struct {
	adkagent.ReadonlyContext
	invID string
}

func (f *fakeInvCtx) InvocationID() string { return f.invID }
