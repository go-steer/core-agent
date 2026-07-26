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
	"path/filepath"
	"testing"
)

// TestGate_DecisionAllowSessionTool_SuppressesFurtherPrompts pins the
// contract that once the user picks DecisionAllowSessionTool on a
// prompt for tool X, every subsequent gate request for tool X (any
// args, any file, any path) MUST go through without prompting again
// until the session ends.
func TestGate_DecisionAllowSessionTool_SuppressesFurtherPrompts(t *testing.T) {
	t.Parallel()
	prompter := &fakePrompter{decision: DecisionAllowSessionTool}
	g := New(Options{Mode: ModeAsk, Prompter: prompter})

	ctx := context.Background()
	if err := g.CheckGeneric(ctx, "read_file", "go.mod"); err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	if len(prompter.calls) != 1 {
		t.Fatalf("first call should prompt exactly once; got %d", len(prompter.calls))
	}

	for _, key := range []string{"go.sum", "internal/tui/model.go", "README.md", "anything-else"} {
		if err := g.CheckGeneric(ctx, "read_file", key); err != nil {
			t.Errorf("read_file %q after AllowSessionTool: unexpected error %v", key, err)
		}
	}
	if len(prompter.calls) != 1 {
		t.Errorf("subsequent read_file calls should be silent; prompter was called %d times total", len(prompter.calls))
	}

	if err := g.CheckGeneric(ctx, "bash", "ls -la"); err != nil {
		t.Errorf("bash call: unexpected error %v", err)
	}
	if len(prompter.calls) != 2 {
		t.Errorf("a different tool should still prompt; got total prompter.calls=%d, want 2", len(prompter.calls))
	}
}

