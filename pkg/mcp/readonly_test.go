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

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

// hintingTool is an upstream tool that declares its OWN dispatch class,
// standing in for the day ADK's mcptoolset surfaces the server-declared
// readOnlyHint annotation. Used to pin per-tool-beats-per-server.
type hintingTool struct {
	name string
	hint bool
}

func (h hintingTool) Name() string        { return h.name }
func (h hintingTool) Description() string { return "hinting tool" }
func (h hintingTool) IsLongRunning() bool { return false }
func (h hintingTool) ReadOnlyHint() bool  { return h.hint }
func (h hintingTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: h.name}
}
func (h hintingTool) Run(tool.Context, any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

// plainTool declares nothing — the shape every MCP tool has today,
// because ADK's adapter does not surface readOnlyHint.
type plainTool struct{ name string }

func (p plainTool) Name() string        { return p.name }
func (p plainTool) Description() string { return "plain tool" }
func (p plainTool) IsLongRunning() bool { return false }
func (p plainTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: p.name}
}
func (p plainTool) Run(tool.Context, any) (map[string]any, error) {
	return map[string]any{"ok": true}, nil
}

type hintToolset struct {
	name  string
	tools []tool.Tool
}

func (f hintToolset) Name() string { return f.name }
func (f hintToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) {
	return append([]tool.Tool(nil), f.tools...), nil
}

func toolsOf(t *testing.T, ts tool.Toolset) map[string]tool.Tool {
	t.Helper()
	got, err := ts.Tools(asReadonly(context.Background()))
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	out := make(map[string]tool.Tool, len(got))
	for _, tl := range got {
		out[tl.Name()] = tl
	}
	return out
}

// TestServerReadOnly_ClassifiesEveryTool is the headline of #693: an
// operator who mounted a provider's read-only endpoint says so once in
// mcp.json and every tool from that server classifies read-only.
//
// Both wrap compositions are covered because the digest wrap is the
// DEFAULT path — a fix that only reached withNamespace would be dead on
// every shipped configuration.
func TestServerReadOnly_ClassifiesEveryTool(t *testing.T) {
	t.Parallel()
	inner := hintToolset{name: "gke", tools: []tool.Tool{plainTool{name: "get_pod"}}}

	for _, tc := range []struct {
		wrap string
		opts *DigestOptions
	}{
		{"namespace-only", nil},
		{"digest-wrapped", &DigestOptions{}},
	} {
		t.Run(tc.wrap, func(t *testing.T) {
			t.Parallel()
			ro := toolsOf(t, withNamespaceAndDigest(inner, "gke", "gke", tc.opts, true))["gke_get_pod"]
			if ro == nil {
				t.Fatal("gke_get_pod missing from wrapped toolset")
			}
			if !coretools.IsReadOnlyTool(ro) {
				t.Errorf("read_only server: gke_get_pod classified mutating")
			}

			rw := toolsOf(t, withNamespaceAndDigest(inner, "gke", "gke", tc.opts, false))["gke_get_pod"]
			if coretools.IsReadOnlyTool(rw) {
				t.Errorf("undeclared server: gke_get_pod classified read-only — the fail-safe default is mutating")
			}
		})
	}
}

// TestServerReadOnly_HinterIsReachableOnTheValue is the receiver-shape
// regression. Tools() yields renamedTool and digestingTool VALUES, so a
// pointer-receiver ReadOnlyHint is absent from the interface's method
// set and every IsReadOnlyTool assertion misses it silently. That is
// exactly how #460's forward sat dead until #693, and the failure mode
// is invisible: no compile error, no test failure, just a hint that
// never applies.
func TestServerReadOnly_HinterIsReachableOnTheValue(t *testing.T) {
	t.Parallel()
	inner := hintToolset{name: "gke", tools: []tool.Tool{plainTool{name: "get_pod"}}}

	for _, tc := range []struct {
		wrap string
		opts *DigestOptions
	}{
		{"namespace-only", nil},
		{"digest-wrapped", &DigestOptions{}},
	} {
		t.Run(tc.wrap, func(t *testing.T) {
			t.Parallel()
			tl := toolsOf(t, withNamespaceAndDigest(inner, "gke", "gke", tc.opts, true))["gke_get_pod"]
			if _, ok := tl.(coretools.ReadOnlyHinter); !ok {
				t.Fatalf("%T does not satisfy ReadOnlyHinter as returned by Tools() — pointer receiver?", tl)
			}
		})
	}
}

// TestServerReadOnly_PerToolHintWins pins the order of authority. A
// server that annotates a subset keeps its own answer for those tools,
// in BOTH directions: a tool that says "I mutate" is not laundered
// read-only by the server-level declaration, and a tool that says "I
// only read" keeps that on a server that made no declaration.
func TestServerReadOnly_PerToolHintWins(t *testing.T) {
	t.Parallel()
	inner := hintToolset{name: "gke", tools: []tool.Tool{
		hintingTool{name: "delete_pod", hint: false},
		hintingTool{name: "get_pod", hint: true},
	}}

	onReadOnlyServer := toolsOf(t, withNamespaceAndDigest(inner, "gke", "gke", &DigestOptions{}, true))
	if coretools.IsReadOnlyTool(onReadOnlyServer["gke_delete_pod"]) {
		t.Error("a tool declaring itself mutating was laundered read-only by the server declaration")
	}

	onPlainServer := toolsOf(t, withNamespaceAndDigest(inner, "gke", "gke", &DigestOptions{}, false))
	if !coretools.IsReadOnlyTool(onPlainServer["gke_get_pod"]) {
		t.Error("a tool declaring itself read-only lost that on a server that declared nothing")
	}
}

// TestWrapServerToolset_ThreadsTheSpecDeclaration closes the gap
// between "mcp.json parses read_only" and "the tools the model gets
// classify read-only". Those are two claims, and every earlier defect
// in this area was one of them holding while the other quietly didn't
// (#460's forward, reachable in source and dead in practice).
//
// A gate is wired because that is the shipped composition: gatedTool
// is the outermost wrapper, so it is the one whose classification the
// mutation serializer and wait_and_verify actually see.
func TestWrapServerToolset_ThreadsTheSpecDeclaration(t *testing.T) {
	t.Parallel()
	inner := hintToolset{name: "gke", tools: []tool.Tool{plainTool{name: "get_pod"}}}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})

	for _, tc := range []struct {
		name string
		spec ServerSpec
		want bool
	}{
		{"read_only server", ServerSpec{Transport: "http", URL: "u", ReadOnly: true}, true},
		{"undeclared server", ServerSpec{Transport: "http", URL: "u"}, false},
		{"read_only + agentic_never", ServerSpec{Transport: "http", URL: "u", ReadOnly: true, AgenticNever: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := wrapServerToolset(inner, "gke", tc.spec, &DigestOptions{}, gate)
			tl := toolsOf(t, ts)["gke_get_pod"]
			if tl == nil {
				t.Fatal("gke_get_pod missing from the composed toolset")
			}
			if got := coretools.IsReadOnlyTool(tl); got != tc.want {
				t.Errorf("IsReadOnlyTool = %v, want %v (spec %+v)", got, tc.want, tc.spec)
			}
		})
	}
}
