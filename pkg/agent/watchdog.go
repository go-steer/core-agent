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
	"encoding/json"

	"google.golang.org/adk/session"

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

// observeToolCallsForWatchdog walks ev's content parts and feeds
// any function-call parts to the wired watchdog. Args are JSON-
// serialized so the watchdog's literal-string-compare detector
// has stable input — Go's map iteration order would otherwise
// make logically-identical calls compare unequal.
//
// Best-effort: if a part's args don't JSON-marshal cleanly we
// fall back to a recognizable placeholder; the alternative would
// be skipping the observation entirely, which silently weakens
// the signal. Better to compare on the placeholder than miss
// observations.
func (a *Agent) observeToolCallsForWatchdog(ev *session.Event) {
	if a.watchdog == nil || ev == nil || ev.Content == nil {
		return
	}
	for _, p := range ev.Content.Parts {
		if p == nil || p.FunctionCall == nil {
			continue
		}
		args := serializeArgsForWatchdog(p.FunctionCall.Args)
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
	if a.onWatchdogAlert == nil {
		return
	}
	for _, alert := range alerts {
		a.onWatchdogAlert(alert)
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
