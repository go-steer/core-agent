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
	"sync"
	"testing"
)

// fakeGrantStore records persisted grants and optionally fails.
type fakeGrantStore struct {
	mu     sync.Mutex
	grants []Grant
	err    error
}

func (s *fakeGrantStore) Persist(_ context.Context, g Grant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.grants = append(s.grants, g)
	return nil
}

func (s *fakeGrantStore) all() []Grant {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Grant(nil), s.grants...)
}

// TestAllowAlways_BashGrant_PersistsAndInstallsPolicy is the contract
// test for the #386 grant-persistence fix: a DecisionAllowAlways on a
// bash prompt must (a) install a real in-memory policy pattern so the
// next identical call short-circuits without a prompt, and (b) hand
// the fully-expanded grant to the GrantStore.
func TestAllowAlways_BashGrant_PersistsAndInstallsPolicy(t *testing.T) {
	t.Parallel()
	store := &fakeGrantStore{}
	prompter := &fakePrompter{decision: DecisionAllowAlways}
	g := New(Options{Prompter: prompter, GrantStore: store})

	if err := g.CheckBash(context.Background(), "git status"); err != nil {
		t.Fatalf("CheckBash with AllowAlways: %v", err)
	}
	if len(prompter.calls) != 1 {
		t.Fatalf("prompter calls = %d, want 1", len(prompter.calls))
	}

	got := store.all()
	if len(got) != 1 {
		t.Fatalf("persisted grants = %d, want 1", len(got))
	}
	want := Grant{Kind: PromptKindBash, Tool: "bash", Key: "git status", Pattern: "bash:git status"}
	if got[0] != want {
		t.Errorf("grant = %+v, want %+v", got[0], want)
	}

	// The in-memory policy add means the SAME command now passes at
	// the policy layer — no second prompt, even on a fresh derived
	// session (session-allow maps start empty there, so only the
	// shared policy can explain the pass).
	sub := g.DeriveForSession("s2", prompter)
	if err := sub.CheckBash(context.Background(), "git status"); err != nil {
		t.Fatalf("repeat CheckBash on derived session: %v", err)
	}
	if len(prompter.calls) != 1 {
		t.Errorf("repeat call re-prompted (calls = %d); policy pattern not installed", len(prompter.calls))
	}
}

// TestAllowAlways_GenericGrant_PolicyPatternShape pins the pattern
// grammar for non-bash generic prompts: "<PersistTool>:<PersistKey>".
func TestAllowAlways_GenericGrant_PolicyPatternShape(t *testing.T) {
	t.Parallel()
	store := &fakeGrantStore{}
	prompter := &fakePrompter{decision: DecisionAllowAlways}
	g := New(Options{Prompter: prompter, GrantStore: store})

	if err := g.CheckGeneric(context.Background(), "fetch_url", "https://example.com"); err != nil {
		t.Fatalf("CheckGeneric: %v", err)
	}
	got := store.all()
	if len(got) != 1 || got[0].Pattern != "fetch_url:https://example.com" {
		t.Fatalf("grants = %+v, want one with Pattern fetch_url:https://example.com", got)
	}
	if got[0].Access != AccessNone {
		t.Errorf("non-path grant Access = %v, want AccessNone", got[0].Access)
	}
}

// TestAllowAlways_PathScopeGrant_ExpandsAndPromotes covers the path
// branch: the persisted Pattern must be the subtree-expanded form the
// gate installed in-memory, and Access must reflect the read→r /
// write→rw promotion.
func TestAllowAlways_PathScopeGrant_ExpandsAndPromotes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	cases := []struct {
		name       string
		op         Access
		wantAccess Access
	}{
		{"read promotes to r", AccessRead, AccessRead},
		{"write promotes to rw", AccessWrite, AccessReadWrite},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeGrantStore{}
			prompter := &fakePrompter{decision: DecisionAllowAlways}
			scope, err := NewPathScope(dir, "", nil)
			if err != nil {
				t.Fatalf("NewPathScope: %v", err)
			}
			g := New(Options{Prompter: prompter, Scope: scope, GrantStore: store})

			// Out-of-scope path forces the PromptKindPathScope prompt.
			outside := filepath.Join(t.TempDir(), "other.txt")
			var checkErr error
			if tc.op == AccessRead {
				checkErr = g.CheckFileRead(context.Background(), "read_file", outside)
			} else {
				checkErr = g.CheckFileWrite(context.Background(), "write_file", outside)
			}
			if checkErr != nil {
				t.Fatalf("check with AllowAlways: %v", checkErr)
			}

			got := store.all()
			if len(got) != 1 {
				t.Fatalf("persisted grants = %d, want 1", len(got))
			}
			if got[0].Kind != PromptKindPathScope {
				t.Errorf("Kind = %v, want PromptKindPathScope", got[0].Kind)
			}
			if got[0].Access != tc.wantAccess {
				t.Errorf("Access = %v, want %v", got[0].Access, tc.wantAccess)
			}
			if !strings.HasSuffix(got[0].Pattern, "/...") {
				t.Errorf("Pattern = %q, want subtree-expanded form ending in /...", got[0].Pattern)
			}
		})
	}
}

