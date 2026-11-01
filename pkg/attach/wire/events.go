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

package wire

import "time"

// SSE event-type names per the protocol spec (section 2).
const (
	EventCapabilities = "capabilities"
	EventStatusUpdate = "status-update"
	EventUsageUpdate  = "usage-update"
	EventInbox        = "inbox"
	EventTurnComplete = "turn-complete"
	EventTurnError    = "turn-error"

	// EventPause (v1.5.0) reports the session's pause gate opening or
	// closing. Emitted for every transition regardless of which client
	// caused it, so a second TUI — or mast-web watching alongside one —
	// learns that someone else parked the agent without polling
	// /status.
	EventPause = "pause"

	// EventWake (v1.7.0) reports that the session's wake signal fired:
	// something out-of-band decided the loop should look at the world
	// again. In a stock daemon that means an operator's POST /wake; the
	// other producer is a host that calls Agent.RequestWake itself,
	// which is how a background alert is meant to wake a sleeping
	// supervisor (see dev/uat/scheduled-monitor). No in-tree code wires
	// that up for you, so do not read this event as "an alert arrived".
	//
	// It is an ATTENTION signal, not a state change: there is no
	// matching "unwake", and nothing to reconcile on reconnect.
	//
	// Deliberately NOT emitted for the wake that Inject fires
	// internally: that one already has its own frame on the wire (the
	// `inbox` queued event), and emitting both would make every
	// operator-typed prompt raise a "something needs you" notice about
	// the operator's own typing. POST /wake carrying a `prompt` is the
	// one case that produces both, because it is an inject and an
	// explicit wake request in the same call.
	EventWake = "wake"

	// EventGuardrailTrip (v1.13.0) reports that a guardrail halted the
	// session. Non-terminal — see the GuardrailTrip payload for why a
	// trip stopped being a turn outcome.
	//
	// The name matches GuardrailTripEventName, the durable eventlog row
	// #643 already writes for the same fact, deliberately: one thing
	// that happened, one name for it, whether you read it off the wire
	// or out of the session's history.
	EventGuardrailTrip = "guardrail-trip"

	// EventAgent is the legacy event type carrying ADK session.Event
	// payloads (stream-chunk / tool-call / tool-result are all
	// multiplexed onto this one event today). Kept for back-compat
	// indefinitely — Phase 1 clients in poll mode rely on it, and
	// even push-mode clients still consume it for the model's
	// streamed text output.
	EventAgent = "agent"
)

// Capabilities is the first frame on every newly-opened stream
// (spec section 2.1). Required so clients can decide push vs poll.
//
// v1.4.0 (#329) added the Features/SlashCommands/Agent/CallerID
// fields. All four are optional — older clients that don't know
// about them ignore silently; older servers omit them and the
// consumer sees the pre-1.4.0 shape unchanged.
type Capabilities struct {
	ProtocolVersion string   `json:"protocol_version"`
	EventTypes      []string `json:"event_types"`
	Server          string   `json:"server,omitempty"`

	// Features is a feature-flag map derived from live runtime state
	// (are MCP servers registered? does the daemon have multi-session
	// on? etc.). Consumers should treat absent keys as "off / unknown"
	// and unknown keys as forward-compat additions. Suggested initial
	// keys are defined as the feature* string constants below.
	Features map[string]bool `json:"features,omitempty"`

	// SlashCommands lists the server-side slash-command names the
	// producer will accept via POST /sessions/.../slash/<name>. Derived
	// from capability-interface presence, not a registry table. Clients
	// use this to render only the slashes that will actually work
	// against the connected agent.
	SlashCommands []string `json:"slash_commands,omitempty"`

	// Agent identifies the producing agent — same source that feeds
	// the /.well-known/agent-card.json endpoint plus per-session
	// runtime state (model/provider). Consolidates fields today
	// scattered across the agent card, GET /status, and the free-form
	// Server banner. Absent when the server doesn't know its own
	// identity (rare — implies neither AgentCardConfig nor a
	// StatusProvider is wired).
	Agent *AgentIdentity `json:"agent,omitempty"`

	// CallerID is the resolved Caller.Identity after the auth
	// middleware ran, echoed back to the consumer as a display hint.
	// The canonical source is GET /whoami (which also carries admin
	// + auth source). Empty when the caller couldn't be resolved.
	CallerID string `json:"caller_id,omitempty"`
}

