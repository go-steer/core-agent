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

package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/agent"
	"github.com/go-steer/core-agent/config"
	"github.com/go-steer/core-agent/models/mock"
)

func TestParseSubagentArgs_GoalOnly(t *testing.T) {
	t.Parallel()
	spec, err := parseSubagentArgs("monitor the deploy pipeline for errors")
	if err != nil {
		t.Fatalf("parseSubagentArgs: %v", err)
	}
	if spec.Goal != "monitor the deploy pipeline for errors" {
		t.Errorf("Goal = %q, want full string", spec.Goal)
	}
	if spec.Name != "" {
		t.Errorf("Name should be empty (filled by caller), got %q", spec.Name)
	}
	if spec.SystemPrompt != "" {
		t.Errorf("SystemPrompt should be empty (filled by caller), got %q", spec.SystemPrompt)
	}
}

func TestParseSubagentArgs_AllFlagsThenGoal(t *testing.T) {
	t.Parallel()
	spec, err := parseSubagentArgs("--name=research --prompt=\"You are a researcher.\" --tools=read_file,grep --extras=kubectl_get --max-turns=20 --max-cost=0.50 --max-wallclock=10m --scheduler=sleep find every TODO in the repo and tally")
	if err != nil {
		t.Fatalf("parseSubagentArgs: %v", err)
	}
	if spec.Name != "research" {
		t.Errorf("Name = %q, want research", spec.Name)
	}
	if !strings.Contains(spec.SystemPrompt, "researcher") {
		t.Errorf("SystemPrompt = %q, want researcher mention", spec.SystemPrompt)
	}
	if len(spec.Tools) != 2 || spec.Tools[0] != "read_file" || spec.Tools[1] != "grep" {
		t.Errorf("Tools = %#v, want [read_file grep]", spec.Tools)
	}
	if len(spec.Extras) != 1 || spec.Extras[0] != "kubectl_get" {
		t.Errorf("Extras = %#v, want [kubectl_get]", spec.Extras)
	}
	if spec.Budgets.MaxTurns != 20 {
		t.Errorf("MaxTurns = %d, want 20", spec.Budgets.MaxTurns)
	}
	if spec.Budgets.MaxCost != 0.50 {
		t.Errorf("MaxCost = %v, want 0.50", spec.Budgets.MaxCost)
	}
	if spec.Budgets.MaxWallclock != 10*time.Minute {
		t.Errorf("MaxWallclock = %v, want 10m", spec.Budgets.MaxWallclock)
	}
	if spec.Scheduler != "sleep" {
		t.Errorf("Scheduler = %q, want sleep", spec.Scheduler)
	}
	if !strings.Contains(spec.Goal, "find every TODO") {
		t.Errorf("Goal = %q, want it to start with the goal text", spec.Goal)
	}
}

func TestParseSubagentArgs_SpaceSeparatedFlagValue(t *testing.T) {
	t.Parallel()
	spec, err := parseSubagentArgs("--name research the goal text")
	if err != nil {
		t.Fatalf("parseSubagentArgs: %v", err)
	}
	if spec.Name != "research" {
		t.Errorf("Name = %q, want research", spec.Name)
	}
	if spec.Goal != "the goal text" {
		t.Errorf("Goal = %q, want %q", spec.Goal, "the goal text")
	}
}

func TestParseSubagentArgs_SkillAliasFeedsExtras(t *testing.T) {
	t.Parallel()
	spec, err := parseSubagentArgs("--skill=code-review review the latest diff")
	if err != nil {
		t.Fatalf("parseSubagentArgs: %v", err)
	}
	if len(spec.Extras) != 1 || spec.Extras[0] != "code-review" {
		t.Errorf("Extras = %#v, want [code-review]", spec.Extras)
	}
}

func TestParseSubagentArgs_RejectsUnknownFlag(t *testing.T) {
	t.Parallel()
	_, err := parseSubagentArgs("--bogus=value some goal")
	if err == nil {
		t.Errorf("expected error for unknown flag, got nil")
	} else if !strings.Contains(err.Error(), "--bogus") {
		t.Errorf("error should name the unknown flag: %v", err)
	}
}

func TestParseSubagentArgs_RejectsBadNumeric(t *testing.T) {
	t.Parallel()
	cases := []string{
		"--max-turns=abc goal",
		"--max-cost=-1 goal",
		"--max-wallclock=ten-minutes goal",
		"--scheduler=invalid goal",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := parseSubagentArgs(in)
			if err == nil {
				t.Errorf("expected error for %q, got nil", in)
			}
		})
	}
}

