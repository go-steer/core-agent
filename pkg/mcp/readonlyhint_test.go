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
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

// newAnnotatedToolset spins up an in-memory MCP server that publishes
// the three annotation shapes a real server mixes, and returns the
// adapter toolset core-agent actually builds for it.
//
// A real server is used rather than a stub tool because the whole
// defect lives between the wire and the adapter: a stub that
// implements ReadOnlyHint in Go proves nothing about whether the
// annotation survives mcptoolset. `container.googleapis.com/mcp`
// publishes readOnlyHint on all 23 of its tools, and before this the
// 14 read tools among them all classified mutating.
func newAnnotatedToolset(t *testing.T) tool.Toolset {
	t.Helper()
	clientTransport, serverTransport := mcpsdk.NewInMemoryTransports()

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "v1"}, nil)
	type input struct {
		Msg string `json:"msg" jsonschema:"anything"`
	}
	type output struct {
		Echo string `json:"echo"`
	}
	run := func(_ context.Context, _ *mcpsdk.CallToolRequest, in input) (*mcpsdk.CallToolResult, output, error) {
		return nil, output{Echo: in.Msg}, nil
	}
	// readOnlyHint true: the server says this one only reads.
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "get_pod",
		Description: "Reads a pod",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true},
	}, run)
	// Annotated, readOnlyHint false: the server says this one mutates.
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "delete_pod",
		Description: "Deletes a pod",
		Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: false},
	}, run)
	// No annotations at all: the server said nothing.
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name:        "legacy_thing",
		Description: "Unannotated",
	}, run)

	if _, err := server.Connect(context.Background(), serverTransport, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}

	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: implementationName, Version: "0.1.0"}, nil)
	ts, err := mcpToolsetWithHints(client, clientTransport)
	if err != nil {
		t.Fatalf("toolset: %v", err)
	}
	return ts
}

// TestReadOnlyHint_ReachesTheModelFacingTool is the headline: a tool
// the server annotated readOnlyHint=true classifies read-only even
// though nobody declared `read_only` in mcp.json.
//
// Both wrap compositions, because the digest wrap is the DEFAULT path
// and a fix that reached only withNamespace would be dead on every
// shipped configuration.
func TestReadOnlyHint_ReachesTheModelFacingTool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		wrap string
		opts *DigestOptions
	}{
		{"namespace-only", nil},
		{"digest-wrapped", &DigestOptions{}},
	} {
		t.Run(tc.wrap, func(t *testing.T) {
			t.Parallel()
			// read_only NOT set: the annotation is the only source.
			tools := toolsOf(t, withNamespaceAndDigest(newAnnotatedToolset(t), "gke", "gke", tc.opts, false, nil))

			ro := tools["gke_get_pod"]
			if ro == nil {
				t.Fatal("gke_get_pod missing from the wrapped toolset")
			}
			if !coretools.IsReadOnlyTool(ro) {
				t.Error("gke_get_pod classified mutating — the server's readOnlyHint=true did not survive the adapter")
			}

			if coretools.IsReadOnlyTool(tools["gke_delete_pod"]) {
				t.Error("gke_delete_pod classified read-only — readOnlyHint=false was not honored")
			}
		})
	}
}

// TestReadOnlyHint_AbsentAnnotationLeavesTheServerDeclarationStanding
// guards the failure mode the shape of hintedToolset.Tools exists to
// prevent. The annotation's zero value IS "mutating", so a wrapper
// that stamped every tool with a hint would make an unannotated tool
// report mutating and silently override an operator's
// `read_only: true` — converting a working declaration into a no-op
// for exactly the servers old enough not to annotate.
func TestReadOnlyHint_AbsentAnnotationLeavesTheServerDeclarationStanding(t *testing.T) {
	t.Parallel()
	onReadOnlyServer := toolsOf(t, withNamespaceAndDigest(newAnnotatedToolset(t), "gke", "gke", &DigestOptions{}, true, nil))

	legacy := onReadOnlyServer["gke_legacy_thing"]
	if legacy == nil {
		t.Fatal("gke_legacy_thing missing from the wrapped toolset")
	}
	if !coretools.IsReadOnlyTool(legacy) {
		t.Error("an unannotated tool lost the server's read_only declaration")
	}

	// And the other direction: an annotation still beats the
	// declaration, so a mis-declared server cannot launder a tool the
	// server itself calls mutating.
	if coretools.IsReadOnlyTool(onReadOnlyServer["gke_delete_pod"]) {
		t.Error("readOnlyHint=false was laundered read-only by read_only: true")
	}
}

