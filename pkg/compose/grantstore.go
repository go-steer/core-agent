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
	"path/filepath"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// ConfigGrantStore persists "allow always" grants into
// .agents/config.json (permissions.allow / path_scope.allow_paths),
// atomically, via config.Load + config.Save. It is the reference
// permissions.GrantStore implementation — wire it with
// permissions.Options.GrantStore or Gate.SetGrantStore and the gate's
// DecisionAllowAlways path persists through it for every prompter
// (TUI, stdin, HTTP broker), closing the pre-v2.8 gap where only the
// bundled TUI's callbacks implemented the persistent half of the
// contract.
//
// An empty AgentsDir makes Persist a no-op, matching the TUI's
// historical "no .agents dir resolved ⇒ fall back to allow-session"
// behavior. A present-but-unwritable dir errors — the operator asked
// to persist and it failed; the gate surfaces that to the call.
type ConfigGrantStore struct {
	AgentsDir string
}

// Persist implements permissions.GrantStore. Path-scope grants land
// as typed path_scope.allow_paths entries carrying the grant's
// resolved access ("r" / "rw" after the gate's read→r / write→rw
// promotion) — NOT the legacy bare path_scope.allow list, whose
// entries reload as rw and would silently broaden a read-only grant
// on the next restart. Everything else lands in permissions.allow
// with the gate's fully-expanded "<tool>:<key>" pattern.
func (s *ConfigGrantStore) Persist(_ context.Context, g permissions.Grant) error {
	if s.AgentsDir == "" {
		return nil
	}
	switch g.Kind {
	case permissions.PromptKindPathScope:
		return AppendPathScopeEntry(s.AgentsDir, g.Pattern, g.Access.String())
	default:
		return AppendPermissionsAllow(s.AgentsDir, []string{g.Pattern})
	}
}

var _ permissions.GrantStore = (*ConfigGrantStore)(nil)

// AppendPathScopeEntry adds a typed entry to .agents/config.json's
// path_scope.allow_paths list and rewrites the file atomically.
// access is "r" / "w" / "rw" (permissions.ParseAccess grammar).
// Idempotent: an existing entry for the same path with the same
// access is a no-op; the same path with a different access is
// widened to the union of the two (a later rw grant upgrades an
// earlier r entry in place rather than appending a duplicate).
func AppendPathScopeEntry(agentsDir, path, access string) error {
	want, err := permissions.ParseAccess(access)
	if err != nil {
		return err
	}
	cfg, err := config.Load(agentsDir)
	if err != nil {
		return err
	}
	for i, e := range cfg.PathScope.AllowPaths {
		if e.Path != path {
			continue
		}
		have, err := permissions.ParseAccess(e.Access)
		if err != nil {
			// Malformed existing entry; overwrite with the union of
			// what we can prove (the new grant) rather than failing
			// the persist on someone else's typo.
			have = permissions.AccessNone
		}
		merged := have | want
		if merged == have {
			return nil
		}
		cfg.PathScope.AllowPaths[i].Access = merged.String()
		return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
	}
	cfg.PathScope.AllowPaths = append(cfg.PathScope.AllowPaths, config.PathScopeAllowEntry{
		Path:   path,
		Access: want.String(),
	})
	return config.Save(filepath.Join(agentsDir, config.ConfigFileName), cfg)
}
