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
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// repoRoot: tests run in the package directory.
const repoRoot = "../.."

func liveInvocations(t *testing.T) []Invocation {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	var all []Invocation
	for _, root := range roots {
		found, err := scanTree(root)
		if err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
		all = append(all, found...)
	}
	return all
}

// The invariant the presubmit enforces, also asserted by `go test ./...`
// so a local run catches it before CI does.
func TestLiveTreeIsPinned(t *testing.T) {
	invs := liveInvocations(t)
	if len(invs) == 0 {
		t.Fatal("scanned the harness and found no invocations at all — the scanner has stopped recognising the call sites, which is a silent pass")
	}
	for _, in := range invs {
		if !in.Pinned && in.Exempt == "" {
			t.Errorf("%s:%d: unpinned invocation: %s %s", in.File, in.Line, in.Text, in.Args)
		}
	}
}

var pinFlag = regexp.MustCompile(`\s-c\s+('[^']*'|"[^"]*"|\S+)`)

// Every pinned site, un-pinned one at a time, must come back as a
// finding. The mutations are applied to the REAL script text rather than
// to a fixture that imitates it: a hand-written imitation drifts, and
// the scanner passing on a shape no script actually has proves nothing.
func TestRemovingEachPinIsCaught(t *testing.T) {
	invs := liveInvocations(t)

	// liveInvocations left us at the repo root.
	tested := 0
	for _, in := range invs {
		if !in.Pinned {
			continue
		}
		name := fmt.Sprintf("%s:%d", in.File, in.Line)
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(in.File)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(src), "\n")
			idx := in.Line - 1
			if idx < 0 || idx >= len(lines) {
				t.Fatalf("line %d out of range for %s (%d lines)", in.Line, in.File, len(lines))
			}
			before := lines[idx]
			lines[idx] = pinFlag.ReplaceAllString(before, "")
			if lines[idx] == before {
				t.Fatalf("mutation changed nothing — line %d of %s does not carry the pin the scanner credited it with:\n  %s",
					in.Line, in.File, before)
			}

			got := Scan(in.File, strings.Join(lines, "\n"))
			var unpinned []Invocation
			for _, g := range got {
				if !g.Pinned && g.Exempt == "" {
					unpinned = append(unpinned, g)
				}
			}
			if len(unpinned) != 1 {
				t.Fatalf("removing one pin produced %d findings, want exactly 1: %+v", len(unpinned), unpinned)
			}
			if unpinned[0].Line != in.Line {
				t.Errorf("finding reported on line %d, want %d", unpinned[0].Line, in.Line)
			}
		})
		tested++
	}
	if tested == 0 {
		t.Fatal("no pinned sites to mutate — either the harness stopped invoking the binary or the scanner stopped seeing it")
	}
}

// The count is asserted, not just the absence of failures. A scanner
// that quietly stops recognising a script's call sites reports nothing
// and passes; only a number noticing they went missing catches that.
// Update this deliberately when a harness script gains or loses a site.
func TestInvocationCensus(t *testing.T) {
	invs := liveInvocations(t)
	byFile := map[string]int{}
	for _, in := range invs {
		byFile[in.File]++
	}
	want := map[string]int{
		"dev/smoke/01-gemini-basic.sh":              1,
		"dev/smoke/02-vertex-basic.sh":              1,
		"dev/smoke/03-vertex-grounding.sh":          1,
		"dev/smoke/04-background-spawn.sh":          1,
		"dev/smoke/05-headless-gate.sh":             1,
		"dev/smoke/07-mcp-google-oauth.sh":          1,
		"dev/smoke/09-multi-session-bearer.sh":      2,
		"dev/smoke/09-vertex-anthropic-toolloop.sh": 2,
		"dev/smoke/10-multi-session-resume.sh":      1,
		"dev/uat/attach/run.sh":                     4, // 2 dispatched + attach + ls
	}
	for f, n := range want {
		if byFile[f] != n {
			t.Errorf("%s: %d invocation(s), want %d", f, byFile[f], n)
		}
		delete(byFile, f)
	}
	for f, n := range byFile {
		t.Errorf("%s: %d unexpected invocation(s) — add it to the census", f, n)
	}
}
