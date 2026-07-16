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
	"errors"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/model"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"
)

func TestSanitizePrefix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"github", "github"},
		{"my-server", "my_server"},
		{"file.system", "file_system"},
		{"abc 123", "abc_123"},
		{"_alpha", "_alpha"},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := sanitizePrefix(tc.in); got != tc.want {
				t.Errorf("sanitizePrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestWithNamespace_NilSafe(t *testing.T) {
	t.Parallel()
	if got := withNamespace(nil, "x"); got != nil {
		t.Errorf("nil toolset should pass through, got %v", got)
	}
	stub := newInMemoryToolset(t)
	if got := withNamespace(stub, ""); got != stub {
		t.Errorf("empty prefix should pass through")
	}
}

func TestWithNamespace_PrefixesToolNames(t *testing.T) {
	t.Parallel()
	inner := newInMemoryToolset(t)
	wrapped := withNamespace(inner, "demo")

	tools, err := wrapped.Tools(asReadonly(context.Background()))
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("expected at least one tool from the in-memory MCP server")
	}
	for _, tl := range tools {
		if got := tl.Name(); got == "" {
			t.Errorf("empty tool name")
		} else if got[:5] != "demo_" {
			t.Errorf("tool %q missing demo_ prefix", got)
		}
	}
}

func TestWithNamespace_NameDescriptionLongRunningPassthrough(t *testing.T) {
	t.Parallel()
	inner := newInMemoryToolset(t)
	wrapped := withNamespace(inner, "demo")

	tools, err := wrapped.Tools(asReadonly(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools {
		if tl.Description() == "" {
			t.Errorf("description should pass through (got empty for %s)", tl.Name())
		}
		if tl.IsLongRunning() {
			t.Errorf("expected IsLongRunning=false for %s", tl.Name())
		}
	}
}

func TestRenamedTool_DeclarationFromInner(t *testing.T) {
	t.Parallel()
	inner := newInMemoryToolset(t)
	wrapped := withNamespace(inner, "demo")
	tools, err := wrapped.Tools(asReadonly(context.Background()))
	if err != nil {
		t.Fatal(err)
	}

	for _, tl := range tools {
		if rt, ok := tl.(runnable); ok {
			d := rt.Declaration()
			if d == nil {
				t.Errorf("nil declaration for %s", tl.Name())
				continue
			}
			if d.Name != tl.Name() {
				t.Errorf("declaration.Name = %q, want %q", d.Name, tl.Name())
			}
		}
	}
}

func TestRenamedTool_ProcessRequest_PacksWrapper(t *testing.T) {
	t.Parallel()
	// This is the regression test for the latent bug surfaced when
	// the first real MCP smoke (07-mcp-google-oauth.sh) hit the GKE
	// MCP server: ADK's base_flow.go:280 requires every tool in
	// f.Tools to implement RequestProcessor. Our renamedTool wrapper
	// must satisfy that contract by packing itself (the wrapper)
	// rather than letting the inner mcpTool pack itself — otherwise
	// the model would see the un-namespaced declaration AND ADK's
	// call-back dispatch would bypass the namespace prefix.
	inner := newInMemoryToolset(t)
	wrapped := withNamespace(inner, "demo")
	tools, err := wrapped.Tools(asReadonly(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) == 0 {
		t.Fatal("expected at least one wrapped tool")
	}

	req := &model.LLMRequest{}
	for _, tl := range tools {
		rp, ok := tl.(interface {
			ProcessRequest(ctx tool.Context, req *model.LLMRequest) error
		})
		if !ok {
			t.Fatalf("renamedTool %q does not implement ProcessRequest — regression of the GKE MCP bug", tl.Name())
		}
		if err := rp.ProcessRequest(nil, req); err != nil {
			t.Fatalf("ProcessRequest(%q): %v", tl.Name(), err)
		}

		// Packed entry must be the WRAPPER (with prefixed name),
		// not the inner mcpTool.
		got, ok := req.Tools[tl.Name()]
		if !ok {
			t.Errorf("req.Tools missing wrapper entry for %q", tl.Name())
			continue
		}
		if _, isInner := got.(interface {
			ProcessRequest(ctx tool.Context, req *model.LLMRequest) error
		}); !isInner {
			t.Errorf("packed entry for %q must itself implement ProcessRequest", tl.Name())
		}
		// And the un-prefixed inner name must NOT appear — otherwise
		// the model would see two tools (raw + prefixed) and call-back
		// dispatch could hit either.
		if tl.Name() != "echo" {
			if _, leaked := req.Tools["echo"]; leaked {
				t.Errorf("inner tool name %q leaked into req.Tools alongside wrapper %q", "echo", tl.Name())
			}
		}
	}

	// The declared name in req.Config.Tools must use the prefix the
	// LLM sees, not the inner name.
	if req.Config == nil || len(req.Config.Tools) == 0 {
		t.Fatal("declarations not added to req.Config.Tools")
	}
	decls := req.Config.Tools[0].FunctionDeclarations
	for _, d := range decls {
		if d.Name == "echo" {
			t.Errorf("declaration name %q must be the prefixed form (demo_echo), not the raw inner name", d.Name)
		}
	}
}

func TestSimpleErr(t *testing.T) {
	t.Parallel()
	if got := errNotRunnable.Error(); got == "" {
		t.Errorf("expected non-empty error message")
	}
	var err error = simpleErr("boom")
	if !errors.Is(err, simpleErr("boom")) {
		_ = err.Error()
	}
}

// newInMemoryToolset spins up an in-memory MCP server with one trivial
// tool and returns a connected mcptoolset for testing the wrapper.
func newInMemoryToolset(t *testing.T) tool.Toolset {
	t.Helper()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "v1"}, nil)
	type input struct {
		Msg string `json:"msg" jsonschema:"echo input"`
	}
	type output struct {
		Echo string `json:"echo"`
	}
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "echo",
		Description: "Echoes its input back",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, in input) (*mcpsdk.CallToolResult, output, error) {
		return nil, output{Echo: in.Msg}, nil
	})
	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}

	ts, err := mcptoolset.New(mcptoolset.Config{Transport: clientTransport})
	if err != nil {
		t.Fatalf("toolset: %v", err)
	}
	return ts
}

