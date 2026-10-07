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

package auth_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// The tokens below are what a holder presents. Only their digests go
// into the tables these tests write — the shape #1213 asks operators
// to deploy.
const (
	dispatcherToken = "4f9c2a7e1b3d5f60718293a4b5c6d7e8f9a0b1c2d3e4f5061728394a5b6c7d8e" //nolint:gosec // test fixture
	aliceToken      = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90" //nolint:gosec // test fixture
)

// bearer runs one request carrying token in the named header through a.
func bearer(t *testing.T, a auth.Authenticator, header, token string) (auth.Caller, error) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	switch header {
	case "Authorization":
		r.Header.Set("Authorization", "Bearer "+token)
	default:
		r.Header.Set(header, token)
	}
	return a.Authenticate(r)
}

func loadAuth(t *testing.T, body string) (*auth.UsersFile, *auth.BearerTokenAuth) {
	t.Helper()
	uf, err := auth.LoadUsersFile(writeUsersFile(t, body, 0o600))
	if err != nil {
		t.Fatalf("LoadUsersFile: %v", err)
	}
	return uf, auth.NewBearerTokenAuth(uf.Users, nil, nil)
}

func TestHashToken_IsHexSHA256(t *testing.T) {
	t.Parallel()
	// FIPS 180-2 test vector for SHA-256("abc").
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := auth.HashToken("abc"); got != want {
		t.Errorf("HashToken(abc) = %s, want %s", got, want)
	}
}

func TestLoadUsersFile_HashedRowsAuthenticate(t *testing.T) {
	t.Parallel()
	uf, a := loadAuth(t, `{"version": 1, "users": [
		{"identity": "sa:selfdev-dispatcher", "token_sha256": "`+auth.HashToken(dispatcherToken)+`"},
		{"identity": "alice@example.com", "token_sha256": "`+strings.ToUpper(auth.HashToken(aliceToken))+`", "labels": {"team": "platform"}}
	]}`)
	if got := uf.PlaintextIdentities(); len(got) != 0 {
		t.Errorf("PlaintextIdentities = %v, want none for an all-hashed table", got)
	}
	for _, header := range []string{"Authorization", "X-Attach-Token"} {
		c, err := bearer(t, a, header, dispatcherToken)
		if err != nil || c.Identity != "sa:selfdev-dispatcher" {
			t.Errorf("%s: dispatcher token = (%+v, %v), want sa:selfdev-dispatcher", header, c, err)
		}
		// Upper-case hex names the same digest.
		c, err = bearer(t, a, header, aliceToken)
		if err != nil || c.Identity != "alice@example.com" || c.Labels["team"] != "platform" {
			t.Errorf("%s: alice token = (%+v, %v), want alice with labels", header, c, err)
		}
	}
}

func TestBearerTokenAuth_HashedRowRejectsWrongToken(t *testing.T) {
	t.Parallel()
	_, a := loadAuth(t, `{"version": 1, "users": [
		{"identity": "sa:selfdev-dispatcher", "token_sha256": "`+auth.HashToken(dispatcherToken)+`"}
	]}`)
	wrong := []string{
		"not-a-real-token",
		dispatcherToken[:len(dispatcherToken)-1],         // a prefix
		dispatcherToken + "0",                            // an extension
		strings.ToUpper(dispatcherToken),                 // tokens are case-sensitive
		auth.HashToken(dispatcherToken),                  // the stored digest itself
		strings.ToUpper(auth.HashToken(dispatcherToken)), // ...in either case
	}
	for _, tok := range wrong {
		if c, err := bearer(t, a, "Authorization", tok); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("token %q authenticated as %+v, want ErrUnauthenticated", tok, c)
		}
	}
}

func TestLoadUsersFile_MixedRows(t *testing.T) {
	t.Parallel()
	uf, a := loadAuth(t, `{"version": 1, "users": [
		{"identity": "alice@example.com", "token_sha256": "`+auth.HashToken(aliceToken)+`"},
		{"identity": "bob@example.com", "token": "tok_bob_legacy"},
		{"identity": "ops@example.com", "token": "tok_ops_legacy"}
	]}`)
	got := uf.PlaintextIdentities()
	if strings.Join(got, ",") != "bob@example.com,ops@example.com" {
		t.Errorf("PlaintextIdentities = %v, want [bob ops] in file order", got)
	}
	for tok, want := range map[string]string{
		aliceToken:       "alice@example.com",
		"tok_bob_legacy": "bob@example.com",
		"tok_ops_legacy": "ops@example.com",
	} {
		if c, err := bearer(t, a, "Authorization", tok); err != nil || c.Identity != want {
			t.Errorf("token for %s = (%+v, %v)", want, c, err)
		}
	}
	// A legacy row is matched by its digest like any other, so its
	// digest is no more a credential than a hashed row's.
	if _, err := bearer(t, a, "Authorization", auth.HashToken("tok_bob_legacy")); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("digest of a plaintext row authenticated; err = %v", err)
	}
}