// TestGate_FileTool_SessionGrant_PreservesPathBoundary is the #380
// regression pin. A per-tool session grant must NOT drop the path
// boundary for file tools: out-of-scope reads keep escalating via the
// path-scope prompt every time, while in-scope operations pass
// silently once trusted.
func TestGate_FileTool_SessionGrant_PreservesPathBoundary(t *testing.T) {
	t.Parallel()
	inScope, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewPathScope(inScope, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The prompter grants the tool session-wide on the first ask.
	prompter := &fakePrompter{decision: DecisionAllowSessionTool}
	g := New(Options{Mode: ModeAsk, Scope: scope, Prompter: prompter})
	ctx := context.Background()

	// First out-of-scope read prompts (and the user grants the tool).
	first := filepath.Join(outDir, "a.txt")
	if err := g.CheckFileRead(ctx, "read_file", first); err != nil {
		t.Fatalf("first out-of-scope read: %v", err)
	}
	if len(prompter.calls) != 1 {
		t.Fatalf("first out-of-scope read should prompt; got %d", len(prompter.calls))
	}

	// The session-tool grant must NOT suppress subsequent out-of-scope
	// prompts — the path boundary is preserved (#380). Flip to deny so
	// we can also confirm the denial propagates.
	prompter.decision = DecisionDeny
	for _, p := range []string{"b.txt", "c.txt"} {
		before := len(prompter.calls)
		if err := g.CheckFileRead(ctx, "read_file", filepath.Join(outDir, p)); err == nil {
			t.Errorf("out-of-scope read %q after grant: want prompt+deny, got nil", p)
		}
		if len(prompter.calls) != before+1 {
			t.Errorf("out-of-scope read %q should re-prompt; calls %d -> %d", p, before, len(prompter.calls))
		}
	}

	// In-scope reads pass silently (scope already grants; no prompt).
	inFile := filepath.Join(inScope, "ok.txt")
	before := len(prompter.calls)
	if err := g.CheckFileRead(ctx, "read_file", inFile); err != nil {
		t.Errorf("in-scope read: %v", err)
	}
	if len(prompter.calls) != before {
		t.Errorf("in-scope read must not prompt; calls %d -> %d", before, len(prompter.calls))
	}
}

// TestGate_FileWrite_SessionGrant_SuppressesInScopePrompt pins the
// other half of #380: for an IN-SCOPE write, a per-tool session grant
// suppresses the mode (ask) prompt, but an out-of-scope write still
// escalates.
func TestGate_FileWrite_SessionGrant_SuppressesInScopePrompt(t *testing.T) {
	t.Parallel()
	inScope, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	scope, err := NewPathScope(inScope, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	prompter := &fakePrompter{decision: DecisionAllowSessionTool}
	g := New(Options{Mode: ModeAsk, Scope: scope, Prompter: prompter})
	ctx := context.Background()

	// First in-scope write prompts (ask mode) and grants the tool.
	if err := g.CheckFileWrite(ctx, "write_file", filepath.Join(inScope, "a.txt")); err != nil {
		t.Fatalf("first in-scope write: %v", err)
	}
	if len(prompter.calls) != 1 {
		t.Fatalf("first in-scope write should prompt; got %d", len(prompter.calls))
	}
	// Subsequent in-scope writes are silent.
	if err := g.CheckFileWrite(ctx, "write_file", filepath.Join(inScope, "b.txt")); err != nil {
		t.Fatalf("second in-scope write: %v", err)
	}
	if len(prompter.calls) != 1 {
		t.Errorf("in-scope write after grant should be silent; got %d", len(prompter.calls))
	}
	// Out-of-scope write still escalates despite the grant.
	prompter.decision = DecisionDeny
	if err := g.CheckFileWrite(ctx, "write_file", filepath.Join(outDir, "c.txt")); err == nil {
		t.Error("out-of-scope write after grant: want prompt+deny, got nil")
	}
	if len(prompter.calls) != 2 {
		t.Errorf("out-of-scope write should re-prompt; got %d calls", len(prompter.calls))
	}
}

// TestGate_CheckToolCall_PerToolSessionGrant is the #379 regression
// pin: a DecisionAllowSessionTool grant made for one MCP tool must NOT
// trust a different MCP tool — whether from a different server or the
// same server — and the same isolation holds for the skill namespace.
func TestGate_CheckToolCall_PerToolSessionGrant(t *testing.T) {
	t.Parallel()
	prompter := &fakePrompter{decision: DecisionAllowSessionTool}
	g := New(Options{Mode: ModeAsk, Prompter: prompter})
	ctx := context.Background()

	// Grant "mcp" tool A (server fs) for the session.
	if err := g.CheckToolCall(ctx, "mcp", "fs_read_file", "fs_read_file {}"); err != nil {
		t.Fatalf("grant call: %v", err)
	}
	if len(prompter.calls) != 1 {
		t.Fatalf("first call should prompt; got %d", len(prompter.calls))
	}

	// Same tool again: silent (rides the per-tool grant).
	if err := g.CheckToolCall(ctx, "mcp", "fs_read_file", "fs_read_file {\"p\":2}"); err != nil {
		t.Fatalf("same tool again: %v", err)
	}
	if len(prompter.calls) != 1 {
		t.Errorf("same MCP tool should not re-prompt; got %d", len(prompter.calls))
	}

	// Different tool, SAME server: must re-prompt (deny to confirm).
	prompter.decision = DecisionDeny
	if err := g.CheckToolCall(ctx, "mcp", "fs_write_file", "fs_write_file {}"); err == nil {
		t.Error("different tool on same server should not ride the grant")
	}
	if len(prompter.calls) != 2 {
		t.Errorf("different tool (same server) should prompt; got %d", len(prompter.calls))
	}

	// Different tool, DIFFERENT server: must re-prompt.
	if err := g.CheckToolCall(ctx, "mcp", "gh_create_issue", "gh_create_issue {}"); err == nil {
		t.Error("different tool on different server should not ride the grant")
	}
	if len(prompter.calls) != 3 {
		t.Errorf("different tool (different server) should prompt; got %d", len(prompter.calls))
	}
}

// TestGate_CheckToolCall_SkillNamespaceIsolated pins the same per-tool
// isolation for the skill namespace (#379).
func TestGate_CheckToolCall_SkillNamespaceIsolated(t *testing.T) {
	t.Parallel()
	prompter := &fakePrompter{decision: DecisionAllowSessionTool}
	g := New(Options{Mode: ModeAsk, Prompter: prompter})
	ctx := context.Background()

	if err := g.CheckToolCall(ctx, "skill", "load_skill", "load_skill {}"); err != nil {
		t.Fatalf("grant call: %v", err)
	}
	prompter.decision = DecisionDeny
	if err := g.CheckToolCall(ctx, "skill", "load_skill_resource", "load_skill_resource {}"); err == nil {
		t.Error("different skill tool should not ride the grant")
	}
	if len(prompter.calls) != 2 {
		t.Errorf("different skill tool should prompt; got %d", len(prompter.calls))
	}
}

// TestGate_CheckGeneric_LegacyBlanketGrantMatchesNothingNew documents
// that a blanket-namespace grant (the old CheckGeneric path, keyed by
// the whole "mcp" namespace) does not satisfy the new per-tool checks
// CheckToolCall performs — legacy blanket grants match nothing under
// the new keying (#379).
func TestGate_CheckGeneric_LegacyBlanketGrantMatchesNothingNew(t *testing.T) {
	t.Parallel()
	prompter := &fakePrompter{decision: DecisionAllowSessionTool}
	g := New(Options{Mode: ModeAsk, Prompter: prompter})
	ctx := context.Background()

	// Establish a legacy blanket "mcp" grant via CheckGeneric.
	if err := g.CheckGeneric(ctx, "mcp", "fs_read_file {}"); err != nil {
		t.Fatalf("legacy grant: %v", err)
	}
	// A CheckToolCall for any mcp tool must still prompt — the blanket
	// grant is keyed "mcp", the per-tool check looks up "mcp/<tool>".
	// Use a distinct arg summary so the per-call exact-match cache
	// (a separate, legitimate mechanism) isn't what's being tested.
	prompter.decision = DecisionDeny
	if err := g.CheckToolCall(ctx, "mcp", "fs_read_file", "fs_read_file {\"other\":true}"); err == nil {
		t.Error("legacy blanket mcp grant should not satisfy the per-tool check")
	}
	if len(prompter.calls) != 2 {
		t.Errorf("per-tool check should re-prompt despite legacy grant; got %d", len(prompter.calls))
	}
}
