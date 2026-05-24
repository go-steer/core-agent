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
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/config"
)

// fakePrompter records the requests it was asked about and returns a
// scripted decision.
type fakePrompter struct {
	decision Decision
	err      error
	calls    []PromptRequest
}

func (f *fakePrompter) AskApproval(_ context.Context, req PromptRequest) (Decision, error) {
	f.calls = append(f.calls, req)
	return f.decision, f.err
}

// defaultGateConfig returns a minimal valid config for FromConfig
// tests: model name set so Validate() would pass, ask mode, no
// user-supplied allow/deny entries. The temp dir signature mirrors
// how callers use it (one t.TempDir() per test) even though we don't
// use the value here.
func defaultGateConfig(t *testing.T) *config.Config {
	t.Helper()
	return config.DefaultConfig()
}

func TestGate_BashDenylistAlwaysWins(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo}) // even yolo can't override
	err := g.CheckBash(context.Background(), "rm -rf /")
	if err == nil || !strings.Contains(err.Error(), "filesystem root") {
		t.Fatalf("expected denylist refusal, got %v", err)
	}
}

func TestGate_AllowMode_RequiresExplicitAllow(t *testing.T) {
	t.Parallel()
	pol, _ := NewPolicy([]string{"bash:git status"}, nil)
	g := New(Options{Mode: ModeAllow, Policy: pol})

	if err := g.CheckBash(context.Background(), "git status"); err != nil {
		t.Errorf("allowlisted command rejected: %v", err)
	}
	if err := g.CheckBash(context.Background(), "git push"); err == nil {
		t.Errorf("non-allowlisted command should be denied in allow mode")
	}
}

func TestGate_YoloMode_AllowsAnythingExceptDenylist(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeYolo})
	if err := g.CheckBash(context.Background(), "git push"); err != nil {
		t.Errorf("yolo should allow git push: %v", err)
	}
}

func TestGate_AskMode_PromptsAndHonors(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionAllowOnce}
	g := New(Options{Mode: ModeAsk, Prompter: p})

	if err := g.CheckBash(context.Background(), "git push"); err != nil {
		t.Fatalf("expected approval to allow: %v", err)
	}
	if len(p.calls) != 1 || p.calls[0].Detail != "git push" {
		t.Errorf("prompt not called with expected request: %+v", p.calls)
	}
	g.CheckBash(context.Background(), "git push")
	if len(p.calls) != 2 {
		t.Errorf("AllowOnce should not cache; call count = %d", len(p.calls))
	}
}

func TestGate_AskMode_AllowSessionCaches(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionAllowSession}
	g := New(Options{Mode: ModeAsk, Prompter: p})

	g.CheckBash(context.Background(), "git push")
	g.CheckBash(context.Background(), "git push")
	if len(p.calls) != 1 {
		t.Errorf("AllowSession should cache; call count = %d", len(p.calls))
	}
}

func TestGate_AskMode_NoPrompterFailsClearly(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeAsk}) // no prompter
	err := g.CheckBash(context.Background(), "git push")
	if err == nil || !errors.Is(err, ErrNoPrompter) {
		t.Errorf("expected ErrNoPrompter, got %v", err)
	}
}

func TestGate_AskMode_DenialReturnsError(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionDeny}
	g := New(Options{Mode: ModeAsk, Prompter: p})
	err := g.CheckBash(context.Background(), "git push")
	if err == nil || !strings.Contains(err.Error(), "denied by user") {
		t.Errorf("expected user-denial error, got %v", err)
	}
}

func TestGate_FileRead_InScopeNoPrompt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	scope, _ := NewPathScope(root, "", nil)
	p := &fakePrompter{decision: DecisionDeny}
	g := New(Options{Mode: ModeAsk, Scope: scope, Prompter: p})

	if err := g.CheckFileRead(context.Background(), "read_file", filepath.Join(root, "x.txt")); err != nil {
		t.Errorf("in-scope read should not prompt: %v", err)
	}
	if len(p.calls) != 0 {
		t.Errorf("prompt should not be called for in-scope reads")
	}
}

func TestGate_FileRead_OutOfScopePrompts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	other := t.TempDir()
	scope, _ := NewPathScope(root, "", nil)
	p := &fakePrompter{decision: DecisionAllowOnce}
	g := New(Options{Mode: ModeAsk, Scope: scope, Prompter: p})

	err := g.CheckFileRead(context.Background(), "read_file", filepath.Join(other, "y.txt"))
	if err != nil {
		t.Errorf("out-of-scope read denied after approval: %v", err)
	}
	if len(p.calls) != 1 {
		t.Errorf("expected one prompt, got %d", len(p.calls))
	}
}

func TestGate_FileWrite_AlwaysAllowExtendsScope(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	other := t.TempDir()
	scope, _ := NewPathScope(root, "", nil)
	p := &fakePrompter{decision: DecisionAllowAlways}
	g := New(Options{Mode: ModeAsk, Scope: scope, Prompter: p})

	target := filepath.Join(other, "deep", "file.txt")
	if err := g.CheckFileWrite(context.Background(), "write_file", target); err != nil {
		t.Fatalf("AllowAlways should pass: %v", err)
	}
	in, _ := scope.Contains(target)
	if !in {
		t.Errorf("AllowAlways should extend the path scope to include the target")
	}
}

// --- Verb-scoped session allow (DecisionAllowSessionVerb) ---

