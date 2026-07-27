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

package compose

import (
	"context"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

func TestConfigGrantStore_PolicyGrant_AppendsAllowPattern(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := &ConfigGrantStore{AgentsDir: dir}

	g := permissions.Grant{
		Kind:    permissions.PromptKindBash,
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
		t.Fatalf("permissions.allow has %d copies of the pattern, want 1 (allow=%v)", count, cfg.Permissions.Allow)
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
	g := permissions.Grant{
		Kind:    permissions.PromptKindPathScope,
		Tool:    "path_scope",
		Key:     "/data/reports/q3.csv",
		Pattern: "/data/reports/...",
		Access:  permissions.AccessRead,
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
	err := s.Persist(context.Background(), permissions.Grant{
		Kind: permissions.PromptKindBash, Pattern: "bash:ls",
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

	always := &scriptedPrompter{decision: permissions.DecisionAllowAlways}
	g1 := permissions.New(permissions.Options{Prompter: always})
	g1.SetGrantStore(&ConfigGrantStore{AgentsDir: dir})
	if err := g1.CheckBash(context.Background(), "kubectl get pods"); err != nil {
		t.Fatalf("first gate CheckBash: %v", err)
	}

	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	g2, err := permissions.FromConfig(cfg, dir, "", &scriptedPrompter{decision: permissions.DecisionDeny})
	if err != nil {
		t.Fatalf("FromConfig (restart): %v", err)
	}
	// The deny-everything prompter proves no prompt fires: only the
	// reloaded allow pattern can explain a pass.
	if err := g2.CheckBash(context.Background(), "kubectl get pods"); err != nil {
		t.Fatalf("restarted gate CheckBash: %v (grant did not survive restart)", err)
	}
}

// scriptedPrompter returns a fixed decision for every prompt.
type scriptedPrompter struct{ decision permissions.Decision }

func (p *scriptedPrompter) AskApproval(_ context.Context, _ permissions.PromptRequest) (permissions.Decision, error) {
	return p.decision, nil
}
