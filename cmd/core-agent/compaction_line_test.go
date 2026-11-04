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

package main

import (
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	adktool "google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/models/mock"
)

func ptr(v float64) *float64 { return &v }

// The token figure is computed from the model's REAL window, which is the
// mistake the line exists to prevent: 0.021 of gemini-3.7-flash's
// 1,048,576-token window is 22,020, not the 21,000 a reader assumes.
func TestTheCompactionBootLineStatesTokensFromTheRealWindow(t *testing.T) {
	t.Parallel()
	dc := compose.BuildCompactor(config.CompactionConfig{Threshold: ptr(0.021)}).(*agent.DefaultCompactor)
	got := compactionBootLine(dc, "gemini-3.7-flash")
	if want := "fires at 0.021 of gemini-3.7-flash's 1048576-token window = 22020 tokens"; !strings.Contains(got, want) {
		t.Fatalf("boot line = %q\nwant it to contain %q", got, want)
	}
}

// The line reports the threshold the daemon will actually use. With
// nothing configured that is the frontier tier default, not the 0.85
// fallback by coincidence of value — so check a mid-tier model, whose
// default (0.65) differs from the fallback.
func TestTheCompactionBootLineReportsTheEffectiveTierThreshold(t *testing.T) {
	t.Parallel()
	dc := compose.BuildCompactor(config.CompactionConfig{}).(*agent.DefaultCompactor)
	got := compactionBootLine(dc, "gemini-3.5-flash")
	if !strings.Contains(got, "fires at 0.65 of gemini-3.5-flash's") {
		t.Fatalf("boot line = %q; want the mid-tier default 0.65, the threshold ShouldCompact will use", got)
	}
}

// An uncatalogued model does not stop compaction: the runtime assumes a
// 128,000-token window (agent.AssumedContextWindowSize). The line states
// that figure — the one the daemon acts on — not "unknown", which the
// first draft printed and which the review caught as the one case where
// line and decision disagreed.
func TestTheCompactionBootLineStatesTheAssumedWindowForAnUnknownModel(t *testing.T) {
	t.Parallel()
	dc := compose.BuildCompactor(config.CompactionConfig{}).(*agent.DefaultCompactor)
	got := compactionBootLine(dc, "some-unlisted-model")
	if want := "assumed 128000-token window = 108800 tokens"; !strings.Contains(got, want) {
		t.Fatalf("boot line = %q\nwant it to contain %q (0.85 of the assumed window)", got, want)
	}
}

// #1376: When a model has a long-context pricing cliff below tier × window
// (e.g. claude-haiku-5-5 at 100k vs 0.35×1M=350k, or gemini-2.5-pro at
// 200k vs 0.65×1.048M=681,574), compaction fires at cliff - margin and the
// startup line shows both figures and that the long-context cliff won.
func TestCompactionBootLine_LongContextCliffWins(t *testing.T) {
	t.Parallel()
	dc := compose.BuildCompactor(config.CompactionConfig{}).(*agent.DefaultCompactor)

	haikuLine := compactionBootLine(dc, "claude-haiku-5-5")
	if want := "1000000-token window = 68000 tokens (long-context cliff 100000 - 32000 margin < 0.35 tier window = 350000 tokens)"; !strings.Contains(haikuLine, want) {
		t.Errorf("claude-haiku-5-5 boot line = %q\nwant substring %q", haikuLine, want)
	}

	proLine := compactionBootLine(dc, "gemini-2.5-pro")
	if want := "1048576-token window = 168000 tokens (long-context cliff 200000 - 32000 margin < 0.65 tier window = 681574 tokens)"; !strings.Contains(proLine, want) {
		t.Errorf("gemini-2.5-pro boot line = %q\nwant substring %q", proLine, want)
	}
}

// #1376: When an operator threshold puts tier × window below the model's
// long-context cliff minus margin, tier × window wins and the startup line
// states that.
func TestCompactionBootLine_TierWindowBeatsLongContextCliff(t *testing.T) {
	t.Parallel()
	dc := compose.BuildCompactor(config.CompactionConfig{Threshold: ptr(0.021)}).(*agent.DefaultCompactor)
	got := compactionBootLine(dc, "gemini-2.5-pro")
	if want := "fires at 0.021 of gemini-2.5-pro's 1048576-token window = 22020 tokens (tier window < 200000 long-context cliff - 32000 margin = 168000 tokens)"; !strings.Contains(got, want) {
		t.Errorf("boot line = %q\nwant substring %q", got, want)
	}
}

func makeBareBuiltinTools(t *testing.T) []adktool.Tool {
	t.Helper()
	var out []adktool.Tool
	for _, name := range []string{"read_file", "fetch_url", "grep", "list_dir", "glob"} {
		tl, err := functiontool.New(functiontool.Config{
			Name:        name,
			Description: "stub " + name,
		}, func(_ adkagent.Context, _ struct{}) (struct{}, error) {
			return struct{}{}, nil
		})
		if err != nil {
			t.Fatalf("functiontool.New(%s): %v", name, err)
		}
		out = append(out, tl)
	}
	return out
}

// #1377: When models.ResolveSmallModel returns "" (a provider with no
// cheap-tier default and no --agentic-small-model override), resolveAgenticTools
// skips registering the agentic_* cost-lever wrappers and logs why.
func TestResolveAgenticTools_SkipsWhenNoSmallModelResolved(t *testing.T) {
	t.Parallel()
	builtins := makeBareBuiltinTools(t)
	prov := mock.NewEcho()

	agTools, bootLine, err := resolveAgenticTools(builtins, func() *agent.Agent { return nil }, prov, "")
	if err != nil {
		t.Fatalf("resolveAgenticTools: %v", err)
	}
	if len(agTools) != 0 {
		t.Errorf("got %d agentic tools, want 0 when ResolveSmallModel returns empty", len(agTools))
	}
	if !strings.Contains(bootLine, "agentic subtasks: off") || !strings.Contains(bootLine, "--agentic-small-model") {
		t.Errorf("bootLine = %q, want it to state agentic subtasks are off and mention --agentic-small-model", bootLine)
	}

	// Explicit --agentic-small-model override still registers the wrappers.
	agToolsOverride, bootLineOverride, err := resolveAgenticTools(builtins, func() *agent.Agent { return nil }, prov, "custom-small")
	if err != nil {
		t.Fatalf("resolveAgenticTools(override): %v", err)
	}
	if len(agToolsOverride) == 0 {
		t.Errorf("expected agentic tools when --agentic-small-model override is set")
	}
	if want := "agentic subtasks: custom-small (operator override)"; bootLineOverride != want {
		t.Errorf("bootLineOverride = %q, want %q", bootLineOverride, want)
	}
}