func TestLoadUsersFile_CollisionsCompareDigests(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"hashed and hashed": `{"version": 1, "users": [
			{"identity": "alice@example.com", "token_sha256": "` + auth.HashToken(aliceToken) + `"},
			{"identity": "bob@example.com", "token_sha256": "` + auth.HashToken(aliceToken) + `"}]}`,
		"same digest, different case": `{"version": 1, "users": [
			{"identity": "alice@example.com", "token_sha256": "` + auth.HashToken(aliceToken) + `"},
			{"identity": "bob@example.com", "token_sha256": "` + strings.ToUpper(auth.HashToken(aliceToken)) + `"}]}`,
		"plaintext and its digest": `{"version": 1, "users": [
			{"identity": "alice@example.com", "token": "` + aliceToken + `"},
			{"identity": "bob@example.com", "token_sha256": "` + auth.HashToken(aliceToken) + `"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := auth.LoadUsersFile(writeUsersFile(t, body, 0o600))
			if err == nil || !strings.Contains(err.Error(), "token collides with row for identity") {
				t.Fatalf("want a collision error, got %v", err)
			}
			if strings.Contains(err.Error(), aliceToken) || strings.Contains(strings.ToLower(err.Error()), auth.HashToken(aliceToken)) {
				t.Errorf("collision error quotes a credential: %v", err)
			}
		})
	}
}

func TestLoadUsersFile_RejectsBadCredentialFields(t *testing.T) {
	t.Parallel()
	const pasted = "tok_pasted_into_the_wrong_field_0123456789"
	cases := map[string]struct{ row, want string }{
		"both fields":       {`"token": "tok_a", "token_sha256": "` + auth.HashToken("tok_a") + `"`, "exactly one"},
		"plaintext in hash": {`"token_sha256": "` + pasted + `"`, "64-character hex"},
		"short digest":      {`"token_sha256": "` + auth.HashToken("x")[:63] + `"`, "64-character hex"},
		"non-hex digest":    {`"token_sha256": "` + strings.Repeat("z", 64) + `"`, "64-character hex"},
		"neither":           {`"labels": {}`, "token is required"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := `{"version": 1, "users": [{"identity": "alice@example.com", ` + tc.row + `}]}`
			_, err := auth.LoadUsersFile(writeUsersFile(t, body, 0o600))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
			for _, secret := range []string{pasted, "tok_a", strings.Repeat("z", 64)} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error quotes the credential value %q: %v", secret, err)
				}
			}
		})
	}
}

func TestNewBearerTokenAuth_SkipsUnusableRows(t *testing.T) {
	t.Parallel()
	a := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "both", Token: "tok_both", TokenSHA256: auth.HashToken("tok_both")},
		{Identity: "malformed", TokenSHA256: "tok_malformed"},
		{Identity: "neither"},
		{Identity: "", TokenSHA256: auth.HashToken("tok_anon")},
		{Identity: "ok", TokenSHA256: auth.HashToken("tok_ok")},
	}, nil, nil)
	for _, tok := range []string{"tok_both", "tok_malformed", "tok_anon", ""} {
		if _, err := bearer(t, a, "Authorization", tok); !errors.Is(err, auth.ErrUnauthenticated) {
			t.Errorf("token %q authenticated through an unusable row", tok)
		}
	}
	for _, id := range []string{"both", "malformed", "neither"} {
		if a.HasIdentity(id) {
			t.Errorf("HasIdentity(%q) = true for an unusable row", id)
		}
	}
	if c, ok := a.LookupIdentity("ok"); !ok || c.Identity != "ok" {
		t.Errorf("LookupIdentity(ok) = (%+v, %v)", c, ok)
	}
}

// TestHashedTable_ReaderLearnsNothingThatAuthenticates is the end-to-end
// claim #1213 rests on: an agent whose bash reads the whole users.json
// of a hashed table gets no string it can authenticate with — not a
// value, not a key, not a digest in either case, not any word of the
// raw text — while the holder's token still works.
func TestHashedTable_ReaderLearnsNothingThatAuthenticates(t *testing.T) {
	t.Parallel()
	body := `{"version": 1, "users": [
		{"identity": "sa:selfdev-dispatcher", "token_sha256": "` + auth.HashToken(dispatcherToken) + `", "labels": {"role": "task_from"}},
		{"identity": "maintainer@example.com", "token_sha256": "` + auth.HashToken(aliceToken) + `"}
	]}`
	path := writeUsersFile(t, body, 0o600)
	uf, err := auth.LoadUsersFile(path)
	if err != nil {
		t.Fatalf("LoadUsersFile: %v", err)
	}
	a := auth.NewBearerTokenAuth(uf.Users, []string{"maintainer@example.com"}, []string{"sa:selfdev-dispatcher"})

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	candidates := readerCandidates(t, raw)
	if len(candidates) < 10 {
		t.Fatalf("only %d candidate strings harvested; the harvester is broken", len(candidates))
	}
	for _, s := range candidates {
		for _, header := range []string{"Authorization", "X-Attach-Token"} {
			if c, err := bearer(t, a, header, s); !errors.Is(err, auth.ErrUnauthenticated) {
				t.Errorf("%s: string %q read from the table authenticated as %+v", header, s, c)
			}
		}
	}
	if c, err := bearer(t, a, "Authorization", dispatcherToken); err != nil || c.Identity != "sa:selfdev-dispatcher" {
		t.Errorf("holder's token = (%+v, %v), want sa:selfdev-dispatcher", c, err)
	}
}

// readerCandidates harvests every string a reader of raw could try:
// each JSON string (keys and values), its upper- and lower-case forms,
// the raw bytes of anything that hex-decodes, and every whitespace- or
// punctuation-delimited word of the raw text.
func readerCandidates(t *testing.T, raw []byte) []string {
	t.Helper()
	seen := map[string]struct{}{}
	add := func(s string) {
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				add(k)
				walk(val)
			}
		case []any:
			for _, val := range x {
				walk(val)
			}
		case string:
			add(x)
			add(strings.ToUpper(x))
			add(strings.ToLower(x))
			if b, err := hex.DecodeString(x); err == nil {
				add(string(b))
			}
		}
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	walk(doc)
	for _, w := range strings.FieldsFunc(string(raw), func(r rune) bool {
		return strings.ContainsRune(" \t\n\r{}[]\",:", r)
	}) {
		add(w)
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	return out
}
