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

package autonomous

import (
	"context"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
)

const (
	rca  = "root cause: the deployment references an image tag that was never pushed"
	idle = "standing by in a healthy, inactive state"
)

// The bug, in one test: a run whose substantive work happened on turn 1
// must not return turn 2's idle narration.
//
// FinalText is the fallback return value everywhere DoneDetail is
// absent — a budget cap, a watchdog halt, a provider failure — so
// last-wins meant the parent reliably received the worst thing the
// subagent said. This is the 2026-08-13 GKE UAT verbatim: RCA early,
// "standing by in a healthy, inactive state" at return time.
func TestRunAutonomous_FinalTextKeepsTheSubstantiveTurn(t *testing.T) {
	t.Parallel()
	var toolCalls atomic.Int32
	llm := &stubLLM{scenarios: []scenarioFn{
		spinCallTurn(1),      // turn 1: looks at the world...
		textTurn(rca, 5, 5),  // ...and reports what it found
		textTurn(idle, 5, 5), // turn 2: nothing left to do
	}}
	res, err := Run(context.Background(),
		buildSpinAgent(llm, "final-text-substantive", &toolCalls),
		"go",
		WithMaxTurns(2))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Turns != 2 {
		t.Fatalf("Turns = %d, want 2 — the idle turn has to actually run for this to prove anything", res.Turns)
	}
	if res.FinalText != rca {
		t.Errorf("FinalText = %q, want the substantive turn's text %q", res.FinalText, rca)
	}
}

// A later substantive turn still wins. The rule is "the last turn that
// did something", not "the first" — a standing worker that finds a new
// problem on turn 3 must return that, not its turn-1 findings.
func TestRunAutonomous_FinalTextTakesTheLatestSubstantiveTurn(t *testing.T) {
	t.Parallel()
	var toolCalls atomic.Int32
	llm := &stubLLM{scenarios: []scenarioFn{
		spinCallTurn(1),
		textTurn("first finding", 5, 5),
		textTurn(idle, 5, 5),
		spinCallTurn(1),
		textTurn("second finding", 5, 5),
	}}
	res, err := Run(context.Background(),
		buildSpinAgent(llm, "final-text-latest", &toolCalls),
		"go",
		WithMaxTurns(3))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != "second finding" {
		t.Errorf("FinalText = %q, want %q", res.FinalText, "second finding")
	}
}

// A run that never uses a tool keeps last-wins. For a pure-reasoning
// loop the newest text really is the best one — iterative refinement,
// not idle narration — and freezing on turn 1 forever would be a worse
// bug than the one being fixed.
func TestRunAutonomous_FinalTextToollessRunKeepsLastWins(t *testing.T) {
	t.Parallel()
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("draft", 5, 5),
		textTurn("revised draft", 5, 5),
	}}
	res, err := Run(context.Background(),
		buildAgent(llm, "final-text-toolless"),
		"go",
		WithMaxTurns(2))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != "revised draft" {
		t.Errorf("FinalText = %q, want %q — a tool-less run has nothing better to hold on to", res.FinalText, "revised draft")
	}
}

// The rule survives a restart. Resume re-seeds its totals from the
// checkpoint, so without the persisted flag it would re-derive "nothing
// substantive yet" and let the first idle turn after the restart
// overwrite the carried-forward findings — #731 with an extra step.
func TestResumeAutonomous_FinalTextSurvivesTheRestart(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestEventLog(t)
	defer cleanup()

	var toolCalls atomic.Int32
	build := func(extras []tool.Tool) (*agent.Agent, error) {
		return spinAgentWithLog(&stubLLM{scenarios: []scenarioFn{
			spinCallTurn(1),
			textTurn(rca, 1, 1),
		}}, h, "app", "u", "restart", extras, &toolCalls)
	}
	first, err := Run(context.Background(), build, "go", WithMaxTurns(1))
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if first.FinalText != rca {
		t.Fatalf("first FinalText = %q, want %q", first.FinalText, rca)
	}

	llm2 := &stubLLM{scenarios: []scenarioFn{textTurn(idle, 1, 1)}}
	res, err := Resume(context.Background(),
		func(extras []tool.Tool, sessionID string) (*agent.Agent, error) {
			return spinAgentWithLog(llm2, h, "app", "u", sessionID, extras, &toolCalls)
		},
		SessionRef{Handle: h, AppName: "app", UserID: "u", SessionID: "restart"},
		WithMaxTurns(2))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.FinalText != rca {
		t.Errorf("FinalText = %q, want the pre-restart findings %q — the idle turn clobbered them", res.FinalText, rca)
	}
}

