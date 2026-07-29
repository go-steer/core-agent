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

import "github.com/go-steer/core-agent/v2/pkg/config"

// The .agents/config.json persistence helpers moved out of compose
// (#492): the pure config mutations live in pkg/config (built on
// config.Mutate, which owns the #482 serialization lock), and the
// permissions-grammar pieces (ConfigGrantStore, AppendPathScopeEntry)
// live in pkg/permissions. These forwarders keep existing compose
// callers building; new code should import the real homes.

// AppendPathScope adds pattern to path_scope.allow.
//
// Deprecated: use config.AppendPathScope.
func AppendPathScope(agentsDir, pattern string) error {
	return config.AppendPathScope(agentsDir, pattern)
}

// AppendPermissionsAllow adds patterns to permissions.allow.
//
// Deprecated: use config.AppendPermissionsAllow.
func AppendPermissionsAllow(agentsDir string, patterns []string) error {
	return config.AppendPermissionsAllow(agentsDir, patterns)
}

// AppendPermissionsDeny adds patterns to permissions.deny.
//
// Deprecated: use config.AppendPermissionsDeny.
func AppendPermissionsDeny(agentsDir string, patterns []string) error {
	return config.AppendPermissionsDeny(agentsDir, patterns)
}

// AppendBuiltinAllowExtra adds name to permissions.builtin_allow_extras.
//
// Deprecated: use config.AppendBuiltinAllowExtra.
func AppendBuiltinAllowExtra(agentsDir, name string) error {
	return config.AppendBuiltinAllowExtra(agentsDir, name)
}

// PersistModelChoice writes the new model name.
//
// Deprecated: use config.PersistModelChoice.
func PersistModelChoice(agentsDir, modelID string) error {
	return config.PersistModelChoice(agentsDir, modelID)
}

// PersistThemeChoice writes the theme selection.
//
// Deprecated: use config.PersistThemeChoice.
func PersistThemeChoice(agentsDir, themeName string) error {
	return config.PersistThemeChoice(agentsDir, themeName)
}
