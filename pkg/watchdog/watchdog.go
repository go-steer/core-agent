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

// Package watchdog implements the out-of-band behavioral observer
// from docs/model-selection-design.md (issue #123, PR 2 of 2).
//
// The watchdog catches sessions going off the rails — repeated
// identical tool calls, tools without intervening assistant text,
// progress stalls — via pure heuristics on per-turn telemetry. No
// LLM calls. Pairs with #119's per-tier compaction (context signal)
// and #145's cost ceiling (dollar signal); this is the *behavioral*
// signal layer.
//
// v1 scope (this PR):
//
//   - Watchdog interface + Alert / Signal / Telemetry types.
//   - DefaultWatchdog implementing two loop detectors: repeated
//     identical tool calls (the read_file loop pattern from #144 +
//     family) and, since #649, alternating cycles (a → b → a → b) plus
//     path-canonicalized argument comparison, which are the two
//     evasions the v1 detector documented and could not catch.
//     The shape is built so adding signals (tools-without-text,
//     files-not-touched, rate-based growth) is mechanical — each
//     signal is a small struct with an Observe/Check pair, the
//     DefaultWatchdog just fans observations across them.
//   - "Warn" mode: alerts are logged to stderr by the agent wiring.
//     No interactive prompt, no model swap.
//   - "Feedback" mode (#159): warn, plus the alert's model-facing
//     Guidance is injected into the agent's next-turn context as a
//     "[watchdog]" block. Detection is unchanged; the difference is
//     who reads the observation. A warn-mode alert reaches an
//     operator who may not be there, and even when they are, the
//     model — the only party that can stop making the call — never
//     learns it is looping.
//   - "Enforce" mode (#623): feedback, plus a Critical alert halts
//     the agent — the next turn is refused until the operator
//     resets, mirroring the cost-ceiling kill switch. The signal
//     detection is identical to warn mode; only the agent-side
//     reaction differs (see pkg/agent/watchdog.go).
//
// Future scope (deferred — see design doc §"Piece 2"):
//
//   - Additional signals: tools-without-text, files-not-touched,
//     context-growth-rate, cost-burn-rate. Semantic (rather than
//     syntactic) loop detection — "these two calls ask the same
//     question differently" — is v3.0 scope.
//   - "Prompt" mode: pause turn, ask operator y/n via the existing
//     permissions prompter, resume on either path.
//   - "Auto" mode: invoke Agent.SwapModel (also unshipped) to
//     escalate to a frontier model without operator interaction.
//   - `/escalate [model]` slash for operator-driven model swaps.
//   - SSE event surface for alerts (today they go to stderr only).
//
// The interface is designed so consumers can plug in their own
// implementation — same composability pattern as Compactor /
// Checkpointer. Default ships sensible for most operators; specific
// deployments override.

package watchdog

import (
	"fmt"
	"strings"
	"sync"
)

// Severity classifies the urgency of an alert. Warn is the operator-
// visible-but-not-action-blocking level; Critical marks a runaway the
// agent should act on — under --watchdog=enforce (#623) a Critical
// alert halts the agent (refuses further turns until the operator
// resets), while warn mode logs it like any other alert.
type Severity string

const (
	SeverityWarn     Severity = "warn"
	SeverityCritical Severity = "critical"
)

// Alert is what a triggered signal returns. Signal is the stable
// string ID the rest of the system can dispatch on (future "auto"
// mode picks behavior per signal).
//
// Reason and Guidance are the same observation addressed to two
// different readers, and the split is load-bearing. Reason is
// operator-facing — the agent's wiring logs it verbatim, and it may
// name operator affordances ("/interrupt", "--max-turn-cost-usd")
// that only a human at a terminal can act on. Guidance is model-
// facing: what the *agent* should do differently on its next turn,
// written as an instruction with no reference to operator controls,
// because under --watchdog=feedback (#159) it is injected into the
// model's next-turn context. An unattended daemon has no operator to
// read Reason, so for it Guidance is the only half that does work.
//
// Guidance is optional. A third-party Signal that leaves it empty
// still produces feedback — FormatFeedback falls back to Reason —
// but the fallback carries operator advice the model can't take, so
// built-in signals should always set it.
type Alert struct {
	Signal   string
	Severity Severity
	Reason   string
	Guidance string
}

// ToolCall is the per-tool-call observation the watchdog needs.
// Name is the canonical tool name (e.g. "read_file",
// "mcp.gke.list_clusters"). Args is the JSON-serialized argument
// blob, passed through as the caller produced it: the detectors
// canonicalize path-shaped values themselves (#649, canonical.go)
// rather than requiring every caller to agree on a normal form, and a
// third-party Watchdog implementation still sees the raw args.
type ToolCall struct {
	Name string
	Args string
}

