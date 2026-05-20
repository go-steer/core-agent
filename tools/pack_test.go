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

package tools

import (
	"strings"
	"testing"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// stubPackable is a minimal Packable for PackTool tests.
type stubPackable struct {
	name string
	decl *genai.FunctionDeclaration
}

func (s stubPackable) Name() string                            { return s.name }
func (s stubPackable) Declaration() *genai.FunctionDeclaration { return s.decl }

func TestPackTool_RegistersToolAndDeclaration(t *testing.T) {
	t.Parallel()
	req := &model.LLMRequest{}
	tool := stubPackable{
		name: "demo_echo",
		decl: &genai.FunctionDeclaration{Name: "demo_echo", Description: "echo"},
	}
	if err := PackTool(req, tool); err != nil {
		t.Fatalf("PackTool: %v", err)
	}
	got, ok := req.Tools["demo_echo"]
	if !ok {
		t.Fatalf("req.Tools missing entry; have %v", req.Tools)
	}
	if got != tool {
		t.Errorf("req.Tools[demo_echo] = %v, want wrapper %v", got, tool)
	}
	if req.Config == nil || len(req.Config.Tools) == 0 {
		t.Fatalf("declaration not added to req.Config.Tools")
	}
	decls := req.Config.Tools[0].FunctionDeclarations
	if len(decls) != 1 || decls[0].Name != "demo_echo" {
		t.Errorf("FunctionDeclarations = %+v, want one decl named demo_echo", decls)
	}
}

func TestPackTool_AppendsToExistingFunctionTool(t *testing.T) {
	t.Parallel()
	req := &model.LLMRequest{}
	a := stubPackable{name: "a", decl: &genai.FunctionDeclaration{Name: "a"}}
	b := stubPackable{name: "b", decl: &genai.FunctionDeclaration{Name: "b"}}
	if err := PackTool(req, a); err != nil {
		t.Fatalf("PackTool a: %v", err)
	}
	if err := PackTool(req, b); err != nil {
		t.Fatalf("PackTool b: %v", err)
	}
	if got := len(req.Config.Tools); got != 1 {
		t.Fatalf("expected one genai.Tool aggregating both decls, got %d", got)
	}
	decls := req.Config.Tools[0].FunctionDeclarations
	if len(decls) != 2 {
		t.Errorf("expected two function declarations, got %d", len(decls))
	}
}

func TestPackTool_RejectsDuplicate(t *testing.T) {
	t.Parallel()
	req := &model.LLMRequest{}
	t1 := stubPackable{name: "dup", decl: &genai.FunctionDeclaration{Name: "dup"}}
	t2 := stubPackable{name: "dup", decl: &genai.FunctionDeclaration{Name: "dup"}}
	if err := PackTool(req, t1); err != nil {
		t.Fatalf("first PackTool: %v", err)
	}
	err := PackTool(req, t2)
	if err == nil || !strings.Contains(err.Error(), "duplicate tool") {
		t.Fatalf("expected duplicate-tool error, got %v", err)
	}
}

func TestPackTool_NilDeclarationStillRegisters(t *testing.T) {
	t.Parallel()
	req := &model.LLMRequest{}
	tool := stubPackable{name: "no_decl", decl: nil}
	if err := PackTool(req, tool); err != nil {
		t.Fatalf("PackTool: %v", err)
	}
	if _, ok := req.Tools["no_decl"]; !ok {
		t.Errorf("req.Tools[no_decl] missing")
	}
	if req.Config != nil && len(req.Config.Tools) > 0 {
		t.Errorf("nil declaration should not touch req.Config.Tools, got %+v", req.Config.Tools)
	}
}

func TestPackTool_NilArgs(t *testing.T) {
	t.Parallel()
	if err := PackTool(nil, stubPackable{name: "x"}); err == nil {
		t.Error("nil request: expected error")
	}
	if err := PackTool(&model.LLMRequest{}, nil); err == nil {
		t.Error("nil tool: expected error")
	}
}
