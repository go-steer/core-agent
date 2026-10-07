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
	"regexp"
	"strconv"
	"strings"
)

// The text rules of dev/tools/verify-no-agent-attribution, ported so the
// dispatcher refuses what that check would fail before anything is
// public: a banned trailer key, and a "Generated with <agent>" footer in
// any of the forms the script recognises. Keep the three expressions in
// step with BANNED_KEY_RE, FOOTER_RE and ROBOT_RE there. Credit trailers
// and identities are handled more strictly by verifyCommit (only the soak
// identity at all), so they are not repeated here.
var (
	bannedKeyRe = regexp.MustCompile(`(?i)^\s*(self-development-run|ai-assisted|ai-generated|ai-model|generated-with)\s*:`)

	footerAgent  = `(claude|github copilot|copilot|chatgpt|openai codex|codex|cursor|devin|aider|windsurf|gemini|amazon[ -]?q|kiro|openhands|cline|antigravity)([^[:alnum:]_]|$)`
	footerMarkup = `([^[:alnum:]]|<[a-z]+>)*`
	footerRe     = regexp.MustCompile(`(?i)^` + footerMarkup + `(this (pr|pull request|commit|change|patch) (was|is) )?` +
		`(generated|created|written|drafted|built|authored|produced|made|powered|assisted|co-authored)\s+(with|by|using|via|in)[\s:]+` +
		footerMarkup + footerAgent)
	robotRe = regexp.MustCompile(`(?i)🤖` + footerMarkup + `(generated|` + footerAgent + `)`)
)

// attributionLine returns the first line of text that the attribution
// check would fail, or "" when there is none.
func attributionLine(text string) string {
	for _, line := range lineSplitRe.Split(text, -1) {
		if bannedKeyRe.MatchString(line) || footerRe.MatchString(line) || robotRe.MatchString(line) {
			return line
		}
	}
	return ""
}

// closingRe matches a GitHub closing keyword and the issue it closes, in
// the forms GitHub honours: "#N", "owner/repo#N", or an issue URL.
var closingRe = regexp.MustCompile(`(?i)\b(close[sd]?|fix(?:e[sd])?|resolve[sd]?)(\s*:?\s+)(#([0-9]+)|[\w.-]+/[\w.-]+#[0-9]+|https?://\S+/(?:issues|pull)/[0-9]+)`)

// neutralizeClosers rewrites every closing keyword in agent-supplied text
// to "refs", so merging the PR closes only the issue the dispatcher's own
// "Fixes #N" names — never one the agent chose.
func neutralizeClosers(text string) string {
	return closingRe.ReplaceAllString(text, "refs$2$3")
}

// foreignCloser returns the first closing keyword in a commit message
// that would close a mirror issue other than number, or "". A commit's
// "Fixes #M" closes #M when the PR merges, so an agent commit may close
// only its own issue. A closer aimed at another repository (the upstream
// issue, decision 12) is left alone: it can't close anything from the
// mirror, and upstream it is the maintainer's cherry-pick to judge.
func foreignCloser(msg string, number int, mirror string) string {
	own := strconv.Itoa(number)
	for _, m := range closingRe.FindAllStringSubmatch(msg, -1) {
		target := strings.ToLower(m[3])
		switch {
		case m[4] != "":
			if m[4] != own {
				return strings.TrimSpace(m[0])
			}
		case strings.Contains(target, strings.ToLower(mirror)):
			if !strings.HasSuffix(target, "/"+own) && !strings.HasSuffix(target, "#"+own) {
				return strings.TrimSpace(m[0])
			}
		}
	}
	return ""
}

// verifyCommitTexts applies checkCommitText to every commit on the branch.
func verifyCommitTexts(commits []commit, number int, mirror string) error {
	for _, c := range commits {
		if err := checkCommitText(c.Message, number, mirror); err != nil {
			return fmt.Errorf("commit %.12s %w", c.SHA, err)
		}
	}
	return nil
}

// checkCommitText applies the text rules to one commit message.
func checkCommitText(msg string, number int, mirror string) error {
	if line := attributionLine(msg); line != "" {
		return fmt.Errorf("carries agent attribution (%q)", truncate(line, 120))
	}
	if c := foreignCloser(msg, number, mirror); c != "" {
		return fmt.Errorf("would close another issue on merge (%q)", truncate(c, 120))
	}
	return nil
}
