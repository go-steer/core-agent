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

// Package mcp wires user-configured Model Context Protocol servers
// into the agent loop.
//
// At startup the host calls Build, which reads .agents/mcp.json,
// spawns each declared server (stdio child or Streamable HTTP
// client), wraps the resulting MCP toolsets via ADK's
// google.golang.org/adk/tool/mcptoolset, and returns:
//
//   - the toolsets, so they can be passed to agent.New(WithToolsets…)
//   - per-server records the host can render (e.g. a /mcp slash
//     command).
//
// Failures are non-fatal: a server whose process won't start surfaces
// in the per-server record with its error; the agent continues with
// whichever servers did connect.
package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/go-steer/core-agent/v2/pkg/agentenv"
)

// MCPFileName is the project-local MCP config file inside .agents/.
const MCPFileName = "mcp.json"

// Servers is the on-disk schema for .agents/mcp.json.
//
// AgenticWrap + AgenticWrapThreshold are the operator knobs for the
// structural-digester wrap layer (#130). CLI --no-mcp-digest kills the
// whole surface regardless; per-server AgenticNever opts one server
// out without touching the global flag.
//
// AgenticWrapLLM + AgenticWrapModel gate the #223 LLM subagent
// second-chance path — a small-tier subagent that digests responses
// the structural pruner can't reduce under threshold. Default off:
// the mechanism is safe but the cost profile depends on the
// operator's MCP surface, so opt-in until dogfooded.
type Servers struct {
	Version              int                   `json:"version"`
	Servers              map[string]ServerSpec `json:"servers"`
	AgenticWrap          *bool                 `json:"agentic_wrap,omitempty"`           // default true; ptr for explicit off
	AgenticWrapThreshold int                   `json:"agentic_wrap_threshold,omitempty"` // default 8000
	AgenticWrapLLM       *bool                 `json:"agentic_wrap_llm,omitempty"`       // default false; ptr for explicit on
	AgenticWrapModel     string                `json:"agentic_wrap_model,omitempty"`     // MCP-specific small-model override; empty falls through to --agentic-small-model resolution
}

// DefaultAgenticWrapThreshold is the default byte threshold below which
// MCP responses bypass the digest wrapper. 8000 bytes ≈ 2000 tokens —
// the wrap/router overhead exceeds the bloat cost below this.
const DefaultAgenticWrapThreshold = 8000

// AgenticWrapEnabled reports whether the operator has opted the wrap
// layer in. Default true; the flag ships as a kill switch (matches
// #217 OTel posture — no --enable-* flags), so absence == on.
func (s *Servers) AgenticWrapEnabled() bool {
	if s == nil || s.AgenticWrap == nil {
		return true
	}
	return *s.AgenticWrap
}

// AgenticWrapThresholdBytes returns the operator-configured threshold
// or the built-in default when unset / zero.
func (s *Servers) AgenticWrapThresholdBytes() int {
	if s == nil || s.AgenticWrapThreshold <= 0 {
		return DefaultAgenticWrapThreshold
	}
	return s.AgenticWrapThreshold
}

// AgenticWrapLLMEnabled reports whether the operator has opted the
// LLM subagent second-chance path in via mcp.json. Absence == off
// (opposite of AgenticWrap): the structural pruner is a safe default
// but the LLM fallback trades wall-clock + subagent cost for its
// compression win, so opt-in until an operator confirms the
// trade-off works for their MCP surface.
func (s *Servers) AgenticWrapLLMEnabled() bool {
	if s == nil || s.AgenticWrapLLM == nil {
		return false
	}
	return *s.AgenticWrapLLM
}

// ServerSpec describes one MCP server. Either Command (stdio) or URL
// (Streamable HTTP) must be set; we intentionally don't support both.
//
// AgenticNever opts this specific server out of the digest wrap layer
// — the operator escape hatch for debug-sensitive or known-tiny
// servers where wrapping would hurt more than it helps.
type ServerSpec struct {
	Transport    string            `json:"transport"`               // "stdio" | "http"
	Command      string            `json:"command,omitempty"`       // stdio
	Args         []string          `json:"args,omitempty"`          // stdio
	Env          map[string]string `json:"env,omitempty"`           // stdio
	URL          string            `json:"url,omitempty"`           // http
	Headers      map[string]string `json:"headers,omitempty"`       // http
	Auth         *AuthSpec         `json:"auth,omitempty"`          // http
	AgenticNever bool              `json:"agentic_never,omitempty"` // skip digest wrap for this server
}

// AuthSpec selects an authentication strategy for an HTTP MCP server.
// Exactly one inner field may be set. Future strategies (audience-
// scoped ID tokens for Cloud Run / IAP, mTLS, etc.) slot in as
// sibling pointers.
type AuthSpec struct {
	GoogleOAuth *GoogleOAuthAuth `json:"google_oauth,omitempty"`
}

