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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// The authn builder is the gate between the operator's config block
// and who the daemon believes is calling. Its failure modes are all
// silent-and-permissive: an unknown auth kind that falls through to
// "no authenticator" means multi_session.enabled = true with every
// request served as anonymous. So these assert on what the returned
// Authenticator actually decides about a request, not on whether it
// is non-nil.

const (
	sreToken = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	botToken = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// writeUsersFile drops a two-user table and returns its path.
func writeUsersFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "users.json")
	body := `{"version":1,"users":[
		{"identity":"sre@example.com","token":"` + sreToken + `"},
		{"identity":"sa:bot","token":"` + botToken + `"}
	]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// authenticate runs a bearer-token request through an Authenticator.
func authenticate(t *testing.T, a auth.Authenticator, token string) (auth.Caller, error) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return a.Authenticate(r)
}

func TestBuildMultiSessionAuthn_Disabled(t *testing.T) {
	t.Parallel()

	authn, fallback, err := BuildMultiSessionAuthn(config.MultiSessionConfig{})
	if err != nil {
		t.Fatalf("BuildMultiSessionAuthn: %v", err)
	}
	if authn != nil {
		t.Errorf("authenticator = %v, want nil so the attach server keeps its own default", authn)
	}
	if fallback.Identity != auth.Anonymous.Identity || fallback.Admin {
		t.Errorf("fallback caller = %+v, want auth.Anonymous", fallback)
	}
}

func TestBuildMultiSessionAuthn_DisabledHonoursDefaultIdentity(t *testing.T) {
	t.Parallel()

	// Single-user mode still stamps a caller on every request. An
	// operator who set default_identity is naming who that is, and
	// the ACL rows written for owned sessions carry it.
	_, fallback, err := BuildMultiSessionAuthn(config.MultiSessionConfig{DefaultIdentity: "local-operator"})
	if err != nil {
		t.Fatalf("BuildMultiSessionAuthn: %v", err)
	}
	if fallback.Identity != "local-operator" {
		t.Errorf("fallback identity = %q, want %q", fallback.Identity, "local-operator")
	}
	if fallback.Admin {
		t.Error("default identity must not be admin by default")
	}
}

func TestBuildMultiSessionAuthn_BearerTable(t *testing.T) {
	t.Parallel()

	// Empty Kind and the explicit kind must resolve identically —
	// the config contract says empty means bearer_table, and a
	// divergence would leave configs that omit the field unguarded.
	for _, kind := range []string{"", config.MultiSessionAuthKindBearerTable} {
		t.Run("kind="+kind, func(t *testing.T) {
			t.Parallel()
			cfg := config.MultiSessionConfig{
				Enabled:         true,
				AdminIdentities: []string{"sre@example.com"},
				ProxyIdentities: []string{"sa:bot"},
			}
			cfg.Auth.Kind = kind
			cfg.Auth.TableFile = writeUsersFile(t)

			authn, _, err := BuildMultiSessionAuthn(cfg)
			if err != nil {
				t.Fatalf("BuildMultiSessionAuthn: %v", err)
			}
			if authn == nil {
				t.Fatal("authenticator is nil — multi-session would serve every request unauthenticated")
			}

			// The admin list must reach the authenticator: this is the
			// difference between an SRE who can act on other tenants'
			// sessions and one who can't.
			admin, err := authenticate(t, authn, sreToken)
			if err != nil {
				t.Fatalf("authenticate sre: %v", err)
			}
			if admin.Identity != "sre@example.com" {
				t.Errorf("identity = %q, want sre@example.com", admin.Identity)
			}
			if !admin.Admin {
				t.Error("admin_identities did not reach the authenticator")
			}

			// A user not on the admin list authenticates as themselves
			// and nothing more.
			bot, err := authenticate(t, authn, botToken)
			if err != nil {
				t.Fatalf("authenticate bot: %v", err)
			}
			if bot.Identity != "sa:bot" || bot.Admin {
				t.Errorf("bot caller = %+v, want sa:bot without admin", bot)
			}

			// An unknown token must be rejected, not silently mapped
			// to the fallback caller.
			if c, err := authenticate(t, authn, strings.Repeat("f", 64)); err == nil {
				t.Errorf("unknown token authenticated as %+v, want an error", c)
			}
		})
	}
}

func TestBuildMultiSessionAuthn_MissingTableFileIsFatal(t *testing.T) {
	t.Parallel()

	cfg := config.MultiSessionConfig{Enabled: true}
	cfg.Auth.TableFile = filepath.Join(t.TempDir(), "absent.json")

	authn, _, err := BuildMultiSessionAuthn(cfg)
	if err == nil {
		t.Fatal("missing users file must be a fatal startup error, not a daemon that boots with no tokens")
	}
	if authn != nil {
		t.Errorf("authenticator = %v on error, want nil", authn)
	}
	if !strings.Contains(err.Error(), "load users file") {
		t.Errorf("error = %v, want it to name the users file", err)
	}
}

func TestBuildMultiSessionAuthn_UnknownKindIsFatal(t *testing.T) {
	t.Parallel()

	// config.Validate() should reject this earlier. If it ever stops
	// doing so, the builder must still fail loudly: falling through
	// to a nil authenticator would turn a typo'd auth kind into an
	// open daemon.
	cfg := config.MultiSessionConfig{Enabled: true}
	cfg.Auth.Kind = "oidc"

	authn, fallback, err := BuildMultiSessionAuthn(cfg)
	if err == nil {
		t.Fatal("unknown auth.kind must be fatal")
	}
	if authn != nil {
		t.Errorf("authenticator = %v, want nil", authn)
	}
	if fallback.Identity != auth.Anonymous.Identity || fallback.Admin {
		t.Errorf("fallback = %+v, want auth.Anonymous", fallback)
	}
	if !strings.Contains(err.Error(), "oidc") {
		t.Errorf("error = %v, want it to quote the unsupported kind", err)
	}
}

func TestBuildSessionFactory(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	factory := BuildSessionFactory(SessionFactoryDeps{
		DaemonCtx: ctx,
		Model:     stubLLM{},
		Template:  permissions.New(permissions.Options{}),
	})
	if factory == nil {
		t.Fatal("BuildSessionFactory returned nil")
	}

	caller := auth.Caller{Identity: "sre@example.com"}
	first, cancelFirst, err := factory(ctx, caller)
	if err != nil {
		t.Fatalf("factory (first): %v", err)
	}
	t.Cleanup(cancelFirst)
	second, cancelSecond, err := factory(ctx, caller)
	if err != nil {
		t.Fatalf("factory (second): %v", err)
	}
	t.Cleanup(cancelSecond)

	// Two POST /sessions from the same caller must not collide on a
	// session id: the (app, user, sid) triple keys the eventlog, so a
	// repeat would splice a second tenant's turns into the first
	// one's conversation history.
	if first.SessionID() == second.SessionID() {
		t.Errorf("both sessions got id %q — the factory is not minting fresh ids", first.SessionID())
	}
	if first.SessionID() == "" {
		t.Error("session id is empty")
	}
	if cancelFirst == nil || cancelSecond == nil {
		t.Error("factory must return a cancel so eviction stops the per-session wake loop")
	}
}

func TestNewSessionID(t *testing.T) {
	t.Parallel()

	const n = 64
	seen := make(map[string]bool, n)
	var ids []string
	for range n {
		id := newSessionID()
		if len(id) != 36 {
			t.Fatalf("id %q is not a canonical UUID string", id)
		}
		if seen[id] {
			t.Fatalf("duplicate session id %q", id)
		}
		seen[id] = true
		ids = append(ids, id)
	}

	// UUIDv7 is time-ordered, and the daemon leans on that: "newest
	// session" is a lexical max over the ACL rows rather than a
	// timestamp column. A silent downgrade to V4 would still be
	// unique and would break that ordering, so assert the property
	// rather than the version nibble alone.
	for i := 1; i < len(ids); i++ {
		if ids[i] < ids[i-1] {
			t.Fatalf("ids are not lexically time-ordered: %q sorted before %q", ids[i], ids[i-1])
		}
	}
	if v := ids[0][14]; v != '7' {
		t.Errorf("UUID version nibble = %q, want '7' (v7 is what makes ids sortable)", v)
	}
}
