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

import "time"

// ProtocolVersion is the SSE event-stream protocol semver this
// server speaks. Bumped on any change to the contract per
// go-steer/core-tui's docs/sse-event-stream-protocol.md. Clients
// fall back to poll-only mode if their major doesn't match.
const ProtocolVersion = "1.0.0"

// SSE event-type names per the protocol spec (section 2).
const (
	EventCapabilities = "capabilities"
	EventStatusUpdate = "status-update"
	EventUsageUpdate  = "usage-update"
	EventInbox        = "inbox"
	EventTurnComplete = "turn-complete"
	EventTurnError    = "turn-error"

	// EventAgent is the legacy event type carrying ADK session.Event
	// payloads (stream-chunk / tool-call / tool-result are all
	// multiplexed onto this one event today). Kept for back-compat
	// indefinitely — Phase 1 clients in poll mode rely on it, and
	// even push-mode clients still consume it for the model's
	// streamed text output.
	EventAgent = "agent"
)

// SupportedEventTypes lists every event type this server emits.
// Surfaced in the Capabilities event on stream open so consumers
// can detect push-mode support without probing. The list includes
// legacy sub-types (stream-chunk / tool-call / tool-result) even
// though they all ride on EventAgent today — the consumer cares
// about the logical surface, not the SSE event name they currently
// share.
var SupportedEventTypes = []string{
	EventStatusUpdate,
	EventUsageUpdate,
	EventInbox,
	EventTurnComplete,
	EventTurnError,
	"stream-chunk",
	"tool-call",
	"tool-result",
}

// Capabilities is the first frame on every newly-opened stream
// (spec section 2.1). Required so clients can decide push vs poll.
type Capabilities struct {
	ProtocolVersion string   `json:"protocol_version"`
	EventTypes      []string `json:"event_types"`
	Server          string   `json:"server,omitempty"`
}

// Turn-state values per spec section 2.2.
const (
	TurnStateIdle               = "idle"
	TurnStateStreaming          = "streaming"
	TurnStateAwaitingPermission = "awaiting_permission"
	TurnStateAwaitingElicit     = "awaiting_elicit"
)

// StatusUpdate is emitted on session-level state changes (turn
// start/end, model swap, perm-mode change, provider change) and
// also once right after Capabilities as a full snapshot.
//
// Merge semantics: fields not present in an update are unchanged on
// the consumer side. TurnState is always present on every emission
// (snapshot or delta) per spec.
type StatusUpdate struct {
	Model      string `json:"model,omitempty"`
	Provider   string `json:"provider,omitempty"`
	PermMode   string `json:"perm_mode,omitempty"`
	TurnState  string `json:"turn_state"`
	ContextPct *int   `json:"context_pct,omitempty"`
}

// UsageUpdate is emitted after each turn finalizes and as a
// cumulative snapshot on stream open. ByModel is optional — present
// when the tracker has more than one model bucketed (typical when
// --agentic-tools routes subtasks to a small model).
type UsageUpdate struct {
	TokensInTotal  int                     `json:"tokens_in_total"`
	TokensOutTotal int                     `json:"tokens_out_total"`
	CostUSDTotal   float64                 `json:"cost_usd_total"`
	TurnsTotal     int                     `json:"turns_total"`
	ByModel        map[string]UsageByModel `json:"by_model,omitempty"`
}

// UsageByModel is one model's bucket inside UsageUpdate.ByModel.
type UsageByModel struct {
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
	Turns     int     `json:"turns"`
}

// Inbox states per spec section 2.4. The spec reserves room for
// future states (e.g. "injected"); consumers tolerate unknown values.
const (
	InboxStateQueued   = "queued"
	InboxStateDequeued = "dequeued"
)

// InboxEvent fires when an operator-typed prompt changes inbox
// state. PromptID is the correlation handle that links this event
// to downstream turn-complete / turn-error for the same turn.
type InboxEvent struct {
	State    string    `json:"state"`
	PromptID string    `json:"prompt_id"`
	QueuedAt time.Time `json:"queued_at,omitempty"`
}

// TurnComplete fires once per turn after the last stream-chunk for
// that turn and before the next turn's events. All fields required
// per spec section 2.5.
type TurnComplete struct {
	PromptID  string  `json:"prompt_id"`
	Model     string  `json:"model"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
	LatencyMs int64   `json:"latency_ms"`
}

// TurnError kinds per spec section 2.6. Consumers MUST treat unknown
// values as TurnErrorUnknown (forward-compat for new categories).
const (
	TurnErrorConfig        = "config_error"
	TurnErrorAuth          = "auth_error"
	TurnErrorModelNotFound = "model_not_found"
	TurnErrorRateLimited   = "rate_limited"
	TurnErrorTransientNet  = "transient_network"
	TurnErrorUnknown       = "unknown"
)

// TurnError is emitted on a pipeline failure that should reach the
// operator. Successful retries do NOT emit this (the spec's
// "if something is wrong, tell the operator" contract — successful
// internal retries are not operator-facing failures).
type TurnError struct {
	Kind      string `json:"kind"`
	Code      string `json:"code,omitempty"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Hint      string `json:"hint,omitempty"`
}

// EmitTarget is the optional capability a Registrant can implement
// so the broadcaster wires its Emit method as the agent's typed-event
// callback at first-subscriber time, and clears it when the last
// subscriber disconnects.
//
// Agents that don't implement EmitTarget still get the legacy
// `event: agent` frames pumped from the eventlog (back-compat with
// every poll-mode client) — they just won't emit typed events
// (capabilities still fires from the broadcaster directly, and the
// snapshot frames still flow because they read agent state via
// StatusProvider / UsageProvider, not via Emit).
type EmitTarget interface {
	SetAttachEmitter(func(eventType string, payload any))
}
