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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/go-steer/core-agent/internal/version"
)

// agentCardProtocolVersion pins the A2A schema version we conform to.
// Bumps in lockstep with pkg/attach/testdata/agentcard.schema.json —
// see testdata/AGENTCARD_SCHEMA_NOTICE.md for the refresh policy.
const agentCardProtocolVersion = "0.3.0"

// agentCardDefaultName is the fallback when neither AgentCardConfig.Name
// nor any registered Registrant carries an AppName.
const agentCardDefaultName = "core-agent"

// AgentCardConfig configures the /.well-known/agent-card.json
// endpoint. Zero value disables it; setting Description enables it.
//
// The card's `url` field is derived from the request that fetched
// it (Host header + X-Forwarded-Proto/Host for proxied setups), so
// the operator doesn't have to know their own external address.
// ExternalURL is an optional override for the rare case of wanting
// to publish a canonical URL different from the fetch URL.
//
// The card serves discovery metadata (e.g. Google Cloud Agent Registry
// indexes the skills array for keyword search) — it does NOT imply
// that the binary speaks the A2A JSON-RPC transport. See
// docs/agent-card-design.md.
type AgentCardConfig struct {
	// Name is the human-readable agent name. Defaults to the first
	// registered Registrant's AppName, else "core-agent".
	Name string

	// Description is required to enable the endpoint. The only piece
	// of card metadata that can't be auto-derived from the running
	// process — the operator has to say in one sentence what the
	// agent does.
	Description string

	// ExternalURL, when set, is the literal value emitted as the
	// card's `url` field — overrides the per-request derivation.
	// Use when the binary serves on multiple addresses but you want
	// consumers to see one canonical URL; otherwise leave empty and
	// the handler echoes the fetch URL back.
	ExternalURL string

	// Version is the agent's own version string. Defaults to
	// internal/version.Version (the ldflag-injected build version).
	Version string

	// Provider, when set, must have both Organization and URL — the
	// A2A spec marks both as required if provider is present.
	Provider AgentCardProvider

	// DocumentationURL is optional; omitted from the card if empty.
	DocumentationURL string

	// ExtraSkills are curated skills merged with the registrant's
	// AttachSkills() output. Curated entries win on ID collision.
	ExtraSkills []AgentCardSkill
}

// AgentCardProvider mirrors the A2A AgentProvider definition.
type AgentCardProvider struct {
	Organization string
	URL          string
}

// AgentCardSkill mirrors the A2A AgentSkill definition.
type AgentCardSkill struct {
	ID          string
	Name        string
	Description string
	Tags        []string // defaults to ["curated"] if empty for curated entries
	Examples    []string
}

// Enabled reports whether the endpoint should be registered.
// Description is the only required field — the URL is derived from
// each incoming request (overridable via ExternalURL). When
// Description is empty in the config, the handler still attempts a
// registry-based DescriptionProvider fallback at request time, but
// the route only registers if the config itself supplies a value —
// callers that want the registry fallback to gate the endpoint
// should set Description from their registrant's Description() at
// construction time.
func (c AgentCardConfig) Enabled() bool {
	return c.Description != ""
}

// Validate rejects half-populated nested fields. Zero AgentCardConfig
// is valid (endpoint disabled). Provider, if present at all, must
// have both Organization and URL per the A2A spec.
func (c AgentCardConfig) Validate() error {
	if (c.Provider.Organization != "") != (c.Provider.URL != "") {
		return errors.New("attach: AgentCardConfig: Provider.Organization and Provider.URL must be set together")
	}
	for i, s := range c.ExtraSkills {
		if s.ID == "" {
			return fmt.Errorf("attach: AgentCardConfig: ExtraSkills[%d].ID is required", i)
		}
		if s.Name == "" {
			return fmt.Errorf("attach: AgentCardConfig: ExtraSkills[%d].Name is required", i)
		}
		if s.Description == "" {
			return fmt.Errorf("attach: AgentCardConfig: ExtraSkills[%d].Description is required", i)
		}
	}
	return nil
}

