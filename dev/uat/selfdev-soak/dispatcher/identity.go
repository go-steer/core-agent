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
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Decisions 8 and 16: the agent commits as the soak's dedicated human
// identity with a matching DCO sign-off, and the App only pushes. The
// dispatcher is the last point before a commit becomes public, so it
// refuses to push a branch any of whose commits says otherwise. The rules
// follow what dev/tools/verify-no-agent-attribution fails on, narrowed to
// the one identity this rig allows.

// botEmailRe matches GitHub's bot noreply address (`[bot]@users.noreply…`)
// and any address containing "noreply"/"no-reply": a soak commit carrying
// one is either an App's identity or nobody's, and both fail decision 16.
var botEmailRe = regexp.MustCompile(`(?i)\[bot\]@|no-?reply`)

// creditKeyRe matches the trailers that credit a person: the same key
// set verify-no-agent-attribution scans.
var creditKeyRe = regexp.MustCompile(`(?i)^\s*(co-authored-by|co-developed-by|assisted-by|generated-by|authored-by|helped-by|suggested-by|reviewed-by|reported-by|tested-by|acked-by|signed-off-by)\s*:\s*(.*)$`)

// lineSplitRe splits a message on any line ending, so a trailer after a
// bare carriage return is still seen as its own line.
var lineSplitRe = regexp.MustCompile(`\r\n|\r|\n`)

// validateIdentity rejects a configured identity that could never pass
// verifyCommits, so a misconfiguration fails at startup rather than on
// the first issue.
func validateIdentity(id identity) error {
	if strings.TrimSpace(id.Name) == "" || strings.TrimSpace(id.Email) == "" {
		return errors.New("--commit-name and --commit-email are required (decision 16: the soak's dedicated identity)")
	}
	if !strings.Contains(id.Email, "@") {
		return fmt.Errorf("--commit-email %q is not an email address", id.Email)
	}
	if botEmailRe.MatchString(id.Email) || strings.HasSuffix(strings.TrimSpace(id.Name), "[bot]") {
		return fmt.Errorf("commit identity %s is a bot or noreply identity; decision 8 needs a human-looking identity", id)
	}
	return nil
}

// verifyCommits returns nil only when every commit is authored AND
// committed by id, carries at least one Signed-off-by, every sign-off
// names id, and no other credit trailer appears. The error names the
// first offending commit.
func verifyCommits(commits []commit, id identity) error {
	if len(commits) == 0 {
		return errors.New("no commits past the base")
	}
	for _, c := range commits {
		if err := verifyCommit(c, id); err != nil {
			return fmt.Errorf("commit %.12s: %w", c.SHA, err)
		}
	}
	return nil
}

func verifyCommit(c commit, id identity) error {
	for _, who := range []struct{ role, name, email string }{
		{"author", c.AuthorName, c.AuthorEmail},
		{"committer", c.CommitterName, c.CommitterEmail},
	} {
		if botEmailRe.MatchString(who.email) {
			return fmt.Errorf("%s %s <%s> is a bot or noreply identity", who.role, who.name, who.email)
		}
		if who.name != id.Name || !strings.EqualFold(who.email, id.Email) {
			return fmt.Errorf("%s is %s <%s>, not the soak identity %s", who.role, who.name, who.email, id)
		}
	}
	if i := strings.IndexFunc(c.Message, func(r rune) bool {
		return (r < 0x20 && r != '\n' && r != '\r' && r != '\t') || r == 0x7f
	}); i >= 0 {
		return fmt.Errorf("message has a control byte at offset %d", i)
	}
	signoffs := 0
	for _, line := range lineSplitRe.Split(c.Message, -1) {
		m := creditKeyRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, val := strings.ToLower(m[1]), strings.TrimSpace(m[2])
		if key != "signed-off-by" {
			return fmt.Errorf("carries a %s trailer (%s); soak commits credit only the soak identity", m[1], val)
		}
		if !sameIdentity(val, id) {
			return fmt.Errorf("signed off by %s, not the soak identity %s", val, id)
		}
		signoffs++
	}
	if signoffs == 0 {
		return fmt.Errorf("has no Signed-off-by trailer (decision 8: DCO sign-off as %s)", id)
	}
	return nil
}

// sameIdentity compares a "Name <email>" trailer value to id: the name
// exactly, the email case-insensitively.
func sameIdentity(val string, id identity) bool {
	open, closing := strings.LastIndex(val, "<"), strings.LastIndex(val, ">")
	if open < 0 || closing < open {
		return false
	}
	name := strings.TrimSpace(val[:open])
	email := strings.TrimSpace(val[open+1 : closing])
	return name == id.Name && strings.EqualFold(email, id.Email)
}
