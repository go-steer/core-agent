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

package main

import (
	"context"
	"fmt"

	"github.com/glebarez/sqlite"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/statestore/agentengine"
)

// openSessionStore opens the eventlog Handle for this run: the SQLite
// file at path holds everything when state.transcript.backend is sql,
// and only the control and state tables when the transcript lives in
// Agent Engine Sessions (#1192). The returned line, when non-empty, is
// the stderr description of the external store for the startup log.
//
// WithMetadataExtractor wires the auth-aware extractor that reads
// auth.Caller / proxy_by off the request context and stamps them onto
// each event's metadata sidecar (docs/multi-session-design.md). In
// single-user mode the extractor returns nil maps (no Caller on
// context), so no sidecar is written; multi-session deployments get
// per-event identity threading in the audit log automatically.
func openSessionStore(ctx context.Context, cfg *config.Config, path string) (*eventlog.Handle, string, error) {
	extractor := eventlog.WithMetadataExtractor(agent.EventlogMetadataExtractor())
	switch cfg.TranscriptBackend() {
	case config.TranscriptBackendSQL:
		h, err := eventlog.Open(ctx, sqlite.Open(path), extractor)
		if err != nil {
			return nil, "", fmt.Errorf("open session db %s: %w", path, err)
		}
		return h, "", nil
	case config.TranscriptBackendAgentEngine:
		ae := cfg.State.Transcript.AgentEngine
		transcript, err := agentengine.Open(ctx, agentengine.Config{
			ReasoningEngine: ae.ReasoningEngine,
			SessionTTL:      cfg.AgentEngineSessionTTL(),
		})
		if err != nil {
			return nil, "", fmt.Errorf("session store: %w", err)
		}
		h, err := eventlog.OpenTranscript(ctx, transcript, sqlite.Open(path), extractor)
		if err != nil {
			_ = transcript.Close()
			return nil, "", fmt.Errorf("open session db %s for the local stores: %w", path, err)
		}
		return h, "session store: agentengine " + ae.ReasoningEngine + " (transcript); local db holds control and state", nil
	default:
		// config.Validate refuses anything else before we get here.
		return nil, "", fmt.Errorf("state.transcript.backend %q is not supported", cfg.TranscriptBackend())
	}
}

// sessionDBConflictMessage is the first line of the error for
// --session-db=false on a shape that cannot run without the database.
func sessionDBConflictMessage(cfg *config.Config) string {
	if cfg.TranscriptIsExternal() {
		return "--session-db=false cannot be combined with state.transcript.backend=" + cfg.TranscriptBackend() +
			": the run lock, guardrail state, spend ledger and session ACLs still live in the local database"
	}
	return "--session-db=false cannot be combined with attach mode: the live-tail broadcaster, /events replay and subagent history all read the event log"
}

// sessionDBImplier names what turned --session-db on without the
// operator asking, for the startup line and the not-writable error.
func sessionDBImplier(cfg *config.Config, attaching bool) string {
	if cfg.TranscriptIsExternal() {
		return "state.transcript.backend=" + cfg.TranscriptBackend()
	}
	if attaching {
		return "attach mode"
	}
	return "configuration"
}
