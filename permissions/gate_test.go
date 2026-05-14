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
