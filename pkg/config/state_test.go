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
	"strings"
	"testing"
	"time"
)

func TestValidateState(t *testing.T) {
	t.Parallel()
	const engine = "projects/p/locations/us-central1/reasoningEngines/123"
	cases := []struct {
		name    string
		state   StateConfig
		wantErr string
	}{
		{name: "absent is sql"},
		{name: "explicit sql", state: StateConfig{Transcript: StateTranscriptConfig{Backend: "sql"}}},
		{name: "unknown backend", state: StateConfig{Transcript: StateTranscriptConfig{Backend: "redis"}}, wantErr: `"redis" is not one of`},
		{name: "agentengine without engine", state: StateConfig{Transcript: StateTranscriptConfig{Backend: "agentengine"}}, wantErr: "reasoning_engine is required"},
		{name: "agentengine bare id", state: StateConfig{Transcript: StateTranscriptConfig{Backend: "agentengine", AgentEngine: StateAgentEngineConfig{ReasoningEngine: "123"}}}, wantErr: "is not projects/P"},
		{name: "agentengine ok", state: StateConfig{Transcript: StateTranscriptConfig{Backend: "agentengine", AgentEngine: StateAgentEngineConfig{ReasoningEngine: engine}}}},
		{name: "agentengine ttl ok", state: StateConfig{Transcript: StateTranscriptConfig{Backend: "agentengine", AgentEngine: StateAgentEngineConfig{ReasoningEngine: engine, SessionTTL: "48h"}}}},
		{name: "agentengine ttl unparsable", state: StateConfig{Transcript: StateTranscriptConfig{Backend: "agentengine", AgentEngine: StateAgentEngineConfig{ReasoningEngine: engine, SessionTTL: "two days"}}}, wantErr: "session_ttl"},
		{name: "agentengine ttl too short", state: StateConfig{Transcript: StateTranscriptConfig{Backend: "agentengine", AgentEngine: StateAgentEngineConfig{ReasoningEngine: engine, SessionTTL: "1h"}}}, wantErr: "24h minimum"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &Config{State: tc.state}
			err := c.validateState()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validateState: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateState = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestTranscriptBackendAccessors(t *testing.T) {
	t.Parallel()
	var nilCfg *Config
	if nilCfg.TranscriptBackend() != TranscriptBackendSQL || nilCfg.TranscriptIsExternal() {
		t.Fatal("nil Config must read as sql")
	}
	c := &Config{}
	if c.TranscriptBackend() != TranscriptBackendSQL || c.TranscriptIsExternal() || c.AgentEngineSessionTTL() != 0 {
		t.Fatal("empty state must read as sql with no TTL")
	}
	c.State.Transcript.Backend = TranscriptBackendAgentEngine
	c.State.Transcript.AgentEngine.SessionTTL = "48h"
	if !c.TranscriptIsExternal() || c.AgentEngineSessionTTL() != 48*time.Hour {
		t.Fatalf("agentengine: external=%v ttl=%s", c.TranscriptIsExternal(), c.AgentEngineSessionTTL())
	}
}

// Validate as a whole runs the state check, so a bad block is refused
// at load time rather than at the first session.
func TestValidate_RunsStateCheck(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	c.State.Transcript.Backend = "redis"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "state.transcript.backend") {
		t.Fatalf("Validate = %v, want the state error", err)
	}
}