// Watchdog observes per-turn telemetry and returns any alerts that
// triggered during observation. Implementations must be safe for
// concurrent use — the agent calls Observe* methods from the
// streaming event handler and Check from the post-turn hook;
// concurrency is bounded but real.
//
// The interface is intentionally narrow for v1: just tool-call
// observation + alert reporting. Richer telemetry (turn timing,
// per-turn cost delta, files-touched diff) can be added as
// additional Observe* methods as new signals need them.
type Watchdog interface {
	// ObserveToolCall records one tool invocation. Called by the
	// agent's event-tap as tool calls stream by; safe to call from
	// any goroutine.
	ObserveToolCall(ToolCall)

	// Check returns alerts triggered since the last Check call and
	// resets the per-call alert buffer. Returns nil when no signal
	// has tripped. Typically called from the agent's post-turn
	// hook; an alert returned here is "for the turn just ended."
	Check() []Alert

	// Reset clears all accumulated state. Called when the agent
	// resets (e.g. via a hypothetical /clear that clears history)
	// so signals don't carry across a logical session boundary.
	Reset()
}

// DefaultWatchdog is the package-default implementation. Fans
// observations across the configured signals; Check collects
// alerts from each.
type DefaultWatchdog struct {
	mu      sync.Mutex
	signals []Signal
	alerts  []Alert
}

// Signal is the per-detector interface inside DefaultWatchdog. Each
// signal owns its own state and decides when to emit an alert.
// Implementations must be safe to call serially from
// DefaultWatchdog (which holds a mutex across observations); they
// do NOT need to be concurrency-safe themselves.
//
// Adding a new signal: implement Signal, append to NewDefaultWatchdog's
// signal list (or to a constructor variant). No changes to
// DefaultWatchdog itself.
type Signal interface {
	// Name returns the stable signal ID used in Alert.Signal.
	Name() string

	// ObserveToolCall updates the signal's internal state with one
	// tool invocation. Returning a non-nil Alert means the signal
	// tripped on this observation; DefaultWatchdog appends it to
	// the pending-alerts buffer.
	ObserveToolCall(ToolCall) *Alert

	// Reset clears the signal's state. Called from
	// DefaultWatchdog.Reset.
	Reset()
}

// NewDefaultWatchdog returns a DefaultWatchdog wired with the
// default signal set:
//
//   - RepeatedToolCall (threshold 5): 5 consecutive calls to the same
//     tool with path-canonicalized-identical args.
//   - AlternatingCycle (period ≤ 4, 3 laps): the same short sequence
//     of calls repeated three times — the a → b → a → b shape the
//     repeat detector structurally cannot see (#649).
//
// Both are Critical, so both halt under --watchdog=enforce and both
// reach the model under --watchdog=feedback. Operators wanting
// different thresholds — or only one of the two — construct
// DefaultWatchdog directly with a custom signal list.
func NewDefaultWatchdog() *DefaultWatchdog {
	return &DefaultWatchdog{
		signals: []Signal{
			NewRepeatedToolCallSignal(5),
			NewAlternatingCycleSignal(DefaultCycleMaxPeriod, DefaultCycleRepeats),
		},
	}
}

// ObserveToolCall fans the observation across every wired signal.
func (w *DefaultWatchdog) ObserveToolCall(tc ToolCall) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.signals {
		if alert := s.ObserveToolCall(tc); alert != nil {
			w.alerts = append(w.alerts, *alert)
		}
	}
}

// Check returns any alerts that accumulated since the last Check
// and resets the buffer. Returns nil (not an empty slice) when no
// alerts are pending — lets the caller skip work cheaply.
func (w *DefaultWatchdog) Check() []Alert {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.alerts) == 0 {
		return nil
	}
	out := w.alerts
	w.alerts = nil
	return out
}

// Reset clears alerts + every signal's state. Called on logical
// session boundaries (e.g. operator-initiated /clear).
func (w *DefaultWatchdog) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.alerts = nil
	for _, s := range w.signals {
		s.Reset()
	}
}

// RepeatedToolCallSignal trips when the same (name, args) tool call
// appears Threshold times consecutively. Catches the read_file loop
// pattern from issue #144 and similar runaway-tool-call shapes.
//
// "Consecutive" is the key word: a → b → a → b → a doesn't trip
// (no run of identical calls), but a → a → a → a → a does. This
// matches operator intuition ("the agent is stuck on the same
// thing") without flagging legitimate patterns like
// alternating-tool exploration loops.
//
// Args comparison is path-canonicalized (#649, see canonical.go),
// not literal-string as in v1: "main.go", "./main.go" and
// "/workspace/main.go" are one call, because an agent re-reading one
// file under three spellings is as stuck as one re-reading it under
// the same spelling. Everything else still compares exactly — a
// detector that generalizes too eagerly halts legitimate work.
type RepeatedToolCallSignal struct {
	Threshold int

	lastCall  ToolCall
	runLength int
	tripped   bool // emit one alert per run, not one per observation past threshold
}

// NewRepeatedToolCallSignal constructs a signal with the given
// threshold. Threshold must be ≥ 2 (a "repeated call" requires at
// least two of the same in a row); values < 2 are clamped to 2 to
// avoid the degenerate case where every tool call trips the signal.
func NewRepeatedToolCallSignal(threshold int) *RepeatedToolCallSignal {
	if threshold < 2 {
		threshold = 2
	}
	return &RepeatedToolCallSignal{Threshold: threshold}
}

