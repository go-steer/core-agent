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

// instructionSeenByModel runs one turn against a recordingLLM and
// returns the system instruction the model actually received — the
// end-to-end observable for layer assembly (#459).
func instructionSeenByModel(t *testing.T, opts ...Option) string {
	t.Helper()
	rec := &recordingLLM{}
	a, err := New(rec, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, err := range a.Run(context.Background(), "hi") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) == 0 {
		t.Fatal("model never called")
	}
	cfg := rec.requests[0].Config
	if cfg == nil || cfg.SystemInstruction == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range cfg.SystemInstruction.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

func TestNew_LayerAssemblyReachesModel(t *testing.T) {
	t.Parallel()

	// Defaults: core + interactive; NOT the Gemini quirk (recordingLLM
	// names itself "recording") and NOT the autonomous overlay.
	instr := instructionSeenByModel(t)
	if !strings.Contains(instr, "execute concurrently") || !strings.Contains(instr, "A user is present") {
		t.Errorf("default assembly missing core/interactive: %q", instr)
	}
	if strings.Contains(instr, "do not execute them one by one") {
		t.Error("Gemini quirk applied to a non-Gemini model")
	}
	if strings.Contains(instr, "operating autonomously") {
		t.Error("autonomous overlay applied in default (interactive) mode")
	}

	// Autonomous mode + user layer + extras land in order, after core.
	instr = instructionSeenByModel(t,
		WithMode(ModeAutonomous),
		WithUserInstruction("USER-MEMORY-BLOCK"),
		WithExtraInstruction("EXTRA-BLOCK-1"),
		WithExtraInstruction("EXTRA-BLOCK-2"),
	)
	for _, want := range []string{"operating autonomously", "USER-MEMORY-BLOCK", "EXTRA-BLOCK-1", "EXTRA-BLOCK-2"} {
		if !strings.Contains(instr, want) {
			t.Errorf("autonomous assembly missing %q", want)
		}
	}
	if strings.Contains(instr, "A user is present") {
		t.Error("interactive overlay leaked into autonomous mode")
	}
	core := strings.Index(instr, "execute concurrently")
	user := strings.Index(instr, "USER-MEMORY-BLOCK")
	e1 := strings.Index(instr, "EXTRA-BLOCK-1")
	e2 := strings.Index(instr, "EXTRA-BLOCK-2")
	if core >= user || user >= e1 || e1 >= e2 {
		t.Errorf("layer order wrong: core@%d user@%d e1@%d e2@%d — must be stable→volatile", core, user, e1, e2)
	}

	// Full replace skips layers 1–3; layers 4–5 still append.
	instr = instructionSeenByModel(t,
		WithInstruction("BARE-PROMPT"),
		WithUserInstruction("USER-MEMORY-BLOCK"),
		WithExtraInstruction("EXTRA-BLOCK-1"),
	)
	if strings.Contains(instr, "execute in parallel") || strings.Contains(instr, "A user is present") {
		t.Error("WithInstruction must skip layers 1–3 entirely")
	}
	for _, want := range []string{"BARE-PROMPT", "USER-MEMORY-BLOCK", "EXTRA-BLOCK-1"} {
		if !strings.Contains(instr, want) {
			t.Errorf("full-replace assembly missing %q", want)
		}
	}

	// Deprecated prefix keeps legacy shape: prefix BEFORE the alias.
	instr = instructionSeenByModel(t, WithSystemInstructionPrefix("LEGACY-PREFIX"))
	if !strings.HasPrefix(instr, "LEGACY-PREFIX") {
		t.Errorf("prefix option lost its prepend semantics: %q", instr[:60])
	}
	if !strings.Contains(instr, "A user is present") {
		t.Error("prefix path lost the DefaultInstruction alias body")
	}
}

func TestNew_ModeAccessor(t *testing.T) {
	t.Parallel()
	rec := &recordingLLM{}
	a, err := New(rec, WithMode(ModeAutonomous))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Mode() != ModeAutonomous {
		t.Error("Mode() did not report the WithMode value")
	}
	var nilAgent *Agent
	if nilAgent.Mode() != ModeInteractive {
		t.Error("nil agent Mode() should default to ModeInteractive")
	}
}