// TestAllowAlways_PersistError_Surfaces pins the fail-loud contract:
// when the store errors, the gated call fails — the operator asked
// for a durable grant and must learn it didn't stick, rather than
// silently running with a session-only downgrade.
func TestAllowAlways_PersistError_Surfaces(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk full")
	store := &fakeGrantStore{err: boom}
	prompter := &fakePrompter{decision: DecisionAllowAlways}
	g := New(Options{Prompter: prompter, GrantStore: store})

	err := g.CheckBash(context.Background(), "ls -la")
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("CheckBash with failing store: err = %v, want wrapped %v", err, boom)
	}
}

// TestAllowAlways_NilStore_InMemoryOnly pins the default: no store
// wired, no error, grant applies for the process lifetime.
func TestAllowAlways_NilStore_InMemoryOnly(t *testing.T) {
	t.Parallel()
	prompter := &fakePrompter{decision: DecisionAllowAlways}
	g := New(Options{Prompter: prompter})

	if err := g.CheckBash(context.Background(), "make test"); err != nil {
		t.Fatalf("CheckBash nil store: %v", err)
	}
	// Still short-circuits on repeat via the installed policy pattern.
	if err := g.CheckBash(context.Background(), "make test"); err != nil {
		t.Fatalf("repeat CheckBash nil store: %v", err)
	}
	if len(prompter.calls) != 1 {
		t.Errorf("calls = %d, want 1 (policy add should suppress re-prompt)", len(prompter.calls))
	}
}

// TestSetGrantStore_And_DeriveShares covers the two wiring seams:
// SetGrantStore swaps the backend post-construction (the mid-startup
// UI-swap pattern), and DeriveForSession shares the template's store
// by reference — a sub-gate's always-grant persists through the same
// daemon-wide backend, consistent with the documented Policy/Scope
// sharing rules.
func TestSetGrantStore_And_DeriveShares(t *testing.T) {
	t.Parallel()
	store := &fakeGrantStore{}
	prompter := &fakePrompter{decision: DecisionAllowAlways}
	template := New(Options{Prompter: prompter})
	template.SetGrantStore(store)

	sub := template.DeriveForSession("sid-1", prompter)
	if err := sub.CheckGeneric(context.Background(), "todo", "write list"); err != nil {
		t.Fatalf("sub-gate CheckGeneric: %v", err)
	}
	got := store.all()
	if len(got) != 1 || got[0].Pattern != "todo:write list" {
		t.Fatalf("template store saw %+v, want the sub-gate's grant", got)
	}
}

// TestControlPlaneWrite_NeverPersists pins the elevated-approval
// carve-out: a control-plane write approval routes through its own
// prompt path and must NOT reach the GrantStore even when the
// prompter answers "always" — no standing bypass for config.json.
func TestControlPlaneWrite_NeverPersists(t *testing.T) {
	t.Parallel()
	store := &fakeGrantStore{}
	prompter := &fakePrompter{decision: DecisionAllowAlways}
	g := New(Options{Prompter: prompter, GrantStore: store})

	agentsDir := t.TempDir()
	cp := filepath.Join(agentsDir, "config.json")
	if err := g.checkControlPlaneWrite(context.Background(), "write_file", cp); err != nil {
		t.Fatalf("checkControlPlaneWrite: %v", err)
	}
	if got := store.all(); len(got) != 0 {
		t.Fatalf("control-plane approval persisted %+v; want no grants", got)
	}
}
