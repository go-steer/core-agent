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
	"os/exec"
	"strings"
	"testing"
)

// #1209. A comment is bash's to ignore, so no comment can change what
// this gate finds. Before the fix, one could: a heredoc inside a command
// substitution inside double quotes (dev/uat/self-dev/run.sh's wake_with)
// left the lexer with inverted quote parity for the rest of the file, and
// a later comment with an odd number of apostrophes then decided whether
// two pinned invocations were credited. The file had passed only because
// its apostrophes happened to balance.
//
// So: for every script the gate finds invocations in, add an apostrophe
// to EVERY comment line — an odd count on each — and require identical
// findings; then the same with an unbalanced `"`, the other character a
// lexer can take for a quote. Line numbers do not move, so the findings
// compare directly.
//
// A `#` line inside a multi-line single-quoted string is not a comment,
// and an apostrophe there really does change the shell; `bash -n` on the
// mutated file is the arbiter, and a script it rejects is skipped with a
// log line rather than counted. The test fails if every script is
// skipped, so it cannot pass by checking nothing.
//
// The property is only as wide as the tree: it sees the comment shapes the
// scanned scripts contain, which is why the shapes that broke a draft of
// this fix are also oracle cases. And it fails LOUD, never silent, on one
// shape: a `#` line inside a heredoc body that holds a pinned invocation
// is body text, so the mutation really changes what is scanned there.
func TestAnApostropheInACommentChangesNoFinding(t *testing.T) {
	files := map[string]bool{}
	for _, in := range liveInvocations(t) {
		files[in.File] = true
	}
	if len(files) == 0 {
		t.Fatal("no scanned script has an invocation; the test would check nothing")
	}
	for _, suffix := range []string{" it's", ` say "hi`} {
		t.Run(suffix, func(t *testing.T) { checkCommentSuffix(t, files, suffix) })
	}
}

func checkCommentSuffix(t *testing.T, files map[string]bool, suffix string) {
	checked := 0
	for f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		mutated, n := suffixEveryComment(string(src), suffix)
		if n == 0 {
			continue
		}
		if !bashAccepts(t, mutated) {
			// Some `#` line sits inside a quoted span, where an apostrophe
			// is real shell text. Fall back to adding them one line at a
			// time and keeping only the ones bash still accepts — the
			// script that matters most here, run.sh, needs this path.
			mutated, n = suffixCommentsBashAccepts(t, string(src), suffix)
			t.Logf("%s: mutated %d comment lines in groups (bash rejected all-at-once)", f, n)
			if n == 0 {
				continue
			}
		}
		checked++
		before, after := findingsKey(Scan(f, string(src))), findingsKey(Scan(f, mutated))
		if before != after {
			t.Errorf("%s: %q appended to each of its %d comment lines changed what the gate finds\nbefore:\n%s\nafter:\n%s", f, suffix, n, before, after)
		}
	}
	if checked == 0 {
		t.Fatal("every script was skipped; the comment-parity property was checked nowhere")
	}
}

func suffixEveryComment(src, suffix string) (string, int) {
	lines := strings.Split(src, "\n")
	n := 0
	for i, l := range lines {
		trim := strings.TrimLeft(l, " \t")
		if strings.HasPrefix(trim, "#") && !strings.HasPrefix(trim, "#!") {
			lines[i] = l + suffix
			n++
		}
	}
	return strings.Join(lines, "\n"), n
}

// suffixCommentsBashAccepts adds the suffix to as many comment
// lines as bash will still accept. Only a few `#` lines sit inside quoted
// spans, so it tries whole groups and splits a group in half only when
// bash rejects it: a few dozen `bash -n` runs on run.sh instead of one per
// comment line (which took the test from under a second to 49s).
func suffixCommentsBashAccepts(t *testing.T, src, suffix string) (string, int) {
	t.Helper()
	lines := strings.Split(src, "\n")
	var idx []int
	for i, l := range lines {
		trim := strings.TrimLeft(l, " \t")
		if strings.HasPrefix(trim, "#") && !strings.HasPrefix(trim, "#!") {
			idx = append(idx, i)
		}
	}
	kept := 0
	var try func(group []int)
	try = func(group []int) {
		if len(group) == 0 {
			return
		}
		for _, i := range group {
			lines[i] += suffix
		}
		if bashAccepts(t, strings.Join(lines, "\n")) {
			kept += len(group)
			return
		}
		for _, i := range group {
			lines[i] = strings.TrimSuffix(lines[i], suffix)
		}
		if len(group) == 1 {
			return
		}
		try(group[:len(group)/2])
		try(group[len(group)/2:])
	}
	try(idx)
	return strings.Join(lines, "\n"), kept
}

func bashAccepts(t *testing.T, script string) bool {
	t.Helper()
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	return cmd.Run() == nil
}

func findingsKey(invs []Invocation) string {
	var b strings.Builder
	for _, in := range invs {
		fmt.Fprintf(&b, "%d pinned=%v exempt=%q %s\n", in.Line, in.Pinned, in.Exempt, in.Text)
	}
	return b.String()
}
