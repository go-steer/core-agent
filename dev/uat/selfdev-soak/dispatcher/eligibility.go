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
	"strings"
)

// Labels the dispatcher reads and writes on the mirror.
const (
	labelQueue   = "soak:queue"   // the maintainer queued this issue
	labelPause   = "soak:pause"   // any open issue carrying it pauses the dispatcher
	labelActive  = "soak:active"  // the dispatcher claimed this issue
	labelStopped = "soak:stopped" // the dispatcher gave up on it; a person retries (decision 17)
)

// claimable is the current-state filter: open, queued, not a pull
// request, and not already claimed or stopped. It says nothing about who
// put the issue there — that is checkProvenance, which needs the events.
func claimable(is ghIssue) bool {
	return is.State == "open" && is.PullRequest == nil &&
		is.hasLabel(labelQueue) && !is.hasLabel(labelActive) && !is.hasLabel(labelStopped)
}

// checkProvenance is decision 6: an issue becomes a task only if the
// maintainer wrote it, queued it and assigned it. nil means eligible;
// an error names the first rule that failed.
//
// Read from the issue's event history, not its current state, because
// current state cannot say who acted: an issue that carries soak:queue
// today may carry it because someone else applied it. So every labeled
// event for soak:queue must be the maintainer's, not merely the latest
// one — a label once applied by anyone else disqualifies the issue for
// good, and re-applying it does not launder that. Same for assignment.
//
// Title changes ("renamed") are checked too, because the title is part
// of the injected task; body edits are not in the REST events API, so
// checkEditors reads them from GraphQL. Reopening the issue, and removing
// soak:stopped (decision 17: only a person retries a stopped issue), must
// be the maintainer's as well. Neither the issue nor any of those events
// may have been performed through a GitHub App.
func checkProvenance(is ghIssue, events []ghEvent, maintainer string) error {
	if !sameLogin(is.User.Login, maintainer) {
		return fmt.Errorf("authored by %q, not %q", is.User.Login, maintainer)
	}
	if is.ViaApp != nil {
		return fmt.Errorf("created through the GitHub App %q", is.ViaApp.Slug)
	}
	if len(is.Assignees) == 0 {
		return fmt.Errorf("not assigned")
	}
	seen := map[string]int{}
	for _, ev := range events {
		rule := eventRule(ev)
		if rule == "" {
			continue
		}
		seen[rule]++
		if ev.ViaApp != nil {
			return fmt.Errorf("%s was performed through the GitHub App %q", rule, ev.ViaApp.Slug)
		}
		who := ev.actor()
		if rule == "assigned" {
			// Both fields must be the maintainer: see ghEvent.assigner for
			// why either one alone could name the assignee instead.
			if !sameLogin(ev.assigner(), maintainer) {
				who = ev.assigner()
			}
		}
		if !sameLogin(who, maintainer) {
			return fmt.Errorf("%s by %q, not %q", rule, who, maintainer)
		}
	}
	if seen[ruleQueued] == 0 {
		return fmt.Errorf("no %s labeled event in the issue's history", labelQueue)
	}
	if seen["assigned"] == 0 {
		return fmt.Errorf("no assigned event in the issue's history")
	}
	return nil
}

const ruleQueued = labelQueue + " applied"

// eventRule names the decision-6 rule an event falls under, or "" for an
// event that carries no authority (a mention, the dispatcher's own
// soak:active label, a comment).
func eventRule(ev ghEvent) string {
	label := ""
	if ev.Label != nil {
		label = ev.Label.Name
	}
	switch {
	case ev.Event == "labeled" && strings.EqualFold(label, labelQueue):
		return ruleQueued
	case ev.Event == "unlabeled" && strings.EqualFold(label, labelQueue):
		return labelQueue + " removed"
	case ev.Event == "unlabeled" && strings.EqualFold(label, labelStopped):
		return labelStopped + " removed (a retry)"
	case ev.Event == "assigned":
		return "assigned"
	case ev.Event == "renamed":
		return "title changed"
	case ev.Event == "reopened":
		return "reopened"
	}
	return ""
}

// checkEditors refuses an issue whose body anyone but the maintainer
// edited. An edit GitHub can't attribute, or more edits than one page
// shows, is refused too: an unread edit is not a checked one.
func checkEditors(editors []string, total int, maintainer string) error {
	if total > len(editors) || total > maxContentEdits {
		return fmt.Errorf("body edited %d times; more than the %d the check reads", total, len(editors))
	}
	for _, e := range editors {
		if !sameLogin(e, maintainer) {
			return fmt.Errorf("body edited by %q, not %q", e, maintainer)
		}
	}
	return nil
}

// sameLogin compares GitHub logins, which are case-insensitive. An empty
// login (a deleted "ghost" account, or a missing actor) never matches.
func sameLogin(a, b string) bool {
	return a != "" && strings.EqualFold(a, b)
}

// upstreamLink returns the first link to an issue in upstreamRepo
// ("owner/name") found in body, or "" when there is none. Decision 12:
// every seed names its upstream issue, because mirror numbers don't match.
func upstreamLink(body, upstreamRepo string) string {
	re := regexp.MustCompile(`https://github\.com/` + regexp.QuoteMeta(upstreamRepo) + `/(?:issues|pull)/[0-9]+`)
	return re.FindString(body)
}