func TestParseSubagentArgs_NoGoalErrors(t *testing.T) {
	t.Parallel()
	cases := []string{
		"--name=onlyflags",
		"--name onlyflags",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := parseSubagentArgs(in)
			if err == nil {
				t.Errorf("expected error for goal-less input %q, got nil", in)
			}
		})
	}
}

func TestTokenizeSubagentArgs_HandlesQuotes(t *testing.T) {
	t.Parallel()
	toks := tokenizeSubagentArgs(`--prompt="You are X." goal one two`)
	want := []string{`--prompt=You are X.`, "goal", "one", "two"}
	if len(toks) != len(want) {
		t.Fatalf("tokenize = %#v, want %#v", toks, want)
	}
	for i := range toks {
		if toks[i] != want[i] {
			t.Errorf("token[%d] = %q, want %q", i, toks[i], want[i])
		}
	}
}

func TestHandleSubagentCommand_EmptyArgsShowsUsage(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleSubagentCommand("")
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "Usage:") {
		t.Errorf("expected usage hint, got %q", last)
	}
}

func TestHandleSubagentCommand_NoManagerErrors(t *testing.T) {
	t.Parallel()
	// The newOperatorInputTestModel agent is constructed without
	// a BackgroundAgentManager, so /subagent must refuse gracefully.
	m := newOperatorInputTestModel(t)
	m.handleSubagentCommand("monitor the deploy")
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "BackgroundAgentManager") {
		t.Errorf("expected helpful no-manager message, got %q", last)
	}
}

func TestHandleSubagentCommand_BadFlagErrors(t *testing.T) {
	t.Parallel()
	m := newOperatorInputTestModel(t)
	m.handleSubagentCommand("--max-turns=abc some goal")
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "max-turns") {
		t.Errorf("expected max-turns error, got %q", last)
	}
}

// newSubagentTestModel mints a Model whose Agent IS wired to a
// BackgroundAgentManager. Lets the happy-path spawn test exercise
// the full call chain (parse → mgr.Spawn → success message) without
// duplicating the BG-manager construction boilerplate.
func newSubagentTestModel(t *testing.T) *Model {
	t.Helper()
	prov := mock.NewEcho()
	mgr, err := agent.NewBackgroundAgentManager(
		agent.WithBackgroundProvider(prov, "echo"),
		agent.WithBackgroundMaxConcurrent(2),
		agent.WithBackgroundAlertBuffer(8),
	)
	if err != nil {
		t.Fatalf("NewBackgroundAgentManager: %v", err)
	}
	llm, err := prov.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("provider.Model: %v", err)
	}
	a, err := agent.New(llm, agent.WithBackgroundManager(mgr))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })

	cfg := config.DefaultConfig()
	m := NewModel(cfg, a, "dark")
	m.SetProgram(nopProgramSender{})
	return m
}

func TestHandleSubagentCommand_HappyPathReportsSuccess(t *testing.T) {
	t.Parallel()
	m := newSubagentTestModel(t)
	m.handleSubagentCommand("--name=opspawn monitor the deploy")
	last := lastSystemMessage(t, m)
	if !strings.Contains(last, "opspawn") {
		t.Errorf("system message missing subagent name: %q", last)
	}
	if !strings.Contains(last, "branch") {
		t.Errorf("system message missing branch info: %q", last)
	}
}

func TestHandleSubagentCommand_DuplicateNameSurfacesError(t *testing.T) {
	t.Parallel()
	m := newSubagentTestModel(t)
	m.handleSubagentCommand("--name=dup goal one")
	// Second spawn with the same name should fail (manager refuses
	// duplicate names while the first is still running).
	m.handleSubagentCommand("--name=dup goal two")
	last := lastSystemMessage(t, m)
	// Last message will be the error since the second spawn failed.
	if !strings.Contains(last, "dup") {
		t.Errorf("expected error mentioning the duplicate name, got %q", last)
	}
}

func TestParseSlash_SubagentAndAlias(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantAct SlashAction
	}{
		{"/subagent monitor the queue", SlashSubagent},
		{"/sub --name=research find TODOs", SlashSubagent},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			act, _, _, ok := ParseSlash(tc.in)
			if !ok || act != tc.wantAct {
				t.Errorf("ParseSlash(%q) = (%v, ok=%v), want (%v, true)", tc.in, act, ok, tc.wantAct)
			}
		})
	}
}
