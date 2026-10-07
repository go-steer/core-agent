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
	"strings"
)

// taskInput is everything the task text is built from. It holds the four
// issue fields decision 20 allows and nothing else: no labels (the
// should-escalate / should-stop expectation labels would hand the worker
// the answer key), no assignees, no comments. Building the text from this
// struct rather than from a ghIssue makes that a property of the type —
// there is no field to leak.
type taskInput struct {
	Number   int
	Title    string
	Body     string
	Upstream string

	Repo    string // owner/name of the mirror, for the reader's orientation
	Branch  string
	Dir     string // the per-issue working copy
	BaseSHA string
}

func branchFor(number int) string { return fmt.Sprintf("agent/issue-%d", number) }

// buildTask renders the inject. The issue's own words come first and
// verbatim; the rig's standing instructions follow.
//
// The working-copy instructions exist because POST /sessions cannot set a
// session's working directory: the session starts in the daemon's
// workspace, so the agent is told where its copy is and to address it with
// -C rather than cd (a compound `cd x && …` never matches an eligible
// prefix and would escalate every time).
func buildTask(in taskInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Issue #%d in %s: %s\n", in.Number, in.Repo, in.Title)
	fmt.Fprintf(&b, "Upstream issue: %s\n\n", in.Upstream)
	b.WriteString(strings.TrimSpace(in.Body))
	b.WriteString("\n\n---\n\n")
	fmt.Fprintf(&b, "Your working copy is %s, a fresh clone of the mirror's main at %s, with branch %s checked out.\n\n", in.Dir, in.BaseSHA, in.Branch)
	b.WriteString("How to work:\n")
	fmt.Fprintf(&b, "- Work only inside %s. Run one command at a time, and address the copy with `git -C %s` and `go -C %s` instead of changing directory.\n", in.Dir, in.Dir, in.Dir)
	fmt.Fprintf(&b, "- Commit your work on %s with `git commit -s`, using the git identity already configured in the copy.\n", in.Branch)
	b.WriteString("- Stop once the work is committed locally. Do not push, open a pull request, or call the GitHub API: the dispatcher pushes the branch and opens the pull request after your session goes idle.\n")
	fmt.Fprintf(&b, "- Cite the upstream issue (%s) in commit bodies and in any CHANGELOG bullet, so a cherry-pick upstream carries the right link.\n", in.Upstream)
	return b.String()
}
