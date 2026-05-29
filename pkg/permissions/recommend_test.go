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

package permissions

import (
	"strings"
	"testing"
	"time"
)

func mkApprovals(rows ...[2]string) []ApprovalLog {
	out := make([]ApprovalLog, 0, len(rows))
	now := time.Now()
	for _, r := range rows {
		out = append(out, ApprovalLog{Tool: r[0], Key: r[1], Decision: DecisionAllowOnce, At: now})
	}
	return out
}

func patterns(recs []Recommendation) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Pattern
	}
	return out
}

func TestRecommend(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		input  []ApprovalLog
		expect []string
	}{
		{
			"empty log -> nothing",
			nil,
			nil,
		},
		{
			"single bash command -> exact pattern",
			mkApprovals([2]string{"bash", "git status"}),
			[]string{"bash:git status"},
		},
		{
			"multiple bash sharing a verb -> verb glob",
			mkApprovals(
				[2]string{"bash", "git status"},
				[2]string{"bash", "git log -p"},
				[2]string{"bash", "git diff HEAD"},
			),
			[]string{"bash:git *"},
		},
		{
			"multiple bash with no shared verb -> tool-wide",
			mkApprovals(
				[2]string{"bash", "ls -la"},
				[2]string{"bash", "pwd"},
				[2]string{"bash", "echo hi"},
			),
			[]string{"bash:*"},
		},
		{
			"file reads with shared dir prefix -> path glob",
			mkApprovals(
				[2]string{"read_file", "internal/tui/model.go"},
				[2]string{"read_file", "internal/tui/view.go"},
				[2]string{"read_file", "internal/tui/update.go"},
			),
			[]string{"read_file:internal/tui/**"},
		},
		{
			"file reads with no shared dir -> tool-wide",
			mkApprovals(
				[2]string{"read_file", "go.mod"},
				[2]string{"read_file", "README.md"},
				[2]string{"read_file", "Makefile"},
			),
			[]string{"read_file:*"},
		},
		{
			"two tools each get their own recommendation",
			mkApprovals(
				[2]string{"bash", "git status"},
				[2]string{"bash", "git log"},
				[2]string{"read_file", "go.mod"},
			),
			[]string{"bash:git *", "read_file:go.mod"},
		},
		{
			"duplicate keys collapse to one",
			mkApprovals(
				[2]string{"bash", "git status"},
				[2]string{"bash", "git status"},
				[2]string{"bash", "git status"},
			),
			[]string{"bash:git status"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := patterns(Recommend(tc.input))
			if !sliceEq(got, tc.expect) {
				t.Errorf("Recommend produced %v; want %v", got, tc.expect)
			}
		})
	}
}

func TestRecommend_FileToolPrefixDropsBasename(t *testing.T) {
	t.Parallel()
	got := patterns(Recommend(mkApprovals(
		[2]string{"read_file", "internal/tui/model.go"},
		[2]string{"read_file", "internal/tui/model.go"},
	)))
	for _, p := range got {
		if strings.HasSuffix(p, ".go/**") {
			t.Errorf("recommendation %q glob-pasted onto a filename, not a directory", p)
		}
	}
}

func TestSortRecommendations(t *testing.T) {
	t.Parallel()
	recs := []Recommendation{
		{Pattern: "bash:*"},
		{Pattern: "read_file:go.mod"},
		{Pattern: "bash:git *"},
		{Pattern: "read_file:internal/tui/**"},
	}
	SortRecommendations(recs)
	got := patterns(recs)
	want := []string{"read_file:go.mod", "bash:*", "bash:git *", "read_file:internal/tui/**"}
	if !sliceEq(got, want) {
		t.Errorf("SortRecommendations -> %v; want %v", got, want)
	}
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
