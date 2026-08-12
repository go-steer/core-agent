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
	"slices"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// persist.go and grantstore.go are deprecated forwarders left behind
// by the #492 package split. They look untestable — every body is a
// single delegation — but a forwarder is exactly the thing that can
// be silently wrong: point AppendPermissionsDeny at the allow-list
// helper and every caller still compiles, every call still succeeds,
// and the operator's deny grant lands in the wrong field. These tests
// assert the OBSERVABLE effect on disk, not that the call returns
// nil, so a mis-aimed forwarder fails here instead of in production.
//
// They also pin the field each helper writes, which is the property a
// future "just delete the deprecated shims" cleanup needs in order to
// verify it moved every caller to an equivalent call.

func TestPersistForwarders_WriteTheFieldTheyName(t *testing.T) {
	// Not parallel: config.Mutate serializes on a package-wide lock
	// (#482) and these subtests each own a distinct agentsDir, so
	// parallelism buys nothing and only lengthens lock contention.
	cases := []struct {
		name string
		call func(agentsDir string) error
		// want inspects the reloaded config and reports what's wrong,
		// or "" when the write landed in the right place.
		want func(cfg *config.Config) string
	}{
		{
			name: "AppendPathScope → path_scope.allow",
			call: func(dir string) error { return AppendPathScope(dir, "/srv/data/**") },
			want: func(cfg *config.Config) string {
				if !slices.Contains(cfg.PathScope.Allow, "/srv/data/**") {
					return "path_scope.allow missing the pattern"
				}
				return ""
			},
		},
		{
			name: "AppendPermissionsAllow → permissions.allow",
			call: func(dir string) error { return AppendPermissionsAllow(dir, []string{"read_file:*", "grep:*"}) },
			want: func(cfg *config.Config) string {
				if !slices.Contains(cfg.Permissions.Allow, "read_file:*") ||
					!slices.Contains(cfg.Permissions.Allow, "grep:*") {
					return "permissions.allow missing a pattern"
				}
				if len(cfg.Permissions.Deny) != 0 {
					return "wrote into permissions.deny — forwarder is aimed at the wrong helper"
				}
				return ""
			},
		},
		{
			name: "AppendPermissionsDeny → permissions.deny",
			call: func(dir string) error { return AppendPermissionsDeny(dir, []string{"write_file:*"}) },
			want: func(cfg *config.Config) string {
				if !slices.Contains(cfg.Permissions.Deny, "write_file:*") {
					return "permissions.deny missing the pattern"
				}
				if len(cfg.Permissions.Allow) != 0 {
					return "wrote into permissions.allow — forwarder is aimed at the wrong helper"
				}
				return ""
			},
		},
		{
			name: "AppendBuiltinAllowExtra → permissions.builtin_allow_extras",
			call: func(dir string) error { return AppendBuiltinAllowExtra(dir, "read-only") },
			want: func(cfg *config.Config) string {
				if !slices.Contains(cfg.Permissions.BuiltinAllowExtras, "read-only") {
					return "permissions.builtin_allow_extras missing the bundle"
				}
				return ""
			},
		},
		{
			name: "PersistModelChoice → model.name",
			call: func(dir string) error { return PersistModelChoice(dir, "gemini-3.5-flash") },
			want: func(cfg *config.Config) string {
				if cfg.Model.Name != "gemini-3.5-flash" {
					return "model.name = " + cfg.Model.Name
				}
				return ""
			},
		},
		{
			name: "PersistThemeChoice → ui.theme",
			call: func(dir string) error { return PersistThemeChoice(dir, "dark") },
			want: func(cfg *config.Config) string {
				if cfg.UI.Theme != "dark" {
					return "ui.theme = " + cfg.UI.Theme
				}
				return ""
			},
		},
		{
			name: "AppendPathScopeEntry → path_scope.allow_paths (typed)",
			// "read" is a long-form alias; it must land on disk in
			// the canonical short form, so the grant reads the same
			// however the operator spelled it.
			call: func(dir string) error { return AppendPathScopeEntry(dir, "/srv/logs", "read") },
			want: func(cfg *config.Config) string {
				for _, e := range cfg.PathScope.AllowPaths {
					if e.Path != "/srv/logs" {
						continue
					}
					if e.Access != "r" {
						return "access stored as " + e.Access + ", want the canonical \"r\""
					}
					return ""
				}
				return "path_scope.allow_paths missing the typed entry"
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := tc.call(dir); err != nil {
				t.Fatalf("call: %v", err)
			}
			cfg, err := config.Load(dir)
			if err != nil {
				t.Fatalf("reload config: %v", err)
			}
			if problem := tc.want(cfg); problem != "" {
				t.Errorf("%s", problem)
			}
		})
	}
}

