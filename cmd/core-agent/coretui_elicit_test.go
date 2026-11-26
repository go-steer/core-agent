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

//go:build !no_tui

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	coretui "github.com/go-steer/core-tui/tui"
)

// stubElicitor is a coretui.Elicitor that returns a fixed answer.
type stubElicitor struct {
	res coretui.ElicitResult
	err error
}

func (s stubElicitor) Elicit(context.Context, string, coretui.ElicitRequest) (coretui.ElicitResult, error) {
	return s.res, s.err
}

// oneStringSchema is the smallest request translateMCPSchemaToElicitRequest
// accepts, so these tests exercise the Elicit return path rather than
// tripping the translator's own decline.
func oneStringSchema() *mcpsdk.ElicitParams {
	return &mcpsdk.ElicitParams{
		Message: "which region?",
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"region": map[string]any{"type": "string"},
			},
		},
	}
}

// An unrenderable request must reach the calling MCP server as a
// protocol-level decline, not as a JSON-RPC error.
//
// core-tui v0.21.0 moved "I cannot draw this" out of the Action enum
// and into the error return (ErrElicitUnsupported + a placeholder
// ElicitActionCancel). Before that, the same request produced a plain
// decline with a nil error. Forwarding the new error verbatim would
// silently convert a clean answer into a server-side failure, so this
// pins the wire value.
func TestCoreMCPElicitor_UnsupportedBecomesDecline(t *testing.T) {
	c := &coreMCPElicitor{inner: stubElicitor{
		// The placeholder core-tui pairs with the sentinel — proving
		// we branch on err and not on Action.
		res: coretui.ElicitResult{Action: coretui.ElicitActionCancel},
		err: fmt.Errorf("%w: nested object", coretui.ErrElicitUnsupported),
	}}

	out, err := c.elicit(context.Background(), "srv", &mcpsdk.ElicitRequest{Params: oneStringSchema()})
	if err != nil {
		t.Fatalf("unsupported elicit must not surface as an error, got %v", err)
	}
	if out.Action != "decline" {
		t.Errorf("expected wire action %q, got %q", "decline", out.Action)
	}
	if out.Content != nil {
		t.Errorf("a declined request must carry no content, got %v", out.Content)
	}
}

// Every other error is a genuine failure to carry the request out and
// must still propagate — a cancelled context above all.
func TestCoreMCPElicitor_OtherErrorsPropagate(t *testing.T) {
	sentinel := errors.New("boom")
	c := &coreMCPElicitor{inner: stubElicitor{
		res: coretui.ElicitResult{Action: coretui.ElicitActionCancel},
		err: sentinel,
	}}

	out, err := c.elicit(context.Background(), "srv", &mcpsdk.ElicitRequest{Params: oneStringSchema()})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the underlying error to propagate, got %v", err)
	}
	if out.Action != "cancel" {
		t.Errorf("expected wire action %q, got %q", "cancel", out.Action)
	}
}

