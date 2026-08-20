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
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// meteredLLM is a subagent model that reports UsageMetadata, which
// the mock providers deliberately do not. Each script entry is one
// GenerateContent call; chunks within an entry are yielded in order,
// so a single entry can model a streaming turn whose UsageMetadata is
// cumulative across its chunks (the Gemini convention TurnTap exists
// for).
type meteredLLM struct {
	name   string
	script [][]*adkmodel.LLMResponse

	mu    sync.Mutex
	calls int
}

func (l *meteredLLM) Name() string { return l.name }

func (l *meteredLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	l.mu.Lock()
	n := l.calls
	l.calls++
	l.mu.Unlock()
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if n >= len(l.script) {
			// Past the script: a plain final turn with no usage, so a
			// surplus call can never be mistaken for billed spend.
			yield(finalText("done"), nil)
			return
		}
		for _, resp := range l.script[n] {
			if !yield(resp, nil) {
				return
			}
		}
	}
}

// usedText builds one final, turn-completing response carrying usage.
func usedText(text string, in, out int) *adkmodel.LLMResponse {
	r := finalText(text)
	r.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:     int32(in),  //nolint:gosec // test-sized constants
		CandidatesTokenCount: int32(out), //nolint:gosec // test-sized constants
	}
	return r
}

func finalText(text string) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: text}},
		},
		TurnComplete: true,
	}
}

// partialText is a streaming chunk: not turn-completing, carrying the
// running cumulative usage a provider reports mid-turn.
func partialText(text string, in, out int) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: text}},
		},
		Partial: true,
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     int32(in),  //nolint:gosec // test-sized constants
			CandidatesTokenCount: int32(out), //nolint:gosec // test-sized constants
		},
	}
}

// callTool builds one turn-completing response that asks for a tool.
func callTool(name string, in, out int) *adkmodel.LLMResponse {
	return &adkmodel.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{Name: name, Args: map[string]any{}},
			}},
		},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     int32(in),  //nolint:gosec // test-sized constants
			CandidatesTokenCount: int32(out), //nolint:gosec // test-sized constants
		},
		TurnComplete: true,
	}
}

func pingTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, err := functiontool.New(
		functiontool.Config{Name: "ping", Description: "return pong"},
		func(_ tool.Context, _ struct{}) (map[string]any, error) {
			return map[string]any{"result": "pong"}, nil
		})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}
	return tl
}

// runParentDelegating builds a parent whose first model call delegates
// to child and whose later calls finish, runs one parent turn, and
// returns the tracker it billed to.
func runParentDelegating(t *testing.T, child *Agent, tracker *usage.Tracker, h *eventlog.Handle) {
	t.Helper()
	opts := []Option{
		WithName("parent"),
		WithEventLog(h),
		WithSession("u", "parent"),
		WithSubagents([]*Agent{child}),
	}
	if tracker != nil {
		opts = append(opts, WithUsageTracker(tracker))
	}
	parent, err := New(&fnCallThenDoneLLM{target: child.AgentName()}, opts...)
	if err != nil {
		t.Fatalf("New parent: %v", err)
	}
	for _, err := range parent.Run(context.Background(), "go") {
		if err != nil {
			t.Fatalf("parent.Run: %v", err)
		}
	}
}

// newTestEventLog is openTestEventLog with the cleanup registered, so
// the parallel tests below read as three lines of setup.
func newTestEventLog(t *testing.T) *eventlog.Handle {
	t.Helper()
	h, cleanup := openTestEventLog(t)
	t.Cleanup(cleanup)
	return h
}

// TestSubagentTool_BillsDelegatedTurnsToTheParentTracker is the #713
// regression: a declarative subagent reached as a TOOL runs on its own
// ADK runner inside the tool handler, so its events never reach the
// iterator the harness taps for usage — and the subagent is built with
// no tracker of its own. Every token it spent was therefore invisible
// to /usage, to /stats, and to the session and per-turn cost ceilings
// that are the only backstop an unattended run has.
//
// Fails on pre-fix code with totals of exactly zero.
func TestSubagentTool_BillsDelegatedTurnsToTheParentTracker(t *testing.T) {
	t.Parallel()
	h := newTestEventLog(t)
	tracker := usage.NewTracker()

	child, err := New(&meteredLLM{
		name:   "child-model",
		script: [][]*adkmodel.LLMResponse{{usedText("child answer", 1000, 200)}},
	}, WithName("child"), WithEventLog(h), WithSession("u", "child"))
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	runParentDelegating(t, child, tracker, h)

	tot := tracker.Totals()
	if tot.InputTokens != 1000 || tot.OutputTokens != 200 {
		t.Errorf("parent tracker = %d in / %d out, want 1000/200 (delegated spend off the books)",
			tot.InputTokens, tot.OutputTokens)
	}
	if tot.Turns != 1 {
		t.Errorf("turns = %d, want 1", tot.Turns)
	}
	// Attribution is by the SUBAGENT's model, which is routinely a
	// cheaper tier than the parent's — filing it under the parent's
	// model would misprice the row and misreport the mix.
	byModel := tracker.TotalsByModel()
	if got, ok := byModel["child-model"]; !ok {
		t.Errorf("no per-model row for the subagent's model; got %v", keysOf(byModel))
	} else if got.InputTokens != 1000 {
		t.Errorf("child-model row = %d in, want 1000", got.InputTokens)
	}
}