func TestPersistForwarders_AreIdempotent(t *testing.T) {
	// The underlying helpers return ErrNoChange on a repeat and skip
	// the save. A forwarder that swallowed or mistranslated that
	// would either error on the second call or grow the file — an
	// operator re-running /allow shouldn't do either.
	dir := t.TempDir()
	for range 3 {
		if err := AppendPermissionsAllow(dir, []string{"read_file:*"}); err != nil {
			t.Fatalf("AppendPermissionsAllow: %v", err)
		}
		if err := AppendPathScope(dir, "/srv/data/**"); err != nil {
			t.Fatalf("AppendPathScope: %v", err)
		}
		if err := AppendBuiltinAllowExtra(dir, "read-only"); err != nil {
			t.Fatalf("AppendBuiltinAllowExtra: %v", err)
		}
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if n := len(cfg.Permissions.Allow); n != 1 {
		t.Errorf("permissions.allow has %d entries after 3 identical appends, want 1: %v", n, cfg.Permissions.Allow)
	}
	if n := len(cfg.PathScope.Allow); n != 1 {
		t.Errorf("path_scope.allow has %d entries after 3 identical appends, want 1: %v", n, cfg.PathScope.Allow)
	}
	if n := len(cfg.Permissions.BuiltinAllowExtras); n != 1 {
		t.Errorf("builtin_allow_extras has %d entries after 3 identical appends, want 1: %v", n, cfg.Permissions.BuiltinAllowExtras)
	}
}

func TestPersistForwarders_AccumulateRatherThanReplace(t *testing.T) {
	// A forwarder that dropped through to a "set" rather than an
	// "append" would pass every single-call test above and silently
	// revoke the operator's earlier grants.
	dir := t.TempDir()
	if err := AppendPermissionsAllow(dir, []string{"read_file:*"}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := AppendPermissionsAllow(dir, []string{"grep:*"}); err != nil {
		t.Fatalf("second append: %v", err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if !slices.Contains(cfg.Permissions.Allow, "read_file:*") {
		t.Errorf("second append dropped the first grant: %v", cfg.Permissions.Allow)
	}
	if !slices.Contains(cfg.Permissions.Allow, "grep:*") {
		t.Errorf("second grant missing: %v", cfg.Permissions.Allow)
	}
}

func TestAppendPathScopeEntry_UnionsAccessOnTheSamePath(t *testing.T) {
	// Granting read and then write to one path must widen the
	// existing entry, not append a second one that shadows it — a
	// duplicate whose access the scope checker reads first would
	// silently narrow the grant back to read.
	dir := t.TempDir()
	if err := AppendPathScopeEntry(dir, "/srv/logs", "r"); err != nil {
		t.Fatalf("grant read: %v", err)
	}
	if err := AppendPathScopeEntry(dir, "/srv/logs", "w"); err != nil {
		t.Fatalf("grant write: %v", err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	var entries []config.PathScopeAllowEntry
	for _, e := range cfg.PathScope.AllowPaths {
		if e.Path == "/srv/logs" {
			entries = append(entries, e)
		}
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries for /srv/logs, want 1 merged entry: %+v", len(entries), entries)
	}
	if entries[0].Access != "rw" {
		t.Errorf("access = %q, want %q", entries[0].Access, "rw")
	}
}

// acceptsPermissionsStore takes the pkg/permissions spelling only. A
// *compose.ConfigGrantStore is assignable to it while the two names are
// aliases; if either becomes a defined type in its own right, the two
// pointer types stop being assignable and the call fails to compile.
func acceptsPermissionsStore(*permissions.ConfigGrantStore) {}

func TestConfigGrantStore_IsTheRealType(t *testing.T) {
	t.Parallel()
	// ConfigGrantStore is a type ALIAS (=), not a defined type. If it
	// ever gets rewritten as `type ConfigGrantStore
	// permissions.ConfigGrantStore` — a one-character change that
	// still compiles at the declaration — the two stop being
	// interchangeable and every host passing a compose.ConfigGrantStore
	// where a permissions.GrantStore is wanted breaks.
	acceptsPermissionsStore(&ConfigGrantStore{})
}
