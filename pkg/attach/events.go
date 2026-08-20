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

// protocolVersion is the SSE event-stream protocol semver this
// server speaks. Bumped on any change to the contract per
// go-steer/core-tui's docs/sse-event-stream-protocol.md.
//
// Negotiation (#389): a client MAY declare the version it speaks on an
// /events request via the ?protocol= query param or the
// X-Attach-Protocol-Version header. The server echoes this constant
// back on every /events response header and rejects a declared major
// that differs from protocolVersion's major with 409 Conflict (a
// malformed declaration is 400). Clients that declare nothing are
// accepted unchanged for back-compat. See negotiateProtocolVersion.
//
// v1.1.0 (core-tui#42): turn-complete.cost_usd demoted from required
// to optional with documented fallback semantics (the immediately-
// following usage-update carries authoritative cost). This server
// emits TurnComplete with CostUSD = nil so the field is omitted from
// the wire entirely — the "cost deferred" signal is explicit.
//
// v1.2.0 (#277): tool-result response payloads now carry a
// `latency_ms` sidecar (int64, milliseconds) reporting the wall-
// clock time spent in the upstream tool call. Additive — consumers
// on older schema versions simply don't see the field. Populated
// by both the MCP digest wrap (pkg/mcp/digest_wrap.go) and the
// plain rename passthrough (pkg/mcp/namespace.go), so operators
// see per-call timing whether digest is enabled or not.
//
// v1.3.0 (#223 Phase 4): tool-result response payloads now carry
// an optional `savings` object reporting the digest wrap's per-call
// byte + token reduction, router path, and (agentic path only)
// subagent usage. Sidecar rides the same response-map channel as
// v1.2.0's latency_ms. Fully additive.
//
// v1.4.0 (#329): capabilities frame extended with four optional
// fields — `features` (feature-flag map), `slash_commands` (dynamic
// list of server-side slash names), `agent` (name/version/model/
// provider/url/description identity block), and `caller_id` (resolved
// Caller.Identity). Enables backend-agnostic clients (mast-web) to
// render without a code change per producer. Also spec'd an optional
// `capabilities` merge field on status-update for future hot changes.
//
// v1.5.0 (operator interrupt/steer, docs/operator-interrupt-design.md):
// new `pause` event type reporting the session's pause gate opening and
// closing; new POST /sessions/.../pause + /resume endpoints; POST
// /interrupt gained an optional request body (`hold`, `stop_subagents`)
// and richer response; GET /status can now report state="paused" plus
// `paused_since` / `pause_reason` / `interrupted`. Fully additive — a
// 1.4.0 client sees the pre-existing shapes unchanged, and an empty
// /interrupt body keeps working (though its DEFAULT behavior now holds
// the loop; see the endpoint docs).
//
// v1.6.0 (#808): GET /sessions rows carry an optional `title` — a short
// operator-facing label derived from the session's first prompt, so a
// session picker lists work rather than IDs. Omitted when the host
// doesn't implement the capability, when titling is off, and before the
// first turn lands, so every client needs the pre-1.6.0 fallback to the
// session ID regardless of the version it negotiated. Fully additive.
//
// v1.7.0 (#802): new `wake` event type reporting that the agent's wake
// signal fired — an operator called POST /wake, or a host wired
// Agent.RequestWake to something (a background alert, say). Emitted
// from the agent (like `pause`), so a wake that never touches a handler
// still reaches remote operators. A client detects it by looking for
// "wake" in the capabilities frame's `event_types`; a pre-1.7.0 daemon
// omits it and the consumer simply never gets the signal, which is what
// every client saw before this version. Fully additive — an older
// client drops the unknown event name per spec §3.
//
// v1.8.0 (#816): new `canceled` value in the turn-error `kind` enum
// (§2.6), carrying `retryable: false`. A cancelled turn used to be
// reported as `transient_network` / `retryable: true`, which told a
// client to offer a re-run of the work an operator had just
// deliberately stopped. Additive in the same way `cost_ceiling` and
// `watchdog` were: no new event type, no new field, and the spec
// already requires consumers to treat an unrecognized kind as
// `unknown`, so an older client has a defined fallback rather than
// undefined behavior (core-tui v0.22.0, the pinned client, in fact
// prints the kind verbatim). It is still a behavior change for
// this one input — a client keying a retry affordance off `retryable`
// stops offering one after an interrupt, which is the point.
//
// v1.9.0 (#768): GET /sessions/{sid}/subagents rows carry an optional
// `tools` — the subagent's configured tool grant, in the same ToolInfo
// shape and `source` vocabulary as GET /tools. Same additive shape as
// v1.6.0's `title`: a pre-1.9.0 daemon omits the key, and so does a
// 1.9.0 daemon for a subagent granted no tools, so a client cannot read
// absence as "this specialist has nothing" — it has to fall back to
// showing the roster without a grant, exactly as it did before. It
// lists what was configured, not what the runtime wires on top.
//
// v1.10.0 (#797): new GET + PATCH /sessions/{sid}/acl endpoints, and
// POST /sessions gained an optional request body carrying `viewers` /
// `contributors` for the new session's ACL. Until now a session's
// Viewers and Contributors were enforced everywhere but settable
// nowhere over HTTP, so the only reachable ACL was "owner plus
// admins" and a second participant was 404'd with no request that
// could change it. Additive in the same way v1.5.0's new endpoints
// were: no existing shape moves, and POST /sessions with no body
// behaves exactly as before (the body is optional, and an absent one
// yields the owner-only ACL that was the sole outcome previously). A
// pre-1.10.0 daemon answers the new paths with 404, which is also
// what it answers an unauthorized caller, so a client must read the
// negotiated version rather than probing.
//
// v1.10.0 (#830) also attributes approvals: POST
// /sessions/{sid}/perms/respond answers with an optional `approver`,
// GET /sessions/{sid}/perms history rows carry an optional `by`, and
// the request body accepts an optional `approver` that the server
// CHECKS against the caller it verified (mismatch → 400) rather than
// believes. Both response fields are omitted when the daemon verified
// no identity for the responder — an unauthenticated loopback
// listener, say — so a client must render "who approved this" as
// unknown rather than assuming the key is always there. Additive: the
// pre-existing `{"acknowledged":true}` shape is unchanged, and a
// client that sends no `approver` sees no new behavior.
const protocolVersion = "1.10.0"

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

	// EventAgent is the legacy event type carrying ADK session.Event
	// payloads (stream-chunk / tool-call / tool-result are all
	// multiplexed onto this one event today). Kept for back-compat
	// indefinitely — Phase 1 clients in poll mode rely on it, and
	// even push-mode clients still consume it for the model's
	// streamed text output.
	EventAgent = "agent"
)

