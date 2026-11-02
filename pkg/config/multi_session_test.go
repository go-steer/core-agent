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
	"encoding/json"
	"strings"
	"testing"
)

func TestMultiSessionConfig_RoundTrip(t *testing.T) {
	t.Parallel()
	body := `{
		"version": 1,
		"model": { "name": "claude-opus-4-7" },
		"attach": {
			"multi_session": {
				"enabled": true,
				"users_dir": "/var/lib/core-agent/users/",
				"auth": {
					"kind": "bearer_table",
					"table_file": "/etc/core-agent/users.json"
				},
				"admin_identities": ["ops@example.com"],
				"allow_anonymous": false,
				"default_identity": "anon",
				"proxy_identities": ["sa:slack-bot"],
				"asserted_caller_header": "X-Asserted-Caller"
			}
		}
	}`

	var c Config
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	ms := c.Attach.MultiSession
	if !ms.Enabled {
		t.Error("Enabled not parsed")
	}
	if ms.UsersDir != "/var/lib/core-agent/users/" {
		t.Errorf("UsersDir: got %q", ms.UsersDir)
	}
	if ms.Auth.Kind != "bearer_table" {
		t.Errorf("Auth.Kind: got %q", ms.Auth.Kind)
	}
	if ms.Auth.TableFile != "/etc/core-agent/users.json" {
		t.Errorf("Auth.TableFile: got %q", ms.Auth.TableFile)
	}
	if len(ms.AdminIdentities) != 1 || ms.AdminIdentities[0] != "ops@example.com" {
		t.Errorf("AdminIdentities: got %v", ms.AdminIdentities)
	}
	if ms.DefaultIdentity != "anon" {
		t.Errorf("DefaultIdentity: got %q", ms.DefaultIdentity)
	}
	if len(ms.ProxyIdentities) != 1 || ms.ProxyIdentities[0] != "sa:slack-bot" {
		t.Errorf("ProxyIdentities: got %v", ms.ProxyIdentities)
	}
	if ms.AssertedCallerHeader != "X-Asserted-Caller" {
		t.Errorf("AssertedCallerHeader: got %q", ms.AssertedCallerHeader)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate (well-formed multi-session config): %v", err)
	}
}

func TestMultiSessionConfig_DisabledByDefault(t *testing.T) {
	t.Parallel()
	body := `{"version": 1, "model": {"name": "claude-opus-4-7"}}`
	var c Config
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Attach.MultiSession.Enabled {
		t.Error("MultiSession.Enabled defaulted to true; must be false (single-user backward-compat)")
	}
	if err := c.Validate(); err != nil {
		t.Errorf("default config must validate: %v", err)
	}
}

func TestMultiSessionConfig_Validate_RequiresTableFileWhenEnabled(t *testing.T) {
	t.Parallel()
	c := &Config{
		Version: SchemaVersion,
		Model:   ModelConfig{Name: "claude-opus-4-7"},
		Attach: AttachConfig{
			MultiSession: MultiSessionConfig{
				Enabled: true,
				Auth:    MultiSessionAuthConfig{Kind: MultiSessionAuthKindBearerTable},
			},
		},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected validation error when bearer_table is enabled without table_file")
	}
	if !strings.Contains(err.Error(), "table_file is required") {
		t.Errorf("error should explain missing table_file, got: %v", err)
	}
}

func TestMultiSessionConfig_Validate_DefaultKindAcceptsTableFile(t *testing.T) {
	t.Parallel()
	// Empty Kind should default to bearer_table (the only kind shipped
	// in v2.4); a table_file is still required.
	c := &Config{
		Version: SchemaVersion,
		Model:   ModelConfig{Name: "claude-opus-4-7"},
		Attach: AttachConfig{
			MultiSession: MultiSessionConfig{
				Enabled: true,
				Auth:    MultiSessionAuthConfig{TableFile: "/etc/core-agent/users.json"},
			},
		},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("empty Kind with TableFile must validate (defaults to bearer_table): %v", err)
	}
}

func TestMultiSessionConfig_Validate_RejectsUnshippedKinds(t *testing.T) {
	t.Parallel()
	// OIDC / mTLS / K8s ServiceAccount are designed but explicitly
	// deferred per docs/multi-session-design.md §"Non-goals". A config
	// that references them must fail loudly so an operator who copies
	// a future-version example doesn't silently fall back.
	for _, kind := range []string{"oidc", "mtls", "k8s_sa"} {
		t.Run(kind, func(t *testing.T) {
			c := &Config{
				Version: SchemaVersion,
				Model:   ModelConfig{Name: "claude-opus-4-7"},
				Attach: AttachConfig{
					MultiSession: MultiSessionConfig{
						Enabled: true,
						Auth:    MultiSessionAuthConfig{Kind: kind, TableFile: "/x"},
					},
				},
			}
			err := c.Validate()
			if err == nil {
				t.Fatalf("expected validation error for unshipped kind %q", kind)
			}
			if !strings.Contains(err.Error(), kind) {
				t.Errorf("error should name the rejected kind %q, got: %v", kind, err)
			}
		})
	}
}

