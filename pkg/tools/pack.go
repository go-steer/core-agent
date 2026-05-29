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
	"fmt"

	"google.golang.org/adk/model"
	"google.golang.org/genai"
)

// Packable is the minimum surface PackTool needs from a tool. Both
// the ADK-internal toolinternal.RequestProcessor contract and our own
// wrapper types satisfy it.
type Packable interface {
	Name() string
	Declaration() *genai.FunctionDeclaration
}

// PackTool registers t with req so ADK can dispatch tool calls back
// to it (via req.Tools[name]) and so the model sees its declaration
// (via req.Config.Tools[*].FunctionDeclarations).
//
// This is a re-implementation of
// google.golang.org/adk/internal/toolinternal/toolutils.PackTool —
// that package is internal to ADK and we can't import it. The
// algorithm is identical: register the tool in req.Tools, then
// either append the declaration onto an existing genai.Tool that
// already carries FunctionDeclarations, or create a new one.
//
// Wrappers that forward methods to an inner tool (e.g. an MCP
// namespace prefixer or a permission-gate wrapper) MUST call this
// with themselves rather than delegating to inner.ProcessRequest.
// Packing the wrapper ensures (a) the model sees the wrapper's
// renamed Declaration and (b) ADK's call-back dispatch hits the
// wrapper's Run, preserving namespacing and gating.
func PackTool(req *model.LLMRequest, t Packable) error {
	if req == nil {
		return fmt.Errorf("tools.PackTool: nil request")
	}
	if t == nil {
		return fmt.Errorf("tools.PackTool: nil tool")
	}
	if req.Tools == nil {
		req.Tools = make(map[string]any)
	}
	name := t.Name()
	if _, ok := req.Tools[name]; ok {
		return fmt.Errorf("tools.PackTool: duplicate tool: %q", name)
	}
	req.Tools[name] = t
	decl := t.Declaration()
	if decl == nil {
		return nil
	}
	if req.Config == nil {
		req.Config = &genai.GenerateContentConfig{}
	}
	for _, gt := range req.Config.Tools {
		if gt != nil && gt.FunctionDeclarations != nil {
			gt.FunctionDeclarations = append(gt.FunctionDeclarations, decl)
			return nil
		}
	}
	req.Config.Tools = append(req.Config.Tools, &genai.Tool{
		FunctionDeclarations: []*genai.FunctionDeclaration{decl},
	})
	return nil
}
