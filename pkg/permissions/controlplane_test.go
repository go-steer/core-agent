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

package permissions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// TestControlPlaneBasenames_MatchConfigLoader keeps the literal
// control-plane basenames in lockstep with the config loader
// constants. (The mcp.json side is asserted in pkg/mcp, which imports
// permissions; asserting it here would create an import cycle.)
func TestControlPlaneBasenames_MatchConfigLoader(t *testing.T) {
	t.Parallel()
	if _, ok := controlPlaneBasenames[config.ConfigFileName]; !ok {
		t.Errorf("controlPlaneBasenames missing config.ConfigFileName %q", config.ConfigFileName)
	}
	if config.AgentsDirName != controlPlaneDirName {
		t.Errorf("controlPlaneDirName %q != config.AgentsDirName %q", controlPlaneDirName, config.AgentsDirName)
	}
}

func TestIsControlPlanePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{"/home/u/proj/.agents/config.json", true},
		{"/home/u/proj/.agents/mcp.json", true},
		{"/home/u/.agents/config.json", true}, // home scope
		{"/home/u/.agents/mcp.json", true},
		{"/home/u/proj/.agents/AGENTS.md", false},         // instruction-bearing
		{"/home/u/proj/.agents/skills/x/SKILL.md", false}, // skills content
		{"/home/u/proj/config.json", false},               // not under .agents
		{"/home/u/proj/.agents/sub/config.json", false},   // not directly in .agents
		{"/home/u/proj/src/main.go", false},
	}
	for _, tt := range tests {
		if got := isControlPlanePath(tt.path); got != tt.want {
			t.Errorf("isControlPlanePath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// cpScopeGate builds a gate whose scope root is agentsParent (so the
// .agents tree is fully in-scope for ordinary file rules) with the
// given mode and prompter.
func cpScopeGate(t *testing.T, root string, mode Mode, prompter Prompter) *Gate {
	t.Helper()
	scope, err := NewPathScope(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return New(Options{Mode: mode, Scope: scope, Prompter: prompter})
}

func agentsLayout(t *testing.T) (root, configJSON string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agents := filepath.Join(root, ".agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	configJSON = filepath.Join(agents, "config.json")
	if err := os.WriteFile(configJSON, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, configJSON
}

// TestControlPlaneWrite_YoloDeniedWithoutPrompter pins that yolo mode
// does NOT auto-approve a control-plane write, and with no prompter
// the write is denied with the ErrControlPlaneWrite sentinel.
func TestControlPlaneWrite_YoloDeniedWithoutPrompter(t *testing.T) {
	t.Parallel()
	root, configJSON := agentsLayout(t)
	g := cpScopeGate(t, root, ModeYolo, nil)
	err := g.CheckFileWrite(context.Background(), "write_file", configJSON)
	if err == nil {
		t.Fatal("yolo control-plane write should be denied without a prompter")
	}
	if !errors.Is(err, ErrControlPlaneWrite) {
		t.Errorf("error = %v, want ErrControlPlaneWrite", err)
	}
}

// TestControlPlaneWrite_AcceptEditsDenied pins that acceptEdits (which
// auto-approves ordinary writes) does NOT auto-approve a control-plane
// write.
func TestControlPlaneWrite_AcceptEditsDenied(t *testing.T) {
	t.Parallel()
	root, configJSON := agentsLayout(t)
	g := cpScopeGate(t, root, ModeAcceptEdits, nil)
	if err := g.CheckFileWrite(context.Background(), "write_file", configJSON); err == nil {
		t.Fatal("acceptEdits control-plane write should not auto-approve without a prompter")
	}
}

// TestControlPlaneWrite_SessionToolGrantIgnored pins that a prior
// per-tool session grant does NOT suppress the elevated prompt.
func TestControlPlaneWrite_SessionToolGrantIgnored(t *testing.T) {
	t.Parallel()
	root, configJSON := agentsLayout(t)
	p := &fakePrompter{decision: DecisionAllowSessionTool}
	g := cpScopeGate(t, root, ModeAsk, p)
	ctx := context.Background()

	// Establish a session-tool grant for write_file via an ordinary
	// in-scope file.
	ordinary := filepath.Join(root, "notes.txt")
	if err := g.CheckFileWrite(ctx, "write_file", ordinary); err != nil {
		t.Fatalf("ordinary write: %v", err)
	}
	before := len(p.calls)

	// Control-plane write must still prompt (elevated), not ride the grant.
	if err := g.CheckFileWrite(ctx, "write_file", configJSON); err != nil {
		t.Fatalf("control-plane write with prompter approving: %v", err)
	}
	if len(p.calls) != before+1 {
		t.Fatalf("control-plane write should re-prompt; calls %d -> %d", before, len(p.calls))
	}
	last := p.calls[len(p.calls)-1]
	if last.Kind != PromptKindControlPlaneWrite {
		t.Errorf("prompt kind = %v, want PromptKindControlPlaneWrite", last.Kind)
	}
}

// TestControlPlaneWrite_AllowlistIgnored pins that an allowlist entry
// covering the path does NOT satisfy the elevated gate.
func TestControlPlaneWrite_AllowlistIgnored(t *testing.T) {
	t.Parallel()
	root, configJSON := agentsLayout(t)
	policy, err := NewPolicy([]string{"write_file:" + configJSON}, nil)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewPathScope(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// No prompter: if the allowlist (wrongly) satisfied the elevated
	// gate, this would return nil. It must deny.
	g := New(Options{Mode: ModeAsk, Policy: policy, Scope: scope})
	if err := g.CheckFileWrite(context.Background(), "write_file", configJSON); err == nil {
		t.Fatal("allowlist entry must not satisfy the elevated control-plane gate")
	}
}

// TestControlPlaneWrite_PromptApproves pins that an explicit
// interactive approval passes the elevated gate — and that it is NOT
// remembered (the next control-plane write re-prompts).
func TestControlPlaneWrite_PromptApproves(t *testing.T) {
	t.Parallel()
	root, configJSON := agentsLayout(t)
	p := &fakePrompter{decision: DecisionAllowOnce}
	g := cpScopeGate(t, root, ModeAsk, p)
	ctx := context.Background()

	if err := g.CheckFileWrite(ctx, "write_file", configJSON); err != nil {
		t.Fatalf("approved control-plane write: %v", err)
	}
	if err := g.CheckFileWrite(ctx, "write_file", configJSON); err != nil {
		t.Fatalf("second approved control-plane write: %v", err)
	}
	if len(p.calls) != 2 {
		t.Errorf("elevated gate must re-prompt every time; got %d prompts", len(p.calls))
	}
}

// TestControlPlaneWrite_DenyByUser pins that a user denial blocks the
// write.
func TestControlPlaneWrite_DenyByUser(t *testing.T) {
	t.Parallel()
	root, configJSON := agentsLayout(t)
	p := &fakePrompter{decision: DecisionDeny}
	g := cpScopeGate(t, root, ModeAsk, p)
	if err := g.CheckFileWrite(context.Background(), "write_file", configJSON); err == nil {
		t.Fatal("user-denied control-plane write should error")
	}
}

// TestControlPlaneWrite_SymlinkLaunderingCaught pins that a symlink
// inside the scope pointing at .agents/config.json is classified as
// control-plane (resolution happens before classification).
func TestControlPlaneWrite_SymlinkLaunderingCaught(t *testing.T) {
	t.Parallel()
	root, configJSON := agentsLayout(t)
	link := filepath.Join(root, "innocent.txt")
	if err := os.Symlink(configJSON, link); err != nil {
		t.Fatal(err)
	}
	g := cpScopeGate(t, root, ModeYolo, nil)
	if err := g.CheckFileWrite(context.Background(), "write_file", link); err == nil {
		t.Fatal("symlink to control-plane file should be gated as control-plane")
	}
}

// TestControlPlaneWrite_AgentsMdUnaffected pins the two-tier boundary:
// AGENTS.md (instruction-bearing) stays normally writable — under yolo
// it auto-approves like any other file.
func TestControlPlaneWrite_AgentsMdUnaffected(t *testing.T) {
	t.Parallel()
	root, _ := agentsLayout(t)
	agentsMd := filepath.Join(root, ".agents", "AGENTS.md")
	g := cpScopeGate(t, root, ModeYolo, nil)
	if err := g.CheckFileWrite(context.Background(), "write_file", agentsMd); err != nil {
		t.Errorf("AGENTS.md write should behave as an ordinary write: %v", err)
	}
}