func TestMultiSessionConfig_Validate_DisabledIgnoresAuthFields(t *testing.T) {
	t.Parallel()
	// When MultiSession is disabled, garbage in the auth sub-tree must
	// not block validation — operators may keep a half-edited config
	// around with multi_session.enabled=false while iterating.
	c := &Config{
		Version: SchemaVersion,
		Model:   ModelConfig{Name: "claude-opus-4-7"},
		Attach: AttachConfig{
			MultiSession: MultiSessionConfig{
				Enabled: false,
				Auth:    MultiSessionAuthConfig{Kind: "made-up-kind"},
			},
		},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("disabled multi-session must not validate the auth sub-tree: %v", err)
	}
}

// #1483: token_identity is the other way to give bearer_table its rows —
// the attach token as a one-row table — so it satisfies the table
// requirement on its own, and naming both is refused rather than one
// silently winning.
func TestMultiSessionConfig_Validate_TokenIdentity(t *testing.T) {
	t.Parallel()
	cfg := func(a MultiSessionAuthConfig) *Config {
		return &Config{
			Version: SchemaVersion,
			Model:   ModelConfig{Name: "claude-opus-4-7"},
			Attach:  AttachConfig{MultiSession: MultiSessionConfig{Enabled: true, Auth: a}},
		}
	}
	if err := cfg(MultiSessionAuthConfig{TokenIdentity: "shipmind"}).Validate(); err != nil {
		t.Errorf("token_identity alone must validate: %v", err)
	}
	if err := cfg(MultiSessionAuthConfig{Kind: MultiSessionAuthKindBearerTable, TokenIdentity: "shipmind"}).Validate(); err != nil {
		t.Errorf("token_identity with the explicit kind must validate: %v", err)
	}
	err := cfg(MultiSessionAuthConfig{TableFile: "/etc/core-agent/users.json", TokenIdentity: "shipmind"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("table_file with token_identity: err = %v, want mutually exclusive", err)
	}
	err = cfg(MultiSessionAuthConfig{}).Validate()
	if err == nil || !strings.Contains(err.Error(), "table_file is required") || !strings.Contains(err.Error(), "token_identity") {
		t.Errorf("neither: err = %v, want it to name table_file and token_identity", err)
	}
	err = cfg(MultiSessionAuthConfig{TokenIdentity: " shipmind"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Errorf("padded identity: err = %v, want a whitespace error", err)
	}
	for _, bad := range []string{"ship mind", "ship\tmind", "ship\nmind", string([]byte{0xff, 0xfe}), strings.Repeat("a", 257)} {
		err = cfg(MultiSessionAuthConfig{TokenIdentity: bad}).Validate()
		if err == nil || !strings.Contains(err.Error(), "whitespace or control characters") {
			t.Errorf("token_identity %q: err = %v, want whitespace/control error", bad, err)
		}
	}
}

func TestMultiSessionConfig_Validate_ProxyUnprovisionedRequiresProxyIdentities(t *testing.T) {
	t.Parallel()
	c := &Config{
		Version: SchemaVersion,
		Model:   ModelConfig{Name: "claude-opus-4-7"},
		Attach: AttachConfig{
			MultiSession: MultiSessionConfig{
				Enabled:            true,
				Auth:               MultiSessionAuthConfig{TokenIdentity: "switchboard"},
				ProxyUnprovisioned: true,
			},
		},
	}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "proxy_unprovisioned requires proxy_identities") {
		t.Fatalf("proxy_unprovisioned without proxy_identities: err = %v, want requires proxy_identities error", err)
	}
	c.Attach.MultiSession.ProxyIdentities = []string{"switchboard"}
	if err := c.Validate(); err != nil {
		t.Fatalf("proxy_unprovisioned with proxy_identities must validate: %v", err)
	}
}

func TestMultiSessionConfig_TokenIdentityRoundTrip(t *testing.T) {
	t.Parallel()
	var c MultiSessionAuthConfig
	if err := json.Unmarshal([]byte(`{"kind":"bearer_table","token_identity":"shipmind"}`), &c); err != nil {
		t.Fatal(err)
	}
	if c.TokenIdentity != "shipmind" {
		t.Errorf("token_identity = %q", c.TokenIdentity)
	}
}
