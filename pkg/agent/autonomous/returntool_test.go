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

package autonomous

import (
	"strings"
	"testing"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/toolconfirmation"
)

// runnableTool mirrors ADK's unexported runnable interface so a test
// can invoke a built function tool directly. Same shape the pkg/tools
// tests use.
type runnableTool interface {
	Run(ctx tool.Context, args any) (map[string]any, error)
}

// stubToolContext satisfies tool.Context far enough for functiontool's
// Run, which dereferences ToolConfirmation() before dispatching. The
// embedded nil interface panics on anything else, which is the point:
// these tools must not touch the rest of the context.
type stubToolContext struct{ tool.Context }

func (stubToolContext) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

// call invokes a built tool by name with the given args, returning the
// tool's result map. Fails the test if the name isn't among tools.
func call(t *testing.T, tools []tool.Tool, name string, args map[string]any) map[string]any {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() != name {
			continue
		}
		r, ok := tl.(runnableTool)
		if !ok {
			t.Fatalf("tool %q is not runnable (%T)", name, tl)
		}
		got, err := r.Run(stubToolContext{}, args)
		if err != nil {
			t.Fatalf("%s: Run: %v", name, err)
		}
		return got
	}
	t.Fatalf("tool %q not built; got %v", name, toolNames(tools))
	return nil
}

func toolNames(tools []tool.Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tl := range tools {
		out = append(out, tl.Name())
	}
	return out
}

// TestBuildDoneTools_DefaultIsUnchangedLifecycleTool pins the opt-in
// nature of #728: a consumer that never calls WithReturnTool keeps the
// historical report_done(state, detail) tool exactly as before.
func TestBuildDoneTools_DefaultIsUnchangedLifecycleTool(t *testing.T) {
	t.Parallel()
	cfg := defaultAutoConfig()
	doneCh := make(chan string, 1)
	tools, err := buildDoneTools(&cfg, doneCh)
	if err != nil {
		t.Fatalf("buildDoneTools: %v", err)
	}
	if got := toolNames(tools); len(got) != 1 || got[0] != "report_done" {
		t.Fatalf("default done tools = %v, want [report_done]", got)
	}
	call(t, tools, "report_done", map[string]any{"state": "done", "detail": "all good"})
	select {
	case got := <-doneCh:
		if got != "all good" {
			t.Errorf("signalled detail = %q, want %q", got, "all good")
		}
	default:
		t.Error("lifecycle done tool did not signal")
	}
}

// TestBuildDoneTools_ReturnToolAndAliasesAllSignal is the core #728
// regression: every alias must actually END the run, not merely ack.
// Before the fix, report_completed pushed an alert and returned ok
// while the loop kept re-driving the model, and mark_task_done was not
// registered at all — so a subagent that had the answer had no reliable
// way to hand it back.
func TestBuildDoneTools_ReturnToolAndAliasesAllSignal(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"return_result", "report_done", "report_completed", "mark_task_done"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := defaultAutoConfig()
			WithReturnTool(ReturnToolConfig{
				Aliases: []string{"report_done", "report_completed", "mark_task_done"},
			})(&cfg)
			doneCh := make(chan string, 1)
			tools, err := buildDoneTools(&cfg, doneCh)
			if err != nil {
				t.Fatalf("buildDoneTools: %v", err)
			}
			call(t, tools, name, map[string]any{"result": "root cause: bad image tag"})
			select {
			case got := <-doneCh:
				if got != "root cause: bad image tag" {
					t.Errorf("signalled result = %q", got)
				}
			default:
				t.Errorf("%s did not signal the run to end", name)
			}
		})
	}
}

