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

// Agent-side wiring for the behavioral watchdog (pkg/watchdog).
// The watchdog itself is concern-free of the agent's internals;
// this file is the bridge that extracts tool-call observations
// from session events as they stream and surfaces alerts via the
// post-turn hook. See pkg/watchdog/watchdog.go for the package
// docstring and the failure modes / v1 scoping.

package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// WithWatchdog wires a behavioral watchdog. The agent calls
// w.ObserveToolCall as tool calls stream by, and w.Check from the
// post-turn hook. Returned alerts are passed to onAlert if non-nil;
// when onAlert is nil the alerts are collected and discarded each
// turn (useful for tests, or for hosts that read the watchdog's
// own state via a side channel).
//
// Composable with everything else: pass alongside WithCompactor /
// WithCheckpointer / WithCostCeiling / etc. The watchdog runs in
// the same post-turn hook the others use.
func WithWatchdog(w watchdog.Watchdog, onAlert func(watchdog.Alert)) Option {
	return func(o *options) {
		o.watchdog = w
		o.onWatchdogAlert = onAlert
	}
}

// WithWatchdogEnforce promotes the watchdog from observe-only ("warn"
// mode) to a kill switch ("enforce" mode, #623). When set, a Critical
// alert (today: the repeated-tool-call runaway signal) trips the agent:
// it emits a watchdog turn-error and refuses subsequent turns until the
// operator calls ResetWatchdog — the same halt contract as the cost
// ceiling. No-op unless a watchdog is also wired via WithWatchdog. Warn-
// mode alerts (non-Critical) never halt, even under enforce.
func WithWatchdogEnforce() Option {
	return func(o *options) {
		o.watchdogEnforce = true
	}
}

// WithWatchdogFeedback routes watchdog alerts back into the model's
// next-turn context ("feedback" mode, #159): after a turn that tripped
// a signal, the next Run prepends a "[watchdog]" block carrying each
// alert's model-facing Guidance to the prompt.
//
// The warn-mode surface it extends reaches an operator — who, on an
// unattended daemon, is not there, and who even at a terminal can only
// interrupt a turn already in flight. The party that can stop making
// the looping call is the model, and it never learned it was looping.
//
// Implied by WithWatchdogEnforce, which is deliberate rather than
// incidental: an enforce-mode halt is cleared by an operator reset,
// and a reset resumes a model whose context still ends in the loop it
// was halted for. Without the injected observation, the very next turn
// re-issues the same call and re-trips — the reset would be a treadmill.
//
// No-op unless a watchdog is also wired via WithWatchdog.
func WithWatchdogFeedback() Option {
	return func(o *options) {
		o.watchdogFeedback = true
	}
}

// maxPendingWatchdogFeedback caps the queue of alerts awaiting
// injection. The queue drains on every Run, so it only grows when a
// host observes turns without starting new ones; a bound keeps that
// case from turning into an ever-growing prompt prefix. Oldest are
// dropped: the newest observation describes the behavior the model is
// about to repeat.
const maxPendingWatchdogFeedback = 4

// watchdogError is returned by Agent.Run when a prior turn tripped the
// watchdog under enforce mode and the operator hasn't reset it. Mirrors
// costCeilingError: a distinct type so hosts can classify "operator must
// reset" apart from retryable failures.
type watchdogError struct {
	reason string
}

func (e *watchdogError) Error() string { return e.reason }

// AsTurnError reports a watchdog refusal as the watchdog kind rather
// than letting the substring classifier guess from the reason. Mirrors
// costCeilingError.AsTurnError and exists for the same reason (#818):
// the reason text matches none of the classifier's needles, so a
// refused turn recorded `error.type: unknown` — and the watchdog case
// is worse than the ceiling's, because the reason embeds arbitrary
// trigger prose, which made the recorded kind a lottery over whatever
// a runaway happened to be looping on.
// Nil-receiver safe for the same reason as costCeilingError's.
func (e *watchdogError) AsTurnError() attach.TurnError {
	if e == nil {
		return watchdogTurnError("")
	}
	return watchdogTurnError(e.reason)
}

