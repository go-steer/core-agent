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

package attach

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/go-steer/core-agent/pkg/eventlog"
)

// fixtureRegistrant is a minimal Registrant + SkillsProvider +
// DescriptionProvider used by the card builder tests so we don't
// depend on *agent.Agent.
type fixtureRegistrant struct {
	app, user, sid string
	description    string
	skills         []SkillInfo
}

func (f fixtureRegistrant) AppName() string            { return f.app }
func (f fixtureRegistrant) UserID() string             { return f.user }
func (f fixtureRegistrant) SessionID() string          { return f.sid }
func (f fixtureRegistrant) EventLog() *eventlog.Handle { return nil }
func (f fixtureRegistrant) Inject(string) error        { return nil }
func (f fixtureRegistrant) RequestWake()               {}
func (f fixtureRegistrant) AttachSkills() []SkillInfo  { return f.skills }
func (f fixtureRegistrant) Description() string        { return f.description }

func TestAgentCardConfig_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cfg     AgentCardConfig
		wantErr string
	}{
		{"zero is valid (endpoint disabled)", AgentCardConfig{}, ""},
		{"description alone is valid (URL derived from request)", AgentCardConfig{Description: "x"}, ""},
		{"description + override is valid", AgentCardConfig{Description: "x", ExternalURL: "https://x"}, ""},
		{"URL override without description still valid struct-wise (endpoint disabled)", AgentCardConfig{ExternalURL: "https://x"}, ""},
		{"provider org without url", AgentCardConfig{Provider: AgentCardProvider{Organization: "x"}}, "Provider.Organization and Provider.URL"},
		{"provider url without org", AgentCardConfig{Provider: AgentCardProvider{URL: "https://x"}}, "Provider.Organization and Provider.URL"},
		{"both provider fields", AgentCardConfig{Provider: AgentCardProvider{Organization: "x", URL: "https://x"}}, ""},
		{"extra skill missing id", AgentCardConfig{
			Description: "x",
			ExtraSkills: []AgentCardSkill{{Name: "n", Description: "d"}},
		}, "ExtraSkills[0].ID"},
		{"extra skill missing name", AgentCardConfig{
			Description: "x",
			ExtraSkills: []AgentCardSkill{{ID: "i", Description: "d"}},
		}, "ExtraSkills[0].Name"},
		{"extra skill missing description", AgentCardConfig{
			Description: "x",
			ExtraSkills: []AgentCardSkill{{ID: "i", Name: "n"}},
		}, "ExtraSkills[0].Description"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestBuildAgentCardWireFormat pins the full JSON shape against a
// golden file. Diff-noisy on purpose — any change to the card's wire
// layout requires the goldenfile to update.
//
// The `url` field here is the value we pass to buildAgentCard
// directly. In production it's resolved per-request by resolveCardURL
// (echoed from Host / X-Forwarded-* headers, or overridden via
// ExternalURL). See TestResolveCardURL for that matrix.
func TestBuildAgentCardWireFormat(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(fixtureRegistrant{
		app: "core-agent", user: "local", sid: "s-1",
		skills: []SkillInfo{
			{Name: "migrate-db", Description: "Run zero-downtime schema migrations against a target environment."},
			{Name: "triage-incident", Description: "Pull on-call signals and produce a first-pass incident summary."},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	cfg := AgentCardConfig{
		Description:      "Production-incident response agent for the platform fleet.",
		ExternalURL:      "https://agent.prod.svc.cluster.local:7777",
		Version:          "v2.2.0",
		Provider:         AgentCardProvider{Organization: "Platform Team", URL: "https://example.internal/platform"},
		DocumentationURL: "https://example.internal/platform/runbooks/core-agent",
		ExtraSkills: []AgentCardSkill{
			{
				ID:          "rollback-deploy",
				Name:        "Rollback a deploy",
				Description: "Curated: revert a Cloud Deploy release to the previous revision.",
				Tags:        []string{"curated", "deploy"},
			},
		},
	}
	auth := AuthConfig{ClientCAFile: "ca.pem", BearerToken: "abc"}
	card := buildAgentCard(cfg, reg, auth, "https://agent.prod.svc.cluster.local:7777")

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(card); err != nil {
		t.Fatalf("encode card: %v", err)
	}
	got := buf.Bytes()

	want, err := os.ReadFile("testdata/agentcard_golden.json")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(canonicalizeJSON(t, got), canonicalizeJSON(t, want)) {
		t.Fatalf("wire format drifted from testdata/agentcard_golden.json\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// canonicalizeJSON round-trips a JSON byte slice through encoding/json
// so map-key ordering between Go and the goldenfile doesn't cause
// spurious diffs (Go's encoder sorts map keys alphabetically; the
// goldenfile is hand-written).
func canonicalizeJSON(t *testing.T, in []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(in, &v); err != nil {
		t.Fatalf("canonicalize unmarshal: %v\ninput: %s", err, in)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("canonicalize marshal: %v", err)
	}
	return out
}

func TestBuildAgentCard_SecuritySchemes(t *testing.T) {
	t.Parallel()
	base := AgentCardConfig{Description: "x", ExternalURL: "https://x"}
	cases := []struct {
		name     string
		auth     AuthConfig
		wantMTLS bool
		wantBear bool
		wantReq  []string // expected keys present in security[0] (order ignored)
	}{
		{"no auth", AuthConfig{}, false, false, nil},
		{"mtls only", AuthConfig{ClientCAFile: "ca.pem"}, true, false, []string{"mtls"}},
		{"bearer only", AuthConfig{BearerToken: "t"}, false, true, []string{"bearer"}},
		{"both", AuthConfig{ClientCAFile: "ca.pem", BearerToken: "t"}, true, true, []string{"mtls", "bearer"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			card := buildAgentCard(base, NewSessionRegistry(), tc.auth, "https://x")
			gotMTLS := card.SecuritySchemes != nil && card.SecuritySchemes.MTLS != nil
			gotBear := card.SecuritySchemes != nil && card.SecuritySchemes.Bearer != nil
			if gotMTLS != tc.wantMTLS || gotBear != tc.wantBear {
				t.Fatalf("schemes: got mtls=%v bearer=%v, want mtls=%v bearer=%v", gotMTLS, gotBear, tc.wantMTLS, tc.wantBear)
			}
			if tc.wantReq == nil {
				if card.Security != nil {
					t.Fatalf("security: got %v, want nil", card.Security)
				}
				return
			}
			if len(card.Security) != 1 {
				t.Fatalf("security: got %d entries, want 1", len(card.Security))
			}
			for _, key := range tc.wantReq {
				if _, ok := card.Security[0][key]; !ok {
					t.Errorf("security[0] missing key %q (got %v)", key, card.Security[0])
				}
			}
		})
	}
}

func TestBuildAgentCard_SkillMerge(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	if _, err := reg.Register(fixtureRegistrant{
		app: "a", user: "u", sid: "s-1",
		skills: []SkillInfo{
			{Name: "alpha", Description: "auto-alpha"},
			{Name: "beta", Description: "auto-beta"},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	cfg := AgentCardConfig{
		Description: "x", ExternalURL: "https://x",
		ExtraSkills: []AgentCardSkill{
			// Collides with auto-derived "beta" — curated wins.
			{ID: "beta", Name: "Curated Beta", Description: "manual override"},
			// New curated skill with no Tags — should default to ["curated"].
			{ID: "delta", Name: "Curated Delta", Description: "added skill"},
		},
	}
	card := buildAgentCard(cfg, reg, AuthConfig{}, "https://x")

	if len(card.Skills) != 3 {
		t.Fatalf("got %d skills, want 3 (alpha + beta + delta)", len(card.Skills))
	}
	// Must be sorted by id.
	ids := []string{card.Skills[0].ID, card.Skills[1].ID, card.Skills[2].ID}
	wantIDs := []string{"alpha", "beta", "delta"}
	for i := range ids {
		if ids[i] != wantIDs[i] {
			t.Errorf("skills[%d].ID = %q, want %q (full order: %v)", i, ids[i], wantIDs[i], ids)
		}
	}
	// alpha came from registry → tags ["skill"]
	if got := card.Skills[0].Tags; len(got) != 1 || got[0] != "skill" {
		t.Errorf("alpha tags = %v, want [skill]", got)
	}
	// beta was overridden by curated → name + description from curated, tags default ["curated"]
	if card.Skills[1].Name != "Curated Beta" || card.Skills[1].Description != "manual override" {
		t.Errorf("beta override lost: %+v", card.Skills[1])
	}
	if got := card.Skills[1].Tags; len(got) != 1 || got[0] != "curated" {
		t.Errorf("beta tags = %v, want [curated] (curated default)", got)
	}
	// delta is new curated → tags default ["curated"]
	if got := card.Skills[2].Tags; len(got) != 1 || got[0] != "curated" {
		t.Errorf("delta tags = %v, want [curated]", got)
	}
}

func TestBuildAgentCard_NameFallbacks(t *testing.T) {
	t.Parallel()
	base := AgentCardConfig{Description: "x", ExternalURL: "https://x"}

	// Explicit name wins.
	got := buildAgentCard(AgentCardConfig{Name: "explicit", Description: "x"}, NewSessionRegistry(), AuthConfig{}, "https://x")
	if got.Name != "explicit" {
		t.Errorf("explicit name: got %q, want %q", got.Name, "explicit")
	}
	// AppName fallback when no explicit name.
	reg := NewSessionRegistry()
	if _, err := reg.Register(fixtureRegistrant{app: "my-app", user: "u", sid: "s"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got = buildAgentCard(base, reg, AuthConfig{}, "https://x")
	if got.Name != "my-app" {
		t.Errorf("registrant fallback: got %q, want %q", got.Name, "my-app")
	}
	// Default fallback when no registrant.
	got = buildAgentCard(base, NewSessionRegistry(), AuthConfig{}, "https://x")
	if got.Name != agentCardDefaultName {
		t.Errorf("default fallback: got %q, want %q", got.Name, agentCardDefaultName)
	}
}

func TestResolveDescription(t *testing.T) {
	t.Parallel()

	// override wins regardless of registry
	regWith := NewSessionRegistry()
	if _, err := regWith.Register(fixtureRegistrant{app: "a", user: "u", sid: "s1", description: "from registry"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := resolveDescription("explicit", regWith); got != "explicit" {
		t.Errorf("override wins: got %q, want %q", got, "explicit")
	}

	// empty override → registry fallback
	if got := resolveDescription("", regWith); got != "from registry" {
		t.Errorf("registry fallback: got %q, want %q", got, "from registry")
	}

	// empty override + empty registry → empty string
	regEmpty := NewSessionRegistry()
	if got := resolveDescription("", regEmpty); got != "" {
		t.Errorf("no source: got %q, want empty", got)
	}

	// empty override + registrant without DescriptionProvider → empty
	// (use stubRegistrant which doesn't implement DescriptionProvider)
	regNoDesc := NewSessionRegistry()
	if _, err := regNoDesc.Register(&stubRegistrant{app: "a", user: "u", sid: "s"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := resolveDescription("", regNoDesc); got != "" {
		t.Errorf("non-DescriptionProvider registrant: got %q, want empty", got)
	}

	// registrant with empty description → keep walking (no other registrants → empty)
	regEmptyDesc := NewSessionRegistry()
	if _, err := regEmptyDesc.Register(fixtureRegistrant{app: "a", user: "u", sid: "s1", description: ""}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if got := resolveDescription("", regEmptyDesc); got != "" {
		t.Errorf("empty registrant description: got %q, want empty", got)
	}
}

func TestResolveCardURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		override string
		host     string
		tls      bool
		xfProto  string
		xfHost   string
		want     string
	}{
		{"override wins over everything", "https://canonical.example", "fetcher.example:7777", true, "https", "ingress.example", "https://canonical.example"},
		{"plain http direct", "", "agent.example:7777", false, "", "", "http://agent.example:7777"},
		{"tls direct", "", "agent.example:7777", true, "", "", "https://agent.example:7777"},
		{"x-forwarded-proto promotes http to https", "", "agent.example:7777", false, "https", "", "https://agent.example:7777"},
		{"x-forwarded-host replaces host", "", "10.0.0.5:7777", false, "https", "agent.example", "https://agent.example"},
		{"both forwarded headers", "", "10.0.0.5:7777", false, "https", "agent.example", "https://agent.example"},
		{"empty host returns empty", "", "", false, "", "", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, "/.well-known/agent-card.json", nil)
			req.Host = tc.host
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			if tc.xfProto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.xfProto)
			}
			if tc.xfHost != "" {
				req.Header.Set("X-Forwarded-Host", tc.xfHost)
			}
			got := resolveCardURL(req, tc.override)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAgentCardHandler_DerivesURLFromRequest(t *testing.T) {
	t.Parallel()
	cfg := AgentCardConfig{Description: "x"}
	h := agentCardHandler(cfg, NewSessionRegistry(), AuthConfig{})
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent-card.json", nil)
	req.Host = "agent.example.com:7777"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	var card map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &card); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := card["url"], "http://agent.example.com:7777"; got != want {
		t.Errorf("url = %v, want %v — should echo Host header", got, want)
	}
}

func TestAgentCardHandler_MethodNotAllowed(t *testing.T) {
	t.Parallel()
	cfg := AgentCardConfig{Description: "x"}
	h := agentCardHandler(cfg, NewSessionRegistry(), AuthConfig{})

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/.well-known/agent-card.json", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: got %d, want %d", method, w.Code, http.StatusMethodNotAllowed)
		}
		if got := w.Header().Get("Allow"); !strings.Contains(got, "GET") {
			t.Errorf("%s: Allow header = %q, want GET", method, got)
		}
	}

	// GET should succeed.
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent-card.json", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: got %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("GET Content-Type = %q, want application/json; charset=utf-8", ct)
	}

	// HEAD should succeed with no body.
	req = httptest.NewRequest(http.MethodHead, "/.well-known/agent-card.json", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD: got %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.Len() != 0 {
		t.Errorf("HEAD: got body %d bytes, want 0", w.Body.Len())
	}
}

// TestAgentCardSchemaValidation validates an emitted card against the
// vendored A2A JSON Schema. Two failure modes:
//   - Our builder drops a required field / mistypes one → fails here.
//   - The A2A schema changes upstream → bump the vendored file and
//     this test surfaces any divergence.
func TestAgentCardSchemaValidation(t *testing.T) {
	t.Parallel()
	schemaBytes, err := os.ReadFile("testdata/agentcard.schema.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var raw jsonschema.Schema
	if err := json.Unmarshal(schemaBytes, &raw); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	// Wrap the bundle so $ref resolves AgentCard as the entrypoint.
	wrapper := &jsonschema.Schema{
		Ref:         "#/definitions/AgentCard",
		Definitions: raw.Definitions,
	}
	resolved, err := wrapper.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Build a populated card and validate.
	reg := NewSessionRegistry()
	if _, err := reg.Register(fixtureRegistrant{
		app: "core-agent", user: "u", sid: "s",
		skills: []SkillInfo{{Name: "alpha", Description: "alpha skill"}},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	cfg := AgentCardConfig{
		Description: "test card",
		ExternalURL: "https://example.invalid:7777",
		Version:     "v0.0.1-test",
		Provider:    AgentCardProvider{Organization: "Test Org", URL: "https://example.invalid"},
	}
	card := buildAgentCard(cfg, reg, AuthConfig{BearerToken: "t"}, "https://example.invalid:7777")
	cardBytes, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	var inst any
	if err := json.Unmarshal(cardBytes, &inst); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	if err := resolved.Validate(inst); err != nil {
		t.Fatalf("schema validation failed against vendored A2A v0.3.0 schema:\n  %v\nemitted card:\n%s", err, cardBytes)
	}

	// Also validate the auth-less case — security blocks should be
	// absent without errors.
	bareCard := buildAgentCard(cfg, reg, AuthConfig{}, "https://example.invalid:7777")
	bareBytes, _ := json.Marshal(bareCard)
	var bareInst any
	if err := json.Unmarshal(bareBytes, &bareInst); err != nil {
		t.Fatalf("unmarshal bare card: %v", err)
	}
	if err := resolved.Validate(bareInst); err != nil {
		t.Fatalf("schema validation failed for auth-less card:\n  %v\nemitted card:\n%s", err, bareBytes)
	}
}