var _ agent.ReadonlyContext = asReadonly(context.Background())

// TestRenamedTool_Run_StampsLatencyMS pins the non-digest wrap
// path for #277: even without the digest layer, every MCP tool
// response carries a `latency_ms` sidecar so operators see
// per-call timing in the TUI regardless of whether MCP digest
// is enabled. Regression signal: if this test fails, tool-row
// timing goes dark for daemons run with --no-mcp-digest.
func TestRenamedTool_Run_StampsLatencyMS(t *testing.T) {
	t.Parallel()
	inner := newInMemoryToolset(t)
	wrapped := withNamespace(inner, "demo")
	tools, err := wrapped.Tools(asReadonly(context.Background()))
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	var echo tool.Tool
	for _, tl := range tools {
		if tl.Name() == "demo_echo" {
			echo = tl
			break
		}
	}
	if echo == nil {
		t.Fatal("demo_echo tool not found on wrapped toolset")
	}
	res, err := echo.(runnable).Run(
		&stubToolCtx{Context: context.Background()},
		map[string]any{"msg": "ping"},
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res == nil {
		t.Fatal("Run returned nil map — no sidecar to inspect")
	}
	v, ok := res["latency_ms"]
	if !ok {
		t.Fatalf("latency_ms missing from response: %+v", res)
	}
	ms, ok := v.(int64)
	if !ok {
		t.Fatalf("latency_ms wrong type: %T (want int64)", v)
	}
	if ms < 0 {
		t.Errorf("latency_ms negative (%d) — clock skew?", ms)
	}
	// Upstream response fields still present. MCP wraps single-arg
	// tool output under `output`; the exact shape isn't the point,
	// just that the merge is additive rather than replacing.
	if _, ok := res["output"]; !ok {
		t.Errorf("upstream output field lost after latency merge: %+v", res)
	}
}