// watchdogTurnError is the one construction site for the watchdog
// payload — shared by the trip emit and the refusal classification
// above so the two cannot drift apart.
func watchdogTurnError(reason string) attach.TurnError {
	return attach.TurnError{
		Kind:      attach.TurnErrorWatchdog,
		Code:      "watchdog",
		Message:   reason,
		Retryable: false, // operator must reset, not the host
	}
}

var _ attach.SelfClassifyingError = (*watchdogError)(nil)

// IsWatchdogTripped reports whether err was returned by Run because a
// prior turn tripped the behavioral watchdog under --watchdog=enforce.
// Hosts use this to distinguish "operator must reset the watchdog" from
// other Run errors that may warrant retry.
func IsWatchdogTripped(err error) bool {
	_, ok := err.(*watchdogError)
	return ok
}

// observeToolCallsForWatchdog walks ev's content parts and feeds
// any function-call parts to the wired watchdog.
//
// Partial events are skipped, and that is what keeps the aggregator's
// re-emission (#363) out of the count without a content key (#915).
// In streaming mode ADK yields one event per chunk with Partial set,
// then one aggregated event with it clear, and the aggregated event
// carries every part of the model call — so a call reaches the
// watchdog exactly once, on the same event ADK itself executes the
// tool from (base_flow: `if resp.Partial { continue }` guards
// handleFunctionCalls). Counting what the runtime runs is the whole
// definition the loop detectors want. It is also what the rest of the
// tree already does with partials: subagent.go, autonomous.go,
// compactor.go and the TUI adapter all gate on ev.Partial.
//
// seen then only has to guard a genuine duplicate emission of the
// same part at non-partial level, so calls dedup on ID alone and an
// ID-LESS call is observed unconditionally — the same reversal #907
// made on the response side, for the same reason: a name+args key
// cannot tell "the same part twice" from "the same call twice", and
// the second is precisely the runaway signal. `repeated-tool-call`
// (5 consecutive identical calls), `alternating-tool-cycle` and
// `dominant-tool-call` all count repetition WITHIN a Run, which is
// the window `seen` spans — it is allocated once per Run, not per
// model turn (agent.go), so a key that collapses repeats inside it
// silences the detectors outright.
//
// Note that ADK does synthesize IDs for providers that omit them
// (finalizeModelResponseEvent → utils.PopulateClientFunctionCallID,
// an `adk-`-prefixed UUID stamped on the part before the event is
// yielded), so ID-less calls are the exception here rather than the
// Gemini norm. That synthesis is also why the partial filter is
// needed and ID dedup alone is not: when the aggregator rebuilds a
// part from streamed PartialArgs instead of forwarding the chunk's
// own pointer, the chunk and the aggregate are two distinct parts and
// get two distinct synthesized IDs. Each real call then arrives twice
// under different args — once with the chunk's empty args, once
// complete — which cannot reach five consecutive identical calls and
// reads to the cycle detector as an a→b→a→b alternation that the
// model never made.
//
// Args are JSON-serialized so the watchdog's literal-string-compare
// detector has stable input — Go's map iteration order would
// otherwise make logically-identical calls compare unequal.
// Best-effort: if a part's args don't JSON-marshal cleanly we
// fall back to a recognizable placeholder; the alternative would
// be skipping the observation entirely, which silently weakens
// the signal. Better to compare on the placeholder than miss
// observations.
func (a *Agent) observeToolCallsForWatchdog(ev *session.Event, seen map[string]struct{}) bool {
	if a.watchdog == nil || ev == nil || ev.Content == nil || ev.Partial {
		return false
	}
	observed := false
	for _, p := range ev.Content.Parts {
		if p == nil || p.FunctionCall == nil {
			continue
		}
		args := serializeArgsForWatchdog(p.FunctionCall.Args)
		if id := p.FunctionCall.ID; id != "" {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
		}
		observed = true
		a.watchdog.ObserveToolCall(watchdog.ToolCall{
			Name: p.FunctionCall.Name,
			Args: args,
		})
	}
	return observed
}