// AgentIdentity is the capabilities.agent block — the producer's
// own identity, consolidating agent-card + per-session status
// fields the client would otherwise have to fan-out fetches to
// assemble. Every field is optional; consumers render only what's
// present.
type AgentIdentity struct {
	Name        string `json:"name,omitempty"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
	Provider    string `json:"provider,omitempty"`
	URL         string `json:"url,omitempty"`
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
//
// Capabilities (v1.4.0+) is an optional merge frame carrying hot
// changes to the capabilities the server advertised on stream
// open — e.g. an MCP server registers mid-session and features.mcp
// flips true, or a new slash provider gets wired. Semantics: any
// field the server sets is merged into the consumer's cached
// capabilities; fields absent from the update stay as-is. Not
// emitted by this server today (spec'd for future use); consumers
// MUST tolerate its absence.
type StatusUpdate struct {
	Model        string        `json:"model,omitempty"`
	Provider     string        `json:"provider,omitempty"`
	PermMode     string        `json:"perm_mode,omitempty"`
	TurnState    string        `json:"turn_state"`
	ContextPct   *int          `json:"context_pct,omitempty"`
	Capabilities *Capabilities `json:"capabilities,omitempty"`
}

// UsageUpdate is emitted after each turn finalizes and as a
// cumulative snapshot on stream open. ByModel is optional — present
// when the tracker has more than one model bucketed (typical when
// --agentic-tools routes subtasks to a small model). LastTurn is
// optional — populated on turn-end emissions (the tracker.Append
// callback path), omitted on the snapshot emission at stream open
// where there's no meaningful "last turn" to attribute.
//
// LastTurn carries the just-completed turn's per-turn cost so
// operator surfaces (remote TUI's per-turn footer) can render
// authoritative cost without needing client-side pricing lookups.
// The server's tracker owns the pricing catalog + cache-discount
// math; clients that recompute inevitably drift.
type UsageUpdate struct {
	TokensInTotal  int                     `json:"tokens_in_total"`
	TokensOutTotal int                     `json:"tokens_out_total"`
	CostUSDTotal   float64                 `json:"cost_usd_total"`
	TurnsTotal     int                     `json:"turns_total"`
	ByModel        map[string]UsageByModel `json:"by_model,omitempty"`
	LastTurn       *UsageLastTurn          `json:"last_turn,omitempty"`
}

// UsageLastTurn captures the just-completed turn's per-turn footer
// fields — enough for a "◇ 12,345 in · 456 out · $0.0125 · gemini-3.1-pro"
// row without a follow-up round-trip. Cached input is surfaced when
// present so future TUI iterations can render a "· 8k cached" tag
// alongside the base tokens.
//
// TokensInCached and TokensInCacheWrite are disjoint subsets of
// TokensIn (the third subset, the uncached remainder, is TokensIn minus
// both). A client that renders only the cached tag on an Anthropic turn
// shows a split that doesn't add up, which is why the write bucket
// ships alongside it rather than waiting for a consumer (#263).
type UsageLastTurn struct {
	TokensIn           int     `json:"tokens_in"`
	TokensInCached     int     `json:"tokens_in_cached,omitempty"`
	TokensInCacheWrite int     `json:"tokens_in_cache_write,omitempty"`
	TokensOut          int     `json:"tokens_out"`
	CostUSD            float64 `json:"cost_usd"`
	Model              string  `json:"model,omitempty"`
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

// Pause states carried on PauseEvent.State (v1.5.0).
const (
	PauseStatePaused  = "paused"
	PauseStateResumed = "resumed"
)

// PauseEvent fires when the session's pause gate closes or opens —
// docs/operator-interrupt-design.md. Consumers render a banner from it
// and (for a TUI) switch the input line into "what do you want me to do
// instead?" mode.
//
// Interrupted distinguishes the two ways in: true when a turn was
// actually cancelled on the way to this pause, false for a plain
// /pause or an /interrupt that landed while the agent was idle. That's
// the difference between "your work was killed" and "the loop just
// won't start", which is the first thing an operator asks.
//
// Mode is set only on a resumed event and echoes the disposition the
// operator chose ("steer" / "continue" / "abandon"), so a second client
// watching the stream can render what happened rather than just that
// something did.
type PauseEvent struct {
	State       string    `json:"state"`
	Reason      string    `json:"reason,omitempty"`
	Interrupted bool      `json:"interrupted,omitempty"`
	Mode        string    `json:"mode,omitempty"`
	At          time.Time `json:"at"`
}

// WakeEvent fires when the agent's wake signal is raised (v1.7.0) —
// see EventWake for which wakes qualify. Carried on the `wake` SSE
// event.
//
// The payload is deliberately just a timestamp. A wake carries no
// state a consumer can render: the thing that woke the loop reports
// itself through its own frames (an alert arrives as an `inbox` event,
// a subagent's result as `agent` events, the resulting work as
// `status-update` / `turn-complete`). What the wake adds is "look now",
// and the only thing worth attaching to that is when. A `reason` field
// nothing on this side can fill would be a promise to consumers that
// the producer cannot keep.
//
// Coalescing: none is promised in either direction. The agent's own
// wake channel is buffered-1 and drops a fire that lands while one is
// already pending, so two wakes microseconds apart may produce one
// frame or two. Consumers must treat this as an edge, not a count.
type WakeEvent struct {
	At time.Time `json:"at"`
}

// Resume modes: the disposition an operator picks when reopening the
// pause gate. Carried on ResumeRequest.Mode over the wire and echoed
// back on PauseEvent.Mode.
const (
	// ResumeModeSteer resumes with the operator's new instruction
	// injected under interrupt framing. Implied when Steer is non-empty
	// and Mode is omitted.
	ResumeModeSteer = "steer"
	// ResumeModeContinue resumes with a "carry on where you left off"
	// note and no new instruction. Implied when Steer is empty and Mode
	// is omitted.
	ResumeModeContinue = "continue"
	// ResumeModeAbandon opens the gate without injecting anything and
	// without waking the loop: the interrupted work is dropped and the
	// agent goes quiet until something else drives it.
	ResumeModeAbandon = "abandon"
)

// TurnComplete fires once per turn after the last stream-chunk for
// that turn and before the next turn's events.
//
// That ordering is enforced, not assumed (#864). `agent` frames reach
// a subscriber through the eventlog pump while typed frames are
// published directly, so the terminal frame used to be able to overtake
// the turn's own final text — and a consumer that finalizes its render
// here then had nowhere to put what followed. The broadcaster holds a
// terminal frame until every subscriber has been sent the log through
// its head; see awaitTerminalBarrier, including the bounded wait that
// degrades to unordered delivery rather than stalling a turn.
//
// CostUSD is *float64 (optional) per spec v1.1.0 §2.5 — servers
// whose model layer doesn't know pricing (this server, since
// agent.* deliberately has no pricing reference) leave it nil and
// the immediately-following usage-update carries authoritative
// cost. Servers with in-band pricing populate it.
type TurnComplete struct {
	PromptID  string   `json:"prompt_id"`
	Model     string   `json:"model"`
	TokensIn  int      `json:"tokens_in"`
	TokensOut int      `json:"tokens_out"`
	CostUSD   *float64 `json:"cost_usd,omitempty"`
	LatencyMs int64    `json:"latency_ms"`
}

// TurnError kinds per spec section 2.6. Consumers MUST treat unknown
// values as TurnErrorUnknown (forward-compat for new categories).
const (
	TurnErrorConfig        = "config_error"
	TurnErrorAuth          = "auth_error"
	TurnErrorModelNotFound = "model_not_found"
	TurnErrorRateLimited   = "rate_limited"
	TurnErrorTransientNet  = "transient_network"
	// TurnErrorCostCeiling fires when a configured per-turn or
	// per-session cost ceiling is exceeded (#145). Agent refuses new
	// turns until the operator calls ResetCostCeiling on the agent
	// (typically via a slash command). Retryable=false on this kind
	// — the host should surface the message + halt automated retry.
	// Since v1.13.0 (#891) this kind classifies the REFUSAL only — a
	// turn the pre-flight declined because a prior turn already tripped
	// the ceiling, which genuinely is that turn's outcome. The trip
	// itself is no longer reported here; it gets its own non-terminal
	// `guardrail-trip` event.
	//
	// Which means core-agent stops putting this kind on the SSE wire
	// altogether, and a client watching for it will simply stop seeing
	// it: the refusal short-circuits above the point where Run installs
	// the cleanup that emits `turn-error`, so it is delivered as the
	// iterator's error and as `error.type` on the invocation metric,
	// not as a frame. The kind stays in the enum because that error and
	// that label are what a host classifies, and because a different
	// producer may well emit it.
	TurnErrorCostCeiling = "cost_ceiling"
	// TurnErrorWatchdog fires when the behavioral watchdog trips a
	// Critical runaway signal under --watchdog=enforce (#623). Like the
	// cost ceiling, the agent refuses new turns until the operator calls
	// ResetWatchdog on the agent. Retryable=false — the host should
	// surface the message + halt automated retry (an auto-continue
	// re-drive would just re-trip the same loop). Also refusal-only
	// since v1.13.0; see the note on TurnErrorCostCeiling.
	TurnErrorWatchdog = "watchdog"
	// TurnErrorRefusalStorm fires when the permission gate ends a turn
	// because the model kept re-issuing a call the operator had already
	// refused in it (#1081). Unlike the two kinds above it is **not** a
	// guardrail: nothing is tripped, nothing needs resetting, and the
	// next turn runs normally — a model that will not take no for an
	// answer has had a bad turn, not a broken session.
	//
	// Like them, core-agent never puts this on the SSE wire. The cut goes
	// through Interrupt, so the turn's one terminal frame is `canceled`,
	// and this kind exists so `error.type` on the invocation metric can
	// say which kind of deliberate stop it was — the #818 part 2 problem,
	// where a metrics backend cannot tell a halt from an operator
	// pressing stop. The durable account of the cut is an audit row in
	// the eventlog (see pkg/agent/refusal_storm.go), not a frame.
	TurnErrorRefusalStorm = "refusal_storm"
	// TurnErrorCanceled fires when the turn's context was cancelled
	// (#816): an operator interrupt (POST /interrupt, the TUI's ESC),
	// a parent-context cancel at shutdown, or a guardrail halting the
	// turn in flight — that last one via Interrupt, and since v1.13.0
	// (#891) it is the cut turn's ONLY terminal frame, preceded by a
	// non-terminal `guardrail-trip` carrying the reason. Every one of
	// those is a deliberate stop,
	// so Retryable=false: re-running the work is the opposite of what
	// was asked for. Distinct from a context.DeadlineExceeded, which
	// stays transient_network and retryable — a call that ran out of
	// time is worth another try.
	TurnErrorCanceled = "canceled"
	TurnErrorUnknown  = "unknown"
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

	// EventID names the durable `turn-error` eventlog row that records
	// this same error (v1.19.0, #1258). A client that saw this frame
	// live will also receive the row as an `agent` frame; the shared id
	// is how it counts the error once. Empty when nothing was persisted
	// — no eventlog, or a classification that never reached the wire.
	EventID string `json:"event_id,omitempty"`
}

// GuardrailTrip (v1.13.0, #891) reports that a guardrail halted the
// session: the watchdog tripped on a runaway, or a spend cap was
// crossed. The session now refuses turns until an operator resets it.
//
// Non-terminal, and that is the whole point. Until v1.13.0 a trip rode
// out as a `turn-error` with kind `cost_ceiling` / `watchdog`, which
// made one frame do two unrelated jobs — "the session has halted, here
// is why" and "this turn ended" — and the two shapes a trip comes in
// need those answered differently:
//
//   - Tripped mid-turn: the guardrail cuts the turn via Interrupt. The
//     turn genuinely ended, so something must be its terminal frame —
//     but the `canceled` turn-error that the cancellation produces is
//     the accurate one, and #818 had to SUPPRESS it to avoid emitting
//     two terminal frames for one turn.
//   - Tripped at the turn boundary, in the post-turn hook: the turn
//     already produced its answer and emits `turn-complete`. So a
//     consumer saw a `turn-error` followed by a `turn-complete` for the
//     same turn and could not tell from either frame whether the
//     guardrail frame was terminal.
//
// Neither half was droppable — without the guardrail frame an operator
// never learns why the agent is about to start refusing, and without
// `turn-complete` a consumer waits forever for a turn that ended. So
// the trip stops being a turn outcome. It does not participate in the
// exactly-one-terminal-frame-per-turn rule, it fires identically in both
// shapes, and the turn's real outcome (`canceled` or `turn-complete`)
// follows it unmodified. #818's suppression is gone with it.
//
// `cost_ceiling` / `watchdog` remain in the turn-error kind enum for the
// REFUSAL path — a turn the pre-flight declined because the session was
// already halted, which is that turn's outcome and nothing else's.
type GuardrailTrip struct {
	// Guardrail is GuardrailWatchdog or GuardrailCostCeiling — the same
	// vocabulary as the durable trip row (#643) and the reset endpoint,
	// so an operator UI has one set of names to know.
	Guardrail string `json:"guardrail"`

	// Reason is the operator-facing explanation, verbatim from the
	// guardrail that tripped, including what to do about it.
	Reason string `json:"reason"`

	// HaltedTurn distinguishes the two shapes: true means a turn was cut
	// short and its terminal frame is the `canceled` turn-error that
	// follows; false means the turn completed and `turn-complete`
	// follows. A client that renders a "turn was killed" affordance
	// needs this, because the frames after it differ.
	//
	// Deliberately NOT omitempty. False is a real answer, and a client
	// must not have to distinguish it from a field the daemon left out.
	HaltedTurn bool `json:"halted_turn"`

	// EventID names the durable eventlog row that records this trip
	// (v1.19.0, #1258): the `guardrail-trip` halt row when the trip
	// halted the session, the `guardrail-turn-trip` row when it did
	// not. Same purpose as TurnError.EventID — one failure, counted
	// once by a client that received both. Omitted when the session has
	// no eventlog.
	EventID string `json:"event_id,omitempty"`
}
