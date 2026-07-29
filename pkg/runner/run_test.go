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

package runner

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// TestRun_ModelRequired pins the options validation: Run without a
// Model must fail fast with ExitConfigError rather than panicking in
// agent construction or mid-turn.
func TestRun_ModelRequired(t *testing.T) {
	t.Parallel()
	code, err := Run(context.Background(), RunOptions{})
	if err == nil || code != ExitConfigError {
		t.Fatalf("Run(no model) = (%d, %v), want (ExitConfigError, error)", code, err)
	}
}

// TestRun_ConstructsAgentAndRunsSeededTurn drives the Agent-nil path
// end to end against the echo mock: Run must construct the agent
// from Model+AgentOptions and run a seeded turn, proving the options
// struct reaches the same replCore the deprecated variants used.
// (The nil→os.Std* I/O defaulting is deliberately untested — it
// would capture the process's real stdin.)
func TestRun_ConstructsAgentAndRunsSeededTurn(t *testing.T) {
	t.Parallel()
	m, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock model: %v", err)
	}
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("/exit\n")
	code, err := Run(context.Background(), RunOptions{
		Model:         m,
		InitialPrompt: "hello-seed",
		Stdin:         stdin,
		Stdout:        &stdout,
		Stderr:        &stderr,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != ExitOK {
		t.Fatalf("exit code = %d, want ExitOK", code)
	}
	if !strings.Contains(stdout.String(), "hello-seed") {
		t.Errorf("stdout %q missing the seeded prompt echo", stdout.String())
	}
}

// TestRun_DerivesModelFromAgent pins the #510 ergonomic: a pre-built
// Agent is enough — Run derives the streaming model via
// Agent.Model() instead of demanding the mismatch-prone pair.
func TestRun_DerivesModelFromAgent(t *testing.T) {
	t.Parallel()
	m, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock model: %v", err)
	}
	a, err := agent.New(m)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code, err := Run(context.Background(), RunOptions{
		Agent:         a, // no Model — derived
		InitialPrompt: "derive-me",
		Stdin:         strings.NewReader("/exit\n"),
		Stdout:        &stdout,
		Stderr:        &stderr,
	})
	if err != nil || code != ExitOK {
		t.Fatalf("Run = (%d, %v), want (ExitOK, nil)", code, err)
	}
	if !strings.Contains(stdout.String(), "derive-me") {
		t.Errorf("stdout %q missing the seeded turn", stdout.String())
	}
}