// GoogleOAuthAuth authenticates outbound MCP requests with a Google
// OAuth 2.0 access token sourced from Application Default Credentials.
// Suitable for Google-hosted API endpoints that accept scoped access
// tokens (e.g. the GKE MCP server at container.googleapis.com/mcp).
//
// For audience-scoped ID-token auth (Cloud Run / IAP / custom OIDC),
// add a sibling GoogleIDToken field when a consumer needs it.
type GoogleOAuthAuth struct {
	// Scopes is the OAuth 2.0 scopes requested on the access token.
	// No default — each server documents its own scope requirements,
	// and an implicit broad default (e.g. cloud-platform) would grant
	// more privilege than necessary. Explicit is safer.
	//
	// For the GKE MCP server, typical values:
	//   https://www.googleapis.com/auth/container.read-only
	//   https://www.googleapis.com/auth/container
	Scopes []string `json:"scopes"`
}

// Validate reports whether the AuthSpec is internally consistent and
// at least one strategy is set.
func (a *AuthSpec) Validate(name string) error {
	if a == nil {
		return nil
	}
	count := 0
	if a.GoogleOAuth != nil {
		count++
		if err := a.GoogleOAuth.Validate(name); err != nil {
			return err
		}
	}
	if count == 0 {
		return fmt.Errorf("mcp: server %q: auth is set but no strategy is configured", name)
	}
	return nil
}

// Validate reports whether the GoogleOAuthAuth is usable.
func (g *GoogleOAuthAuth) Validate(name string) error {
	if len(g.Scopes) == 0 {
		return fmt.Errorf("mcp: server %q: auth.google_oauth.scopes must list at least one scope", name)
	}
	for i, s := range g.Scopes {
		if s == "" {
			return fmt.Errorf("mcp: server %q: auth.google_oauth.scopes[%d] is empty", name, i)
		}
	}
	return nil
}

// Validate checks that the spec describes a single, complete transport.
func (s ServerSpec) Validate(name string) error {
	switch s.Transport {
	case "stdio":
		if s.Command == "" {
			return fmt.Errorf("mcp: server %q: stdio transport requires command", name)
		}
		if s.URL != "" {
			return fmt.Errorf("mcp: server %q: stdio transport must not set url", name)
		}
		if s.Auth != nil {
			return fmt.Errorf("mcp: server %q: auth is only valid for http transport", name)
		}
	case "http":
		if s.URL == "" {
			return fmt.Errorf("mcp: server %q: http transport requires url", name)
		}
		if s.Command != "" {
			return fmt.Errorf("mcp: server %q: http transport must not set command", name)
		}
		if err := s.Auth.Validate(name); err != nil {
			return err
		}
	case "":
		return fmt.Errorf("mcp: server %q: transport is required (\"stdio\" or \"http\")", name)
	default:
		return fmt.Errorf("mcp: server %q: unknown transport %q", name, s.Transport)
	}
	return nil
}

// Load reads <agentsDir>/mcp.json. A missing file is treated as
// "no servers configured" — not an error, since most projects never
// declare MCP servers.
func Load(agentsDir string) (Servers, error) {
	if agentsDir == "" {
		return Servers{}, nil
	}
	path := filepath.Join(agentsDir, MCPFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Servers{}, nil
		}
		return Servers{}, fmt.Errorf("mcp: read %s: %w", path, err)
	}
	var s Servers
	if err := json.Unmarshal(data, &s); err != nil {
		return Servers{}, fmt.Errorf("mcp: parse %s: %w", path, err)
	}
	for name, spec := range s.Servers {
		if err := spec.Validate(name); err != nil {
			return Servers{}, err
		}
	}
	return s, nil
}

// InterpolateEnv is retained as a delegating alias for pkg/agentenv.
// The regex + resolver logic migrated to pkg/agentenv when the wider
// ${env:VAR} substitution mechanism landed (#322); this file keeps the
// symbol so lifecycle.go call sites don't need touching. New callers
// should use agentenv.NewResolver to get manifest-aware interpolation
// (fail-loud required checks, sensitive-value tracking, drift
// diagnostics) rather than this bare-os.Getenv path.
func InterpolateEnv(s string) string { return agentenv.InterpolateEnv(s) }

// InterpolateMap runs each value through InterpolateEnv. Used for
// ServerSpec.Env and ServerSpec.Headers. Same delegation note as
// InterpolateEnv applies.
func InterpolateMap(m map[string]string) map[string]string {
	return agentenv.InterpolateMap(m)
}
