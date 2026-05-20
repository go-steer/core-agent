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

package mcp

import (
	"context"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	coretools "github.com/go-steer/core-agent/tools"
)

// runnable is the unexported interface ADK's runner expects from
// tools that can actually be called. mcptoolset returns objects that
// satisfy it; we re-declare it locally so we can both type-assert and
// implement against it without importing an unexported symbol.
type runnable interface {
	Declaration() *genai.FunctionDeclaration
	Run(ctx tool.Context, args any) (result map[string]any, err error)
}

// namespacedToolset wraps an upstream Toolset and returns each Tool
// with its name prefixed by `<prefix>_`. This both:
//   - prevents collisions with built-in tool names (e.g. an MCP
//     filesystem server's `read_file` would otherwise duplicate a
//     consumer's own `read_file`)
//   - keeps function names within Gemini's `[A-Za-z0-9_]{1,64}`
//     constraint (so `.` as a separator is not an option)
type namespacedToolset struct {
	inner  tool.Toolset
	prefix string
}

// withNamespace prefixes every tool name in inner with prefix + "_".
// Returns inner unchanged if prefix is empty.
func withNamespace(inner tool.Toolset, prefix string) tool.Toolset {
	if inner == nil || prefix == "" {
		return inner
	}
	return &namespacedToolset{inner: inner, prefix: sanitizePrefix(prefix)}
}

func (n *namespacedToolset) Name() string {
	if base := n.inner.Name(); base != "" {
		return n.prefix + "_" + base
	}
	return n.prefix
}

func (n *namespacedToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	upstream, err := n.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tool.Tool, 0, len(upstream))
	for _, t := range upstream {
		out = append(out, renamedTool{inner: t, prefix: n.prefix})
	}
	return out, nil
}

type renamedTool struct {
	inner  tool.Tool
	prefix string
}

func (r renamedTool) Name() string        { return r.prefix + "_" + r.inner.Name() }
func (r renamedTool) Description() string { return r.inner.Description() }
func (r renamedTool) IsLongRunning() bool { return r.inner.IsLongRunning() }

// Declaration delegates to the underlying tool's runnable declaration
// (when it has one) but rewrites the function name to the prefixed
// form. Returns a fresh struct so we don't mutate the upstream copy.
func (r renamedTool) Declaration() *genai.FunctionDeclaration {
	rn, ok := r.inner.(runnable)
	if !ok {
		return nil
	}
	d := rn.Declaration()
	if d == nil {
		return nil
	}
	clone := *d
	clone.Name = r.Name()
	return &clone
}

func (r renamedTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	rn, ok := r.inner.(runnable)
	if !ok {
		return nil, errNotRunnable
	}
	return rn.Run(ctx, args)
}

// ProcessRequest satisfies ADK's internal toolinternal.RequestProcessor
// interface. ADK's `internal/llminternal/base_flow.go` requires every
// tool in `f.Tools` to implement it; without this method, every MCP
// tool would fail preprocess with `tool %q does not implement
// RequestProcessor()`. We pack `r` (the wrapper) — not `r.inner` —
// so the model sees the prefixed name and ADK's call-back dispatch
// routes through this wrapper's Run, preserving the namespace.
//
// This wrapper is the outermost when a permission gate is NOT
// configured. With a gate, gatedTool wraps this in turn and supplies
// its own ProcessRequest; only the outermost wrapper's
// ProcessRequest runs during preprocess.
func (r renamedTool) ProcessRequest(ctx tool.Context, req *model.LLMRequest) error {
	return coretools.PackTool(req, r)
}

// sanitizePrefix normalizes a server name into a Gemini-friendly
// identifier prefix: keeps [A-Za-z0-9_], replaces everything else
// with `_`. Caller passes mcp.json server keys, which users may
// have written with hyphens or other separators.
func sanitizePrefix(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

var errNotRunnable = simpleErr("mcp: wrapped tool does not implement runnable interface")

type simpleErr string

func (e simpleErr) Error() string { return string(e) }

// Compile-time assertion that tool.Context is still a context.Context.
var _ context.Context = (tool.Context)(nil)
