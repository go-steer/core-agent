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

import "testing"

func TestPolicy_Match(t *testing.T) {
	t.Parallel()
	p, err := NewPolicy(
		[]string{"bash:git status", "bash:git diff*", "bash:ls *"},
		[]string{"bash:rm -rf*", "bash:sudo *"},
	)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}

	cases := []struct {
		name string
		tool string
		key  string
		want Outcome
	}{
		{"exact allow", "bash", "git status", OutcomeAllow},
		{"prefix allow", "bash", "git diff main..HEAD", OutcomeAllow},
		{"unrelated bash", "bash", "git push", OutcomeUnmatched},
		{"deny wins over allow", "bash", "rm -rf /tmp/x", OutcomeDeny},
		{"sudo deny", "bash", "sudo apt-get update", OutcomeDeny},
		{"different tool not matched", "read_file", "git status", OutcomeUnmatched},
		{"plain ls glob", "bash", "ls -la /tmp", OutcomeAllow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := p.Match(tc.tool, tc.key)
			if got != tc.want {
				t.Errorf("Match(%q,%q) = %v, want %v", tc.tool, tc.key, got, tc.want)
			}
		})
	}
}

func TestPolicy_AnyToolPattern(t *testing.T) {
	t.Parallel()
	p, _ := NewPolicy([]string{"*foo*"}, nil)
	if p.Match("bash", "echo foobar") != OutcomeAllow {
		t.Errorf("any-tool wildcard did not match bash command")
	}
}
