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
	"testing"
)

func TestDeriveForSession_FreshSessionState(t *testing.T) {
	t.Parallel()
	template := New(Options{Mode: ModeAsk, RequirePlanArtifact: true})
	// Prime the template with some session state so we can verify
	// none of it carries over to the derived sub-gate.
	template.rememberSession("write_file", "/tmp/x")
	template.rememberSessionTool("read_file")
	template.rememberSessionVerb("bash", "ls")
	template.MarkPlanRecorded()
	template.recordApproval("write_file", "/tmp/x", DecisionAllowSession)

	sub := template.DeriveForSession("sess-1", nil)

	// Sub-gate sees none of the template's session state.
	if sub.sessionAllowed("write_file", "/tmp/x") {
		t.Error("sub-gate inherited template's session-allow grant; must start fresh")
	}
	if sub.sessionToolAllowed("read_file") {
		t.Error("sub-gate inherited template's session-tool allow; must start fresh")
	}
	if sub.sessionVerbAllowed("bash", "ls") {
		t.Error("sub-gate inherited template's session-verb allow; must start fresh")
	}
	if sub.IsPlanRecorded() {
		t.Error("sub-gate inherited template's planRecorded; must start fresh")
	}
	if len(sub.Approvals()) != 0 {
		t.Errorf("sub-gate inherited template's approval log; want empty, got %d entries", len(sub.Approvals()))
	}
}

func TestDeriveForSession_InheritsImmutableConfig(t *testing.T) {
	t.Parallel()
	template := New(Options{Mode: ModeYolo, RequirePlanArtifact: true})
	sub := template.DeriveForSession("sess-1", nil)

	if sub.Mode() != ModeYolo {
		t.Errorf("sub-gate Mode: got %q, want %q (copied from template)", sub.Mode(), ModeYolo)
	}
	if !sub.PlanRequired() {
		t.Error("sub-gate PlanRequired: got false, want true (copied from template)")
	}
}

func TestDeriveForSession_SessionGrantsAreIsolated(t *testing.T) {
	t.Parallel()
	// The headline isolation property: a grant on sub-gate A must not
	// be visible to sub-gate B or to the template. This is the exact
	// scenario the design's "User A's /allow write_file allow-session
	// must NOT carry over to user B's session" invariant addresses.
	template := New(Options{Mode: ModeAsk})
	subA := template.DeriveForSession("sess-A", nil)
	subB := template.DeriveForSession("sess-B", nil)

	subA.rememberSession("write_file", "/tmp/alice.txt")
	subA.rememberSessionTool("read_file")
	subA.rememberSessionVerb("bash", "git")

	if subB.sessionAllowed("write_file", "/tmp/alice.txt") {
		t.Error("session-allow grant leaked from sub-gate A to sub-gate B")
	}
	if subB.sessionToolAllowed("read_file") {
		t.Error("session-tool grant leaked from sub-gate A to sub-gate B")
	}
	if subB.sessionVerbAllowed("bash", "git") {
		t.Error("session-verb grant leaked from sub-gate A to sub-gate B")
	}
	if template.sessionAllowed("write_file", "/tmp/alice.txt") {
		t.Error("session-allow grant leaked from sub-gate back to template")
	}
}

func TestDeriveForSession_PlanFlagIsolated(t *testing.T) {
	t.Parallel()
	template := New(Options{Mode: ModeAsk, RequirePlanArtifact: true})
	subA := template.DeriveForSession("sess-A", nil)
	subB := template.DeriveForSession("sess-B", nil)

	subA.MarkPlanRecorded()

	if subB.IsPlanRecorded() {
		t.Error("MarkPlanRecorded leaked from sub-gate A to sub-gate B (would let B's mutating tools bypass plan-first)")
	}
	if template.IsPlanRecorded() {
		t.Error("MarkPlanRecorded leaked from sub-gate back to template")
	}
}

func TestDeriveForSession_ApprovalLogIsolated(t *testing.T) {
	t.Parallel()
	template := New(Options{Mode: ModeAsk})
	subA := template.DeriveForSession("sess-A", nil)
	subB := template.DeriveForSession("sess-B", nil)

	subA.recordApproval("write_file", "/x", DecisionAllowOnce)
	subA.recordApproval("write_file", "/y", DecisionAllowSession)

	if got := len(subB.Approvals()); got != 0 {
		t.Errorf("sub-gate B saw %d approvals from sub-gate A; must be 0", got)
	}
	if got := len(template.Approvals()); got != 0 {
		t.Errorf("template saw %d approvals from sub-gate A; must be 0", got)
	}
	if got := len(subA.Approvals()); got != 2 {
		t.Errorf("sub-gate A own approvals: got %d, want 2", got)
	}
}

