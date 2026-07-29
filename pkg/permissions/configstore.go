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

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// ConfigGrantStore persists "allow always" grants into
// .agents/config.json (permissions.allow / path_scope.allow_paths),
// atomically, via config.Mutate. It is the reference GrantStore
// implementation — wire it with Options.GrantStore or
// Gate.SetGrantStore and the gate's DecisionAllowAlways path persists
// through it for every prompter (TUI, stdin, HTTP broker).
//
// Moved from pkg/compose (#492): the store is the disk half of THIS
// package's GrantStore contract and has no compose dependency; compose
// keeps a deprecated alias.
//
// An empty AgentsDir makes Persist a no-op, matching the TUI's
// historical "no .agents dir resolved ⇒ fall back to allow-session"
// behavior. A present-but-unwritable dir errors — the operator asked
// to persist and it failed; the gate surfaces that to the call.
//
// Safe for concurrent use: one store instance is shared by every
// per-session sub-gate derived from a template gate, so concurrent
// "allow always" answers race. All writes serialize through
// config.Mutate's package-wide lock (#482).
type ConfigGrantStore struct {
	AgentsDir string
}

// Persist implements GrantStore. Path-scope grants land as typed
// path_scope.allow_paths entries carrying the grant's resolved
// access ("r" / "rw" after the gate's read→r / write→rw promotion) —
// NOT the legacy bare path_scope.allow list, whose entries reload as
// rw and would silently broaden a read-only grant on the next
// restart. Everything else lands in permissions.allow with the
// gate's fully-expanded "<tool>:<key>" pattern.
func (s *ConfigGrantStore) Persist(_ context.Context, g Grant) error {
	if s.AgentsDir == "" {
		return nil
	}
	switch g.Kind {
	case PromptKindPathScope:
		return AppendPathScopeEntry(s.AgentsDir, g.Pattern, g.Access.String())
	default:
		return config.AppendPermissionsAllow(s.AgentsDir, []string{g.Pattern})
	}
}

var _ GrantStore = (*ConfigGrantStore)(nil)

// AppendPathScopeEntry adds a typed entry to .agents/config.json's
// path_scope.allow_paths list and rewrites the file atomically.
// access is "r" / "w" / "rw" (ParseAccess grammar). Idempotent: an
// existing entry for the same path with the same access is a no-op;
// the same path with a different access is widened to the union of
// the two (a later rw grant upgrades an earlier r entry in place
// rather than appending a duplicate). Lives here rather than
// pkg/config because it speaks this package's access grammar
// (pkg/config cannot import permissions — permissions imports config).
func AppendPathScopeEntry(agentsDir, path, access string) error {
	want, err := ParseAccess(access)
	if err != nil {
		return err
	}
	return config.Mutate(agentsDir, func(cfg *config.Config) error {
		for i, e := range cfg.PathScope.AllowPaths {
			if e.Path != path {
				continue
			}
			have, err := ParseAccess(e.Access)
			if err != nil {
				// Malformed existing entry; overwrite with the union
				// of what we can prove (the new grant) rather than
				// failing the persist on someone else's typo.
				have = AccessNone
			}
			merged := have | want
			if merged == have {
				return config.ErrNoChange
			}
			cfg.PathScope.AllowPaths[i].Access = merged.String()
			return nil
		}
		cfg.PathScope.AllowPaths = append(cfg.PathScope.AllowPaths, config.PathScopeAllowEntry{
			Path:   path,
			Access: want.String(),
		})
		return nil
	})
}