// observeToolResultsForWatchdog walks ev's content parts and feeds any
// function-response parts to the wired watchdog, when it implements
// the optional watchdog.ToolResultObserver extension (#639). A
// watchdog that only counts calls is left alone.
//
// Success vs failure follows ADK's convention (base_flow.go): a tool
// error is a reserved "error" key inside FunctionResponse.Response.
// Flattening it here means the watchdog never has to know a provider's
// response shape, and one place decides what "failed" means.
//
// Shares the per-Run dedup set with call observation, under a
// distinct key prefix, so a re-emitted FunctionResponse cannot trip a
// streak signal at half its threshold.
//
// A response with NO ID is observed unconditionally, and that is a
// deliberate reversal of what this function used to do (#907). It
// previously fell back to name+error, on the reasoning that
// undercounting is the safe direction to be wrong in. It is not: since
// a rejection is not an error, thirteen consecutive "already recorded
// for this turn" no-ops all hashed to one key and arrived at the
// watchdog as a single observation. The runaway this file exists to
// see is one that repeats WITHIN a Run, so a key that collapses
// repeats within a Run cannot see it.
//
// #907 argued that from "ADK never synthesizes call IDs, so on every
// Gemini deployment EVERY response takes the fallback". That premise
// is wrong and is corrected here (#915): finalizeModelResponseEvent
// calls utils.PopulateClientFunctionCallID, which stamps an
// `adk-`-prefixed UUID on every ID-less call before the event is
// yielded, and handleFunctionCalls copies that ID onto the response.
// The empty-ID shape is real in REPLAYED histories — the ID is
// stripped again on the way back to the model — but not on the live
// event stream this tap reads. The fix stands on its own reasoning:
// an ID-less part is a part nothing can tell apart from a genuine
// repeat, and a repeat is the signal.
//
// Dropping the fallback is safe because the duplication it guarded
// against is a FunctionCall phenomenon, not a FunctionResponse one:
// responses are emitted once, by handleFunctionCalls, and never go
// through the streaming aggregator that re-emits an intermediate
// aggregate plus a final (pkg/runner/events.go says the same, and
// dedups them only symmetrically-in-case). Responses that DO carry an
// ID keep exact ID dedup, so if a future ADK change starts re-emitting
// them, the path where that is detectable is still guarded. Counting
// ID-less parts positionally also matches how history_pairing.go
// handles the identical empty-ID Gemini shape (#367).
func (a *Agent) observeToolResultsForWatchdog(ev *session.Event, seen map[string]struct{}) bool {
	if ev == nil || ev.Content == nil {
		return false
	}
	obs, ok := a.watchdog.(watchdog.ToolResultObserver)
	if !ok {
		return false
	}
	observed := false
	for _, p := range ev.Content.Parts {
		if p == nil || p.FunctionResponse == nil {
			continue
		}
		errText := toolResponseError(p.FunctionResponse.Response)
		if id := p.FunctionResponse.ID; id != "" {
			key := "result\x00" + id
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}
		observed = true
		obs.ObserveToolResult(watchdog.ToolResult{
			Name:  p.FunctionResponse.Name,
			Error: errText,
			NoOp:  toolResponseNoOp(p.FunctionResponse.Response),
		})
	}
	return observed
}

// toolResponseError extracts the tool error from an ADK function
// response, returning "" for a successful call. Mirrors the split the
// TUI adapter does for rendering; both read the same reserved key.
//
// A non-string, non-error value under "error" still counts as a
// failure — a tool that returns a structured error object is failing,
// and treating an unrecognized shape as success would silently drop
// exactly the observations this signal exists to make.
func toolResponseError(resp map[string]any) string {
	v, ok := resp["error"]
	if !ok || v == nil {
		return ""
	}
	switch e := v.(type) {
	case string:
		if e == "" {
			return ""
		}
		return e
	case error:
		return e.Error()
	default:
		return fmt.Sprintf("%v", e)
	}
}

