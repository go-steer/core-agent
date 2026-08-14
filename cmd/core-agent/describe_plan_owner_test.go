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
	"strings"
	"testing"

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
			got := describePlanOwner(tc.in)
			if got != tc.want {
				t.Errorf("describePlanOwner(%+v) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.Contains(got, "another agent") {
				t.Errorf("message asserts an author it never read: %q", got)
			}
		})
	}
}