// TestSubagentTool_DoesNotDoubleCountStreamingChunks pins the reason
// the roll-up goes through usage.TurnTap rather than appending on
// every usage-bearing event: Gemini reports UsageMetadata cumulatively
// across the chunks of one model turn, so a naive append both sums
// running totals and files each chunk as its own turn (#353).
func TestSubagentTool_DoesNotDoubleCountStreamingChunks(t *testing.T) {
	t.Parallel()
	h := newTestEventLog(t)
	tracker := usage.NewTracker()

	child, err := New(&meteredLLM{
		name: "child-model",
		script: [][]*adkmodel.LLMResponse{{
			partialText("chi", 1000, 50),
			partialText("chi ans", 1000, 120),
			usedText("child answer", 1000, 200),
		}},
	}, WithName("child"), WithEventLog(h), WithSession("u", "child"))
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	runParentDelegating(t, child, tracker, h)

	tot := tracker.Totals()
	if tot.Turns != 1 {
		t.Errorf("turns = %d, want 1 (one append per chunk)", tot.Turns)
	}
	if tot.InputTokens != 1000 || tot.OutputTokens != 200 {
		t.Errorf("totals = %d in / %d out, want 1000/200 (cumulative chunks summed)",
			tot.InputTokens, tot.OutputTokens)
	}
}

// TestSubagentTool_BillsEveryTurnOfAMultiTurnDelegation: one tool call
// to the parent is many model turns inside the subagent, and the
// runaway shape #713 describes is precisely a long tail of cheap ones.
// Billing only the last would leave the bulk of a wandering
// delegation unbilled.
func TestSubagentTool_BillsEveryTurnOfAMultiTurnDelegation(t *testing.T) {
	t.Parallel()
	h := newTestEventLog(t)
	tracker := usage.NewTracker()

	child, err := New(&meteredLLM{
		name: "child-model",
		script: [][]*adkmodel.LLMResponse{
			{callTool("ping", 1000, 30)},
			{usedText("child answer", 1200, 70)},
		},
	},
		WithName("child"),
		WithEventLog(h),
		WithSession("u", "child"),
		WithTools([]tool.Tool{pingTool(t)}),
	)
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	runParentDelegating(t, child, tracker, h)

	tot := tracker.Totals()
	if tot.Turns != 2 {
		t.Errorf("turns = %d, want 2 (both subagent turns billed)", tot.Turns)
	}
	if tot.InputTokens != 2200 || tot.OutputTokens != 100 {
		t.Errorf("totals = %d in / %d out, want 2200/100", tot.InputTokens, tot.OutputTokens)
	}
}

// TestSubagentTool_NoParentTrackerStillRuns: a host that wired no
// tracker anywhere keeps the behaviour it already has everywhere else
// — the delegation works, the spend simply has no ledger to land in.
func TestSubagentTool_NoParentTrackerStillRuns(t *testing.T) {
	t.Parallel()
	h := newTestEventLog(t)

	child, err := New(&meteredLLM{
		name:   "child-model",
		script: [][]*adkmodel.LLMResponse{{usedText("child answer", 1000, 200)}},
	}, WithName("child"), WithEventLog(h), WithSession("u", "child"))
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	runParentDelegating(t, child, nil, h)
}

// TestNewSubagentTool_FallsBackToTheInnerTracker covers the direct
// NewSubagentTool consumer who wired the tracker onto the subagent
// rather than passing ParentTracker.
func TestNewSubagentTool_FallsBackToTheInnerTracker(t *testing.T) {
	t.Parallel()
	h := newTestEventLog(t)
	tracker := usage.NewTracker()

	child, err := New(&meteredLLM{
		name:   "child-model",
		script: [][]*adkmodel.LLMResponse{{usedText("child answer", 400, 60)}},
	},
		WithName("child"),
		WithEventLog(h),
		WithSession("u", "child"),
		WithUsageTracker(tracker),
	)
	if err != nil {
		t.Fatalf("New child: %v", err)
	}
	// Parent deliberately has no tracker, so ParentTracker is nil.
	runParentDelegating(t, child, nil, h)

	if tot := tracker.Totals(); tot.InputTokens != 400 {
		t.Errorf("inner tracker = %d in, want 400 (fallback not taken)", tot.InputTokens)
	}
}

func keysOf(m map[string]usage.Totals) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
