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
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// maxPlanBytes keeps the PR body under GitHub's 65536-character limit
// with room for the header.
const maxPlanBytes = 60000

// maxPlanFileBytes bounds the read of one plan artifact. Anything larger
// is truncated for the PR body anyway.
const maxPlanFileBytes = 256 << 10

// planNameRe matches an active plan artifact (pkg/tools/record_plan.go's
// naming); a revoked plan's "-revoked" suffix does not match.
var planNameRe = regexp.MustCompile(`^plan-([0-9]+)\.md$`)

// planFor returns the body of the newest plan artifact the session
// recorded, frontmatter removed, or "" when there is none. agentsDir is
// the daemon's .agents directory as the dispatcher sees it; "" disables
// the lookup.
//
// The plans directory is agent-writable, and the dispatcher reads it in
// its own pod, where a symlink resolves against the dispatcher's
// filesystem — the App key included. So this does not use
// tools.ActivePlans, which follows links and reads unbounded: the
// directory must be a real directory, each file is opened without
// following a link and must be a regular file, and it is read once,
// bounded, with the frontmatter checked on the same bytes that are
// published.
func planFor(agentsDir, sessionID string) string {
	if agentsDir == "" || sessionID == "" {
		return ""
	}
	dir := filepath.Join(agentsDir, "plans")
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() { // #nosec G703 -- operator-configured path.
		return ""
	}
	entries, err := os.ReadDir(dir) // #nosec G703 -- operator-configured path.
	if err != nil {
		return ""
	}
	type candidate struct {
		seq  int
		name string
	}
	var cands []candidate
	for _, e := range entries {
		if m := planNameRe.FindStringSubmatch(e.Name()); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				cands = append(cands, candidate{n, e.Name()})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].seq > cands[j].seq })
	for _, c := range cands {
		raw, ok := readPlanFile(filepath.Join(dir, c.name))
		if !ok || planSession(raw) != sessionID {
			continue
		}
		body := strings.TrimSpace(stripFrontmatter(raw))
		if len(body) > maxPlanBytes {
			body = strings.ToValidUTF8(body[:maxPlanBytes], "") + "\n\n[plan truncated]"
		}
		return body
	}
	return ""
}

// readPlanFile reads one plan, without following a symlink, and only if
// it is a regular file.
func readPlanFile(path string) (string, bool) {
	f, err := openNoFollow(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPlanFileBytes))
	if err != nil {
		return "", false
	}
	return string(raw), true
}

// planSession reads the session attribution out of a plan's frontmatter
// (pkg/tools/record_plan.go's planFrontmatter), or "" when there is none.
func planSession(raw string) string {
	if !strings.HasPrefix(raw, "---\n") {
		return ""
	}
	block, _, ok := strings.Cut(raw[len("---\n"):], "\n---")
	if !ok {
		return ""
	}
	for _, line := range strings.Split(block, "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "session" {
			continue
		}
		val = strings.TrimSpace(val)
		if unq, err := strconv.Unquote(val); err == nil {
			val = unq
		}
		return val
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