// The driver's own tools don't make a turn substantive. A scheduled
// worker calls schedule_next_turn on every idle cycle, so counting it
// as work would mark every turn substantive and undo the fix for
// exactly the population that still runs the re-drive loop.
func TestRunAutonomous_FinalTextIgnoresTheDriversOwnTools(t *testing.T) {
	t.Parallel()
	var toolCalls atomic.Int32
	llm := &stubLLM{scenarios: []scenarioFn{
		spinCallTurn(1),     // turn 1: real work...
		textTurn(rca, 1, 1), // ...with the findings
		// Turn 2: nothing to do, so it books the next cycle and says so.
		scheduleCallTurn(1, "rescan", "cadence"),
		textTurn(idle, 1, 1),
	}}
	res, err := Run(context.Background(),
		buildSpinAgent(llm, "final-text-driver-tools", &toolCalls),
		"monitor",
		WithScheduler(&recordingScheduler{}),
		WithMaxTurns(2))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FinalText != rca {
		t.Errorf("FinalText = %q, want %q — schedule_next_turn is bookkeeping, not work", res.FinalText, rca)
	}
}

// spinAgentWithLog is buildSpinAgent's durable twin: the same no-op
// "spin" tool, plus the event log Resume reads its checkpoints from.
func spinAgentWithLog(llm *stubLLM, h *eventlog.Handle, app, user, sess string, extras []tool.Tool, calls *atomic.Int32) (*agent.Agent, error) {
	type empty struct{}
	spin, err := functiontool.New(
		functiontool.Config{Name: "spin", Description: "no-op that keeps the turn going"},
		func(_ tool.Context, _ empty) (empty, error) {
			calls.Add(1)
			return empty{}, nil
		})
	if err != nil {
		return nil, err
	}
	return agent.New(llm,
		agent.WithAppName(app),
		agent.WithSession(user, sess),
		agent.WithEventLog(h),
		agent.WithTools(append(append([]tool.Tool(nil), extras...), spin)),
		agent.WithInstruction("test agent"),
	)
}

// keepFinalText's three rules, stated once. The call sites in Run and
// Resume are identical, so pinning the predicate directly is cheaper
// than scripting a scenario per case.
func TestKeepFinalText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		text            string
		usedTools       bool
		haveSubstantive bool
		want            bool
	}{
		{"empty text never replaces anything", "", true, false, false},
		{"a tool-using turn always wins", "found it", true, true, true},
		{"the first text wins when nothing is held", "hello", false, false, true},
		{"an idle turn cannot displace a substantive one", idle, false, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := keepFinalText(tc.text, tc.usedTools, tc.haveSubstantive); got != tc.want {
				t.Errorf("keepFinalText(%q, %v, %v) = %v, want %v", tc.text, tc.usedTools, tc.haveSubstantive, got, tc.want)
			}
		})
	}
}

// The flag has to survive the map round-trip ADK's CustomMetadata
// forces on it, or Resume reads back false and the restart case above
// regresses silently.
func TestCheckpointPayload_FinalTextSubstantiveRoundTrip(t *testing.T) {
	t.Parallel()
	for _, want := range []bool{true, false} {
		got := checkpointFromMap(checkpointPayload{
			FinalText:            rca,
			FinalTextSubstantive: want,
		}.toMap())
		if got.FinalTextSubstantive != want {
			t.Errorf("FinalTextSubstantive round-tripped to %v, want %v", got.FinalTextSubstantive, want)
		}
	}
	// A pre-v2.9 checkpoint has no such key at all. It must decode to
	// false — the permissive direction, matching the old behavior.
	if checkpointFromMap(map[string]any{"final_text": rca}).FinalTextSubstantive {
		t.Error("a checkpoint written before the field existed decoded as substantive")
	}
}
