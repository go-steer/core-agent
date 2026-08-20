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
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// fakeInnerTool implements the bits of adktool.Tool the wrapper
// touches. It does NOT itself implement RequestProcessor — the
// gate wrapper must supply its own.
type fakeInnerTool struct {
	name string
	decl *genai.FunctionDeclaration
}

func (f *fakeInnerTool) Name() string                            { return f.name }
func (f *fakeInnerTool) Description() string                     { return "fake" }
func (f *fakeInnerTool) IsLongRunning() bool                     { return false }
func (f *fakeInnerTool) Declaration() *genai.FunctionDeclaration { return f.decl }
func (f *fakeInnerTool) Run(_ adktool.Context, _ any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

func TestGatedTool_ProcessRequest_PacksWrapperNotInner(t *testing.T) {
	t.Parallel()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	inner := &fakeInnerTool{
		name: "list_clusters",
		decl: &genai.FunctionDeclaration{Name: "list_clusters", Description: "list clusters"},
	}
	gt := &gatedTool{inner: inner, gate: gate, namespace: "mcp"}

	req := &model.LLMRequest{}
	if err := gt.ProcessRequest(nil, req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}

	// The wrapper packs ITSELF — not the inner — so ADK's call-back
	// dispatch on req.Tools[name] hits the wrapper's Run (which
	// checks the gate) instead of bypassing it.
	got, ok := req.Tools["list_clusters"]
	if !ok {
		t.Fatalf("req.Tools missing wrapper entry; have %v", req.Tools)
	}
	if got != gt {
		t.Errorf("req.Tools[list_clusters] should be the gated wrapper, got %T %v", got, got)
	}
	if got, ok := req.Tools["list_clusters"].(*gatedTool); !ok || got.inner != inner {
		t.Errorf("packed entry is not the *gatedTool we constructed")
	}

	// Declaration carries the inner's name (we don't rename in the
	// gate layer — that's the namespace wrapper's job). Verify it
	// surfaces correctly.
	if req.Config == nil || len(req.Config.Tools) == 0 {
		t.Fatalf("declaration not added to req.Config.Tools")
	}
	if got := req.Config.Tools[0].FunctionDeclarations[0].Name; got != "list_clusters" {
		t.Errorf("declared name = %q, want list_clusters", got)
	}
}

// readOnlyInnerTool is a namespaced tool that declares its dispatch
// class — the shape an MCP tool has once its server carries
// `"read_only": true` in mcp.json (#693).
type readOnlyInnerTool struct {
	fakeInnerTool
	readOnly bool
}

func (r *readOnlyInnerTool) ReadOnlyHint() bool { return r.readOnly }

// TestGatedTool_PlanFirstExemptsReadOnlyCalls is the plan-first half of
// #693. The gate can't classify a namespaced call on its own: the
// toolName it sees is the namespace ("mcp"), never the underlying tool,
// and "mcp" is deliberately absent from planExemptTools because a
// server can expose anything. So the wrapper — which holds the live
// tool — classifies and routes to the read-only sibling.
//
// Mode is Yolo on purpose: plan-first is checked BEFORE mode, so Yolo
// isolates the plan-first denial from every other reason a call could
// be refused.
func TestGatedTool_PlanFirstExemptsReadOnlyCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		readOnly  bool
		wantAllow bool
	}{
		{"read-only server call is research, not mutation", true, true},
		{"undeclared call stays gated", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gate := permissions.New(permissions.Options{
				Mode:                permissions.ModeYolo,
				RequirePlanArtifact: true,
			})
			inner := &readOnlyInnerTool{
				fakeInnerTool: fakeInnerTool{name: "get_pod"},
				readOnly:      tc.readOnly,
			}
			gt := &gatedTool{inner: inner, gate: gate, namespace: "mcp"}

			_, err := gt.Run(&planToolCtx{Context: context.Background()}, map[string]any{})
			if tc.wantAllow && err != nil {
				t.Errorf("read-only call denied under plan-first: %v", err)
			}
			if !tc.wantAllow {
				if err == nil {
					t.Fatal("mutating call allowed before a plan was recorded")
				}
				if !strings.Contains(err.Error(), "plan-first") {
					t.Errorf("denial should cite plan-first, got %v", err)
				}
			}
		})
	}
}

// TestGatedTool_ReadOnlyDoesNotBypassPolicy pins the blast radius. The
// classification relaxes plan-first and NOTHING else — a deny pattern
// still denies a read-only MCP tool, or `read_only: true` would become
// an accidental allowlist bypass.
func TestGatedTool_ReadOnlyDoesNotBypassPolicy(t *testing.T) {
	t.Parallel()
	policy, err := permissions.NewPolicy(nil, []string{"mcp:*"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo, Policy: policy})
	inner := &readOnlyInnerTool{
		fakeInnerTool: fakeInnerTool{name: "get_pod"},
		readOnly:      true,
	}
	gt := &gatedTool{inner: inner, gate: gate, namespace: "mcp"}

	if _, err := gt.Run(&planToolCtx{Context: context.Background()}, map[string]any{}); err == nil {
		t.Fatal("a read-only tool escaped a deny policy — read_only is not an allowlist")
	}
}
