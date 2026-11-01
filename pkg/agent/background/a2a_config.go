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

package background

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Sentinel errors returned by A2AClient methods so callers can
// classify failure modes with errors.Is.
var (
	// ErrRemoteUnreachable indicates the remote endpoint could not be
	// reached (DNS, TCP connect, TLS handshake, HTTP transport failure).
	ErrRemoteUnreachable = errors.New("background: remote agent unreachable")

	// ErrRemoteTimeout indicates the remote invocation did not reach a
	// terminal state within the configured wall-clock timeout.
	ErrRemoteTimeout = errors.New("background: remote invocation timed out")

	// ErrRemoteAuthFailed indicates the remote agent rejected the
	// caller's credentials (HTTP 401/403 or missing token env var).
	ErrRemoteAuthFailed = errors.New("background: remote authentication failed")

	// ErrRemoteProtocolMismatch indicates the remote agent's advertised
	// protocol or transport is incompatible with this client.
	ErrRemoteProtocolMismatch = errors.New("background: remote protocol mismatch")

	// ErrRemoteFailed indicates the remote agent accepted the call and
	// terminated in a failed, canceled, rejected, or unsupported state.
	ErrRemoteFailed = errors.New("background: remote invocation failed")
)

// DefaultA2ATimeout bounds an A2A call when neither the config nor the
// per-spawn budget supplies a wall-clock timeout.
const DefaultA2ATimeout = 120 * time.Second

// A2AAuthTypeBearer is the bearer-token authentication type.
const A2AAuthTypeBearer = "bearer"

// A2AAuthConfig configures authentication for an A2AClient. Tokens are
// always referenced by environment variable name, never embedded in
// config files, and resolved per request so rotation needs no restart.
type A2AAuthConfig struct {
	// Type is the authentication scheme ("bearer").
	Type string `json:"type"`
	// TokenEnv names the environment variable holding the bearer token.
	TokenEnv string `json:"token_env,omitempty"`
}

// A2AConfig configures an A2AClient for one remote A2A v0.3 agent.
type A2AConfig struct {
	// Name is the local identifier for the remote agent.
	Name string `json:"name"`

	// AgentCardURL locates the remote agent card. A base URL (empty or
	// "/" path) has /.well-known/agent-card.json appended automatically.
	AgentCardURL string `json:"agent_card_url,omitempty"`

	// Endpoint is the JSON-RPC 2.0 endpoint URL, bypassing card
	// discovery when AgentCardURL is unset. When both are set, Endpoint
	// is used for JSON-RPC transport and AgentCardURL is still fetched
	// for capability and skill discovery.
	Endpoint string `json:"endpoint,omitempty"`

	// Skill is the default skill ID sent in message metadata when
	// spawned via RemoteAgentSpawner.Spawn.
	Skill string `json:"skill,omitempty"`

	// Skills is an optional allowlist of skill IDs this client may
	// invoke; empty permits any skill on the remote agent.
	Skills []string `json:"skills,omitempty"`

	// Auth configures request authentication; nil means unauthenticated.
	Auth *A2AAuthConfig `json:"auth,omitempty"`

	// TimeoutSeconds bounds each invocation; 0 uses DefaultA2ATimeout.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`

	// AllowCardHostChange permits a JSON-RPC endpoint taken from the
	// agent card to live on a different host than the card itself. Off
	// by default: the operator configured one host and the bearer token
	// is sent to whatever endpoint is chosen, so a card that points
	// elsewhere is refused rather than trusted (#1435). A downgrade from
	// https to http is refused regardless of this setting.
	AllowCardHostChange bool `json:"allow_card_host_change,omitempty"`
}

// Timeout returns the effective invocation bound.
func (c A2AConfig) Timeout() time.Duration {
	if c.TimeoutSeconds > 0 {
		return time.Duration(c.TimeoutSeconds) * time.Second
	}
	return DefaultA2ATimeout
}

// Validate checks an A2AConfig for structural errors.
func (c A2AConfig) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return errors.New("a2a: missing required field name")
	}
	if err := validateSpawnName(c.Name); err != nil {
		return fmt.Errorf("a2a: %w", err)
	}
	if strings.TrimSpace(c.AgentCardURL) == "" && strings.TrimSpace(c.Endpoint) == "" {
		return fmt.Errorf("a2a (%s): one of agent_card_url or endpoint is required", c.Name)
	}
	if err := validateA2AURL(c.AgentCardURL); err != nil {
		return fmt.Errorf("a2a (%s): agent_card_url: %w", c.Name, err)
	}
	if err := validateA2AURL(c.Endpoint); err != nil {
		return fmt.Errorf("a2a (%s): endpoint: %w", c.Name, err)
	}
	if c.Auth != nil {
		if c.Auth.Type != A2AAuthTypeBearer {
			return fmt.Errorf("a2a (%s): auth.type %q is not supported (only %q)", c.Name, c.Auth.Type, A2AAuthTypeBearer)
		}
		if strings.TrimSpace(c.Auth.TokenEnv) == "" {
			return fmt.Errorf("a2a (%s): auth.token_env is required for bearer auth", c.Name)
		}
	}
	if c.TimeoutSeconds < 0 {
		return fmt.Errorf("a2a (%s): timeout_seconds must be >= 0", c.Name)
	}
	if s := strings.TrimSpace(c.Skill); s != "" && len(c.Skills) > 0 {
		allowed := false
		for _, a := range c.Skills {
			if a == s {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("a2a (%s): default skill %q is not in skills allowlist %v", c.Name, s, c.Skills)
		}
	}
	return nil
}

func validateA2AURL(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("missing host")
	}
	return nil
}

// ErrRemoteEndpointUntrusted indicates the agent card offered a
// JSON-RPC endpoint this client refuses to send credentials to: a
// different host than the card's (unless AllowCardHostChange) or a
// downgrade from https to http (#1435).
var ErrRemoteEndpointUntrusted = errors.New("background: remote endpoint not trusted")

// trustCardEndpoint decides whether an endpoint URL taken from the agent
// card may be used. The card URL is the operator's trust anchor: the
// endpoint must parse as http(s) with a host, must not downgrade the
// card's https to http, and must share the card's host unless the
// operator opted in. Host comparison includes the port and is
// case-insensitive.
func trustCardEndpoint(cardURL, endpoint string, allowHostChange bool) error {
	if err := validateA2AURL(endpoint); err != nil {
		return fmt.Errorf("%w: card endpoint %q: %v", ErrRemoteEndpointUntrusted, endpoint, err)
	}
	card, err := url.Parse(strings.TrimSpace(cardURL))
	if err != nil {
		return fmt.Errorf("%w: agent card url %q: %v", ErrRemoteEndpointUntrusted, cardURL, err)
	}
	ep, _ := url.Parse(strings.TrimSpace(endpoint)) // validated above
	if card.Scheme == "https" && ep.Scheme != "https" {
		return fmt.Errorf("%w: card endpoint %q downgrades the card's https to %s", ErrRemoteEndpointUntrusted, endpoint, ep.Scheme)
	}
	if !strings.EqualFold(card.Host, ep.Host) && !allowHostChange {
		return fmt.Errorf("%w: card endpoint %q is on a different host than the agent card (%s); set allow_card_host_change to permit it",
			ErrRemoteEndpointUntrusted, endpoint, card.Host)
	}
	return nil
}