// TestReadOnlyHint_SurvivesTheShippedComposition runs the annotation
// through wrapServerToolset with a gate, which is the real stack:
// gatedTool is the outermost wrapper, so it is the classification the
// mutation serializer and wait_and_verify actually read.
func TestReadOnlyHint_SurvivesTheShippedComposition(t *testing.T) {
	t.Parallel()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	spec := ServerSpec{Transport: "http", URL: "u"}
	tools := toolsOf(t, wrapServerToolset(newAnnotatedToolset(t), "gke", spec, &DigestOptions{}, gate))

	for name, want := range map[string]bool{
		"gke_get_pod":      true,
		"gke_delete_pod":   false,
		"gke_legacy_thing": false, // no annotation, no declaration -> fail-safe
	} {
		tl := tools[name]
		if tl == nil {
			t.Fatalf("%s missing from the composed toolset", name)
		}
		if got := coretools.IsReadOnlyTool(tl); got != want {
			t.Errorf("IsReadOnlyTool(%s) = %v, want %v", name, got, want)
		}
	}
}

// TestReadOnlyHint_StillRunnable pins that the extra wrap did not cost
// callability. hintedTool has to forward Declaration and Run or the
// tools reach the model undeclared and fail at call time — a wrapper
// that classifies correctly and cannot be invoked is worse than none.
func TestReadOnlyHint_StillRunnable(t *testing.T) {
	t.Parallel()
	tools := toolsOf(t, withNamespaceAndDigest(newAnnotatedToolset(t), "gke", "gke", nil, false, nil))
	tl := tools["gke_get_pod"]
	if tl == nil {
		t.Fatal("gke_get_pod missing from the wrapped toolset")
	}
	d := tl.(runnable).Declaration()
	if d == nil || d.Name != "gke_get_pod" {
		t.Fatalf("declaration = %+v, want name gke_get_pod", d)
	}
	res, err := tl.(runnable).Run(&stubToolCtx{Context: context.Background()}, map[string]any{"msg": "ping"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := res["output"]; !ok {
		t.Errorf("upstream output field missing after the hint wrap: %+v", res)
	}
}

// TestHintRecorder_PaginationAccumulates pins the one thing the
// middleware can get wrong that no live server in our tests would
// show: mcptoolset pages tools/list, so each page arrives as its own
// response. Rebuilding the map per page would leave only the last one,
// and every tool but the tail would lose its annotation.
func TestHintRecorder_PaginationAccumulates(t *testing.T) {
	t.Parallel()
	rec := newHintRecorder()
	rec.record([]*mcpsdk.Tool{{Name: "a", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}}, true)
	rec.record([]*mcpsdk.Tool{{Name: "b", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}}, false)

	for _, name := range []string{"a", "b"} {
		hint, ok := rec.lookup(name)
		if !ok {
			t.Errorf("%q lost its annotation across pages", name)
			continue
		}
		if !hint {
			t.Errorf("%q hint = false, want true", name)
		}
	}

	// A fresh first page replaces the lot — that is how a reconnect
	// drops annotations a restarted server no longer publishes.
	rec.record([]*mcpsdk.Tool{{Name: "c", Annotations: &mcpsdk.ToolAnnotations{ReadOnlyHint: true}}}, true)
	if _, ok := rec.lookup("a"); ok {
		t.Error(`"a" survived a first page that did not list it`)
	}
}
