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

// TestGate_FileReadWrite_SkipScopeCheckOnSessionTool covers the file-
// path branch. Once read_file (or write_file) is trusted tool-wide,
// even out-of-scope paths must go through without an additional path-
// scope prompt.
func TestGate_FileReadWrite_SkipScopeCheckOnSessionTool(t *testing.T) {
	t.Parallel()
	prompter := &fakePrompter{decision: DecisionAllowSessionTool}
	scope, err := NewPathScope("/tmp/in-scope", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	g := New(Options{Mode: ModeAsk, Scope: scope, Prompter: prompter})

	ctx := context.Background()
	if err := g.CheckFileRead(ctx, "read_file", "/etc/hosts"); err != nil {
		t.Fatalf("first out-of-scope read: unexpected error: %v", err)
	}
	if len(prompter.calls) != 1 {
		t.Fatalf("first out-of-scope read should prompt; got %d", len(prompter.calls))
	}

	for _, p := range []string{"/etc/passwd", "/var/log/syslog", "/srv/secret"} {
		if err := g.CheckFileRead(ctx, "read_file", p); err != nil {
			t.Errorf("read_file %q after AllowSessionTool: unexpected error %v", p, err)
		}
	}
	if len(prompter.calls) != 1 {
		t.Errorf("post-AllowSessionTool reads should be silent; got total %d", len(prompter.calls))
	}
}
