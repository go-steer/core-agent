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

// The two #791 decisions that live in YAML rather than in Go: WHERE the
// summary is written and WHEN the freeze review runs. Both are load
// bearing, both are one edit away from being silently undone, and
// neither is reachable from a test of this package's functions — the
// tool answers correctly no matter which step invokes it.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workflowSteps splits the job's steps apart. A step begins at the
// six-space `- ` that YAML sequence entries take at this nesting depth;
// good enough to ask "which step is this flag on?", which is the only
// question here.
//
// Comment lines are dropped first. This file explains itself at length,
// and a step's rationale routinely names what the NEXT step does — so
// keeping the comments would attribute half the workflow to whichever
// step happens to precede it.
func workflowSteps(t *testing.T) []string {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "workflows", "lookout-pin-check.yml")
	raw, err := os.ReadFile(path) //nolint:gosec // fixed in-tree path
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var steps []string
	var cur strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if strings.HasPrefix(line, "      - ") && cur.Len() > 0 {
			steps = append(steps, cur.String())
			cur.Reset()
		}
		if strings.HasPrefix(line, "      - ") || cur.Len() > 0 {
			cur.WriteString(line + "\n")
		}
	}
	if cur.Len() > 0 {
		steps = append(steps, cur.String())
	}
	if len(steps) < 4 {
		t.Fatalf("parsed %d steps out of %s, so this test is reading the file wrong",
			len(steps), path)
	}
	return steps
}

// stepWith returns the index of the one step containing needle.
func stepWith(t *testing.T, steps []string, needle string) int {
	t.Helper()
	found := -1
	for i, s := range steps {
		if strings.Contains(s, needle) {
			if found >= 0 {
				t.Fatalf("%q appears on more than one step (%d and %d)", needle, found, i)
			}
			found = i
		}
	}
	if found < 0 {
		t.Fatalf("no step contains %q", needle)
	}
	return found
}

// caseBranch returns the body of a `case` arm — everything between its
// pattern and the `;;` that closes it.
func caseBranch(step, pattern string) (string, bool) {
	i := strings.Index(step, pattern)
	if i < 0 {
		return "", false
	}
	rest := step[i+len(pattern):]
	if j := strings.Index(rest, ";;"); j >= 0 {
		return rest[:j], true
	}
	return rest, true
}

// TestWorkflow_TheSummaryIsWrittenOnAStepEveryRunReaches. The whole fix
// is that a clean week says something. Most weeks the pin is current and
// the job stops at the drift check, so a --summary hung off the rewrite
// or the PR step would report only on the weeks something else already
// told you about — and a freeze nobody is reminded of is a freeze nobody
// revisits, which is the failure #791 is about.
func TestWorkflow_TheSummaryIsWrittenOnAStepEveryRunReaches(t *testing.T) {
	t.Parallel()
	steps := workflowSteps(t)
	i := stepWith(t, steps, "--summary=")
	if !strings.Contains(steps[i], "--check ") {
		t.Errorf("--summary is not on the drift-check step, which is the one every run "+
			"reaches; it is on:\n%s", steps[i])
	}
	if strings.Contains(steps[i], "\n        if:") {
		t.Errorf("the step writing --summary is conditional, so a clean week reports "+
			"nothing:\n%s", steps[i])
	}
}

// TestWorkflow_TheFreezeReviewRunsLastAndUnconditionally. An overdue
// case study is a question for a person; the bump PR keeps the pins that
// DO track upstream current. Ordering the review before the PR step
// would let the first block the second, which is why the review step
// sits at the end — and why it carries no `if:`, since the week it most
// needs to fire is a week with no drift at all.
func TestWorkflow_TheFreezeReviewRunsLastAndUnconditionally(t *testing.T) {
	t.Parallel()
	steps := workflowSteps(t)
	freeze := stepWith(t, steps, "--check-freezes")
	pr := stepWith(t, steps, "peter-evans/create-pull-request")

	if freeze < pr {
		t.Errorf("the freeze review (step %d) runs before the pull-request step (%d), so a "+
			"lapsed review can block a real bump", freeze, pr)
	}
	if freeze != len(steps)-1 {
		t.Errorf("the freeze review is step %d of %d; it is meant to be last so nothing it "+
			"fails can stop", freeze, len(steps))
	}
	if strings.Contains(steps[freeze], "\n        if:") {
		t.Errorf("the freeze review is conditional, so it skips the clean weeks it exists "+
			"for:\n%s", steps[freeze])
	}
	// And it fails rather than warns. A warning on a scheduled run is
	// read by nobody, which is the property the step exists to remove.
	// Asked of the overdue BRANCH specifically: the step's catch-all for
	// an unrecognised verdict exits non-zero too, so "the step mentions
	// exit 1 somewhere" would hold just as well for a warning.
	overdue, ok := caseBranch(steps[freeze], "freeze-review=overdue)")
	if !ok {
		t.Fatalf("the freeze review has no freeze-review=overdue branch:\n%s", steps[freeze])
	}
	if !strings.Contains(overdue, "exit 1") {
		t.Errorf("an overdue review does not fail the step, and a warning on a scheduled run "+
			"is read by nobody:\n%s", overdue)
	}
}

// TestWorkflow_BothVerdictsAreReadFromStdoutNotTheExitCode. `go run`
// collapses every non-zero child status to 1, so an unreachable API and
// a real answer would be indistinguishable. Both modes answer on stdout
// and the workflow must reject anything that is not one of the two
// strings, rather than treating an unrecognised line as the happy path.
func TestWorkflow_BothVerdictsAreReadFromStdoutNotTheExitCode(t *testing.T) {
	t.Parallel()
	steps := workflowSteps(t)
	for _, tc := range []struct{ flag, ok, bad string }{
		{"--check ", "drift=true|drift=false", "unexpected --check output"},
		{"--check-freezes", "freeze-review=ok)", "unexpected --check-freezes output"},
	} {
		s := steps[stepWith(t, steps, tc.flag)]
		if !strings.Contains(s, "verdict=\"$(go run") {
			t.Errorf("%s step does not capture stdout:\n%s", tc.flag, s)
		}
		if !strings.Contains(s, tc.ok) {
			t.Errorf("%s step does not match the expected verdict %q:\n%s", tc.flag, tc.ok, s)
		}
		if !strings.Contains(s, tc.bad) {
			t.Errorf("%s step has no catch-all for an unrecognised verdict, so a changed "+
				"contract would read as success:\n%s", tc.flag, s)
		}
	}
}