// agentCardWire is the JSON shape served by the handler. Struct field
// order is the JSON key order — keep stable; the wire-format pinning
// test diffs on changes.
type agentCardWire struct {
	ProtocolVersion    string                `json:"protocolVersion"`
	Name               string                `json:"name"`
	Description        string                `json:"description"`
	URL                string                `json:"url"`
	Version            string                `json:"version"`
	Provider           *providerWire         `json:"provider,omitempty"`
	DocumentationURL   string                `json:"documentationUrl,omitempty"`
	Capabilities       capabilitiesWire      `json:"capabilities"`
	SecuritySchemes    *securitySchemesWire  `json:"securitySchemes,omitempty"`
	Security           []map[string][]string `json:"security,omitempty"`
	DefaultInputModes  []string              `json:"defaultInputModes"`
	DefaultOutputModes []string              `json:"defaultOutputModes"`
	Skills             []skillWire           `json:"skills"`
}

type providerWire struct {
	Organization string `json:"organization"`
	URL          string `json:"url"`
}

type capabilitiesWire struct {
	Streaming              bool `json:"streaming"`
	PushNotifications      bool `json:"pushNotifications"`
	StateTransitionHistory bool `json:"stateTransitionHistory"`
}

type securitySchemesWire struct {
	MTLS   *mtlsSchemeWire `json:"mtls,omitempty"`
	Bearer *httpSchemeWire `json:"bearer,omitempty"`
}

type mtlsSchemeWire struct {
	Type string `json:"type"` // const "mutualTLS"
}

type httpSchemeWire struct {
	Type   string `json:"type"`   // const "http"
	Scheme string `json:"scheme"` // const "Bearer"
}

type skillWire struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Examples    []string `json:"examples,omitempty"`
}

// buildAgentCard projects the live config + registry state into the
// wire shape. Pure; no I/O. Called per-request so newly-loaded skills
// surface without restart and the URL reflects the current caller.
//
// url is the value emitted as the card's `url` field — already
// resolved by the handler (override or request-derived).
func buildAgentCard(cfg AgentCardConfig, reg *SessionRegistry, auth AuthConfig, url string) agentCardWire {
	card := agentCardWire{
		ProtocolVersion:    agentCardProtocolVersion,
		Name:               resolveName(cfg.Name, reg),
		Description:        resolveDescription(cfg.Description, reg),
		URL:                url,
		Version:            resolveVersion(cfg.Version),
		DocumentationURL:   cfg.DocumentationURL,
		Capabilities:       capabilitiesWire{Streaming: true},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             mergeSkills(cfg.ExtraSkills, registrySkills(reg)),
	}
	if cfg.Provider.Organization != "" {
		card.Provider = &providerWire{
			Organization: cfg.Provider.Organization,
			URL:          cfg.Provider.URL,
		}
	}
	if schemes, sec := deriveSecurity(auth); schemes != nil {
		card.SecuritySchemes = schemes
		card.Security = sec
	}
	return card
}

