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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// The approver's instructions file is privilege-bearing (#1175
// decision 7). These tests pin that a write to it takes the elevated
// control-plane path however the gate was built, and that nothing else
// does.

func instructionsLayout(t *testing.T) (root, file string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(root, "policy", "approver.md")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("refuse kubectl delete"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, file
}

func TestApproverInstructionsWrite_IsControlPlane(t *testing.T) {
	t.Parallel()
	root, file := instructionsLayout(t)
	link := filepath.Join(root, "innocent.md")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	scope, err := NewPathScope(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	viaOptions := func() *Gate {
		return New(Options{Mode: ModeYolo, Scope: scope, ApproverInstructionsFile: file})
	}
	viaSetter := func() *Gate {
		g := New(Options{Mode: ModeYolo, Scope: scope})
		g.SetApprover(nil, file)
		return g
	}
	derived := func() *Gate { return viaSetter().DeriveForSession("s1", nil) }
	for name, build := range map[string]func() *Gate{"options": viaOptions, "setter": viaSetter, "derived": derived} {
		// The upper-cased spelling is a different file here, but the same
		// one on a case-insensitive filesystem; it errs toward the prompt.
		upper := filepath.Join(filepath.Dir(file), "APPROVER.md")
		for _, path := range []string{file, link, upper} {
			err := build().CheckFileWrite(context.Background(), "write_file", path)
			if !errors.Is(err, ErrControlPlaneWrite) {
				t.Errorf("%s: yolo write to %s = %v, want ErrControlPlaneWrite", name, path, err)
			}
		}
	}
	// A sibling in the same directory is an ordinary write.
	sibling := filepath.Join(filepath.Dir(file), "notes.md")
	if err := viaSetter().CheckFileWrite(context.Background(), "write_file", sibling); err != nil {
		t.Errorf("sibling write: %v, want an ordinary yolo write", err)
	}
	// No instructions file configured: the same path is ordinary.
	if err := New(Options{Mode: ModeYolo, Scope: scope}).CheckFileWrite(context.Background(), "write_file", file); err != nil {
		t.Errorf("write with no instructions file configured: %v", err)
	}
}

// The elevated tier is the prompt, not the approver: in auto, with the
// file eligible by pattern, the write still goes to a person.
func TestApproverInstructionsWrite_NeverReachesApprover(t *testing.T) {
	t.Parallel()
	_, file := instructionsLayout(t)
	a := &stubApprover{verdict: allowVerdict}
	p := &fakePrompter{decision: DecisionAllowOnce}
	g := autoGate(t, a, p, func(o *Options) { o.AutoEligible = mustPolicy(t, []string{"write_file:*"}, nil) })
	g.SetApprover(a, file)
	ctx, _ := taskCtx()
	if err := g.CheckFileWriteWithArgs(ctx, "write_file", file, map[string]any{"path": "x"}); err != nil {
		t.Fatalf("CheckFileWriteWithArgs: %v", err)
	}
	if a.n() != 0 {
		t.Errorf("approver asked %d times about its own instructions file", a.n())
	}
	if len(p.calls) != 1 || p.calls[0].Kind != PromptKindControlPlaneWrite {
		t.Errorf("prompter calls = %+v, want one control-plane prompt", p.calls)
	}
}

// SetApprover also feeds the never-list, so bash naming the file
// escalates without an approver call.
func TestSetApprover_ProtectsMentions(t *testing.T) {
	t.Parallel()
	_, file := instructionsLayout(t)
	a := &stubApprover{verdict: allowVerdict}
	p := &fakePrompter{decision: DecisionAllowOnce}
	// The approver arrives only through the setter.
	g := autoGate(t, nil, p, func(o *Options) { o.AutoEligible = mustPolicy(t, []string{"bash:*"}, nil) })
	g.SetApprover(a, file)
	ctx, _ := taskCtx()
	for _, cmd := range []string{"go test ./... -run " + file, "go test ./... -run approver.md"} {
		if err := bashCall(ctx, g, cmd, json.RawMessage(`{"command":`+mustJSON(cmd)+`}`)); err != nil {
			t.Fatalf("bashCall(%q): %v", cmd, err)
		}
	}
	if a.n() != 0 {
		t.Errorf("approver judged %d calls naming its instructions file, want 0", a.n())
	}
	if err := bashCall(ctx, g, testCmd, testArgs); err != nil {
		t.Fatalf("bashCall: %v", err)
	}
	if a.n() != 1 {
		t.Errorf("approver calls = %d after an ordinary eligible call, want 1 (SetApprover must wire the approver)", a.n())
	}
}

func TestFromConfig_AutoEligible(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{Permissions: config.PermissionsConfig{
		Mode:            "ask",
		ApprovalTimeout: "1m",
		Auto:            &config.AutoApproverConfig{Eligible: []string{"bash:go test *"}},
	}}
	g, err := FromConfig(cfg, t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.autoEligible == nil || g.autoEligible.Match("bash", testCmd) != OutcomeAllow {
		t.Errorf("auto.eligible not applied: %+v", g.autoEligible)
	}
	if g.autoEligible.Match("bash", "rm -rf /") == OutcomeAllow {
		t.Errorf("auto.eligible matched a call it does not list")
	}

	cfg.Permissions.Auto = nil
	if g, err = FromConfig(cfg, t.TempDir(), "", nil); err != nil || g.autoEligible != nil {
		t.Errorf("no auto block: autoEligible = %+v, err = %v; want nil, nil", g.autoEligible, err)
	}

	cfg.Permissions.Auto = &config.AutoApproverConfig{Eligible: []string{"bash:[unclosed"}}
	if _, err := FromConfig(cfg, t.TempDir(), "", nil); err == nil || !strings.Contains(err.Error(), "permissions.auto.eligible") {
		t.Errorf("malformed eligible pattern: err = %v, want one naming permissions.auto.eligible", err)
	}
}
