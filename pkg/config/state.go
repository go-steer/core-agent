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
	"fmt"
	"strings"
	"time"
)

// StateConfig selects the external stores behind docs/external-state-
// design.md (#1192). Only the transcript concern has a non-SQL backend
// so far; control, index and the per-session state stores stay on the
// local --session-db SQLite file whichever transcript is chosen.
type StateConfig struct {
	// Transcript is where a session's ordered event record lives.
	Transcript StateTranscriptConfig `json:"transcript,omitempty" yaml:"transcript,omitempty"`
}

// Transcript backends.
const (
	// TranscriptBackendSQL is the default: the --session-db SQLite file
	// (or any GORM dialector a library caller passes to eventlog.Open)
	// holds the transcript as well as everything else.
	TranscriptBackendSQL = "sql"
	// TranscriptBackendAgentEngine stores the transcript in Vertex AI
	// Agent Engine Sessions (pkg/statestore/agentengine). The local
	// SQLite file is still required for control and state, so this
	// implies --session-db.
	TranscriptBackendAgentEngine = "agentengine"
)

// StateTranscriptConfig picks the transcript backend.
type StateTranscriptConfig struct {
	// Backend is "sql" (default when empty) or "agentengine".
	Backend string `json:"backend,omitempty" yaml:"backend,omitempty"`
	// AgentEngine configures the agentengine backend; required when
	// Backend is "agentengine", ignored otherwise.
	AgentEngine StateAgentEngineConfig `json:"agentengine,omitempty" yaml:"agentengine,omitempty"`
}

// StateAgentEngineConfig is the Agent Engine Sessions connection.
// Authentication is Application Default Credentials; there is no
// secret to name here.
type StateAgentEngineConfig struct {
	// ReasoningEngine is the full resource name of the Agent Engine
	// instance that holds the sessions:
	// projects/P/locations/L/reasoningEngines/N. One per deployment;
	// it can be created without deploying any code. The API endpoint is
	// derived from its location.
	ReasoningEngine string `json:"reasoning_engine,omitempty" yaml:"reasoning_engine,omitempty"`
	// SessionTTL is the expiry set on every session and refreshed on
	// append, as a Go duration string. Default "720h" (30 days); the
	// service's minimum is "24h".
	SessionTTL string `json:"session_ttl,omitempty" yaml:"session_ttl,omitempty"`
}

// TranscriptBackend returns the effective backend name, "sql" when the
// block is absent.
func (c *Config) TranscriptBackend() string {
	if c == nil || c.State.Transcript.Backend == "" {
		return TranscriptBackendSQL
	}
	return c.State.Transcript.Backend
}

// TranscriptIsExternal reports whether the transcript lives outside the
// local SQLite file, which is what makes --session-db implied (the file
// still hosts control and state).
func (c *Config) TranscriptIsExternal() bool {
	return c.TranscriptBackend() != TranscriptBackendSQL
}

// AgentEngineSessionTTL returns the parsed TTL, zero when unset (the
// backend then applies its default). Validate has already checked it.
func (c *Config) AgentEngineSessionTTL() time.Duration {
	if c == nil || c.State.Transcript.AgentEngine.SessionTTL == "" {
		return 0
	}
	d, _ := time.ParseDuration(c.State.Transcript.AgentEngine.SessionTTL)
	return d
}

// validateBackends checks the provider table and the state stores in
// one call from Validate, which sits at the funlen statement ceiling.
func (c *Config) validateBackends() error {
	if err := c.validateProviders(); err != nil {
		return err
	}
	return c.validateState()
}

func (c *Config) validateState() error {
	switch b := c.State.Transcript.Backend; b {
	case "", TranscriptBackendSQL:
		return nil
	case TranscriptBackendAgentEngine:
	default:
		return fmt.Errorf("config: state.transcript.backend %q is not one of %q, %q", b, TranscriptBackendSQL, TranscriptBackendAgentEngine)
	}
	ae := c.State.Transcript.AgentEngine
	if ae.ReasoningEngine == "" {
		return fmt.Errorf("config: state.transcript.agentengine.reasoning_engine is required when state.transcript.backend is %q (projects/P/locations/L/reasoningEngines/N)", TranscriptBackendAgentEngine)
	}
	parts := strings.Split(ae.ReasoningEngine, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "reasoningEngines" ||
		parts[1] == "" || parts[3] == "" || parts[5] == "" {
		return fmt.Errorf("config: state.transcript.agentengine.reasoning_engine %q is not projects/P/locations/L/reasoningEngines/N", ae.ReasoningEngine)
	}
	if ae.SessionTTL != "" {
		d, err := time.ParseDuration(ae.SessionTTL)
		if err != nil {
			return fmt.Errorf("config: state.transcript.agentengine.session_ttl %q: %w", ae.SessionTTL, err)
		}
		if d < 24*time.Hour {
			return fmt.Errorf("config: state.transcript.agentengine.session_ttl %q is below the service's 24h minimum", ae.SessionTTL)
		}
	}
	return nil
}
