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

package attachadapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/tools"
)

// /replan now declines to archive a plan this agent did not record
// (#747), and the message that reports the decline has to state what
// the artifact says rather than infer. The first draft asserted "was
// recorded by another agent", which is wrong in the likeliest way to
// reach the branch at all: the same agent, an earlier session, after a
// daemon restart.
func TestDescribePlanOwner_StatesWhatTheArtifactSays(t *testing.T) {
	cases := []struct {
		name string
		in   tools.PlanInfo
		want string
	}{
		{
			name: "agent and session",
			in:   tools.PlanInfo{Agent: "cluster", Session: "s-8f21"},
			want: `recorded by "cluster" in session s-8f21`,
		},
		{
			name: "agent only",
			in:   tools.PlanInfo{Agent: "core_agent"},
			want: `recorded by "core_agent"`,
		},
		{
			name: "session only",
			in:   tools.PlanInfo{Session: "s-8f21"},
			want: "recorded in session s-8f21",
		},
		{
			name: "no attribution at all",
			in:   tools.PlanInfo{},
			want: "it records no author, though another plan here does",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DescribePlanOwner(tc.in)
			if got != tc.want {
				t.Errorf("DescribePlanOwner(%+v) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "another agent") {
				t.Errorf("message asserts an author it never read: %q", got)
			}
		})
	}
}

// The degenerate wirings have to answer, not panic or lie. A daemon
// hosting many tenants turns a nil gate or an unresolved .agents/ into
// one bad response, and a caller with no identity yet gets the
// documented newest-wins fallback rather than an empty archive.
func TestReplanHandler_DegradesHonestly(t *testing.T) {
	agentsDir := t.TempDir()
	plansDir := filepath.Join(agentsDir, "plans")
	if err := os.MkdirAll(plansDir, 0o755); err != nil {
		t.Fatalf("mkdir plans: %v", err)
	}
	plan := filepath.Join(plansDir, "plan-1.md")
	if err := os.WriteFile(plan, []byte("---\nplan: 1\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	gate := permissions.New(permissions.Options{RequirePlanArtifact: true})
	ctx := context.Background()

	resp, err := ReplanHandler(nil, agentsDir, nil)(ctx, attach.ReplanRequest{})
	if err != nil {
		t.Fatalf("nil gate returned an error instead of a response: %v", err)
	}
	if !strings.Contains(resp.Message, "no permissions gate") || resp.PlanWasActive {
		t.Errorf("nil gate: %+v", resp)
	}
	if _, err := os.Stat(plan); err != nil {
		t.Errorf("nil gate archived a plan whose flag it could not clear: %v", err)
	}

	resp, err = ReplanHandler(gate, "", nil)(ctx, attach.ReplanRequest{})
	if err != nil {
		t.Fatalf("empty agentsDir returned an error instead of a response: %v", err)
	}
	if !strings.Contains(resp.Message, "no .agents/ directory") || resp.PlanWasActive {
		t.Errorf("empty agentsDir: %+v", resp)
	}

	// Nil owner: the zero PlanOwner matches everything, which is the
	// pre-#747 behavior the CLI falls back to before construction.
	resp, err = ReplanHandler(gate, agentsDir, nil)(ctx, attach.ReplanRequest{})
	if err != nil {
		t.Fatalf("nil owner: %v", err)
	}
	if !resp.PlanWasActive || filepath.Base(resp.ArchivedPath) != "plan-1-revoked.md" {
		t.Errorf("nil owner did not fall back to newest-wins: %+v", resp)
	}
}
