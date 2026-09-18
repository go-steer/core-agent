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

// The tools-without-text signal (#655), the last of the designed
// detectors from docs/model-selection-design.md §"Piece 2".
//
// Every other detector in this package asks a question ABOUT the
// calls: is this one identical to the last (repeat), does the recent
// window have a period (cycle), does one name dominate it (dominance),
// is it the same name over and over (toolname), did they all fail
// (failure), did the tools say they did nothing (no-op), is the data
// coming back data we already had (stall). Each of those is a
// comparison, and each has an evasion, which is why there are seven of
// them: a model that reworded its arguments beat the first four, a
// model whose calls all succeeded beat the fifth, a tool that never
// opted into no_op beat the sixth, and a model whose answers genuinely
// differ beats the seventh.
//
// This one compares nothing. It counts how many tool calls went by
// without the model saying a single word, and an agent can only evade
// it by talking — which is the behavior we wanted.
//
// That makes it the weakest evidence in the package and the hardest to
// escape at the same time, and both halves are load-bearing. Weakest,
// because a count with no comparison in it cannot tell a runaway from
// a deep but legitimate sweep; hardest to escape, because silence is
// the one property every recorded runaway in this repo's issue history
// shares. #144's read_file loop, #649's alternation, #702's dominance,
// #905's thirteen mark_task_done calls — all of them ground away
// without a word of assistant text, and none of the four had to.
//
// WHAT COUNTS AS TEXT. Anything the model itself wrote that is not
// whitespace. Not a tool result, which is the runtime talking; not an
// injected inbox message or a watchdog feedback block, which is us
// talking; and — a decision, not an omission — not the model's private
// reasoning, where the provider distinguishes it. The question this
// signal asks is "has anyone been told anything", and a reasoning trace
// nobody is shown is not an answer to it. The agent tap decides what
// reaches here; see observeAssistantTextForWatchdog.
//
// A single non-empty sentence clears the run completely. That is
// deliberate and it is not a loophole: a model that narrates what it is
// doing between sweeps is doing the thing this signal wants, and one
// that narrates its way through a genuine loop is caught by the six
// detectors that read the calls. Nothing here is trying to be the only
// detector; it is trying to be the one with a different blind spot.

package watchdog

import (
	"fmt"
	"strings"
)

// DefaultToolsWithoutText is the number of consecutive tool calls with
// no intervening assistant text that trips ToolsWithoutTextSignal.
//
// Twelve, and unlike most thresholds in this package that number was
// measured rather than argued. The design doc guessed 15 in a table of
// "initial guesses"; sixty-four archived GKE-drill sessions — real runs
// against a live cluster, all of them judged good at the time — put the
// longest textless run at 10, the 95th percentile at 5 and the median
// at 2. Twelve clears the observed ceiling by a fifth and sits under
// #905's recorded loop, which ran fifteen calls without a word.
//
// The corpus is committed (testdata/corpus) and the margin is asserted,
// not remembered: TestDefaultToolsWithoutTextClearsTheRecordedCorpus
// fails if a future extraction contains a good session that would trip
// this. A threshold whose justification is a sentence in a comment
// rots; one whose justification is a test does not.
//
// What the corpus cannot say is how a coding agent behaves — every run
// in it is Kubernetes triage, which is the workload this project aims
// at but not the only one it runs. A twelve-call silent sweep through a
// codebase is plausible in a way a twelve-call silent sweep through a
// cluster is not, and an operator who sees this fire on one should
// raise the threshold rather than treat it as a finding. That it costs
// only a log line is the reason the tighter number is affordable; see
// the Severity paragraph on the type.
const DefaultToolsWithoutText = 12

// AssistantTextObserver is the optional half of Watchdog for the model's
// own output: an implementation that wants to see what the agent SAID,
// as opposed to what it called, implements it, and the agent feeds text
// through a type assertion.
//
// Optional for the reason ToolResultObserver and TurnObserver are.
// Watchdog is documented as a plug-in point, so widening it would break
// every third-party watchdog at a minor version to add one signal, and
// a custom watchdog that only counts calls stays perfectly valid.
type AssistantTextObserver interface {
	ObserveAssistantText(string)
}

// SignalTextObserver is that extension one level down, for signals
// inside DefaultWatchdog. A Signal that doesn't implement it never sees
// assistant text.
type SignalTextObserver interface {
	ObserveAssistantText(string) *Alert
}

