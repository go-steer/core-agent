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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/runner"
	"github.com/go-steer/core-agent/v3/pkg/transcript"
)

func TestStartupSteps_CanonicalOrderValidates(t *testing.T) {
	t.Parallel()
	if err := validateStartupStepOrder(startupSteps); err != nil {
		t.Fatalf("validateStartupStepOrder(startupSteps): %v", err)
	}
	seen := make(map[string]bool, len(startupSteps))
	for i, st := range startupSteps {
		if st.name == "" {
			t.Errorf("startupSteps[%d] has empty name", i)
		}
		if st.run == nil {
			t.Errorf("startupSteps[%d] (%s) has nil run func", i, st.name)
		}
		if seen[st.name] {
			t.Errorf("duplicate startup step name %q", st.name)
		}
		seen[st.name] = true
	}
}

func TestStartupSteps_TransposingOrderDependentStepsFails(t *testing.T) {
	t.Parallel()

	// 1. Swapping any step with one of its declared prerequisites must fail
	// both validateStartupStepOrder and runStartupSteps.
	stepIndex := make(map[string]int, len(startupSteps))
	for i, st := range startupSteps {
		stepIndex[st.name] = i
	}
	for i, st := range startupSteps {
		for _, req := range st.requires {
			reqIdx, ok := stepIndex[req]
			if !ok {
				t.Fatalf("step %q requires unknown step %q", st.name, req)
			}
			transposed := slices.Clone(startupSteps)
			transposed[reqIdx], transposed[i] = transposed[i], transposed[reqIdx]
			if err := validateStartupStepOrder(transposed); err == nil {
				t.Errorf("transposing %q (idx %d) before %q (idx %d) unexpectedly passed validateStartupStepOrder", st.name, i, req, reqIdx)
			}
			state := newRunState(context.Background(), func() {}, runOptions{
				prompt: promptOpts{quiet: true},
			})
			if code := runStartupSteps(state, transposed); code != runner.ExitConfigError {
				t.Errorf("runStartupSteps with %q transposed before %q returned %d, want ExitConfigError (%d)", st.name, req, code, runner.ExitConfigError)
			}
		}
	}

	// 2. Directly invoking order-dependent step functions out of order on a
	// runState must also fail via each step's runtime state precondition latch.
	cases := []struct {
		name string
		fn   func(*runState) int
	}{
		{"load-config-before-validate-flags", stepLoadConfigAndWorkspaceTrust},
		{"apply-cli-before-load-config", stepApplyCLIAndTaskProfileOverrides},
		{"setup-telemetry-before-load-config", stepSetupTelemetryAndMetrics},
		{"resolve-model-before-task-profile", stepResolveModelAndProvider},
		{"build-gate-before-plan-mode-and-model", stepBuildPermissionGate},
		{"load-instructions-before-withhold-credentials", stepLoadInstructionsAndCheckSmallTier},
		{"build-mcp-before-telemetry-and-instructions", stepRegisterCoreMetricsAndBuildMCP},
		{"load-skills-before-gate-and-mcp", stepLoadSkillsAndStartupSummary},
		{"build-tools-before-load-skills", stepBuildToolCatalog},
		{"setup-pricing-before-build-tools", stepSetupPricingAndSubagentTools},
		{"build-subagents-before-pricing", stepBuildDeclarativeSubagentsAndPromptLayers},
		{"assemble-agent-before-subagents", stepAssembleAgentAndEventlog},
		{"start-attach-before-eventlog", stepStartAttachAndPeerHub},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newRunState(context.Background(), func() {}, runOptions{
				prompt: promptOpts{quiet: true},
			})
			s.cfg = config.DefaultConfig()
			if code := tc.fn(s); code != runner.ExitConfigError {
				t.Errorf("%s: got exit code %d, want ExitConfigError (%d)", tc.name, code, runner.ExitConfigError)
			}
		})
	}
}