// The happy path is unchanged by the v0.21.0 error split.
func TestCoreMCPElicitor_SubmitCarriesValues(t *testing.T) {
	c := &coreMCPElicitor{inner: stubElicitor{
		res: coretui.ElicitResult{
			Action: coretui.ElicitActionSubmit,
			Values: map[string]any{"region": "us-central1"},
		},
	}}

	out, err := c.elicit(context.Background(), "srv", &mcpsdk.ElicitRequest{Params: oneStringSchema()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Action != "accept" {
		t.Errorf("expected wire action %q, got %q", "accept", out.Action)
	}
	if got := out.Content["region"]; got != "us-central1" {
		t.Errorf("expected submitted values forwarded, got %v", out.Content)
	}
}

// TestNewMCPElicitor_InteractiveVsNonInteractive verifies that non-interactive
// calls never construct a TUI elicitor and return (nil, nil), while interactive
// calls wire the exact same coretui.Elicitor instance into both the MCP
// callback and the handle passed to launchTUIv2 (#1606).
func TestNewMCPElicitor_InteractiveVsNonInteractive(t *testing.T) {
	t.Parallel()

	factoryCalls := 0
	fn, handle := newMCPElicitor(false, func() coretui.Elicitor {
		factoryCalls++
		return stubElicitor{}
	})
	if fn != nil || handle != nil {
		t.Fatalf("newMCPElicitor(false) = (%v, %v), want (nil, nil)", fn != nil, handle)
	}
	if factoryCalls != 0 {
		t.Errorf("non-interactive call invoked elicitor factory %d time(s), want 0", factoryCalls)
	}

	stub := &stubElicitor{
		res: coretui.ElicitResult{
			Action: coretui.ElicitActionSubmit,
			Values: map[string]any{"region": "europe-west4"},
		},
	}
	fn, handle = newMCPElicitor(true, func() coretui.Elicitor {
		factoryCalls++
		return stub
	})
	if fn == nil || handle == nil || handle.inner == nil {
		t.Fatalf("newMCPElicitor(true) returned nil component: fn=%v handle=%+v", fn != nil, handle)
	}
	if factoryCalls != 1 {
		t.Errorf("interactive call invoked elicitor factory %d time(s), want 1", factoryCalls)
	}
	if gotStub, ok := handle.inner.(*stubElicitor); !ok || gotStub != stub {
		t.Errorf("handle.inner (%T) does not match the elicitor wired into the MCP callback", handle.inner)
	}
	out, err := fn(context.Background(), "srv", &mcpsdk.ElicitRequest{Params: oneStringSchema()})
	if err != nil {
		t.Fatalf("interactive elicitor callback: %v", err)
	}
	if out.Action != "accept" || out.Content["region"] != "europe-west4" {
		t.Errorf("interactive elicitor returned %+v, want accept with region=europe-west4", out)
	}
}

// TestStepBuildMCP_InteractiveTUIAdvertisesElicitation verifies that default
// (!no_tui) builds running interactively on a TTY still construct and wire the
// TUI MCP elicitor and advertise MCP elicitation during initialize (#1606).
//
// Not parallel: isolates HOME via t.Setenv so startup steps neither read the
// user's real ~/.agents/mcp.json nor write to ~/.core-agent/trusted_workspaces.json.
func TestStepBuildMCP_InteractiveTUIAdvertisesElicitation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	parentURL, advertised := startCLIProbeMCPServer(t, true)
	agentsDir, cfgPath := writeProbeWorkspace(t, parentURL, "")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := newRunState(ctx, cancel, runOptions{
		driver: driverOpts{
			yolo: true,
		},
		workspace: workspaceOpts{
			cfgPath:       cfgPath,
			agentsDirFlag: agentsDir,
		},
		model: modelOpts{
			noPricingRefresh: true,
		},
		toolCatalog: toolCatalogOpts{
			noBuiltinTools:     true,
			noBackgroundAgents: true,
			noMCPDigest:        true,
		},
		guardrails: guardrailOpts{
			trust: true,
		},
		prompt: promptOpts{
			quiet: true,
		},
	})
	s.flagSet = flag.NewFlagSet("test", flag.ContinueOnError)
	s.stdinIsTTY = func() bool { return true }
	defer s.close()

	runStepsThrough(t, s, "build-mcp")

	if s.mcpElicitor == nil {
		t.Fatal("s.mcpElicitor == nil in interactive TTY mode; want TUI elicitor wired")
	}
	if s.tuiElicitor == nil || s.tuiElicitor.inner == nil {
		t.Fatalf("s.tuiElicitor = %+v in interactive TTY mode; want non-nil handle for launchTUIv2", s.tuiElicitor)
	}
	if len(s.mcpToolsets) != 1 {
		t.Fatalf("got %d mcpToolsets, want 1", len(s.mcpToolsets))
	}
	res, err := invokeMCPTool(t, ctx, s.mcpToolsets[0], "probe_pick_cluster")
	if err != nil {
		t.Fatalf("probe_pick_cluster: %v", err)
	}
	if !advertised.Load() {
		t.Error("interactive TUI mode did not advertise MCP elicitation capability")
	}
	if got, _ := res["output"].(string); !strings.Contains(got, "advertised=true") {
		t.Errorf("probe_pick_cluster output = %v, want advertised=true", res)
	}
}