// ToolResultNoOpKey is the reserved response key a tool sets to declare
// that an invocation changed nothing (#907). It sits alongside ADK's
// reserved "error" key and is read by exactly one consumer,
// watchdog.NoOpStreakSignal.
//
// A tool opts in. The alternative — a registry of (tool, status) pairs
// the runtime knows to mean no-op, or matching the status prose — puts
// the knowledge somewhere that goes stale the first time a status
// string is reworded, and mark_task_done's repeat status carries a doc
// comment actively inviting that rewording.
//
// The value is a claim about THIS call, not about the tool. A tool that
// sometimes does work and sometimes does not sets it per invocation;
// that is the whole shape the signal reads.
const ToolResultNoOpKey = "no_op"

// toolResponseNoOp reports whether a tool declared its own call inert.
//
// Only a literal true counts. Unlike toolResponseError, which treats an
// unrecognized shape under "error" as a failure, an unrecognized shape
// here is NOT read as a no-op: the failure path is fail-safe (an
// unreadable error still gets counted as an error), while this one is
// fail-open by necessity. NoOpStreakSignal raises Critical alerts,
// which halt the agent under --watchdog=enforce, so a garbled value
// must never be able to manufacture a halt.
//
// JSON round-trips through ADK as bool, but a response that has been
// through a generic map may carry the string "true"; both are accepted
// because both are unambiguous, and nothing else is.
func toolResponseNoOp(resp map[string]any) bool {
	v, ok := resp[ToolResultNoOpKey]
	if !ok || v == nil {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return b == "true"
	default:
		return false
	}
}

// drainWatchdogAlerts is the post-turn hook entry point. Pulls any
// alerts the watchdog accumulated during the just-ended turn and
// dispatches them to the configured onWatchdogAlert callback. No-op
// when no watchdog is wired or no callback is set.
func (a *Agent) drainWatchdogAlerts() {
	if a.watchdog == nil {
		return
	}
	alerts := a.watchdog.Check()
	// Count BEFORE the nil-callback early return (#338): the metric
	// covers every alert the watchdog raised, whether or not a host
	// callback consumes them. The internal buffer drains on Check(),
	// so a sync counter here is the only place these can be counted.
	if a.watchdogAlertCounter != nil {
		for _, alert := range alerts {
			a.watchdogAlertCounter.Add(context.Background(), 1, metric.WithAttributes(
				attribute.String(AttrWatchdogSignal, alert.Signal),
				attribute.String(AttrWatchdogSeverity, string(alert.Severity)),
				attribute.String(AttrMetricSessionID, a.sessionID),
			))
		}
	}
	// Operator callback (the warn-mode surface). Runs regardless of
	// enforce mode so operators still see the alert logged.
	if a.onWatchdogAlert != nil {
		for _, alert := range alerts {
			a.onWatchdogAlert(alert)
		}
	}
	// Feedback mode (#159): queue the alerts for injection into the next
	// turn's prompt. Outside the onWatchdogAlert==nil guard for the same
	// reason enforcement is — the model-facing route must not depend on
	// whether a host wired the operator-facing one.
	if a.watchdogFeedback {
		a.queueWatchdogFeedback(alerts)
	}
	// Enforce mode (#623): a Critical alert halts the agent. Kept after
	// the callback so the operator sees the log line before the refusal,
	// and outside the onWatchdogAlert==nil guard above so enforcement
	// fires even when no warn-mode callback is wired.
	if a.watchdogEnforce {
		a.maybeTripWatchdog(alerts)
	}
}

