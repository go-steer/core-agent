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
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"
	"google.golang.org/genai"
)

// methodListTools is the MCP method whose response carries the
// annotations. The go-sdk keeps its own copy of this constant
// unexported, so the literal is duplicated here rather than derived.
const methodListTools = "tools/list"

// The MCP spec lets a server annotate each tool with a readOnlyHint:
// "if true, the tool does not modify its environment". That is the
// answer core-agent wants for `wait_and_verify` polling, plan-first,
// and the mutation serializer — it is per-tool, and it comes from the
// only party that knows.
//
// ADK's mcptoolset throws it away. convertTool() copies Name,
// Description and the two schemas off *mcp.Tool and drops
// Annotations entirely, so by the time a tool reaches
// renamedTool.ReadOnlyHint() there is nothing left to forward and the
// server-level `read_only` declaration in mcp.json is the only source
// left (#693). The hint is not missing from the wire — it is on the
// wire and discarded one layer above us.
//
// Rather than fork the adapter or open a second session to ask the
// same question twice (which for a stdio server means forking a
// SECOND server process), this reads the annotations off the response
// the adapter already asked for, via client-side sending middleware
// on the same *mcpsdk.Client the adapter uses. One session, one
// tools/list, and the adapter stays untouched.

// hintRecorder remembers the readOnlyHint annotation each tool carried
// on the most recent tools/list, keyed by the UPSTREAM tool name (the
// namespace prefix has not been applied yet at the layer that reads
// this).
//
// Only tools that published an annotation block get an entry. A tool
// with no annotations at all is absent, not false, so the fallback
// order stays what ServerSpec.ReadOnly's documentation promises:
// per-tool annotation, then the server declaration, then the fail-safe
// default of mutating.
type hintRecorder struct {
	mu    sync.Mutex
	hints map[string]bool
}

func newHintRecorder() *hintRecorder {
	return &hintRecorder{hints: map[string]bool{}}
}

// record folds one page of a tools/list response into the map. first
// reports whether this was the opening page (empty cursor), which is
// the only point at which the previous contents may be dropped:
// mcptoolset pages, so resetting on every page would leave only the
// last one, and it restarts pagination from scratch after a
// reconnect, which is exactly when a stale entry should go.
func (h *hintRecorder) record(tools []*mcpsdk.Tool, first bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if first {
		h.hints = make(map[string]bool, len(tools))
	}
	for _, t := range tools {
		if t == nil || t.Annotations == nil {
			continue
		}
		h.hints[t.Name] = t.Annotations.ReadOnlyHint
	}
}

// lookup returns the annotated hint for an upstream tool name, and
// whether the server annotated it at all.
func (h *hintRecorder) lookup(name string) (bool, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.hints[name]
	return v, ok
}

// middleware is the sending-side interceptor to install on the client
// the adapter will use. It never alters the request or the result — it
// only reads the tool list on the way back.
func (h *hintRecorder) middleware() mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			res, err := next(ctx, method, req)
			if err != nil || method != methodListTools {
				return res, err
			}
			list, ok := res.(*mcpsdk.ListToolsResult)
			if !ok {
				return res, err
			}
			first := true
			if p, ok := req.GetParams().(*mcpsdk.ListToolsParams); ok {
				first = p.Cursor == ""
			}
			h.record(list.Tools, first)
			return res, err
		}
	}
}

// mcpToolsetWithHints builds the adapter toolset for one server with
// its published readOnlyHint annotations surfaced.
//
// The middleware goes on before mcptoolset.New because the adapter
// connects lazily — the first tools/list happens on the first Tools()
// call, well after this returns.
func mcpToolsetWithHints(client *mcpsdk.Client, transport mcpsdk.Transport) (tool.Toolset, error) {
	rec := newHintRecorder()
	client.AddSendingMiddleware(rec.middleware())
	ts, err := mcptoolset.New(mcptoolset.Config{Client: client, Transport: transport})
	if err != nil {
		return nil, err
	}
	return withReadOnlyHints(ts, rec), nil
}

// hintedToolset re-attaches the recorded annotation to each tool the
// adapter returns. It sits INSIDE the namespace wrap, so what
// renamedTool.ReadOnlyHint() finds on its inner tool is the server's
// own answer — the seam #460 opened and #693 documented as dormant.
type hintedToolset struct {
	inner    tool.Toolset
	recorder *hintRecorder
}

// withReadOnlyHints wraps inner so annotated tools carry their hint.
func withReadOnlyHints(inner tool.Toolset, rec *hintRecorder) tool.Toolset {
	if inner == nil || rec == nil {
		return inner
	}
	return &hintedToolset{inner: inner, recorder: rec}
}

func (h *hintedToolset) Name() string { return h.inner.Name() }

// Tools returns UNANNOTATED tools untouched, on purpose. Wrapping
// everything would make every tool implement ReadOnlyHinter, and since
// the annotation's own zero value is "mutating" that would silently
// beat a server-level `read_only: true` for tools the server said
// nothing about — turning an operator's declaration into a no-op.
// Absent has to stay distinguishable from false, and the only place it
// still is, is the shape of the value.
func (h *hintedToolset) Tools(ctx agent.ReadonlyContext) ([]tool.Tool, error) {
	upstream, err := h.inner.Tools(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]tool.Tool, 0, len(upstream))
	for _, t := range upstream {
		hint, annotated := h.recorder.lookup(t.Name())
		if !annotated {
			out = append(out, t)
			continue
		}
		out = append(out, hintedTool{inner: t, hint: hint})
	}
	return out, nil
}

// hintedTool is one adapter tool plus the class its server declared.
//
// VALUE receiver on every method, matching renamedTool and
// digestingTool: Tools() yields these as values, and a pointer
// receiver would drop ReadOnlyHint out of the interface method set,
// which is the exact silent failure #693 was.
type hintedTool struct {
	inner tool.Tool
	hint  bool
}

func (h hintedTool) Name() string        { return h.inner.Name() }
func (h hintedTool) Description() string { return h.inner.Description() }
func (h hintedTool) IsLongRunning() bool { return h.inner.IsLongRunning() }

// ReadOnlyHint reports the server's own readOnlyHint annotation.
func (h hintedTool) ReadOnlyHint() bool { return h.hint }

func (h hintedTool) Declaration() *genai.FunctionDeclaration {
	rn, ok := h.inner.(runnable)
	if !ok {
		return nil
	}
	return rn.Declaration()
}

func (h hintedTool) Run(ctx tool.Context, args any) (map[string]any, error) {
	rn, ok := h.inner.(runnable)
	if !ok {
		return nil, errNotRunnable
	}
	return rn.Run(ctx, args)
}