// TestBuildDoneTools_AcceptsDetailAsDeprecatedAlias covers a model
// carrying a report_done(detail=...) prior from training: it must still
// deliver its payload rather than silently returning nothing through a
// field the new schema doesn't offer.
func TestBuildDoneTools_AcceptsDetailAsDeprecatedAlias(t *testing.T) {
	t.Parallel()
	cfg := defaultAutoConfig()
	WithReturnTool(ReturnToolConfig{Aliases: []string{"report_done"}})(&cfg)
	doneCh := make(chan string, 1)
	tools, err := buildDoneTools(&cfg, doneCh)
	if err != nil {
		t.Fatalf("buildDoneTools: %v", err)
	}
	call(t, tools, "report_done", map[string]any{"state": "done", "detail": "findings here"})
	select {
	case got := <-doneCh:
		if got != "findings here" {
			t.Errorf("signalled = %q, want the detail payload", got)
		}
	default:
		t.Error("detail-only call did not signal; a legacy-shaped call must still return")
	}
}

// TestBuildDoneTools_RejectsEmptyResultWithoutSignalling keeps the
// empty return — the exact failure this tool exists to prevent — from
// terminating the run with nothing in hand. The model gets one
// corrective ack instead.
func TestBuildDoneTools_RejectsEmptyResultWithoutSignalling(t *testing.T) {
	t.Parallel()
	cfg := defaultAutoConfig()
	WithReturnTool(ReturnToolConfig{})(&cfg)
	doneCh := make(chan string, 1)
	tools, err := buildDoneTools(&cfg, doneCh)
	if err != nil {
		t.Fatalf("buildDoneTools: %v", err)
	}
	got := call(t, tools, "return_result", map[string]any{"result": "   "})
	ack, _ := got["ack"].(string)
	if !strings.HasPrefix(ack, "rejected:") {
		t.Errorf("ack = %q (full result %v), want a rejection", ack, got)
	}
	select {
	case v := <-doneCh:
		t.Errorf("empty return signalled completion with %q", v)
	default:
	}
}

// TestBuildDoneTools_DedupesNamesAndHonorsOverrides covers the two
// naming knobs: an alias colliding with the primary name must not
// register twice (ADK rejects duplicate function declarations), and
// WithDoneToolName stays the collision escape hatch on this path too.
func TestBuildDoneTools_DedupesNamesAndHonorsOverrides(t *testing.T) {
	t.Parallel()
	t.Run("dedupe", func(t *testing.T) {
		t.Parallel()
		cfg := defaultAutoConfig()
		WithReturnTool(ReturnToolConfig{
			Name:    "return_result",
			Aliases: []string{"return_result", "report_done", "report_done", ""},
		})(&cfg)
		tools, err := buildDoneTools(&cfg, make(chan string, 1))
		if err != nil {
			t.Fatalf("buildDoneTools: %v", err)
		}
		want := []string{"return_result", "report_done"}
		got := toolNames(tools)
		if len(got) != len(want) {
			t.Fatalf("tool names = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("tool names = %v, want %v", got, want)
			}
		}
	})
	t.Run("done-tool-name-override", func(t *testing.T) {
		t.Parallel()
		cfg := defaultAutoConfig()
		WithDoneToolName("finish_up")(&cfg)
		WithReturnTool(ReturnToolConfig{})(&cfg)
		tools, err := buildDoneTools(&cfg, make(chan string, 1))
		if err != nil {
			t.Fatalf("buildDoneTools: %v", err)
		}
		if got := toolNames(tools); len(got) != 1 || got[0] != "finish_up" {
			t.Fatalf("tool names = %v, want [finish_up]", got)
		}
	})
}

// TestWithReturnTool_CopiesAliases guards against a caller mutating the
// slice it passed in after the fact.
func TestWithReturnTool_CopiesAliases(t *testing.T) {
	t.Parallel()
	aliases := []string{"report_done"}
	cfg := defaultAutoConfig()
	WithReturnTool(ReturnToolConfig{Aliases: aliases})(&cfg)
	aliases[0] = "mutated"
	if got := cfg.returnTool.Aliases[0]; got != "report_done" {
		t.Errorf("alias = %q, want the value captured at option time", got)
	}
}
