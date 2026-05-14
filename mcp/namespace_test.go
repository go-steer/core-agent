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
