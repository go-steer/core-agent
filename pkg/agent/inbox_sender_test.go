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

package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// The bug these cover, observed live in session
// 01a03f1e-e215-7acd-81a9-6e4654d91325: a multi-session daemon has
// exactly one write door, POST /sessions/{sid}/inject, so an operator's
// typed "who are you?" and a watcher's k8s-event payload landed in the
// same queue and rendered byte-identically. The turn that answered them
// got the bundle framing — the machine-signal framing — whose branches
// all presupposed the message related to work already in flight. The
// agent had just triaged an OOMKill, so it answered three unrelated
// operator questions in a row with three variations of the same closed-
// incident recap.
//
// Two independent fixes, one per test group below: label who sent each
// bullet, and give the guidance a branch for "someone asked you
// something new".

// TestPrependInboxMessages_LabelsTheSender: the bullet has to say who
// sent it, or the model has nothing to distinguish a person's question
// from a watcher's signal. Fails on pre-fix code, which had no senders
// parameter at all.
func TestPrependInboxMessages_LabelsTheSender(t *testing.T) {
	t.Parallel()

	msgs := []string{"who are you?", `{"kind":"pod.oomkill","pod":"api-7d9"}`}
	senders := []string{"platform-oncall@example.com", "sa:lookout-watch"}

	for _, tc := range []struct {
		name            string
		bundleIsTheTurn bool
	}{
		{"bundle framing", true},
		{"bare list framing", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := prependInboxMessages("", msgs, senders, tc.bundleIsTheTurn)
			if !strings.Contains(got, "- from platform-oncall@example.com: who are you?") {
				t.Errorf("the operator's question is unattributed:\n%s", got)
			}
			if !strings.Contains(got, `- from sa:lookout-watch: {"kind":"pod.oomkill"`) {
				t.Errorf("the watcher's signal is unattributed:\n%s", got)
			}
		})
	}
}

// TestPrependInboxMessages_UnknownSendersRenderUnchanged: the legacy
// Inject path, the CLI, and any out-of-band caller queue without an
// identity, and single-user deployments are entirely made of those. A
// missing identity must cost nothing — not a label, not an empty
// "from : " prefix, not a byte.
func TestPrependInboxMessages_UnknownSendersRenderUnchanged(t *testing.T) {
	t.Parallel()

	msgs := []string{"deadline moved up to 14:00", "pause file writes"}
	want := "[Inbox]\n" +
		"- deadline moved up to 14:00\n" +
		"- pause file writes" +
		"\n\n---\n\n" +
		"what's next?"

	for _, tc := range []struct {
		name    string
		senders []string
	}{
		{"nil senders", nil},
		{"empty identities", []string{"", ""}},
		{"short slice", []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := prependInboxMessages("what's next?", msgs, tc.senders, false); got != want {
				t.Errorf("unattributed messages changed shape:\n got:  %q\n want: %q", got, want)
			}
		})
	}
}

// TestPrependInboxMessages_PartialSendersLabelOnlyWhatIsKnown: a batch
// can mix an identified inject with an unidentified one, and guessing a
// label for the second would be worse than leaving it off.
func TestPrependInboxMessages_PartialSendersLabelOnlyWhatIsKnown(t *testing.T) {
	t.Parallel()

	got := prependInboxMessages("", []string{"first", "second"}, []string{"", "sa:lookout-watch"}, true)
	if !strings.Contains(got, "- first\n") {
		t.Errorf("the unidentified message picked up a label:\n%s", got)
	}
	if !strings.Contains(got, "- from sa:lookout-watch: second\n") {
		t.Errorf("the identified message lost its label:\n%s", got)
	}
}