func TestDeriveForSession_ModeIsolatedFromTemplate(t *testing.T) {
	t.Parallel()
	// A TUI-driven SetMode (e.g., operator toggles the permission-mode
	// chip in their session) must NOT leak to sibling sessions or back
	// to the template. Per-session mode is what makes "user A flips to
	// yolo for a long refactor while user B stays in ask" possible.
	template := New(Options{Mode: ModeAsk})
	subA := template.DeriveForSession("sess-A", nil)
	subB := template.DeriveForSession("sess-B", nil)

	subA.SetMode(ModeYolo)

	if subB.Mode() == ModeYolo {
		t.Error("SetMode leaked from sub-gate A to sub-gate B")
	}
	if template.Mode() == ModeYolo {
		t.Error("SetMode leaked from sub-gate back to template")
	}
	if subA.Mode() != ModeYolo {
		t.Errorf("sub-gate A own mode: got %q, want %q", subA.Mode(), ModeYolo)
	}
}

func TestDeriveForSession_PolicyAndScopeAreSharedByDesign(t *testing.T) {
	t.Parallel()
	// Documented limitation per docs/multi-session-design.md: /allow
	// + /deny + always-allow scope decisions apply daemon-wide because
	// every sub-gate shares the template's *Policy and *PathScope
	// pointers. Per-session policy / scope carve-outs are deferred.
	//
	// This test pins the current pointer-sharing behavior so a future
	// change (per-session policy / scope) updates the test in lockstep
	// with the code.
	template := New(Options{Mode: ModeAllow})
	subA := template.DeriveForSession("sess-A", nil)
	subB := template.DeriveForSession("sess-B", nil)

	if subA.policy != template.policy {
		t.Error("sub-gate A's policy pointer differs from template (would isolate /allow + /deny per session, which is a future feature, not v2.4)")
	}
	if subB.policy != template.policy {
		t.Error("sub-gate B's policy pointer differs from template")
	}
	if subA.scope != template.scope {
		t.Error("sub-gate A's scope pointer differs from template (would isolate AddAlwaysAllow per session, deferred to a future release)")
	}
	if subB.scope != template.scope {
		t.Error("sub-gate B's scope pointer differs from template")
	}
}

func TestDeriveForSession_SessionIDStored(t *testing.T) {
	t.Parallel()
	template := New(Options{Mode: ModeAsk})
	if template.SessionID() != "" {
		t.Errorf("template SessionID: got %q, want empty (templates carry no sid)", template.SessionID())
	}

	sub := template.DeriveForSession("sess-abc-123", nil)
	if sub.SessionID() != "sess-abc-123" {
		t.Errorf("sub-gate SessionID: got %q, want %q", sub.SessionID(), "sess-abc-123")
	}
}

func TestDeriveForSession_EmptySessionIDAccepted(t *testing.T) {
	t.Parallel()
	// Back-compat: callers that haven't threaded a sessionID through
	// yet should still get a working sub-gate. The empty sid just
	// means "no diagnostic label."
	template := New(Options{Mode: ModeAsk})
	sub := template.DeriveForSession("", nil)
	if sub == nil {
		t.Fatal("DeriveForSession with empty sid returned nil; must return a usable gate")
	}
	if sub.SessionID() != "" {
		t.Errorf("empty sid should round-trip as empty; got %q", sub.SessionID())
	}
	// Sanity check: sub-gate still works.
	sub.rememberSession("read_file", "/tmp/x")
	if !sub.sessionAllowed("read_file", "/tmp/x") {
		t.Error("sub-gate built with empty sid doesn't accept grants")
	}
}

func TestDeriveForSession_PrompterIsPerSession(t *testing.T) {
	t.Parallel()
	// Each sub-gate carries its own prompter — typically the
	// HTTP-driven broker for an attach-mode session vs stdin for a
	// local interactive run.
	template := New(Options{Mode: ModeAsk})
	if template.HasPrompter() {
		t.Error("template built without prompter reports HasPrompter=true")
	}

	subA := template.DeriveForSession("sess-A", nil)
	subB := template.DeriveForSession("sess-B", &denyingPrompter{})

	if subA.HasPrompter() {
		t.Error("sub-gate A built with nil prompter reports HasPrompter=true")
	}
	if !subB.HasPrompter() {
		t.Error("sub-gate B built with non-nil prompter reports HasPrompter=false")
	}
	if template.HasPrompter() {
		t.Error("setting a prompter on sub-gate B leaked to template")
	}
}

// denyingPrompter is the minimum that satisfies the Prompter interface
// for the per-session prompter test. AskApproval is never invoked by
// these tests — the prompter's mere presence is what HasPrompter
// reports on.
type denyingPrompter struct{}

func (denyingPrompter) AskApproval(_ context.Context, _ PromptRequest) (Decision, error) {
	return DecisionDeny, nil
}
