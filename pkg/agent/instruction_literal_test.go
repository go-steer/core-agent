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

package agent

import (
	"context"
	"strings"
	"testing"
)

// The system prompt is text, not a template (#1139).
//
// Layer 4 is an operator's AGENTS.md read off disk. Handed to ADK's
// Instruction field it was run through InjectSessionState on every
// request, and a `{word}` in prose — which is how a shell variable gets
// written in documentation — ended the turn before a token was sent.
//
// These assert the whole path, not the wiring: instructionSeenByModel
// runs a real turn and returns what the model received, so a regression
// shows up either as a failed Run (the fatal shapes) or as prose that
// silently changed on its way to the model (the quiet ones).

func TestInstructionWithBracesReachesTheModelVerbatim(t *testing.T) {
	t.Parallel()

	// Each of these is prose a contributor would reasonably write. The
	// first two are the live tokens from this repo's own AGENTS.md; the
	// rest span the shapes ADK's placeholder regex sorts differently,
	// because a fix that only neutralised the fatal ones would leave
	// the silent substitutions in place.
	for _, tc := range []struct {
		name string
		text string
	}{
		{"dollar braced shell var", "Scripts must quote it: `\"${CORE_AGENT}\" --provider=gemini -p \"hi\"`."},
		{"bare braced identifier", "The smoke scripts set ${SMOKE_CONFIG} in _common.sh."},
		{"lone identifier in braces", "A pattern like {word} must survive."},
		// `{x?}` is the optional form: no error, but ADK substitutes
		// the empty string, so a pre-fix run loses the token silently
		// rather than loudly. Worse to debug, same bug.
		{"optional placeholder", "Match on {word?} and keep going."},
		{"go composite literal", "Use agent.RunConfig{StreamingMode: agent.StreamingModeSSE}."},
		{"regex repetition", "Names must match [A-Za-z0-9_]{1,64} exactly."},
		{"brace expansion", "Run dev/ci/presubmits/{build,lint-go,vet}."},
		{"func literal", "The idiom is `func() *agent.Agent { return agentRef }`."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			instr := instructionSeenByModel(t, WithUserInstruction(tc.text))
			if !strings.Contains(instr, tc.text) {
				t.Errorf("instruction rewritten on the way to the model:\n want substring: %q\n got prompt:     %q", tc.text, instr)
			}
		})
	}
}

// A placeholder is not resolved even when the key it names could be
// resolved. The point of the provider is that the prompt is text —
// "resolves to the wrong thing" and "resolves to nothing" are the same
// defect, and only a positive assertion rules out a future change that
// re-enables templating for keys that happen to exist.
func TestInstructionPlaceholderIsNeverSubstituted(t *testing.T) {
	t.Parallel()

	const text = "Report the {app:name} and {user:id} verbatim."
	instr := instructionSeenByModel(t, WithUserInstruction(text))
	if !strings.Contains(instr, text) {
		t.Errorf("scoped placeholders were substituted:\n want: %q\n got:  %q", text, instr)
	}
}

// The fatal shape in every layer that carries caller text, not only
// layer 4. WithInstruction replaces layers 1–3 wholesale and
// WithExtraInstruction appends layer 5; both are strings a consumer
// supplies, and both rode the same template field.
func TestBracesSurviveInEveryCallerSuppliedLayer(t *testing.T) {
	t.Parallel()

	instr := instructionSeenByModel(t,
		WithInstruction("BARE-PROMPT with ${REPLACED_LAYER}"),
		WithUserInstruction("USER-BLOCK with ${USER_LAYER}"),
		WithExtraInstruction("EXTRA-BLOCK with ${EXTRA_LAYER}"),
	)
	for _, want := range []string{"${REPLACED_LAYER}", "${USER_LAYER}", "${EXTRA_LAYER}"} {
		if !strings.Contains(instr, want) {
			t.Errorf("layer lost %q on the way to the model: %q", want, instr)
		}
	}
}

// Assembly still works. literalInstruction moved which ADK field the
// prompt lands in, and the cheapest way to break that is to hand the
// provider something other than the assembled string.
func TestLiteralInstructionCarriesTheAssembledPrompt(t *testing.T) {
	t.Parallel()

	got, err := literalInstruction("ASSEMBLED")(nil)
	if err != nil {
		t.Fatalf("literalInstruction returned an error: %v", err)
	}
	if got != "ASSEMBLED" {
		t.Errorf("literalInstruction rewrote its input: got %q, want %q", got, "ASSEMBLED")
	}

	// And end to end, so a provider wired to the wrong string fails
	// here rather than in a live run.
	instr := instructionSeenByModel(t, WithUserInstruction("USER-MEMORY-BLOCK"))
	if !strings.Contains(instr, "USER-MEMORY-BLOCK") || !strings.Contains(instr, "execute concurrently") {
		t.Errorf("assembled layers did not reach the model through the provider: %q", instr)
	}
}

// No prompt is not the same as an empty prompt, and the two ADK fields
// disagree about that. Instruction's branch skips an empty string;
// InstructionProvider's does not, so wiring the provider unconditionally
// would hand the model an empty system part where a consumer previously
// got no system instruction at all. Reachable with WithInstruction("")
// — which replaces layers 1-3 wholesale — or a whitespace-only
// --system-prompt-file, which joinLayers trims away to the same thing.
// The Anthropic adapter drops empty parts on the floor; Gemini forwards
// them.
func TestEmptyInstructionSendsNoSystemPartAtAll(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		text string
	}{
		{"empty", ""},
		{"whitespace only", "   \n\t "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			llm := &captureLLM{response: "done."}
			a, err := New(llm, WithInstruction(tc.text))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			for range a.Run(context.Background(), "go") {
			}

			llm.mu.Lock()
			defer llm.mu.Unlock()
			if len(llm.reqs) == 0 {
				t.Fatal("no request reached the model")
			}
			for i, req := range llm.reqs {
				if req.Config == nil || req.Config.SystemInstruction == nil {
					continue
				}
				for j, p := range req.Config.SystemInstruction.Parts {
					if strings.TrimSpace(p.Text) == "" {
						t.Errorf("request %d carries an empty system part at index %d; an absent prompt must send no part", i, j)
					}
				}
			}
		})
	}
}

// The subtask agent builds its own llmagent and had the same exposure.
// Run one and assert the braced prose in its system prompt survived.
func TestSubtaskInstructionKeepsItsBraces(t *testing.T) {
	t.Parallel()

	const text = "Scan for `\"${CORE_AGENT}\"` invocations."
	llm := &captureLLM{response: "done."}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := a.RunSubtask(context.Background(), SubtaskSpec{
		Name:         "brace_subtask",
		SystemPrompt: text,
		UserMessage:  "go",
	}); err != nil {
		t.Fatalf("RunSubtask: %v", err)
	}

	llm.mu.Lock()
	defer llm.mu.Unlock()
	var found bool
	for _, req := range llm.reqs {
		if req.Config == nil || req.Config.SystemInstruction == nil {
			continue
		}
		for _, p := range req.Config.SystemInstruction.Parts {
			if strings.Contains(p.Text, text) {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("subtask system prompt lost %q", text)
	}
}
