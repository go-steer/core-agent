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
	"context"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/runner"
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
