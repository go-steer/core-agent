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

// watchdogError is returned by Agent.Run when a prior turn tripped the
// watchdog under enforce mode and the operator hasn't reset it. Mirrors
// costCeilingError: a distinct type so hosts can classify "operator must
// reset" apart from retryable failures.
type watchdogError struct {
	reason string
}

func (e *watchdogError) Error() string { return e.reason }

// IsWatchdogTripped reports whether err was returned by Run because a
// prior turn tripped the behavioral watchdog under --watchdog=enforce.
// Hosts use this to distinguish "operator must reset the watchdog" from
// other Run errors that may warrant retry.
func IsWatchdogTripped(err error) bool {
	_, ok := err.(*watchdogError)
	return ok
}

// observeToolCallsForWatchdog walks ev's content parts and feeds
// any function-call parts to the wired watchdog. Args are JSON-
// serialized so the watchdog's literal-string-compare detector
// has stable input — Go's map iteration order would otherwise
// make logically-identical calls compare unequal.
//
// seen is the per-turn dedup set (#363): ADK's streaming aggregator
// can re-emit the same FunctionCall part across more than one event
// (an intermediate aggregate plus the final — the same duplication
// runner/events.go dedups for display). Calls carrying an ID dedup
// on it (a re-emitted part keeps its ID; a legitimate parallel call
// with identical args gets a fresh one); ID-less calls fall back to
// name+args, which also collapses same-args parallel calls within
// ONE turn — acceptable, since the watchdog's runaway signal is
// repetition ACROSS turns and the set resets each turn.
//
// Best-effort: if a part's args don't JSON-marshal cleanly we
// fall back to a recognizable placeholder; the alternative would
// be skipping the observation entirely, which silently weakens
// the signal. Better to compare on the placeholder than miss
// observations.
func (a *Agent) observeToolCallsForWatchdog(ev *session.Event, seen map[string]struct{}) {
	if a.watchdog == nil || ev == nil || ev.Content == nil {
		return
	}
	for _, p := range ev.Content.Parts {
		if p == nil || p.FunctionCall == nil {
			continue
		}
		args := serializeArgsForWatchdog(p.FunctionCall.Args)
		key := p.FunctionCall.ID
		if key == "" {
			key = p.FunctionCall.Name + "\x00" + args
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		a.watchdog.ObserveToolCall(watchdog.ToolCall{
			Name: p.FunctionCall.Name,
			Args: args,
		})
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
	// Enforce mode (#623): a Critical alert halts the agent. Kept after
	// the callback so the operator sees the log line before the refusal,
	// and outside the onWatchdogAlert==nil guard above so enforcement
	// fires even when no warn-mode callback is wired.
	if a.watchdogEnforce {
		a.maybeTripWatchdog(alerts)
	}
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

	a.emit(attach.EventTurnError, attach.TurnError{
		Kind:      attach.TurnErrorWatchdog,
		Code:      "watchdog",
		Message:   reason,
		Retryable: false, // operator must reset, not the host
	})
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
// (observe and alert), or "enforce" (observe, alert, and halt).
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