// resolveCardURL determines the value of the card's `url` field for a
// given request. Override wins; otherwise derives from the request:
//
//	scheme = "https" if r.TLS != nil else "http"
//	host   = r.Host
//
// X-Forwarded-Proto / X-Forwarded-Host take precedence over the
// direct values when present — production deployments behind ingress
// rely on them. These headers are forgeable by direct callers, but
// the card is a public descriptor with no security implications:
// a bogus URL in a card you fetched yourself is self-inflicted.
//
// Returns empty string only when neither override nor a usable Host
// is available (a misconfigured Unix-socket caller, basically). The
// caller emits whatever this returns; spec validators will flag an
// empty url, which is the correct signal that something's wrong.
func resolveCardURL(r *http.Request, override string) string {
	if override != "" {
		return override
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if v := r.Header.Get("X-Forwarded-Proto"); v != "" {
		scheme = v
	}
	host := r.Host
	if v := r.Header.Get("X-Forwarded-Host"); v != "" {
		host = v
	}
	if host == "" {
		return ""
	}
	return scheme + "://" + host
}

// resolveDescription picks the card description: explicit config
// override wins, otherwise falls back to the first registered
// DescriptionProvider with a non-empty Description(). Returns empty
// string when neither source has one — the card emits "" and schema
// validation would flag it (which is the right signal).
func resolveDescription(override string, reg *SessionRegistry) string {
	if override != "" {
		return override
	}
	if reg == nil {
		return ""
	}
	for _, e := range reg.List() {
		dp, ok := e.Agent.(DescriptionProvider)
		if !ok {
			continue
		}
		if d := dp.Description(); d != "" {
			return d
		}
	}
	return ""
}

// resolveName picks the card name: explicit config → first
// registrant's AppName → default. Sorted List() gives deterministic
// "first" across requests.
func resolveName(configured string, reg *SessionRegistry) string {
	if configured != "" {
		return configured
	}
	if reg != nil {
		for _, e := range reg.List() {
			if e.AppName != "" {
				return e.AppName
			}
		}
	}
	return agentCardDefaultName
}

// resolveVersion uses the configured value or falls back to the
// build version. Avoid the empty string — A2A spec requires version.
func resolveVersion(configured string) string {
	if configured != "" {
		return configured
	}
	if v := version.Version; v != "" {
		return v
	}
	return "0.0.0"
}

// registrySkills collects skills from every Registrant that implements
// SkillsProvider. Deduped by skill name (= card id).
func registrySkills(reg *SessionRegistry) []AgentCardSkill {
	if reg == nil {
		return nil
	}
	seen := map[string]AgentCardSkill{}
	for _, e := range reg.List() {
		sp, ok := e.Agent.(SkillsProvider)
		if !ok {
			continue
		}
		for _, s := range sp.AttachSkills() {
			if s.Name == "" {
				continue
			}
			if _, dup := seen[s.Name]; dup {
				continue
			}
			seen[s.Name] = AgentCardSkill{
				ID:          s.Name,
				Name:        s.Name,
				Description: s.Description,
				Tags:        []string{"skill"},
			}
		}
	}
	out := make([]AgentCardSkill, 0, len(seen))
	for _, s := range seen {
		out = append(out, s)
	}
	return out
}

// mergeSkills unions curated extras with auto-derived skills. Curated
// entries replace auto-derived ones on ID collision. Result is sorted
// by ID. The v0.3.0 spec requires non-nil tags on every skill — empty
// curated tags default to ["curated"].
func mergeSkills(curated, derived []AgentCardSkill) []skillWire {
	combined := map[string]AgentCardSkill{}
	for _, s := range derived {
		combined[s.ID] = s
	}
	for _, s := range curated {
		if len(s.Tags) == 0 {
			s.Tags = []string{"curated"}
		}
		combined[s.ID] = s
	}
	ids := make([]string, 0, len(combined))
	for id := range combined {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]skillWire, 0, len(ids))
	for _, id := range ids {
		s := combined[id]
		out = append(out, skillWire{
			ID:          s.ID,
			Name:        s.Name,
			Description: s.Description,
			Tags:        append([]string(nil), s.Tags...),
			Examples:    append([]string(nil), s.Examples...),
		})
	}
	return out
}

// deriveSecurity translates the attach AuthConfig into A2A v0.3.0
// security blocks. Returns (nil, nil) when no auth is configured —
// caller omits both fields from the card. When mTLS and bearer are
// both required, emits a single AND-combination in the security
// array (middleware enforces both).
func deriveSecurity(auth AuthConfig) (*securitySchemesWire, []map[string][]string) {
	hasMTLS := auth.ClientCAFile != ""
	hasBearer := auth.BearerToken != ""
	if !hasMTLS && !hasBearer {
		return nil, nil
	}
	schemes := &securitySchemesWire{}
	req := map[string][]string{}
	if hasMTLS {
		schemes.MTLS = &mtlsSchemeWire{Type: "mutualTLS"}
		req["mtls"] = []string{}
	}
	if hasBearer {
		schemes.Bearer = &httpSchemeWire{Type: "http", Scheme: "Bearer"}
		req["bearer"] = []string{}
	}
	return schemes, []map[string][]string{req}
}

// agentCardHandler returns an http.Handler that serves the card.
// Always Content-Type: application/json; charset=utf-8. Method other
// than GET → 405. No caching headers — registries fetch at registration
// time, not on every search.
func agentCardHandler(cfg AgentCardConfig, reg *SessionRegistry, auth AuthConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		card := buildAgentCard(cfg, reg, auth, resolveCardURL(r, cfg.ExternalURL))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if r.Method == http.MethodHead {
			return
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(card)
	})
}