// enforceWatchdogInTurn is the in-turn arm of enforce mode (#705). It
// runs from Run's event tap, right after the tool-call/result
// observations, and halts a turn that is looping WHILE it loops.
//
// Why a second call site. Until this, every watchdog decision happened
// at a turn boundary: observations accumulated during the turn and
// drainWatchdogAlerts ran from the post-turn cleanup. That is fine for
// a loop spread across turns — turn N trips, turn N+1 is refused at
// preflight. It is useless for a loop INSIDE one turn, which is the
// shape the tool-calling flow actually produces: the model emits a
// tool call, the flow executes it and calls the model again, all
// within a single Run. An agent stuck in that inner loop never reaches
// the post-turn hook, so the backstop that the boot line advertises as
// "halts the agent" cannot fire at all, and nothing is logged either.
// The cost ceiling has the same structural blind spot and needed its
// own out-of-band re-check to work around it (#362, see Run).
//
// Called only when an observation actually landed this event (see the
// bool the observe helpers return): a signal can only newly trip on a
// new observation, so re-checking on every text delta of a streaming
// turn would be pure mutex traffic.
//
// Enforce only. Below enforce nothing halts, so nothing is drained
// early: warn's contract is an operator log line and feedback queues
// the observation for the NEXT turn's prompt, so draining either one
// sooner would only change when a human or the model reads it. Under
// enforce those two surfaces do fire earlier, which is the point —
// the log line should precede the halt it explains. Legacy timing for
// them: warn's contract is an operator log line, and feedback queues
// the observation for the NEXT turn's prompt, so draining early would
// change when the model sees its own correction. Enforce's contract is
// "stop", and stopping is worth doing at the first opportunity.
//
// Interrupt() is the same mechanism an operator /interrupt uses, so the
// turn unwinds through the ordinary cancelled-runCtx path: the event
// stream ends, cleanup runs, and the post-turn drain is a no-op because
// Check() already emptied the buffer and maybeTripWatchdog is
// idempotent. It does NOT set pendingInterruptAudit — that flag is
// only raised by MarkInterruptPending from the attach handler — so a
// watchdog halt is never mislabeled as an operator interrupt. Nor does
// the cleanup re-report the cancellation as a `canceled` turn-error: the
// trip's own frame is the turn's terminal one (#818).
func (a *Agent) enforceWatchdogInTurn() {
	if a == nil || a.watchdog == nil || !a.watchdogEnforce {
		return
	}
	// Already halted: the Interrupt below has fired and the turn is
	// unwinding. Re-draining would be harmless but re-interrupting a
	// turn whose cancel has been cleared is pointless work on every
	// remaining event in the stream.
	if tripped, _ := a.WatchdogTripped(); tripped {
		return
	}
	a.drainWatchdogAlerts()
	if tripped, _ := a.WatchdogTripped(); tripped {
		// The trip's turn-error is this turn's terminal frame; mark
		// before cutting the turn so the cancellation doesn't produce
		// a second one (#818, see guardrail_halt.go).
		a.markGuardrailHalt(attach.TurnErrorWatchdog)
		a.Interrupt()
	}
}

// queueWatchdogFeedback appends alerts to the pending-injection queue,
// trimming to maxPendingWatchdogFeedback from the front.
func (a *Agent) queueWatchdogFeedback(alerts []watchdog.Alert) {
	if len(alerts) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.watchdogPending = append(a.watchdogPending, alerts...)
	if n := len(a.watchdogPending) - maxPendingWatchdogFeedback; n > 0 {
		a.watchdogPending = a.watchdogPending[n:]
	}
}

// prependWatchdogFeedback drains the pending queue and, when non-empty,
// returns prompt with the "[watchdog]" block in front. Called from Run,
// after the inbox and background-report prepends, so the correction
// about the model's own last turn is the first thing it reads.
//
// Draining on read (rather than on turn success) means an observation
// is delivered exactly once even if the turn it lands in fails. Losing
// it in that case is the right trade: the signal describes behavior
// several turns back by the time a retry lands, and a block that
// re-appears every turn until some turn succeeds is a prompt leak.
func (a *Agent) prependWatchdogFeedback(prompt string) string {
	a.mu.Lock()
	pending := a.watchdogPending
	a.watchdogPending = nil
	a.mu.Unlock()
	block := watchdog.FormatFeedback(pending)
	if block == "" {
		return prompt
	}
	return block + "\n\n---\n\n" + prompt
}

