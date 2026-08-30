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

package config

import (
	"errors"
	"path/filepath"
	"sync"
)

// mutateMu serializes every Load → mutate → Save read-modify-write
// that goes through Mutate. Save's atomic rename keeps a lone writer
// from corrupting the file, but atomicity alone does not make
// concurrent RMWs safe: two goroutines that Load the same snapshot
// each Save their own mutation, and the later rename silently erases
// the earlier one's entry — including a permissions DENY, which
// fails open (#482). One process-wide mutex (rather than
// per-agentsDir) is deliberate: config writes are tiny, rare
// operator decisions, so cross-directory serialization is
// unmeasurable and a per-dir lock table would just add machinery.
// Writers in OTHER processes are out of scope.
//
// Every in-tree config read-modify-write MUST route through Mutate —
// the helpers in this package and pkg/permissions' ConfigGrantStore
// all do (moved here from pkg/compose under #492 so the single-lock
// invariant has one home).
var mutateMu sync.Mutex

// ErrNoChange, returned by a Mutate fn, means "the config is already
// in the desired state — skip the save". Mutate then returns nil.
// Lets idempotent helpers avoid rewriting an unchanged file.
var ErrNoChange = errors.New("config: no change")

// Mutate runs fn against the loaded config for agentsDir and
// atomically persists the result, all under the package-wide
// serialization lock (#482). fn returning ErrNoChange skips the save
// and reports success; any other error aborts without saving.
func Mutate(agentsDir string, fn func(*Config) error) error {
	mutateMu.Lock()
	defer mutateMu.Unlock()
	cfg, err := Load(agentsDir)
	if err != nil {
		return err
	}
	if err := fn(cfg); err != nil {
		if errors.Is(err, ErrNoChange) {
			return nil
		}
		return err
	}
	return Save(filepath.Join(agentsDir, ConfigFileName), cfg)
}

// .agents/config.json persistence helpers behind the operator's
// /permissions, /allow, /deny, /model, and /theme flows. Moved from
// pkg/compose (#492): they are pure config mutations with no compose
// dependency, and hosts that never touch compose (bare library
// embeddings) need them too. The path-scope + grant-store helpers
// that depend on pkg/permissions live there instead
// (permissions.ConfigGrantStore, permissions.AppendPathScopeEntry);
// all of them serialize through Mutate.

// AppendPathScope adds pattern to path_scope.allow and rewrites the
// file atomically. If the file doesn't exist yet it is created with
// defaults so the addition has somewhere to live. Idempotent.
func AppendPathScope(agentsDir, pattern string) error {
	return Mutate(agentsDir, func(cfg *Config) error {
		for _, existing := range cfg.PathScope.Allow {
			if existing == pattern {
				return ErrNoChange
			}
		}
		cfg.PathScope.Allow = append(cfg.PathScope.Allow, pattern)
		return nil
	})
}

// AppendPermissionsAllow adds one or more patterns to
// permissions.allow. Idempotent — duplicate patterns are skipped
// silently so /permissions can be re-run without growing the file.
func AppendPermissionsAllow(agentsDir string, patterns []string) error {
	return Mutate(agentsDir, func(cfg *Config) error {
		return appendUnique(&cfg.Permissions.Allow, patterns)
	})
}

// AppendPermissionsDeny mirrors AppendPermissionsAllow for the deny
// list. Idempotent.
func AppendPermissionsDeny(agentsDir string, patterns []string) error {
	return Mutate(agentsDir, func(cfg *Config) error {
		return appendUnique(&cfg.Permissions.Deny, patterns)
	})
}

// appendUnique appends the not-yet-present patterns to *dst,
// returning ErrNoChange when every pattern was already there.
func appendUnique(dst *[]string, patterns []string) error {
	existing := make(map[string]bool, len(*dst))
	for _, p := range *dst {
		existing[p] = true
	}
	changed := false
	for _, p := range patterns {
		if existing[p] {
			continue
		}
		*dst = append(*dst, p)
		existing[p] = true
		changed = true
	}
	if !changed {
		return ErrNoChange
	}
	return nil
}

// AppendBuiltinAllowExtra adds name to
// permissions.builtin_allow_extras. Idempotent — re-enabling a
// bundle that's already on is a no-op. Validation against the bundle
// catalog (permissions.KnownBundles) happens in the caller's UX
// before this is called, so an invalid name never reaches disk.
func AppendBuiltinAllowExtra(agentsDir, name string) error {
	return Mutate(agentsDir, func(cfg *Config) error {
		for _, existing := range cfg.Permissions.BuiltinAllowExtras {
			if existing == name {
				return ErrNoChange
			}
		}
		cfg.Permissions.BuiltinAllowExtras = append(cfg.Permissions.BuiltinAllowExtras, name)
		return nil
	})
}

// PersistModelChoice writes the new model name so /model survives
// across runs. Caller is responsible for first invoking the
// in-memory rebuild — this is purely the disk side.
func PersistModelChoice(agentsDir, modelID string) error {
	return Mutate(agentsDir, func(cfg *Config) error {
		cfg.Model.Name = modelID
		return nil
	})
}

// PersistThemeChoice writes the picker's selection so /theme
// survives across runs. Validates before save so a bad name surfaces
// as a picker error instead of silently corrupting the file.
func PersistThemeChoice(agentsDir, themeName string) error {
	return Mutate(agentsDir, func(cfg *Config) error {
		cfg.UI.Theme = themeName
		return cfg.Validate()
	})
}

// PersistMouseChoice writes the /mouse toggle so it survives across
// runs (core-agent #859, core-tui #287).
//
// Always writes an explicit value, never clears the field back to nil.
// UI.Mouse is a tristate where nil means "no opinion, use the default"
// — and the default is on, so clearing it on a toggle-off would persist
// the opposite of what the operator just asked for. An operator who
// wants the nil back edits the file.
//
// This one matters more than the other two toggles: while capture is on
// the terminal never sees click-drag, so mouse-on removes native text
// selection. Without this, an operator who wanted selection back re-typed
// /mouse every single launch.
func PersistMouseChoice(agentsDir string, on bool) error {
	return Mutate(agentsDir, func(cfg *Config) error {
		cfg.UI.Mouse = &on
		return cfg.Validate()
	})
}