// supportedEventTypes lists every event type this server emits.
// Surfaced in the Capabilities event on stream open so consumers
// can detect push-mode support without probing. The list includes
// legacy sub-types (stream-chunk / tool-call / tool-result) even
// though they all ride on EventAgent today — the consumer cares
// about the logical surface, not the SSE event name they currently
// share.
var supportedEventTypes = []string{
	EventStatusUpdate,
	EventUsageUpdate,
	EventInbox,
	EventTurnComplete,
	EventTurnError,
	EventPause,
	EventWake,
	"stream-chunk",
	"tool-call",
	"tool-result",
}

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

// Feature flag keys advertised on Capabilities.Features. String
// constants (not a typed enum) so downstream clients — mast-web,
// the coretui adapter, operator scripts — don't have to know a Go
// type to reason about them. Servers MAY advertise additional keys
// clients don't know about (forward-compat); clients MUST NOT crash
// on unknown ones.
const (
	// featureMultiSession is true when the server enforces per-session
	// ACLs (Options.MultiSessionEnabled) — clients rendering a
	// session picker use it to decide whether to show "your sessions"
	// vs an unfiltered fleet view.
	featureMultiSession = "multi_session"
	// featurePermsStream is true when the agent implements the
	// PromptBrokerProvider capability — clients gate the
	// /perms/stream + /perms/respond wiring on it.
	featurePermsStream = "perms_stream"
	// featureCostCeiling is true when the agent has a per-turn or
	// per-session cost ceiling armed — i.e. a turn can actually be
	// refused for spend. Sourced from the guardrail capability
	// (#666); false, not absent, when no ceiling is configured.
	featureCostCeiling = "cost_ceiling"
	// featureGuardrails is true when GET /guardrails and POST
	// /guardrails/reset are serviceable — the client can show a
	// tripped-guardrail banner and offer the reset (#666). Distinct
	// from cost_ceiling, which says whether a spend bound is armed;
	// the guardrail surface also covers the behavioral watchdog.
	featureGuardrails = "guardrails"
	// featureObserverMode is true when the producer exposes a
	// LiveAgent observer surface. Reserved for the observer-mode
	// integration; absent today.
	featureObserverMode = "observer_mode"
	// featureMCP is true when the agent implements MCPProvider
	// (there's at least one MCP server declared).
	featureMCP = "mcp"
	// featureSpecialists is true when the agent supports the
	// SubagentSpawner capability (POST /slash/subagent will work
	// against the agent).
	featureSpecialists = "specialists"
	// featureCrossDaemon is true when the server hosts the peer
	// registry (Options.PeerRegistry != nil) — clients use it to
	// enable the multi-daemon fleet picker.
	featureCrossDaemon = "cross_daemon"
	// featureInterrupt is true when the agent implements
	// InterruptProvider — clients gate ESC → cancel wiring on it.
	featureInterrupt = "interrupt"
	// featurePause is true when the agent implements PauseController
	// (v1.5.0): POST /pause + /resume work, and /interrupt parks the
	// loop instead of only cancelling the turn. Clients gate their
	// "what do you want me to do instead?" prompt on it — offering a
	// steer against a producer that can't hold would promise a park
	// that never happens.
	featurePause = "pause"
)

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
	TurnErrorCostCeiling = "cost_ceiling"
	// TurnErrorWatchdog fires when the behavioral watchdog trips a
	// Critical runaway signal under --watchdog=enforce (#623). Like the
	// cost ceiling, the agent refuses new turns until the operator calls
	// ResetWatchdog on the agent. Retryable=false — the host should
	// surface the message + halt automated retry (an auto-continue
	// re-drive would just re-trip the same loop).
	TurnErrorWatchdog = "watchdog"
	// TurnErrorCanceled fires when the turn's context was cancelled
	// (#816): an operator interrupt (POST /interrupt, the TUI's ESC),
	// a parent-context cancel at shutdown, or a guardrail halting the
	// turn in flight — that last one via Interrupt, so it emits its
	// own cost_ceiling / watchdog turn-error first and this one for
	// the cut turn second. Every one of those is a deliberate stop,
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
}

// OperatorEventTarget is the optional capability a Registrant can
// implement so the broadcaster wires its Emit method as the agent's
// typed operator-event callback at first-subscriber time, and clears
// it when the last subscriber disconnects.
//
// Registrants that implement neither this nor the deprecated
// EmitTarget still get the legacy `event: agent` frames pumped from
// the eventlog (back-compat with every poll-mode client) — they just
// won't emit typed events (capabilities still fires from the
// broadcaster directly, and the snapshot frames still flow because
// they read agent state via StatusProvider / UsageProvider, not via
// Emit).
type OperatorEventTarget interface {
	SetOperatorEventEmitter(func(eventType string, payload any))
}

// EmitTarget is the pre-#506 shape of OperatorEventTarget. The
// broadcaster still probes it as a fallback, so registrants built
// against the old method name keep emitting typed events — the
// failure mode of dropping the fallback would be SILENT (an
// interface assertion quietly failing means the operator stream
// just goes dark), which is why this stays for a full deprecation
// cycle rather than being cut over.
//
// Deprecated: implement OperatorEventTarget.
type EmitTarget interface {
	SetAttachEmitter(func(eventType string, payload any))
}
