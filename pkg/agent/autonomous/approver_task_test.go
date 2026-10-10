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
	"testing"
	"time"

	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v2/internal/testutil"
	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// A goal a person wrote (WithOperatorGoal) governs the whole run, so it
// is the auto-mode approver's task on every turn (#1175). Without the
// option — a background subagent's goal is the parent model's brief —
// it is not. The continuation prompts the loop writes never are.
func TestRun_GoalIsTheApproverTaskOnlyWhenTheOperatorWroteIt(t *testing.T) {
	t.Parallel()
	const goal = "rotate the staging certificates"
	if got := runGoal(t, goal, WithOperatorGoal()); got != goal {
		t.Errorf("with WithOperatorGoal, ApproverTask() = %q, want the goal alone", got)
	}
	if got := runGoal(t, goal); got != "" {
		t.Errorf("without WithOperatorGoal, ApproverTask() = %q, want none", got)
	}
}

func runGoal(t *testing.T, goal string, opts ...Option) string {
	t.Helper()
	llm := &stubLLM{scenarios: []scenarioFn{
		textTurn("working", 10, 5),
		textTurn("still working", 10, 5),
		textTurn("nearly", 10, 5),
	}}
	var built *agent.Agent
	build := func(extras []tool.Tool) (*agent.Agent, error) {
		g := permissions.New(permissions.Options{
			Mode:            permissions.ModeAuto,
			Approver:        &testutil.ApproverProbe{},
			ApprovalTimeout: time.Minute,
		})
		a, err := agent.New(llm, agent.WithName("goal"), agent.WithSession("u-test", "s-goal"),
			agent.WithTools(extras), agent.WithGate(g))
		built = a
		return a, err
	}
	if _, err := Run(context.Background(), build, goal, append(opts, WithMaxTurns(3))...); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return built.ApproverTask()
}
