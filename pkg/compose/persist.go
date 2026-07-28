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
	"path/filepath"
	"sync"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// .agents/config.json persistence helpers behind the operator's
// /permissions, /allow, /deny, /model, and /theme flows, plus the
// ConfigGrantStore the gate persists "allow always" grants through.
// Exported so library consumers (and the bundled TUI's callbacks)
// share one implementation of the on-disk layout: hosts resolve
// their agentsDir once and feed these as closures. config.Save is
// atomic (temp-file + rename), every helper is idempotent, and
// configMu below serializes the read-modify-writes, so they satisfy
// the concurrency + idempotency contract permissions.GrantStore
// documents.

// configMu serializes every config.Load → mutate → config.Save
// read-modify-write in this package. Save's atomic rename keeps a
// lone writer from corrupting the file, but atomicity alone does not
// make concurrent RMWs safe: two goroutines that Load the same
// snapshot each Save their own mutation, and the later rename
// silently erases the earlier one's entry — including a DENY, which
// fails open (#482). One ConfigGrantStore instance is shared by every
// per-session sub-gate, and the slash /allow + /deny flows call these
// helpers directly, so the lock lives at package level (covering all
// writers in the process) rather than inside the store.
//
// Process-wide rather than per-agentsDir on purpose: writes are tiny
// and rare (operator decisions), so cross-directory serialization is
// unmeasurable and a per-dir lock table would just add machinery.
// Writers in OTHER processes are out of scope, same as before.
var configMu sync.Mutex

// AppendPathScope adds pattern to .agents/config.json's
// path_scope.allow list and rewrites the file atomically. If the
// file doesn't exist yet it is created with defaults so the
// addition has somewhere to live.
func AppendPathScope(agentsDir, pattern string) error {
	configMu.Lock()
	defer configMu.Unlock()
	cfg, err := config.Load(agentsDir)
	if err != nil {
		return err
	}
	for _, existing := range cfg.PathScope.Allow {
		if existing == pattern {
			return nil
		}
	}
	cfg.PathScope.Allow = append(cfg.PathScope.Allow, pattern)
	return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
}

// AppendPermissionsAllow adds one or more patterns to
// .agents/config.json's permissions.allow list. Idempotent —
// duplicate patterns are skipped silently so /permissions can be
// re-run without growing the config file.
func AppendPermissionsAllow(agentsDir string, patterns []string) error {
	configMu.Lock()
	defer configMu.Unlock()
	cfg, err := config.Load(agentsDir)
	if err != nil {
		return err
	}
	existing := make(map[string]bool, len(cfg.Permissions.Allow))
	for _, p := range cfg.Permissions.Allow {
		existing[p] = true
	}
	for _, p := range patterns {
		if existing[p] {
			continue
		}
		cfg.Permissions.Allow = append(cfg.Permissions.Allow, p)
		existing[p] = true
	}
	return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
}

// AppendPermissionsDeny mirrors AppendPermissionsAllow for the deny
// list. Idempotent.
func AppendPermissionsDeny(agentsDir string, patterns []string) error {
	configMu.Lock()
	defer configMu.Unlock()
	cfg, err := config.Load(agentsDir)
	if err != nil {
		return err
	}
	existing := make(map[string]bool, len(cfg.Permissions.Deny))
	for _, p := range cfg.Permissions.Deny {
		existing[p] = true
	}
	for _, p := range patterns {
		if existing[p] {
			continue
		}
		cfg.Permissions.Deny = append(cfg.Permissions.Deny, p)
		existing[p] = true
	}
	return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
}

// AppendBuiltinAllowExtra adds name to .agents/config.json's
// permissions.builtin_allow_extras list. Idempotent — re-enabling a
// bundle that's already on is a no-op. Validation against the
// bundle catalog (permissions.KnownBundles) happens in the TUI
// before this is called, so an invalid name never reaches disk.
func AppendBuiltinAllowExtra(agentsDir, name string) error {
	configMu.Lock()
	defer configMu.Unlock()
	cfg, err := config.Load(agentsDir)
	if err != nil {
		return err
	}
	for _, existing := range cfg.Permissions.BuiltinAllowExtras {
		if existing == name {
			return nil
		}
	}
	cfg.Permissions.BuiltinAllowExtras = append(cfg.Permissions.BuiltinAllowExtras, name)
	return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
}

// PersistModelChoice writes the new model name to
// .agents/config.json so /model survives across runs. Caller is
// responsible for first invoking the in-memory rebuild via
// tui.Options.RebuildAgent — this is purely the disk side.
func PersistModelChoice(agentsDir, modelID string) error {
	configMu.Lock()
	defer configMu.Unlock()
	cfg, err := config.Load(agentsDir)
	if err != nil {
		return err
	}
	cfg.Model.Name = modelID
	return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
}

// PersistThemeChoice writes the picker's selection to
// .agents/config.json so /theme survives across runs. Validates
// before save so a bad name surfaces as a picker error instead
// of silently corrupting the file (config.Save itself does not
// validate).
func PersistThemeChoice(agentsDir, themeName string) error {
	configMu.Lock()
	defer configMu.Unlock()
	cfg, err := config.Load(agentsDir)
	if err != nil {
		return err
	}
	cfg.UI.Theme = themeName
	if err := cfg.Validate(); err != nil {
		return err
	}
	return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
}