func TestGate_AllowSessionVerb_BroadensToVerb(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionAllowSessionVerb}
	g := New(Options{Mode: ModeAsk, Prompter: p})

	if err := g.CheckBash(context.Background(), "git status"); err != nil {
		t.Fatalf("first git command: %v", err)
	}
	// The prompt request must have included Verb so the modal can
	// render the option; without it, the host would have hidden the
	// option and the decision would never have been picked.
	if got := p.calls[0].Verb; got != "git" {
		t.Errorf("first prompt Verb = %q, want \"git\"", got)
	}

	// Subsequent `git *` commands must NOT re-prompt.
	for _, cmd := range []string{"git log", "git diff origin/main..HEAD", "git rev-parse HEAD"} {
		if err := g.CheckBash(context.Background(), cmd); err != nil {
			t.Errorf("verb-allow did not cover %q: %v", cmd, err)
		}
	}
	if len(p.calls) != 1 {
		t.Errorf("verb-allow should suppress subsequent prompts; got %d calls", len(p.calls))
	}

	// Different verb must still prompt.
	p.decision = DecisionDeny
	if err := g.CheckBash(context.Background(), "echo hi"); err == nil {
		t.Errorf("non-git command should re-prompt and be denied")
	}
	if len(p.calls) != 2 {
		t.Errorf("non-git command should have prompted; got %d calls", len(p.calls))
	}
}

// TestGate_AllowSessionVerb_RecordsRecommendablePattern checks that the
// approval log entry uses the `<verb> *` shape so /permissions can
// surface "bash:git *" as a permanent allowlist recommendation.
func TestGate_AllowSessionVerb_RecordsRecommendablePattern(t *testing.T) {
	t.Parallel()
	p := &fakePrompter{decision: DecisionAllowSessionVerb}
	g := New(Options{Mode: ModeAsk, Prompter: p})

	if err := g.CheckBash(context.Background(), "git status"); err != nil {
		t.Fatal(err)
	}
	approvals := g.Approvals()
	if len(approvals) != 1 {
		t.Fatalf("expected 1 approval, got %d", len(approvals))
	}
	if approvals[0].Key != "git *" {
		t.Errorf("approval Key = %q, want \"git *\" (drives /permissions recommendation)", approvals[0].Key)
	}
}

// --- Built-in allow bundle (FromConfig) ---

// TestGate_FromConfig_BuiltinAllowEnabledByDefault confirms the
// out-of-the-box experience: with a brand-new config and the default
// "ask" mode, common read-only commands like `pwd` and `ls` must NOT
// prompt. This is the user-facing payoff for the built-in bundle, and
// regressing it sends every fresh user back to per-command approvals.
func TestGate_FromConfig_BuiltinAllowEnabledByDefault(t *testing.T) {
	t.Parallel()
	cfg := defaultGateConfig(t)
	g, err := FromConfig(cfg, t.TempDir(), "", &fakePrompter{decision: DecisionDeny})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	for _, cmd := range []string{"pwd", "ls -la", "cat go.mod", "head -n 5 README.md"} {
		if err := g.CheckBash(context.Background(), cmd); err != nil {
			t.Errorf("built-in read_only should allow %q: %v", cmd, err)
		}
	}
}

func TestGate_FromConfig_BuiltinAllowDisablable(t *testing.T) {
	t.Parallel()
	cfg := defaultGateConfig(t)
	off := false
	cfg.Permissions.UseBuiltinAllow = &off
	g, err := FromConfig(cfg, t.TempDir(), "", &fakePrompter{decision: DecisionDeny})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	// With defaults disabled, pwd should now hit the prompter
	// (which denies) and surface a denial error.
	if err := g.CheckBash(context.Background(), "pwd"); err == nil {
		t.Error("use_builtin_allow=false should make pwd require approval")
	}
}

func TestGate_FromConfig_BuiltinAllowExtras(t *testing.T) {
	t.Parallel()
	cfg := defaultGateConfig(t)
	cfg.Permissions.BuiltinAllowExtras = []string{BundleDevTools}
	g, err := FromConfig(cfg, t.TempDir(), "", &fakePrompter{decision: DecisionDeny})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	if err := g.CheckBash(context.Background(), "git status"); err != nil {
		t.Errorf("dev_tools extra should allow git status: %v", err)
	}
}

func TestGate_FromConfig_UnknownBundleErrors(t *testing.T) {
	t.Parallel()
	cfg := defaultGateConfig(t)
	cfg.Permissions.BuiltinAllowExtras = []string{"bogus"}
	if _, err := FromConfig(cfg, t.TempDir(), "", nil); err == nil {
		t.Error("unknown bundle name should error at gate construction")
	}
}

func TestGate_FromConfig_UserAllowMergedWithBuiltin(t *testing.T) {
	t.Parallel()
	cfg := defaultGateConfig(t)
	cfg.Permissions.Allow = []string{"bash:make *"}
	g, err := FromConfig(cfg, t.TempDir(), "", &fakePrompter{decision: DecisionDeny})
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	// Both the built-in (`pwd`) and the user-supplied entry (`make *`)
	// must be honored — the merge order isn't supposed to clobber.
	if err := g.CheckBash(context.Background(), "pwd"); err != nil {
		t.Errorf("built-in pwd should still pass after user adds entries: %v", err)
	}
	if err := g.CheckBash(context.Background(), "make build"); err != nil {
		t.Errorf("user-supplied make * should pass: %v", err)
	}
}
