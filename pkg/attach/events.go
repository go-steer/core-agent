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
// undefined behavior (core-tui v0.22.0, the client pinned when this
// bump landed, in fact prints the kind verbatim). It is still a
// behavior change for this one input — a client keying a retry
// affordance off `retryable` stops offering one after an interrupt,
// which is the point.
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
//
// v1.10.0 (#808) also adds POST /sessions/{sid}/title, the manual
// override for the session titles v1.6.0 started inferring. The body
// is `{"title": "..."}` with the key REQUIRED — `""` clears the title
// (and re-arms automatic generation), while an omitted key is refused
// with a 400 rather than read as a clear. The 200 answers with what
// was actually stored after the host's normalization, plus a
// `persisted` flag reporting whether the new name reached the durable
// session row: false is not an error and is the norm for a daemon
// with no ACL store, so a client must not read it as a failed rename.
// A host that registered no title-setting capability answers 501,
// which — unlike the 404 an unauthorized caller gets — is safe to
// feature-detect on, since reaching it means the caller was already
// authorized for the session.
//
// v1.10.0 (#698) also gives POST /sessions/{sid}/inject an optional
// `wake` flag. Omitted or true keeps the historical delivery — queue
// the message AND wake, so it preempts a sleep and un-parks a paused
// loop. False queues only: the message is appended, published as the
// usual `inbox`/queued event, and drained by whatever turn happens
// next, but nothing here causes that turn. There is deliberately NO
// promptness guarantee — an autonomous loop reaches the message on its
// own sleep timer, an operator-driven session when the operator next
// says something, a parked session when it is resumed — so a caller
// that needs the message acted on must not send false. The 200 gains a
// `woke` field on BOTH paths, so a client can confirm which delivery it
// got; its absence means a pre-1.10.0 daemon, which always woke. A
// registrant without the deferral capability answers 501 rather than
// waking anyway, since a silent upgrade to a wake would hand back the
// exact preemption the caller asked to avoid.
//
// v1.10.0 (#840) also has POST /sessions/{sid}/inject report the
// `prompt_id` it assigned, on both deliveries, and POST
// /sessions/{sid}/wake report it when the call carried a prompt. It is
// the same id that goes out on `inbox`/queued, comes back on
// `inbox`/dequeued, and names a turn on `turn-complete` — and it is
// the only handle a client has on the turn its message will land in
// from the moment it sends. Everything else arrives too late
// (`turn-complete`), or is not survivable across a reconnect
// (`inbox` events carry the id but have no `seq`, so a mapping
// rebuilt from them desynchronises), or breaks under two concurrent
// injectors (correlating "the frame that just fired" with "the
// request I just made"). Additive and `omitempty`: absent means the
// daemon could not name one — a pre-#840 build, or a host whose
// registrant doesn't implement IdentifyingInjector — so a client must
// tolerate its absence rather than requiring it. It is NOT a turn id:
// the inbox coalesces, so N injects can share one `turn-complete` and
// only one of their ids appears on it.
//
// v1.11.0 (#878) narrows POST /sessions/{sid}/inject: it no longer
// releases a pause hold. A message arriving is queued, published as the
// usual `inbox`/queued event, and left waiting behind the gate; the gate
// is opened by POST /resume and nothing else. Same for POST /wake
// carrying a `prompt`, which injects through the same path.
//
// Through 1.10.0 an inject from any caller but auto-continue called
// Resume for you, so that `POST /interrupt` then `POST /inject` matched
// the pre-1.5.0 world where interrupt didn't park at all. That shim
// assumed a human at the other end of the socket. It cannot check:
// callers carry an identity, not a species, and a machine producer can
// legitimately inject under a person's identity (k8s-lookout's watcher
// asserts its `--owner` through a proxy identity). Any alert with
// inject rights could therefore un-park a session an operator had
// deliberately stopped, which is what happened on the #799 smoke run.
//
// Migration for a client that relied on it: send POST /resume
// {"mode":"steer","steer":"..."} instead of POST /inject. That is one
// call, not two, it frames the message as an interrupt-steer so the
// model knows its last turn was killed, and it is what the endpoint was
// added for. A client that injects into a session it did not park sees
// no change — the gate was already open.
//
// A minor rather than a major, deliberately: no frame, field, or status
// code changes shape, so nothing a client parses is affected and the
// major-mismatch 409 in negotiateProtocolVersion would lock out working
// clients over a change most of them can't observe. Recorded here in
// full because it IS a behavior change and the additive-minor
// convention would otherwise imply it isn't.
//
// v1.12.0 (#896) makes `state:"running"` reachable on GET
// /sessions/{sid}/status and adds `turn_in_flight` beside it. The
// state was declared from the start and never produced: the sole
// StatusProvider had no run-loop signal to read, so a mid-turn
// /status answered "idle" and the SSE status seed answered
// turn_state:"idle". `turn_in_flight` exists because `state` is one
// field and pause outranks running in it — a session parked mid-turn
// still reports "paused" so hold banners keep working, and the bool
// is how a client learns the interrupted turn is nonetheless still
// executing. Additive and omitempty: a client that ignores it reads
// the same `state` values it always did, except that "running" now
// actually occurs where "idle" used to be wrong.
//
// v1.12.0 (#897) also makes POST /sessions/{sid}/agents/{name}/stop
// answer for what the call did rather than for what was asked. Its
// 200 gains `stopped` (false when the subagent had already reached a
// terminal status before the request arrived) and `status` (what it
// terminated as). 404's trigger set does not move — it was already
// exactly "the manager has never registered this name", whatever the
// handler and the docs said about finished subagents — and the docs
// are corrected to it. Through 1.11.0 both cases answered
// `stopped: true`,
// because the capability's bool meant "the name is registered" and a
// finished subagent keeps its handle — so an operator stopping a
// subagent that completed thirty seconds earlier was told they had
// stopped it, and /interrupt's `stopped_subagents` listed every
// subagent that raced them to the finish. The stop_agent tool reported
// the handle's status but not whether the call stopped anything, until
// #1164 gave the model's door the same `stopped` field this one has.
//
// Additive in shape — `stopped` was already there and keeps its type,
// `status` is new and omitempty — but the VALUE of `stopped` changes
// for the already-finished case. A client that reads `stopped: true`
// as "it is no longer running" must switch to reading the 200 itself
// as that claim, and `stopped` only as whether it was the one who did
// it. A minor rather than a major for the same reason as 1.11.0:
// nothing a client parses changes shape.
//
// v1.13.0 (#891): new non-terminal `guardrail-trip` event type,
// `{"guardrail","reason","halted_turn"}`, and a guardrail trip stops
// being reported as a `turn-error`. A trip is not a turn outcome, and
// modelling it as one made a single frame answer two questions that
// come apart in both of the shapes a trip arrives in: cut mid-turn, the
// accurate terminal frame is the `canceled` that the Interrupt produces
// — which #818 had to suppress, because the guardrail frame was already
// claiming that slot; tripped at the turn boundary, the turn genuinely
// completed and emits `turn-complete`, so a consumer saw a `turn-error`
// and a `turn-complete` for one turn with no way to tell which was
// terminal. Now the trip rides its own non-terminal event in both
// shapes, `halted_turn` says which one it is, the turn's real outcome
// follows unmodified, and #818's suppression is deleted.
//
// `cost_ceiling` / `watchdog` stay in the turn-error kind enum for the
// REFUSAL path — a turn the pre-flight declined because the session was
// already halted, which IS that turn's outcome.
//
// Additive in shape and a minor for that reason, but read the next
// paragraph before treating it as a free upgrade. **A pre-1.13.0 client
// stops seeing the trip reason.** It drops the unknown event name per
// spec §3, and the `turn-error` it used to render the reason from is no
// longer emitted for a trip; on the in-flight shape it now gets a bare
// `canceled`, and on the boundary shape nothing at all. Losing the one
// sentence that explains why the agent is about to refuse everything is
// a worse regression than the framing oddity this fixes, which is why
// the change landed together with its consumer (core-tui) rather than
// ahead of it. A client detects support the usual way: look for
// "guardrail-trip" in the capabilities frame's `event_types`.
//
// The durable fact is unchanged and was always modelled correctly — the
// `guardrail-trip` eventlog row (#643) predates this and the wire event
// takes its name, so a trip reads the same off the stream and out of
// history.
// v1.14.0 (#1088): `POST /perms/respond` answers **410 Gone** for a
// prompt whose turn ended before the approval arrived, where it used to
// answer 404. No frame changes shape; this is the same widening 1.12.0
// was — a status a client already had to handle now covers a second
// case — and it is a version rather than a silent fix because the two
// codes are the only thing that tells a late out-of-band approver
// whether the action they just authorized went ahead. 404's body says
// "already responded, cancelled, or never issued", and on a pre-1.14.0
// daemon a guardrail-cut prompt landed in that sentence alongside ids
// the broker never had. A client cannot tell those apart by reading it,
// so it could not tell them apart at all.
//
// 404 keeps the cases it can still honestly claim: an id already
// answered, and one this daemon never issued. Expiry is unchanged — it
// has been 410 since #647 — and the two 410 bodies differ, because
// "expired" tells an operator to answer faster or raise
// approval_timeout, and a cancellation says that would not have helped.
//
// v1.15.0 (#1165): `POST /perms/respond` accepts an optional `reason`
// with a deny, and the model reads it in the refused call's result as
// "The operator's reason: …". Additive: without it a deny is
// byte-for-byte what it was. A reason on any other decision, or over
// MaxDenyReasonBytes, is a 400 and leaves the prompt pending. A pre-1.15.0
// daemon ignores the field, because readJSON does not reject unknown
// keys, so a client that wants to know whether its reason reached the
// model must check protocol_version rather than the status code.
//
// v1.16.0 (#1168): `POST /perms/mode` switches a running session's
// permission mode to ask / acceptEdits / plan / yolo — what the local
// TUI's Shift+Tab chip always could. ActionSessionAdmin (owner or
// admin); each change writes an `attach-perm-mode` eventlog row. Additive.
// A pre-1.16.0 daemon has no such route, and a caller the ACL refuses
// gets the same not-found body, by design (authorize never reveals a
// session to someone it refuses).
//
// v1.17.0 (#1179): on a multi-session daemon, an "allow-always" answer
// to `POST /perms/respond` from anyone but a daemon admin — the session
// owner included — is applied as "allow-session", because an always
// grant widens every session and survives a restart.
// The 200 body gains `decision` (what was applied) and `downgraded`. A
// pre-1.17.0 daemon omits both and applied the always.
//
// v1.18.0 (#1175): auto mode's escalated prompt. A permission prompt
// frame on `/perms/stream`, live or replayed on subscribe, gains
// `approver_model` and `approver_reason` when ModeAuto's approver model
// looked at the call and passed it to a person. The reason is model
// output: a client quotes it as the approver's words and offers only
// allow once and deny, since the daemon applies any allow on such a
// prompt as allow-once, and `POST /perms/respond` now reports that with
// `decision: "allow-once"`, `downgraded: true`. `GET /perms` approval
// rows gain `approver_model` for a call the approver allowed without a
// person. `POST /perms/mode` accepts "auto" for a session that can enter
// it (400 otherwise), and `GET /perms` gains `settable_modes`, the modes
// it will accept for this session, in the chip's order. All additive and
// omitempty; a pre-1.18.0 daemon never sets them and refuses "auto".
// `POST /inject` also takes an optional `task_bytes` (#1230): how many
// leading bytes of `message` the operator wrote, which alone count
// toward the approver's task. core-agent-tui sends it so @-inlined file
// content never does. A pre-1.18.0 daemon ignores it and counts the
// whole message.
//
// v1.19.0 (#1258): a per-turn guardrail trip and every turn error are
// durable. Each one appends an eventlog row — `guardrail-turn-trip`
// (Author=agent/guardrail-turn-trip) for a trip that did not halt the
// session, the existing `guardrail-trip` row for one that did, and
// `turn-error` (Author=agent/turn-error) for a turn error — so they
// replay as ordinary `agent` frames from `?since=0` and reach a client
// that attached late. The typed `guardrail-trip` and `turn-error`
// frames gain an optional `event_id` naming that row, so a client that
// receives both (it was attached when it happened) can count the
// failure once. Additive and omitempty: a pre-1.19.0 daemon writes no
// such rows and never sets the field, and the field is also absent
// when the session has no eventlog to write to.
//
// v1.20.0 (#1283): `GET /agents` rows gain `next_wake_at` and
// `wake_detail`, set only while a subagent sleeps on a
// schedule_next_turn wake; its `status` stays "running" meanwhile.
// `last_report` is now filled while a subagent runs, from its latest
// report_alert text or else its latest model message, instead of only
// once it has finished. Additive: a pre-1.20.0 daemon never sets the
// wake fields and leaves `last_report` empty until the subagent ends.
const protocolVersion = "1.20.0"

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
	EventGuardrailTrip,
	"stream-chunk",
	"tool-call",
	"tool-result",
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
// Deprecated: implement OperatorEventTarget. Removed in v4.0.
type EmitTarget interface {
	SetAttachEmitter(func(eventType string, payload any))
}