func TestStartupSteps_IndividualStepsWithoutFullBinaryBoot(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir .agents: %v", err)
	}
	cfgPath := filepath.Join(agentsDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"model":{"provider":"echo","name":"echo-test"}}`), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newRunState(ctx, cancel, runOptions{
		driver: driverOpts{
			prompt: "hello",
			yolo:   true,
		},
		workspace: workspaceOpts{
			cfgPath:       cfgPath,
			agentsDirFlag: agentsDir,
		},
		model: modelOpts{
			taskClass:        "implement",
			noPricingRefresh: true,
		},
		toolCatalog: toolCatalogOpts{
			noBackgroundAgents: true,
			noMCPDigest:        true,
		},
		guardrails: guardrailOpts{
			trust: true,
		},
		prompt: promptOpts{
			quiet: true,
		},
	})
	s.flagSet = flag.NewFlagSet("test", flag.ContinueOnError)
	defer s.close()

	// Run the first 7 steps individually without booting the full binary.
	for _, st := range startupSteps[:7] {
		if code := st.run(s); code != runner.ExitOK {
			t.Fatalf("step %q failed with code %d", st.name, code)
		}
	}
	if !s.flagsValidated || !s.configLoaded || !s.credentialsWithheld || !s.taskProfileResolved || !s.planModeApplied || !s.modelResolved || !s.gateBuilt || !s.instructionsLoaded {
		t.Errorf("expected all step latches through load-instructions to be set, got %+v", s)
	}
	if s.model == nil || s.gate == nil || s.template == nil {
		t.Errorf("expected model, gate, and template to be constructed")
	}

	// Verify LIFO cleanup ordering on runState.close().
	var order []int
	s2 := newRunState(context.Background(), func() {}, runOptions{})
	s2.addCleanup(func() { order = append(order, 1) })
	s2.addCleanup(func() { order = append(order, 2) })
	s2.addCleanup(func() { order = append(order, 3) })
	s2.close()
	if !slices.Equal(order, []int{3, 2, 1}) {
		t.Errorf("cleanup order = %v, want [3 2 1]", order)
	}
}

func TestHeadlessOneShot_PersistsAnswerAndSurfacesSaveError(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	agentsDir := filepath.Join(tmp, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatalf("mkdir .agents: %v", err)
	}
	cfgPath := filepath.Join(agentsDir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"model":{"provider":"echo","name":"echo-test"}}`), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newRunState(ctx, cancel, runOptions{
		driver: driverOpts{
			prompt: "what is the status?",
			color:  "never",
			yolo:   true,
		},
		workspace: workspaceOpts{
			cfgPath:       cfgPath,
			agentsDirFlag: agentsDir,
		},
		model: modelOpts{
			noPricingRefresh: true,
		},
		toolCatalog: toolCatalogOpts{
			noBuiltinTools:     true,
			noBackgroundAgents: true,
			noMCPDigest:        true,
		},
		guardrails: guardrailOpts{
			trust: true,
		},
		prompt: promptOpts{
			quiet: true,
		},
	})
	s.flagSet = flag.NewFlagSet("test", flag.ContinueOnError)
	defer s.close()

	if code := runStartupSteps(s, startupSteps); code != runner.ExitOK {
		t.Fatalf("runStartupSteps exited with %d, want %d", code, runner.ExitOK)
	}
	if code := s.executeDriver(); code != runner.ExitOK {
		t.Fatalf("executeDriver exited with %d, want %d", code, runner.ExitOK)
	}

	sessDir := filepath.Join(agentsDir, transcript.SessionsDirName)
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d session transcript files, want 1", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(sessDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read transcript file: %v", err)
	}
	var tr transcript.Transcript
	if err := json.Unmarshal(raw, &tr); err != nil {
		t.Fatalf("unmarshal transcript: %v\n%s", err, raw)
	}
	if len(tr.Messages) < 2 {
		t.Fatalf("saved transcript has %d message(s) %+v; want both user prompt and assistant answer (#1553)", len(tr.Messages), tr.Messages)
	}
	if tr.Messages[0].Role != "user" || tr.Messages[0].Text != "what is the status?" {
		t.Errorf("messages[0] = %+v, want user prompt", tr.Messages[0])
	}
	if tr.Messages[len(tr.Messages)-1].Role != "assistant" || tr.Messages[len(tr.Messages)-1].Text != "what is the status?" {
		t.Errorf("final message = %+v, want assistant answer %q", tr.Messages[len(tr.Messages)-1], "what is the status?")
	}

	// Verify a save failure is surfaced on stderr as `core-agent: ...`.
	badAgentsDir := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(badAgentsDir, []byte("file"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	var stderr bytes.Buffer
	persistTranscript(badAgentsDir, "echo-test", "prompt", []transcript.Message{{Role: "assistant", Text: "answer"}}, s.tracker, &stderr)
	if !strings.Contains(stderr.String(), "core-agent: save transcript:") {
		t.Errorf("stderr = %q, want `core-agent: save transcript: ...` error line (#1553)", stderr.String())
	}
}
