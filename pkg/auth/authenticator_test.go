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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

func TestAnonymousAuth_DefaultIdentity(t *testing.T) {
	t.Parallel()
	a := auth.AnonymousAuth{}
	c, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("AnonymousAuth.Authenticate returned error %v; must never fail", err)
	}
	if c.Identity != "anon" {
		t.Errorf("zero-value AnonymousAuth resolved to %q; want %q (the auth.Anonymous default)", c.Identity, "anon")
	}
}

func TestAnonymousAuth_CustomIdentity(t *testing.T) {
	t.Parallel()
	a := auth.AnonymousAuth{Caller: auth.Caller{Identity: "daemon-user"}}
	c, err := a.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Authenticate err: %v", err)
	}
	if c.Identity != "daemon-user" {
		t.Errorf("custom Caller.Identity not honored: got %q, want %q", c.Identity, "daemon-user")
	}
}

func TestBearerTokenAuth_AuthorizationHeader(t *testing.T) {
	t.Parallel()
	a := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "tok_alice", Labels: map[string]string{"team": "platform"}},
		{Identity: "bob@example.com", Token: "tok_bob"},
	}, nil, nil)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer tok_alice")
	c, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate err: %v", err)
	}
	if c.Identity != "alice@example.com" {
		t.Errorf("Identity: got %q, want alice@example.com", c.Identity)
	}
	if c.Labels["team"] != "platform" {
		t.Errorf("Labels not threaded: got %q", c.Labels["team"])
	}
	if c.Admin {
		t.Error("alice should NOT be Admin (not in admin list)")
	}
}

func TestBearerTokenAuth_XAttachTokenHeader(t *testing.T) {
	t.Parallel()
	// The X-Attach-Token side-channel header is the existing convention
	// for identity-gateway-fronted deployments (Cloud Run IAM, IAP). It
	// must work for per-caller bearer auth too.
	a := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "tok_alice"},
	}, nil, nil)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Attach-Token", "tok_alice")
	c, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate err: %v", err)
	}
	if c.Identity != "alice@example.com" {
		t.Errorf("Identity via X-Attach-Token: got %q, want alice@example.com", c.Identity)
	}
}

func TestBearerTokenAuth_UnknownTokenRejected(t *testing.T) {
	t.Parallel()
	a := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "tok_alice"},
	}, nil, nil)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer not-a-real-token")
	_, err := a.Authenticate(r)
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("expected ErrUnauthenticated for unknown token, got %v", err)
	}
}

func TestBearerTokenAuth_MissingTokenRejected(t *testing.T) {
	t.Parallel()
	a := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: "tok_alice"},
	}, nil, nil)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := a.Authenticate(r)
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("expected ErrUnauthenticated for missing token, got %v", err)
	}
}

func TestBearerTokenAuth_AdminFlag(t *testing.T) {
	t.Parallel()
	a := auth.NewBearerTokenAuth(
		[]auth.User{
			{Identity: "alice@example.com", Token: "tok_alice"},
			{Identity: "ops@example.com", Token: "tok_ops"},
		},
		[]string{"ops@example.com"}, // admin
		nil,
	)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer tok_ops")
	c, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate err: %v", err)
	}
	if !c.Admin {
		t.Error("ops should be Admin (listed in adminIdentities)")
	}
}

func TestBearerTokenAuth_SkipsEmptyRows(t *testing.T) {
	t.Parallel()
	// A row missing identity or token should be silently skipped by
	// the authenticator (the loader is responsible for catching the
	// misconfiguration upstream; the authenticator is defensive so a
	// blank token doesn't authenticate every credential-less request).
	a := auth.NewBearerTokenAuth([]auth.User{
		{Identity: "alice@example.com", Token: ""}, // skipped
		{Identity: "", Token: "tok_orphan"},        // skipped
	}, nil, nil)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	_, err := a.Authenticate(r)
	if !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("table with only invalid rows must reject everything: got %v", err)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("Authorization", "Bearer tok_orphan")
	if _, err := a.Authenticate(r2); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Errorf("orphan token (no identity) must not authenticate: got %v", err)
	}
}

func TestBearerTokenAuth_CanProxyAs(t *testing.T) {
	t.Parallel()
	a := auth.NewBearerTokenAuth(
		[]auth.User{
			{Identity: "sa:slack-bot", Token: "tok_bot"},
			{Identity: "alice@example.com", Token: "tok_alice"},
		},
		nil,
		[]string{"sa:slack-bot"},
	)

	// Authenticate via the bot's token, check it can proxy.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer tok_bot")
	bot, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("bot auth: %v", err)
	}
	if !a.CanProxyAs(bot) {
		t.Error("sa:slack-bot is in proxy allowlist but CanProxyAs returned false")
	}

	// Alice (regular user) should NOT be able to proxy.
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("Authorization", "Bearer tok_alice")
	alice, err := a.Authenticate(r2)
	if err != nil {
		t.Fatalf("alice auth: %v", err)
	}
	if a.CanProxyAs(alice) {
		t.Error("alice is NOT in proxy allowlist but CanProxyAs returned true (security violation)")
	}
}

func TestBearerTokenAuth_CanProxyAs_ZeroCallerRejected(t *testing.T) {
	t.Parallel()
	a := auth.NewBearerTokenAuth(nil, nil, []string{"sa:bot"})
	if a.CanProxyAs(auth.Caller{}) {
		t.Error("CanProxyAs(zero-Caller) must return false (defense against accidental authorization)")
	}
}

func TestBearerTokenAuth_LookupIdentity(t *testing.T) {
	t.Parallel()
	a := auth.NewBearerTokenAuth(
		[]auth.User{
			{Identity: "alice@example.com", Token: "tok_alice", Labels: map[string]string{"team": "platform"}},
		},
		[]string{"alice@example.com"},
		nil,
	)
	c, ok := a.LookupIdentity("alice@example.com")
	if !ok {
		t.Fatal("known identity reports HasIdentity=false")
	}
	if !c.Admin {
		t.Error("LookupIdentity must preserve Admin flag from table")
	}
	if c.Labels["team"] != "platform" {
		t.Error("LookupIdentity must preserve Labels from table")
	}
	if _, ok := a.LookupIdentity("not-provisioned@example.com"); ok {
		t.Error("unknown identity reports HasIdentity=true")
	}
}

func TestBearerTokenAuth_HasIdentity(t *testing.T) {
	t.Parallel()
	a := auth.NewBearerTokenAuth(
		[]auth.User{{Identity: "alice@example.com", Token: "tok_alice"}},
		nil, nil,
	)
	if !a.HasIdentity("alice@example.com") {
		t.Error("known identity reports HasIdentity=false")
	}
	if a.HasIdentity("ghost@example.com") {
		t.Error("unknown identity reports HasIdentity=true")
	}
}

// Compile-time assertions that the implementations satisfy the
// expected interfaces. Future refactors that drop a method break the
// build at this line, not at a far-away caller.
var (
	_ auth.Authenticator          = auth.AnonymousAuth{}
	_ auth.Authenticator          = (*auth.BearerTokenAuth)(nil)
	_ auth.AuthenticatorWithProxy = (*auth.BearerTokenAuth)(nil)
)
