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

// Package tools provides ADK-side helpers for wiring tools into the
// agent. Today it holds only the GateToolset wrapper that bridges
// permissions.Gate to ADK's tool.Toolset interface; consumer projects
// add their own tool implementations on top.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/pkg/permissions"
)

// runnableTool is the unexported ADK interface every callable tool
// satisfies. We re-declare it locally so we can both type-assert and
// implement against it for our gating wrapper.
type runnableTool interface {
	Declaration() *genai.FunctionDeclaration
	Run(ctx adktool.Context, args any) (result map[string]any, err error)
}

// GateToolset wraps ts so every tool inside it goes through the
// permission gate before running. namespace is the policy bucket
// used for allow/deny matching ("mcp", "skill", etc.); a nil gate
// returns ts unchanged.
//
// Reuses the existing permission UX: in `ask` mode the same prompt
// surface is used; in `allow` mode the same allowlist patterns apply
// (now with `mcp:<tool>` / `skill:<tool>` keys); in `yolo` mode the
// call proceeds. The bash denylist does NOT apply to non-bash tools.
func GateToolset(ts adktool.Toolset, gate *permissions.Gate, namespace string) adktool.Toolset {
	if ts == nil || gate == nil {
		return ts
	}
	return &gatedToolset{inner: ts, gate: gate, namespace: namespace}
}

type gatedToolset struct {
	inner     adktool.Toolset
	gate      *permissions.Gate
	namespace string
}

func (g *gatedToolset) Name() string { return g.inner.Name() }

func (g *gatedToolset) Tools(ctx agent.ReadonlyContext) ([]adktool.Tool, error) {
	upstream, err := g.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]adktool.Tool, 0, len(upstream))
	for _, t := range upstream {
		out = append(out, &gatedTool{inner: t, gate: g.gate, namespace: g.namespace})
	}
	return out, nil
}

type gatedTool struct {
	inner     adktool.Tool
	gate      *permissions.Gate
	namespace string
}

func (gt *gatedTool) Name() string        { return gt.inner.Name() }
func (gt *gatedTool) Description() string { return gt.inner.Description() }
func (gt *gatedTool) IsLongRunning() bool { return gt.inner.IsLongRunning() }

// Declaration delegates to the underlying tool when it's runnable.
// Returns nil for tools that don't expose a declaration (which the
// runner already handles).
func (gt *gatedTool) Declaration() *genai.FunctionDeclaration {
	if rn, ok := gt.inner.(runnableTool); ok {
		return rn.Declaration()
	}
	return nil
}

// ProcessRequest satisfies ADK's internal toolinternal.RequestProcessor
// interface so this wrapper survives ADK's request-preprocess pass
// (`internal/llminternal/base_flow.go` requires every tool in
// `f.Tools` to implement it). We pack `gt` (the wrapper) — not
// `gt.inner` — so ADK's call-back dispatch routes through the gate
// instead of bypassing it.
func (gt *gatedTool) ProcessRequest(ctx adktool.Context, req *model.LLMRequest) error {
	return PackTool(req, gt)
}

// Run consults the gate before delegating to the underlying tool.
// The args are JSON-marshalled into a short summary so the user-facing
// prompt has context.
func (gt *gatedTool) Run(ctx adktool.Context, args any) (map[string]any, error) {
	rn, ok := gt.inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tools: gated tool %q is not runnable", gt.inner.Name())
	}
	if err := gt.gate.CheckGeneric(context.Background(), gt.namespace, summarizeRequest(gt.inner.Name(), args)); err != nil {
		return nil, err
	}
	return rn.Run(ctx, args)
}

func summarizeRequest(name string, args any) string {
	if args == nil {
		return name
	}
	body, err := json.Marshal(args)
	if err != nil {
		return name
	}
	const max = 200
	if len(body) > max {
		body = append(body[:max], []byte("...")...)
	}
	return name + " " + string(body)
}
