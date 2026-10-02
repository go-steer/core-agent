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
	"context"
	"io"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/internal/testutil"
	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// The -p prompt and the REPL's typed lines share streamTurn, and both
// are the operator's own words: the auto-mode approver's task (#1175).
// A REPL wake turn arrives with an empty prompt and adds nothing.
func TestStreamTurn_PromptIsTheApproverTask(t *testing.T) {
	t.Parallel()
	m, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatal(err)
	}
	g := permissions.New(permissions.Options{
		Mode:            permissions.ModeAuto,
		Approver:        &testutil.ApproverProbe{},
		ApprovalTimeout: time.Minute,
	})
	a, err := agent.New(m, agent.WithSession("u-1175", "s-1175"), agent.WithGate(g))
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{"fix the flaky test", ""} {
		if _, err := streamTurn(context.Background(), a, m, prompt, io.Discard, io.Discard, usage.NewTracker(), usage.Pricing{}, nil); err != nil {
			t.Fatalf("streamTurn(%q): %v", prompt, err)
		}
	}
	if got := a.ApproverTask(); got != "fix the flaky test" {
		t.Errorf("ApproverTask() = %q, want the typed prompt", got)
	}
}
