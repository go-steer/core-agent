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
	"fmt"
	"sync"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

func TestConfigGrantStore_PolicyGrant_AppendsAllowPattern(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := &ConfigGrantStore{AgentsDir: dir}

	g := Grant{
		Kind:    PromptKindBash,
		Tool:    "bash",
		Key:     "git status",
		Pattern: "bash:git status",
	}
	if err := s.Persist(context.Background(), g); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	// Idempotent: same grant again is a no-op, not a duplicate row.
	if err := s.Persist(context.Background(), g); err != nil {
		t.Fatalf("Persist (repeat): %v", err)
	}

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	count := 0
	for _, p := range cfg.Permissions.Allow {
		if p == "bash:git status" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("allow has %d copies of the pattern, want 1 (allow=%v)", count, cfg.Permissions.Allow)
	}
}

func TestConfigGrantStore_PathGrant_TypedEntryCarriesAccess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := &ConfigGrantStore{AgentsDir: dir}

	// Read-only grant must reload as read-only. The legacy bare
	// path_scope.allow list reloads everything as rw — persisting
	// there would silently broaden the grant on restart, which is
	// exactly the surprise the gate's asymmetric promotion exists
	// to prevent.
	g := Grant{
		Kind:    PromptKindPathScope,
		Tool:    "path_scope",
		Key:     "/data/reports/q3.csv",
		Pattern: "/data/reports/...",
		Access:  AccessRead,
	}
	if err := s.Persist(context.Background(), g); err != nil {
		t.Fatalf("Persist: %v", err)
	}

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(cfg.PathScope.Allow) != 0 {
		t.Errorf("legacy path_scope.allow = %v, want empty (typed entries only)", cfg.PathScope.Allow)
	}
	if len(cfg.PathScope.AllowPaths) != 1 {
		t.Fatalf("allow_paths = %+v, want exactly one entry", cfg.PathScope.AllowPaths)
	}
	e := cfg.PathScope.AllowPaths[0]
	if e.Path != "/data/reports/..." || e.Access != "r" {
		t.Errorf("entry = %+v, want {/data/reports/... r}", e)
	}
}

func TestAppendPathScopeEntry_WidensToUnion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := AppendPathScopeEntry(dir, "/logs/...", "r"); err != nil {
		t.Fatalf("append r: %v", err)
	}
	// Same path, same access: no-op.
	if err := AppendPathScopeEntry(dir, "/logs/...", "r"); err != nil {
		t.Fatalf("append r again: %v", err)
	}
	// A later rw grant upgrades the entry in place.
	if err := AppendPathScopeEntry(dir, "/logs/...", "rw"); err != nil {
		t.Fatalf("append rw: %v", err)
	}

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(cfg.PathScope.AllowPaths) != 1 {
		t.Fatalf("allow_paths = %+v, want one (widened) entry", cfg.PathScope.AllowPaths)
	}
	if got := cfg.PathScope.AllowPaths[0].Access; got != "rw" {
		t.Errorf("Access = %q, want rw after widening", got)
	}
}

func TestConfigGrantStore_EmptyAgentsDir_NoOp(t *testing.T) {
	t.Parallel()
	s := &ConfigGrantStore{}
	err := s.Persist(context.Background(), Grant{
		Kind: PromptKindBash, Pattern: "bash:ls",
	})
	if err != nil {
		t.Fatalf("Persist with empty AgentsDir: %v, want nil (session-scoped fallback)", err)
	}
}

// TestConfigGrantStore_RoundTripsThroughGate is the end-to-end
// contract: a gate wired with the store answers "allow always", and
// a SECOND gate built from the saved config (a simulated restart)
// allows the same call without any prompt.
func TestConfigGrantStore_RoundTripsThroughGate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	always := &scriptedPrompter{decision: DecisionAllowAlways}
	g1 := New(Options{Prompter: always})
	g1.SetGrantStore(&ConfigGrantStore{AgentsDir: dir})
	if err := g1.CheckBash(context.Background(), "kubectl get pods"); err != nil {
		t.Fatalf("first gate CheckBash: %v", err)
	}

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	g2, err := FromConfig(cfg, dir, "", &scriptedPrompter{decision: DecisionDeny})
	if err != nil {
		t.Fatalf("FromConfig (restart): %v", err)
	}
	// The deny-everything prompter proves no prompt fires: only the
	// reloaded allow pattern can explain a pass.
	if err := g2.CheckBash(context.Background(), "kubectl get pods"); err != nil {
		t.Fatalf("restarted gate CheckBash: %v (grant did not survive restart)", err)
	}
}

// TestConfigGrantStore_ConcurrentPersistLosesNothing pins the #482
// fix: Persist (and the /allow + /deny slash helpers) are
// Load→mutate→Save read-modify-writes over one shared file, and one
// store instance is shared by every per-session sub-gate. Before the
// package-level configMu, 32 concurrent appends left only 2–3 grants
// on disk (4/4 runs) — and a lost DENY fails open. Every grant, deny,
// and path-scope entry written concurrently must survive.
func TestConfigGrantStore_ConcurrentPersistLosesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := &ConfigGrantStore{AgentsDir: dir}

	const n = 32
	var wg sync.WaitGroup
	errs := make(chan error, 3*n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Three racing writer flavors per iteration: a gate
			// "allow always" grant, an operator /deny, and a typed
			// path-scope grant.
			errs <- s.Persist(context.Background(), Grant{
				Kind:    PromptKindBash,
				Tool:    "bash",
				Pattern: fmt.Sprintf("bash:cmd-%02d", i),
			})
			errs <- config.AppendPermissionsDeny(dir, []string{fmt.Sprintf("bash:evil-%02d", i)})
			errs <- s.Persist(context.Background(), Grant{
				Kind:    PromptKindPathScope,
				Tool:    "path_scope",
				Pattern: fmt.Sprintf("/data/%02d/...", i),
				Access:  AccessRead,
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent persist: %v", err)
		}
	}

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	allow := make(map[string]bool, len(cfg.Permissions.Allow))
	for _, p := range cfg.Permissions.Allow {
		allow[p] = true
	}
	deny := make(map[string]bool, len(cfg.Permissions.Deny))
	for _, p := range cfg.Permissions.Deny {
		deny[p] = true
	}
	paths := make(map[string]bool, len(cfg.PathScope.AllowPaths))
	for _, e := range cfg.PathScope.AllowPaths {
		paths[e.Path] = true
	}
	for i := 0; i < n; i++ {
		if p := fmt.Sprintf("bash:cmd-%02d", i); !allow[p] {
			t.Errorf("allow grant %q lost (have %d of %d)", p, len(allow), n)
		}
		if p := fmt.Sprintf("bash:evil-%02d", i); !deny[p] {
			t.Errorf("DENY %q lost — fail-open (have %d of %d)", p, len(deny), n)
		}
		if p := fmt.Sprintf("/data/%02d/...", i); !paths[p] {
			t.Errorf("path-scope grant %q lost (have %d of %d)", p, len(paths), n)
		}
	}
}

// scriptedPrompter returns a fixed decision for every prompt.
type scriptedPrompter struct{ decision Decision }

func (p *scriptedPrompter) AskApproval(_ context.Context, _ PromptRequest) (Decision, error) {
	return p.decision, nil
}