// TestInboxGuidance_AnswersNewQuestionsFirst: every pre-fix branch
// presupposed the message related to work already in flight — dedup it,
// fold it in, defer it, or wrap up — so a plain question fell through
// to the two branches that say do not re-open the work. The new branch
// has to come FIRST, because the model reads the list in order and the
// corroboration branch is very persuasive when everything looks like a
// signal.
func TestInboxGuidance_AnswersNewQuestionsFirst(t *testing.T) {
	t.Parallel()

	const newQuestion = "- A new question, request, or topic"
	i := strings.Index(inboxHandlingGuidance, newQuestion)
	if i < 0 {
		t.Fatalf("guidance has no branch for a message that is simply a new ask:\n%s", inboxHandlingGuidance)
	}
	corroboration := strings.Index(inboxHandlingGuidance, "- Corroborating detail")
	if corroboration < 0 {
		t.Fatalf("the #697 corroboration branch is load-bearing and must stay:\n%s", inboxHandlingGuidance)
	}
	if i > corroboration {
		t.Errorf("the new-question branch must precede the corroboration branch, which otherwise catches the question first:\n%s",
			inboxHandlingGuidance)
	}
	if !strings.Contains(inboxHandlingGuidance, "Summarizing work you already reported is not a response to a new question") {
		t.Errorf("the guidance does not name the observed failure mode — re-summarizing the last incident:\n%s",
			inboxHandlingGuidance)
	}
}

// TestRun_OperatorAndWatcherAreDistinguishable is the end-to-end shape
// of the live failure, on the path the daemon takes: two injects with
// different identities, drained into one wake-driven turn. The echo
// mock hands back the prompt the model saw.
func TestRun_OperatorAndWatcherAreDistinguishable(t *testing.T) {
	t.Parallel()
	a := newDeferredTestAgent(t)

	if err := a.InjectAs(`{"kind":"pod.oomkill","pod":"api-7d9"}`, auth.Caller{Identity: "sa:lookout-watch"}); err != nil {
		t.Fatalf("InjectAs: %v", err)
	}
	if err := a.InjectAs("can you help me with something else?", auth.Caller{Identity: "platform-oncall@example.com"}); err != nil {
		t.Fatalf("InjectAs: %v", err)
	}

	var saw string
	for ev, err := range a.Run(context.Background(), "") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ev == nil || ev.Content == nil || ev.Partial {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil {
				saw += p.Text
			}
		}
	}
	if !strings.Contains(saw, "from platform-oncall@example.com: can you help me with something else?") {
		t.Errorf("the operator's question reached the model with no attribution; got %q", saw)
	}
	if !strings.Contains(saw, "from sa:lookout-watch:") {
		t.Errorf("the watcher's signal reached the model with no attribution; got %q", saw)
	}
	if !strings.Contains(saw, "A new question, request, or topic") {
		t.Errorf("the turn's guidance had no branch for the operator's question; got %q", saw)
	}
}

// TestPrependInboxMessages_AutoContinueIsNotASender: auto-continue
// injects its "[system note] the turn was interrupted" through the same
// inbox, stamped with AutoContinueOriginator so the pause gate and the
// #624 stand-down can recognise it. That identity is bookkeeping, not a
// correspondent — labelling it would leak machinery in front of a note
// that already carries its own bracketed framing.
func TestPrependInboxMessages_AutoContinueIsNotASender(t *testing.T) {
	t.Parallel()

	note := "[system note] The previous turn was interrupted."
	got := prependInboxMessages("", []string{note, "and roll it back"},
		[]string{AutoContinueOriginator, "platform-oncall@example.com"}, true)

	if strings.Contains(got, AutoContinueOriginator) {
		t.Errorf("auto-continue's bookkeeping identity leaked into the model's prompt:\n%s", got)
	}
	if !strings.Contains(got, "- "+note+"\n") {
		t.Errorf("the system note changed shape:\n%s", got)
	}
	// A real sender in the same bundle still gets its label.
	if !strings.Contains(got, "- from platform-oncall@example.com: and roll it back") {
		t.Errorf("suppressing auto-continue's label suppressed everyone's:\n%s", got)
	}
}

// TestFormatAutoContinueInbox_StaysUnlabelled: the one surface where
// provenance was never ambiguous. Its header already says the notes are
// the operator's, so a per-bullet identity would be redundant — and
// auto-continue's own note is injected under AutoContinueOriginator,
// which is machinery the model has no reason to see.
func TestFormatAutoContinueInbox_StaysUnlabelled(t *testing.T) {
	t.Parallel()

	got := FormatAutoContinueInbox([]string{"stop", "actually, carry on"})
	// Bullets only — the shared guidance below them says "a message
	// from a person", which is prose, not a label.
	if strings.Contains(got, "- from ") {
		t.Errorf("auto-continue notes picked up sender labels:\n%s", got)
	}
	if !strings.Contains(got, "- stop\n") || !strings.Contains(got, "- actually, carry on\n") {
		t.Errorf("auto-continue bullets changed shape:\n%s", got)
	}
}
