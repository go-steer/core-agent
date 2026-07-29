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

import "github.com/go-steer/core-agent/v2/pkg/permissions"

// ConfigGrantStore moved to pkg/permissions (#492) — it is the disk
// half of that package's GrantStore contract. The alias keeps
// existing `&compose.ConfigGrantStore{…}` wiring building unchanged.
//
// Deprecated: use permissions.ConfigGrantStore.
type ConfigGrantStore = permissions.ConfigGrantStore

// AppendPathScopeEntry adds a typed path_scope.allow_paths entry.
//
// Deprecated: use permissions.AppendPathScopeEntry.
func AppendPathScopeEntry(agentsDir, path, access string) error {
	return permissions.AppendPathScopeEntry(agentsDir, path, access)
}
