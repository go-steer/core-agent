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

package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
)

// UsersFileSchemaVersion is the schema version this loader understands.
// Bump when the on-disk shape changes in a way that breaks older
// loaders; LoadUsersFile rejects unknown versions so operators don't
// silently lose new fields.
const UsersFileSchemaVersion = 1

// UsersFile is the on-disk shape of attach.multi_session.auth.table_file.
// Operators populate this directly today; an OIDC / IDP-backed loader
// is layered in later (see docs/multi-session-design.md §"Migration story").
type UsersFile struct {
	Version int    `json:"version"`
	Users   []User `json:"users"`
}

// User is one row in users.json. Identity is the stable opaque ID the
// daemon stamps onto audit log entries; Token is the bearer credential
// clients present. Labels are free-form metadata available to
// downstream authorization / observability.
//
// Identity and Token are both required; rows missing either are
// rejected at load time (silently skipping them would hide
// misconfiguration from the operator).
type User struct {
	Identity string            `json:"identity"`
	Token    string            `json:"token"`
	Labels   map[string]string `json:"labels,omitempty"`
}

// LoadUsersFile reads + validates a users.json file from disk.
//
// Validation:
//   - File mode must be 0600 or stricter on POSIX (group/other bits
//     must be zero). The file holds bearer secrets; world- or
//     group-readable permissions are a configuration error, not a
//     tolerable laxity. Skipped on Windows where Unix mode bits
//     don't map cleanly.
//   - Schema version must match UsersFileSchemaVersion.
//   - Every row must carry both identity and token.
//   - Token values must be unique across rows (duplicate tokens would
//     produce nondeterministic identity resolution).
//   - Identity values must be unique across rows.
func LoadUsersFile(path string) (*UsersFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("auth: stat users file %q: %w", path, err)
	}
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			return nil, fmt.Errorf("auth: users file %q has mode %#o; must be 0600 or stricter (group/other bits must be unset)", path, mode)
		}
	}

	data, err := os.ReadFile(path) //nolint:gosec // path is operator-supplied config, not user input
	if err != nil {
		return nil, fmt.Errorf("auth: read users file %q: %w", path, err)
	}

	var uf UsersFile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&uf); err != nil {
		return nil, fmt.Errorf("auth: parse users file %q: %w", path, err)
	}
	if uf.Version != UsersFileSchemaVersion {
		return nil, fmt.Errorf("auth: users file %q has unsupported schema version %d (expected %d)", path, uf.Version, UsersFileSchemaVersion)
	}

	seenToken := make(map[string]string, len(uf.Users))
	seenIdentity := make(map[string]struct{}, len(uf.Users))
	for i, u := range uf.Users {
		if u.Identity == "" {
			return nil, fmt.Errorf("auth: users file %q row %d: identity is required", path, i)
		}
		if u.Token == "" {
			return nil, fmt.Errorf("auth: users file %q row %d (identity=%q): token is required", path, i, u.Identity)
		}
		if other, ok := seenToken[u.Token]; ok {
			return nil, fmt.Errorf("auth: users file %q row %d (identity=%q): token collides with row for identity %q", path, i, u.Identity, other)
		}
		if _, ok := seenIdentity[u.Identity]; ok {
			return nil, fmt.Errorf("auth: users file %q row %d: duplicate identity %q", path, i, u.Identity)
		}
		seenToken[u.Token] = u.Identity
		seenIdentity[u.Identity] = struct{}{}
	}

	return &uf, nil
}
