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
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/tools"
)

// maxPlanBytes keeps the PR body under GitHub's 65536-character limit
// with room for the header.
const maxPlanBytes = 60000

// planFor returns the body of the newest plan artifact the session
// recorded, frontmatter removed, or "" when there is none. agentsDir is
// the daemon's .agents directory as the dispatcher sees it; "" disables
// the lookup.
func planFor(agentsDir, sessionID string) string {
	if agentsDir == "" || sessionID == "" {
		return ""
	}
	for _, p := range tools.ActivePlans(agentsDir) { // newest first
		if p.Session != sessionID {
			continue
		}
		raw, err := os.ReadFile(p.Path) // #nosec G304 -- a path ActivePlans listed.
		if err != nil {
			return ""
		}
		body := stripFrontmatter(string(raw))
		if len(body) > maxPlanBytes {
			body = strings.ToValidUTF8(body[:maxPlanBytes], "") + "\n\n[plan truncated]"
		}
		return strings.TrimSpace(body)
	}
	return ""
}

func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	rest := s[len("---\n"):]
	if end := strings.Index(rest, "\n---\n"); end >= 0 {
		return rest[end+len("\n---\n"):]
	}
	return s
}

// prTitle is the subject of the agent's first commit — the agent writes
// Conventional Commits — falling back to the issue title.
func prTitle(a *activeIssue, commits []commit) string {
	if len(commits) > 0 {
		first := commits[len(commits)-1] // git log lists newest first
		if subject, _, _ := strings.Cut(first.Message, "\n"); strings.TrimSpace(subject) != "" {
			return strings.TrimSpace(subject)
		}
	}
	return a.Title
}

// prBody links the mirror issue (Fixes #N closes it on merge), the
// upstream issue (decision 12), and the session that did the work.
func prBody(a *activeIssue, repo, plan string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fixes #%d\n\n", a.Number)
	fmt.Fprintf(&b, "Upstream issue: %s\n\n", a.Upstream)
	fmt.Fprintf(&b, "Session: `%s` on the soak daemon. Branch `%s` was committed by that session and pushed by the soak dispatcher, from %s base `%s`.\n\n", a.SessionID, branchFor(a.Number), repo, a.BaseSHA)
	b.WriteString("## Plan\n\n")
	if plan == "" {
		b.WriteString("The session recorded no plan artifact.\n")
	} else {
		b.WriteString(plan)
		b.WriteString("\n")
	}
	return b.String()
}

func stopComment(a *activeIssue, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The soak dispatcher stopped work on this issue.\n\nReason: %s\n\n", reason)
	if a.SessionID != "" {
		fmt.Fprintf(&b, "Session: `%s`.\n\n", a.SessionID)
	}
	fmt.Fprintf(&b, "The dispatcher never retries a stopped issue (decision 17). To retry it, remove the `%s` label.\n", labelStopped)
	return b.String()
}