// Name implements Signal.
func (s *RepeatedToolCallSignal) Name() string { return "repeated-tool-call" }

// ObserveToolCall implements Signal. Tracks the running count of
// consecutive identical calls; emits an alert when count reaches
// Threshold. Returns nil on subsequent observations within the
// same run (already-tripped guard) so we don't re-emit on every
// extra call — operators want one notice per stuck pattern, not
// one per tool call past the threshold.
func (s *RepeatedToolCallSignal) ObserveToolCall(tc ToolCall) *Alert {
	if s.matches(tc) {
		s.runLength++
	} else {
		s.lastCall = tc
		s.runLength = 1
		s.tripped = false
	}
	if s.runLength >= s.Threshold && !s.tripped {
		s.tripped = true
		return &Alert{
			Signal: s.Name(),
			// Critical: a run of identical calls is a genuine runaway,
			// not advisory noise. Under --watchdog=enforce this halts
			// the agent (#623); under warn mode it's logged like any
			// other alert. Kept Critical regardless of mode so severity
			// stays an intrinsic property of the pattern, not the wiring.
			Severity: SeverityCritical,
			Reason: fmt.Sprintf(
				"agent has called %s with identical args %d times in a row — possible tool loop. Args: %s. If the agent is stuck, consider /interrupt and a different prompt phrasing. Cost ceiling (see --max-turn-cost-usd) is the hard backstop.",
				tc.Name, s.runLength, truncate(tc.Args, 200),
			),
			// Model-facing half. Deliberately says nothing about
			// /interrupt or cost ceilings: the reader here is the
			// agent, and the only lever it has is its own next call.
			Guidance: fmt.Sprintf(
				"You called %s %d times in a row with byte-identical arguments (%s). The same call with the same arguments returns the same result, so repeating it cannot make progress. Change the arguments, use a different tool, or — if you have no next step that differs — stop calling tools and say what you are stuck on. Do not repeat this call unchanged.",
				tc.Name, s.runLength, truncate(tc.Args, 200),
			),
		}
	}
	return nil
}

// Reset implements Signal.
func (s *RepeatedToolCallSignal) Reset() {
	s.lastCall = ToolCall{}
	s.runLength = 0
	s.tripped = false
}

// matches reports whether tc is the same call as the run's lastCall,
// comparing args through argsEquivalent so path spellings of one file
// don't split a run. Returns false when there is no run in flight
// (runLength == 0).
func (s *RepeatedToolCallSignal) matches(tc ToolCall) bool {
	if s.runLength == 0 {
		return false
	}
	return s.lastCall.Name == tc.Name && argsEquivalent(s.lastCall.Args, tc.Args)
}

// truncate caps s at maxLen, replacing the middle with "…" so the
// shape stays recognizable. Used to keep Alert.Reason bounded — a
// 10KB JSON blob in the alert text isn't useful and inflates log
// volume.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	const ellipsis = "…" // 3 bytes UTF-8
	if maxLen <= len(ellipsis) {
		return ellipsis[:maxLen]
	}
	half := (maxLen - len(ellipsis)) / 2
	return s[:half] + ellipsis + s[len(s)-half:]
}

// FeedbackHeader opens the block FormatFeedback renders. Exported so
// hosts, tests and transcript tooling can find the boundary without
// re-typing the literal.
const FeedbackHeader = "[watchdog]"

// FormatFeedback renders alerts as the model-facing block
// --watchdog=feedback prepends to the next turn's prompt. Returns ""
// for an empty slice so callers can skip the prepend cheaply.
//
// Two things the wording has to do. It must be unmistakably *about
// the model's own last turn* rather than a message from the user —
// a model that reads "you called grep 5 times" as a user complaint
// will apologize instead of changing behavior. And it must carry an
// instruction, not a description: the whole point of routing this to
// the model is that a description of a loop is what it already had.
//
// Not a trust boundary. A user prompt can contain the literal
// "[watchdog]" string, exactly as it can contain "[Inbox]" or
// "[Background reports]"; this block is a steering signal, and
// nothing downstream grants authority based on it.
func FormatFeedback(alerts []Alert) string {
	if len(alerts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(FeedbackHeader)
	b.WriteString(" Automated observation about your own previous turn — this is not a message from the user, and the user cannot see it.\n")
	for _, a := range alerts {
		text := a.Guidance
		if text == "" {
			// Third-party signal with no model-facing half. Reason is
			// worse (it may name operator controls) but it is the
			// observation, and dropping the alert entirely would make
			// a custom signal silently inert under feedback mode.
			text = a.Reason
		}
		b.WriteString("- ")
		b.WriteString(a.Signal)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	b.WriteString("Adjust your approach on this turn accordingly.")
	return b.String()
}

// String implements fmt.Stringer for Alert so log lines stay
// uniform. Format: "[severity] signal: reason".
func (a Alert) String() string {
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(string(a.Severity))
	b.WriteString("] ")
	b.WriteString(a.Signal)
	b.WriteString(": ")
	b.WriteString(a.Reason)
	return b.String()
}
