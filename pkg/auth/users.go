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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// UsersFileSchemaVersion is the schema version this loader understands.
// Bump when the on-disk shape changes in a way that breaks older
// loaders; LoadUsersFile rejects unknown versions so operators don't
// silently lose new fields.
//
// token_sha256 did not bump it: the field is additive, and a loader
// that predates it refuses a row carrying it as an unknown field, which
// is the fail-closed answer a downgrade needs.
const UsersFileSchemaVersion = 1

// UsersFile is the on-disk shape of attach.multi_session.auth.table_file.
// Operators populate this directly today; an OIDC / IDP-backed loader
// is layered in later (see docs/multi-session-design.md §"Migration story").
type UsersFile struct {
	Version int    `json:"version"`
	Users   []User `json:"users"`
}

// User is one row in users.json. Identity is the stable opaque ID the
// daemon stamps onto audit log entries. Labels are free-form metadata
// available to downstream authorization / observability.
//
// The credential is one of two fields, never both:
//
//   - TokenSHA256 (`token_sha256`) is the hex SHA-256 of the bearer
//     token clients present (see HashToken). The file then holds nothing
//     a request can authenticate with: reading the table, as an agent
//     whose bash reaches it can, yields only digests (#1213).
//   - Token (`token`) is the legacy plaintext form. It still loads, and
//     the daemon warns at startup naming every identity still stored
//     this way (see UsersFile.PlaintextIdentities).
//
// A row missing Identity, or carrying neither or both credential
// fields, is rejected at load time (silently skipping it would hide
// misconfiguration from the operator).
type User struct {
	Identity    string            `json:"identity"`
	Token       string            `json:"token,omitempty"`
	TokenSHA256 string            `json:"token_sha256,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
}

// HashToken returns the value a users.json row stores in token_sha256
// for token: the lowercase hex encoding of its SHA-256.
//
// An unkeyed fast hash is the right tool here and a password hash
// (bcrypt, argon2) is not. Bearer tokens are minted random — 256 bits
// from `openssl rand -hex 32` — so there is no dictionary to slow down
// and a preimage search is the whole attack; a deliberately slow hash
// would only add its cost to every authenticated request. A key would
// have to live where the daemon can read it, which is where the agent
// can read it too. See docs/hashed-bearer-tokens-design.md.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// digest returns the SHA-256 the row authenticates against: the decoded
// token_sha256, or the hash of the plaintext token. ok is false when the
// row carries no usable credential: neither field, both fields, or a
// token_sha256 that is not 64 hex characters.
func (u User) digest() (sum [sha256.Size]byte, ok bool) {
	switch {
	case u.Token != "" && u.TokenSHA256 != "":
		return sum, false
	case u.Token != "":
		return sha256.Sum256([]byte(u.Token)), true
	case u.TokenSHA256 != "":
		return parseTokenDigest(u.TokenSHA256)
	}
	return sum, false
}

// parseTokenDigest decodes a token_sha256 value. Upper-case hex is
// accepted: it names the same digest, and refusing it would fail a
// table some other tool wrote for no security gain.
func parseTokenDigest(s string) (sum [sha256.Size]byte, ok bool) {
	if len(s) != hex.EncodedLen(sha256.Size) {
		return sum, false
	}
	if _, err := hex.Decode(sum[:], []byte(s)); err != nil {
		return sum, false
	}
	return sum, true
}

// PlaintextIdentities returns, in file order, the identities whose row
// still stores its bearer token in plaintext. The daemon warns about
// them at startup; an empty result means reading the file yields
// nothing that authenticates.
func (uf *UsersFile) PlaintextIdentities() []string {
	if uf == nil {
		return nil
	}
	var out []string
	for _, u := range uf.Users {
		if u.Token != "" {
			out = append(out, u.Identity)
		}
	}
	return out
}

// LoadUsersFile reads + validates a users.json file from disk.
//
// Validation:
//   - File mode must be 0600 on POSIX, or 0640 when the owning group
//     is one this process belongs to — the shape a Kubernetes Secret
//     volume with fsGroup produces. Every other group or other bit is
//     a configuration error, not a tolerable laxity. See
//     validateUsersFileMode for the argument. Skipped on Windows where
//     Unix mode bits don't map cleanly.
//   - Schema version must match UsersFileSchemaVersion.
//   - Every row must carry an identity and exactly one of token_sha256
//     or token; a token_sha256 must be 64 hex characters.
//   - Credentials must be unique across rows, compared as digests, so a
//     plaintext row and a hashed row for the same token collide too
//     (duplicate tokens would produce nondeterministic identity
//     resolution).
//   - Identity values must be unique across rows.
//
// No error quotes a credential value, plaintext or digest: a value in
// the wrong field may be a plaintext token, and errors reach logs.
func LoadUsersFile(path string) (*UsersFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("auth: stat users file %q: %w", path, err)
	}
	if err := checkUsersFileMode(path, info); err != nil {
		return nil, err
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
	if err := validateUsers(path, uf.Users); err != nil {
		return nil, err
	}
	return &uf, nil
}

// validateUsers applies LoadUsersFile's per-row rules.
func validateUsers(path string, users []User) error {
	seenDigest := make(map[[sha256.Size]byte]string, len(users))
	seenIdentity := make(map[string]struct{}, len(users))
	for i, u := range users {
		if u.Identity == "" {
			return fmt.Errorf("auth: users file %q row %d: identity is required", path, i)
		}
		if err := checkCredentialFields(u); err != nil {
			return fmt.Errorf("auth: users file %q row %d (identity=%q): %w", path, i, u.Identity, err)
		}
		sum, _ := u.digest()
		if other, ok := seenDigest[sum]; ok {
			return fmt.Errorf("auth: users file %q row %d (identity=%q): token collides with row for identity %q", path, i, u.Identity, other)
		}
		if _, ok := seenIdentity[u.Identity]; ok {
			return fmt.Errorf("auth: users file %q row %d: duplicate identity %q", path, i, u.Identity)
		}
		seenDigest[sum] = u.Identity
		seenIdentity[u.Identity] = struct{}{}
	}
	return nil
}

// checkCredentialFields explains why a row has no usable credential, or
// returns nil when it has exactly one. The messages never quote the
// value.
func checkCredentialFields(u User) error {
	switch {
	case u.Token == "" && u.TokenSHA256 == "":
		return errors.New(`token is required: set "token_sha256" (see core-agent auth hash-token) or the legacy plaintext "token"`)
	case u.Token != "" && u.TokenSHA256 != "":
		return errors.New(`set exactly one of "token_sha256" and "token", not both`)
	}
	if _, ok := u.digest(); !ok {
		return errors.New(`"token_sha256" must be the 64-character hex SHA-256 of the token (see core-agent auth hash-token); the value is not shown, in case it is a plaintext token`)
	}
	return nil
}