// maybeTripWatchdog halts the agent when any alert this turn is
// Critical. Sets watchdogTripped + watchdogReason and emits a
// watchdog turn-error, exactly mirroring maybeEnforceCostCeiling.
// Idempotent: once tripped, later turns' drains are a no-op so we
// don't re-emit. Non-Critical alerts never trip.
func (a *Agent) maybeTripWatchdog(alerts []watchdog.Alert) {
	if len(alerts) == 0 {
		return
	}
	a.mu.Lock()
	if a.watchdogTripped {
		a.mu.Unlock()
		return
	}
	a.mu.Unlock()

	var trigger *watchdog.Alert
	for i := range alerts {
		if alerts[i].Severity == watchdog.SeverityCritical {
			trigger = &alerts[i]
			break
		}
	}
	if trigger == nil {
		return
	}

	reason := fmt.Sprintf(
		"watchdog halted the agent (%s): %s Agent will refuse new turns until the operator resets it (/guardrail reset, or POST /sessions/{id}/guardrails/reset).",
		trigger.Signal, trigger.Reason,
	)
	a.mu.Lock()
	a.watchdogTripped = true
	a.watchdogReason = reason
	a.mu.Unlock()

	// Durable halt (#643). This is the trip that most needs to survive
	// a restart: a runaway loop that ends in an OOM kill is exactly the
	// shape that would otherwise resume looping in the next pod.
	a.queueOutOfBandEvent(attach.NewGuardrailTripEvent(attach.GuardrailWatchdog, reason))

	a.emit(attach.EventTurnError, watchdogTurnError(reason))
}

// preflightWatchdog returns a non-nil watchdogError when a prior turn
// tripped the watchdog under enforce mode and the operator hasn't
// reset it. Called at the very top of Run, before any model calls —
// the refusal is structural, so an auto-continue re-drive of the
// looping turn is refused here rather than re-issuing the runaway call.
func (a *Agent) preflightWatchdog() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.watchdogTripped {
		return nil
	}
	return &watchdogError{reason: a.watchdogReason}
}

// ResetWatchdog clears a tripped enforce-mode watchdog, letting the
// agent accept new turns again. Typically wired to an operator slash
// command after the operator has reviewed why the watchdog tripped.
// Also resets the underlying watchdog's signal state so the next run
// of identical calls has to build back up to the threshold. Safe to
// call when nothing tripped — no-op in that case.
//
// Deliberately does NOT drop queued feedback (#159). A reset resumes a
// model whose context still ends in the loop it was halted for; the
// queued observation is the only thing that stops the first post-reset
// turn from re-issuing the same call. Clearing it here would make the
// reset undo the correction along with the halt.
func (a *Agent) ResetWatchdog() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.watchdogTripped = false
	a.watchdogReason = ""
	w := a.watchdog
	a.mu.Unlock()
	if w != nil {
		w.Reset()
	}
}

// WatchdogTripped reports whether the agent is currently blocking new
// turns because the enforce-mode watchdog fired. Exposed for /stats and
// similar surfaces so "why is the agent refusing my prompts?" has an
// obvious answer. Returns (true, reason) when blocked; (false, "")
// otherwise.
func (a *Agent) WatchdogTripped() (bool, string) {
	if a == nil {
		return false, ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.watchdogTripped, a.watchdogReason
}

// WatchdogMode reports the agent's configured watchdog posture as one
// of the config.Watchdog* strings: "off" (no watchdog wired), "warn"
// (observe and alert), "feedback" (observe, alert, and tell the model
// on its next turn), or "enforce" (all of that, and halt).
//
// Exposed because "is the backstop actually on?" is the question #642
// exists to answer, and the answer must be checkable from outside the
// package — by the startup summary, by operator surfaces, and by the
// wiring tests that keep a future refactor from quietly dropping the
// WithWatchdogEnforce option on a daemon's session-created agents.
func (a *Agent) WatchdogMode() string {
	if a == nil {
		return "off"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch {
	case a.watchdog == nil:
		return "off"
	case a.watchdogEnforce:
		return "enforce"
	case a.watchdogFeedback:
		return "feedback"
	default:
		return "warn"
	}
}

// serializeArgsForWatchdog produces a stable JSON serialization of
// args. Sorted map keys come for free with encoding/json on
// map[string]any (it sorts alphabetically). On marshal failure,
// returns a placeholder rather than skipping the observation —
// the watchdog needs *some* comparable string per call.
func serializeArgsForWatchdog(args map[string]any) string {
	if len(args) == 0 {
		return "{}"
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "<unmarshalable-args>"
	}
	return string(b)
}