// ObserveAssistantText fans one piece of model output across every
// wired signal that implements SignalTextObserver. Implements
// AssistantTextObserver.
func (w *DefaultWatchdog) ObserveAssistantText(text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, s := range w.signals {
		to, ok := s.(SignalTextObserver)
		if !ok {
			continue
		}
		if alert := to.ObserveAssistantText(text); alert != nil {
			w.alerts = append(w.alerts, *alert)
		}
	}
}

// ToolsWithoutTextSignal trips when Threshold tool calls go by with no
// assistant text between them. Any non-whitespace text from the model
// resets the run.
//
// Severity is Warn, and this is the least close call in the package.
// Under --watchdog=enforce a Critical alert stops something, and the
// only thing this signal knows is that a number got large — it has not
// compared two calls, read a result, or been told anything by a tool.
// A deep sweep and a runaway look identical to it by construction. So
// it goes where weak evidence belongs: the operator log, and under
// --watchdog=feedback the model's own next turn, where the note to
// "say what you have found" costs a working agent one paragraph and
// gives a stuck one the only nudge that has ever stopped one of these
// without a human. The loop detectors that CAN prove redundancy are
// the ones that halt.
//
// Scope is left at its zero value because scope is only read for a
// Critical alert (see AlertScope); there is no turn-versus-session
// judgement to make about an alert that never stops anything.
//
// Not turn-scoped, for the reason every streak here is not: an ordinary
// turn ends with the model answering, so the run clears itself at the
// boundary without anyone having to clear it. A turn that ended without
// the model saying anything at all is the shape worth carrying into the
// next one — the person waiting for an answer did not get one, and the
// next turn starting does not change that.
//
// Parallel tool calls count individually. Twelve calls issued at once
// with nothing said about any of them is the same silence as twelve
// issued in sequence, which is the same reading NoOpStreakSignal takes.
type ToolsWithoutTextSignal struct {
	Threshold int

	run     int
	names   []string
	tripped bool // one alert per run, not one per call past the threshold
}

// NewToolsWithoutTextSignal constructs the signal. Threshold below 2 is
// clamped to 2: at 1 every tool call an agent ever makes would alert,
// since a call necessarily precedes the text that reports on it.
func NewToolsWithoutTextSignal(threshold int) *ToolsWithoutTextSignal {
	if threshold < 2 {
		threshold = 2
	}
	return &ToolsWithoutTextSignal{Threshold: threshold}
}

// Name implements Signal.
func (s *ToolsWithoutTextSignal) Name() string { return "tools-without-text" }

// Reset implements Signal.
func (s *ToolsWithoutTextSignal) Reset() {
	s.run = 0
	s.names = nil
	s.tripped = false
}

// ObserveToolCall implements Signal.
func (s *ToolsWithoutTextSignal) ObserveToolCall(tc ToolCall) *Alert {
	s.run++
	// Bound the name list at the threshold: past that it is the same
	// story with more words, and Reason/Guidance are prompt text.
	if len(s.names) < s.Threshold {
		s.names = append(s.names, tc.Name)
	}
	if s.run < s.Threshold || s.tripped {
		return nil
	}
	s.tripped = true
	tools := distinctNames(s.names)
	return &Alert{
		Signal:   s.Name(),
		Severity: SeverityWarn,
		Reason: fmt.Sprintf(
			"%d tool calls in a row (%s) with no assistant text in between. Nothing is being reported to anyone: whatever the agent has learned from those %d calls, it has not said it. This is the shape every recorded runaway in this project has had, and also the shape of a long legitimate sweep — the signal cannot tell them apart, which is why it only warns.",
			s.run, tools, s.run,
		),
		Guidance: fmt.Sprintf(
			"You have made %d tool calls (%s) without saying anything. If you are working through something long, say where you are and what you have found so far — the person you are working for cannot see your tool calls. If you are not converging, say that instead: name what you were trying to establish, what you have actually observed, and what you would need in order to finish. Do not simply continue in silence.",
			s.run, tools,
		),
	}
}

// ObserveAssistantText implements SignalTextObserver. Never alerts —
// text is what clears this signal, not what trips it.
func (s *ToolsWithoutTextSignal) ObserveAssistantText(text string) *Alert {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	s.Reset()
	return nil
}
