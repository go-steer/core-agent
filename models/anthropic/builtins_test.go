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

package anthropic

import (
	"testing"

	"google.golang.org/genai"
)

func TestDefaultBuiltinTools_AllOff(t *testing.T) {
	t.Parallel()
	d := DefaultBuiltinTools()
	if d.WebSearch {
		t.Errorf("WebSearch should be OFF by default — opt-in due to per-search billing")
	}
}

func TestBuiltinTools_AsAnthropicTools_Empty(t *testing.T) {
	t.Parallel()
	if got := (BuiltinTools{}).asAnthropicTools(); len(got) != 0 {
		t.Errorf("zero-value should produce no tools, got %d", len(got))
	}
}

func TestBuiltinTools_AsAnthropicTools_WebSearchOn(t *testing.T) {
	t.Parallel()
	got := BuiltinTools{WebSearch: true}.asAnthropicTools()
	if len(got) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(got))
	}
	if got[0].OfWebSearchTool20260209 == nil {
		t.Errorf("expected OfWebSearchTool20260209 to be set, got %+v", got[0])
	}
}

func TestNew_AppliesBuiltinDefaults(t *testing.T) {
	t.Parallel()
	p, err := New("test-key")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.builtins.WebSearch {
		t.Errorf("WebSearch should be off by default")
	}
}

func TestNew_WithWebSearch(t *testing.T) {
	t.Parallel()
	p, err := New("test-key", WithWebSearch(true))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !p.builtins.WebSearch {
		t.Errorf("WithWebSearch(true) didn't take")
	}
}

func TestNew_WithBuiltinTools_ReplacesWholesale(t *testing.T) {
	t.Parallel()
	// First set webSearch via the per-tool helper, then expect a
	// wholesale replacement to wipe it back to zero-value.
	p, err := New("test-key",
		WithWebSearch(true),
		WithBuiltinTools(BuiltinTools{}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.builtins.WebSearch {
		t.Errorf("WithBuiltinTools should replace wholesale; WebSearch still on")
	}
}

func TestBuildParams_AppendsWebSearchToTools(t *testing.T) {
	t.Parallel()
	cfg := &genai.GenerateContentConfig{
		Tools: []*genai.Tool{{
			FunctionDeclarations: []*genai.FunctionDeclaration{{
				Name: "search", Description: "user-defined",
			}},
		}},
	}
	p, err := buildParams("claude-opus-4-7", nil, cfg, false, BuiltinTools{WebSearch: true})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(p.Tools) != 2 {
		t.Fatalf("expected 2 tools (1 function decl + 1 web_search), got %d", len(p.Tools))
	}
	// Function declarations come first; web_search is appended.
	if p.Tools[0].OfTool == nil || p.Tools[0].OfTool.Name != "search" {
		t.Errorf("first tool should be the function decl, got %+v", p.Tools[0])
	}
	if p.Tools[1].OfWebSearchTool20260209 == nil {
		t.Errorf("second tool should be web_search, got %+v", p.Tools[1])
	}
}

func TestBuildParams_NoBuiltinsWhenAllOff(t *testing.T) {
	t.Parallel()
	cfg := &genai.GenerateContentConfig{
		Tools: []*genai.Tool{{
			FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "search"}},
		}},
	}
	p, err := buildParams("claude-opus-4-7", nil, cfg, false, BuiltinTools{})
	if err != nil {
		t.Fatalf("buildParams: %v", err)
	}
	if len(p.Tools) != 1 {
		t.Errorf("expected 1 tool (function decl only), got %d", len(p.Tools))
	}
}
