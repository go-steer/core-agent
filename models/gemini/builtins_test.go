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

package gemini

import (
	"context"
	"iter"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

func TestDefaultBuiltinTools(t *testing.T) {
	t.Parallel()
	d := DefaultBuiltinTools()
	if !d.GoogleSearch {
		t.Errorf("GoogleSearch should be on by default")
	}
	if !d.URLContext {
		t.Errorf("URLContext should be on by default")
	}
	if d.CodeExecution {
		t.Errorf("CodeExecution should be OFF by default — opt-in only")
	}
}

func TestBuiltinTools_AsTools_Default(t *testing.T) {
	t.Parallel()
	tools := DefaultBuiltinTools().asTools()
	if len(tools) != 2 {
		t.Fatalf("default produces 2 tools, got %d", len(tools))
	}
	if tools[0].GoogleSearch == nil {
		t.Errorf("tools[0] should be GoogleSearch")
	}
	if tools[1].URLContext == nil {
		t.Errorf("tools[1] should be URLContext")
	}
}

func TestBuiltinTools_AsTools_AllOn(t *testing.T) {
	t.Parallel()
	tools := BuiltinTools{
		GoogleSearch:  true,
		URLContext:    true,
		CodeExecution: true,
	}.asTools()
	if len(tools) != 3 {
		t.Fatalf("all-on produces 3 tools, got %d", len(tools))
	}
	if tools[2].CodeExecution == nil {
		t.Errorf("tools[2] should be CodeExecution")
	}
}

func TestBuiltinTools_AsTools_Empty(t *testing.T) {
	t.Parallel()
	tools := BuiltinTools{}.asTools()
	if len(tools) != 0 {
		t.Fatalf("zero-value should produce no tools, got %d", len(tools))
	}
}

func TestNewAPIKey_AppliesDefaults(t *testing.T) {
	t.Parallel()
	p, err := NewAPIKey("test-key")
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if !p.builtins.GoogleSearch || !p.builtins.URLContext {
		t.Errorf("defaults not applied: %+v", p.builtins)
	}
	if p.builtins.CodeExecution {
		t.Errorf("CodeExecution should be off by default")
	}
}

func TestNewAPIKey_OptionsOverrideDefaults(t *testing.T) {
	t.Parallel()
	p, err := NewAPIKey("test-key",
		WithGoogleSearch(false),
		WithCodeExecution(true),
	)
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if p.builtins.GoogleSearch {
		t.Errorf("WithGoogleSearch(false) didn't take")
	}
	if !p.builtins.URLContext {
		t.Errorf("URLContext should still be on (default)")
	}
	if !p.builtins.CodeExecution {
		t.Errorf("WithCodeExecution(true) didn't take")
	}
}

func TestNewAPIKey_WithBuiltinTools_ReplacesWholesale(t *testing.T) {
	t.Parallel()
	p, err := NewAPIKey("test-key",
		WithBuiltinTools(BuiltinTools{CodeExecution: true}),
	)
	if err != nil {
		t.Fatalf("NewAPIKey: %v", err)
	}
	if p.builtins.GoogleSearch || p.builtins.URLContext {
		t.Errorf("WithBuiltinTools should replace wholesale, got: %+v", p.builtins)
	}
	if !p.builtins.CodeExecution {
		t.Errorf("CodeExecution: true didn't survive")
	}
}

// fakeLLM records the most recent request it was asked to handle so
// tests can assert how the wrapper mutates Config.
type fakeLLM struct {
	last *adkmodel.LLMRequest
}

func (f *fakeLLM) Name() string { return "fake" }

func (f *fakeLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	f.last = req
	return func(yield func(*adkmodel.LLMResponse, error) bool) {}
}

func TestBuiltinsLLM_InjectsIntoConfigTools(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{}
	wrapped := &builtinsLLM{
		inner:    fake,
		builtins: DefaultBuiltinTools().asTools(),
	}
	req := &adkmodel.LLMRequest{}
	for range wrapped.GenerateContent(context.Background(), req, false) {
		// drain
	}
	if fake.last.Config == nil {
		t.Fatalf("Config should have been initialized by the wrapper")
	}
	if len(fake.last.Config.Tools) != 2 {
		t.Fatalf("expected 2 injected tools, got %d", len(fake.last.Config.Tools))
	}
}

func TestBuiltinsLLM_PreservesExistingTools(t *testing.T) {
	t.Parallel()
	fake := &fakeLLM{}
	wrapped := &builtinsLLM{
		inner:    fake,
		builtins: BuiltinTools{GoogleSearch: true}.asTools(),
	}
	// Caller already supplied a function-declaration tool. The wrapper
	// must append, not replace.
	userTool := &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{{Name: "my_func"}},
	}
	req := &adkmodel.LLMRequest{
		Config: &genai.GenerateContentConfig{Tools: []*genai.Tool{userTool}},
	}
	for range wrapped.GenerateContent(context.Background(), req, false) {
	}
	if len(fake.last.Config.Tools) != 2 {
		t.Fatalf("expected 1 user tool + 1 injected, got %d", len(fake.last.Config.Tools))
	}
	if fake.last.Config.Tools[0] != userTool {
		t.Errorf("user tool should remain at index 0")
	}
	if fake.last.Config.Tools[1].GoogleSearch == nil {
		t.Errorf("injected tool should be GoogleSearch")
	}
}

func TestBuiltinsLLM_NameDelegates(t *testing.T) {
	t.Parallel()
	wrapped := &builtinsLLM{inner: &fakeLLM{}}
	if wrapped.Name() != "fake" {
		t.Errorf("Name should delegate to inner LLM")
	}
}
