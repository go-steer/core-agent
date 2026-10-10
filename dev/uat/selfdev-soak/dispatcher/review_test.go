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

//go:build unix

package main

// Regression tests for the adversarial review's findings on the first
// draft. Each names the finding it pins.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// P1: separator bytes in a commit message or identity must not let a
// foreign trailer or identity past the check. The first draft parsed a
// `git log --format` rendering split on \x1f/\x1e, and both forgeries
// below verified clean.
func TestForgedSeparatorsCannotHideAnotherIdentity(t *testing.T) {
	cases := map[string]agentFunc{
		"record separator hides a co-author trailer": func(t *testing.T, dir string) []sseFrame {
			writeFile(t, filepath.Join(dir, "fix.go"), "package fix\n")
			gitT(t, dir, "add", "fix.go")
			msg := filepath.Join(t.TempDir(), "msg")
			writeFile(t, msg, "fix: thing\n\nSigned-off-by: "+testIdentity.String()+"\n\x1e\nCo-authored-by: Claude <noreply@anthropic.com>\n")
			gitT(t, dir, "commit", "--quiet", "--cleanup=verbatim", "-F", msg)
			return []sseFrame{turnComplete()}
		},
		"field separators in the name forge the soak identity": func(t *testing.T, dir string) []sseFrame {
			writeFile(t, filepath.Join(dir, "fix.go"), "package fix\n")
			gitT(t, dir, "add", "fix.go")
			forged := testIdentity.Name + "\x1f" + testIdentity.Email + "\x1f" + testIdentity.Name + "\x1f" + testIdentity.Email + "\x1f"
			gitT(t, dir, "-c", "user.name="+forged, "-c", "user.email=evil@bot.example", "commit", "--quiet", "-m", "fix: thing\n\nSigned-off-by: "+testIdentity.String())
			return []sseFrame{turnComplete()}
		},
	}
	for name, agent := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, agent)
			r.gh.seedIssue(1)
			out, err := runOnce(t, r)
			if err != nil || !out.Stopped || !strings.Contains(out.Reason, "refusing to push") && !strings.Contains(out.Reason, "control byte") {
				t.Fatalf("outcome %+v, err %v; want the branch refused", out, err)
			}
			if _, err := os.Stat(filepath.Join(r.mirror, "refs/heads/agent/issue-1")); err == nil {
				t.Fatal("the forged branch reached the mirror")
			}
		})
	}
}

func TestParseCommitObject(t *testing.T) {
	raw := "tree abc\nparent def\nauthor Soak Worker <soak-worker@example.com> 1700000000 +0000\n" +
		"committer Soak Worker <soak-worker@example.com> 1700000000 +0000\ngpgsig -----BEGIN-----\n line\n -----END-----\n\nfix: x\n\nbody\n"
	c, err := parseCommitObject("sha", raw)
	if err != nil || c.AuthorName != "Soak Worker" || c.CommitterEmail != "soak-worker@example.com" || !strings.HasPrefix(c.Message, "fix: x") {
		t.Fatalf("%+v %v", c, err)
	}
	for name, bad := range map[string]string{
		"two authors":   "author A <a@x> 1 +0000\nauthor B <b@x> 1 +0000\ncommitter A <a@x> 1 +0000\n\nm",
		"no committer":  "author A <a@x> 1 +0000\n\nm",
		"control byte":  "author A\x1fB <a@x> 1 +0000\ncommitter A <a@x> 1 +0000\n\nm",
		"stray bracket": "author A <a@x> <b@y> 1 +0000\ncommitter A <a@x> 1 +0000\n\nm",
	} {
		if _, err := parseCommitObject("sha", bad); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

// P2: on a resume (replay from seq 0), a turn error that a later turn
// recovered from must not stop the issue.
func TestWatcherForgetsATurnErrorALaterTurnRecoveredFrom(t *testing.T) {
	w := newWatcher(time.Now())
	w.reconnected = true
	te := attach.NewTurnErrorEvent(attach.TurnError{Kind: attach.TurnErrorRateLimited, Message: "429"}, "p1", "")
	w.observe(attach.Frame{Seq: 3, Event: te}, time.Now())
	later := modelRow("all done")
	w.observe(attach.Frame{Seq: 9, Event: later}, time.Now())
	if w.end.TurnError != nil {
		t.Fatalf("stale turn error kept: %+v", w.end.TurnError)
	}
	user := modelRow("operator text")
	user.Author = "user"
	w.observe(attach.Frame{Seq: 10, Event: te}, time.Now())
	w.observe(attach.Frame{Seq: 11, Event: user}, time.Now())
	if w.end.TurnError == nil {
		t.Fatal("a user row cleared a turn error that no model output followed")
	}
}

func modelRow(text string) *session.Event {
	ev := session.NewEvent(context.Background(), "turn")
	ev.Author = "core-agent"
	ev.Content = genai.NewContentFromText(text, genai.RoleModel)
	return ev
}

// P2: a turn-complete lost in a reconnect gap, or on a stream that died
// silently, must not turn finished work into a "never ran" stop.
func TestWatcherLostTurnCompleteIsNotNeverRan(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	settle, start := 30*time.Second, 10*time.Minute
	idle := attach.StatusInfo{State: attach.AgentStateIdle}

	w := newWatcher(t0)
	w.observe(attach.Frame{Type: attach.EventStatusUpdate, TypedData: &attach.StatusUpdate{TurnState: attach.TurnStateStreaming}}, t0.Add(time.Minute))
	w.reconnected = true
	if !w.done(idle, t0.Add(time.Minute+settle), t0, settle, start) || w.end.NeverRan {
		t.Fatalf("after a reconnect: done=false or NeverRan=%v", w.end.NeverRan)
	}

	silent := newWatcher(t0)
	silent.observe(attach.Frame{Type: attach.EventStatusUpdate, TypedData: &attach.StatusUpdate{TurnState: attach.TurnStateStreaming}}, t0.Add(time.Minute))
	if silent.done(idle, t0.Add(2*time.Minute), t0, settle, start) {
		t.Fatal("done on activity alone, before start-timeout of quiet")
	}
	if !silent.done(idle, t0.Add(12*time.Minute), t0, settle, start) || silent.end.NeverRan {
		t.Fatalf("a dead stream after activity: NeverRan=%v", silent.end.NeverRan)
	}
}

// P2: the plans directory is agent-writable; a symlink there must not be
// followed into the dispatcher's filesystem, and a FIFO must not hang it.
func TestPlanForRefusesLinksAndSpecialFiles(t *testing.T) {
	agents := t.TempDir()
	plans := filepath.Join(agents, "plans")
	if err := os.MkdirAll(plans, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "app-key.pem")
	writeFile(t, secret, "---\nsession: \"sess-1\"\n---\n\nBEGIN PRIVATE KEY\n")
	if err := os.Symlink(secret, filepath.Join(plans, "plan-9.md")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(plans, "plan-8.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(plans, "plan-7.md"), "---\nplan: 7\nsession: \"other\"\n---\n\nnot mine\n")
	writeFile(t, filepath.Join(plans, "plan-6.md"), "---\nplan: 6\nsession: \"sess-1\"\n---\n\nmy plan\n")
	writeFile(t, filepath.Join(plans, "plan-10-revoked.md"), "---\nsession: \"sess-1\"\n---\n\nrevoked\n")
	done := make(chan string, 1)
	go func() { done <- planFor(agents, "sess-1") }()
	select {
	case got := <-done:
		if got != "my plan" {
			t.Fatalf("planFor = %q, want the regular file's plan", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("planFor hung on a FIFO")
	}

	linked := t.TempDir()
	if err := os.Symlink(plans, filepath.Join(linked, "plans")); err != nil {
		t.Fatal(err)
	}
	if got := planFor(linked, "sess-1"); got != "" {
		t.Fatalf("followed a symlinked plans directory: %q", got)
	}
}

// P3: provenance on page 2 of the events is still read, and an event
// history past the page bound is an error, not a truncated pass.
func TestProvenanceReadsEveryEventPage(t *testing.T) {
	r := newRig(t, commitAs())
	r.gh.seedIssue(1)
	for i := 0; i < 140; i++ {
		r.gh.events[1] = append(r.gh.events[1], ghEvent{Event: "mentioned", Actor: &ghUser{Login: "someone"}})
	}
	r.gh.events[1] = append(r.gh.events[1], ghEvent{Event: "labeled", Actor: &ghUser{Login: "mallory"}, Label: &ghLabel{Name: labelQueue}})
	if _, err := r.d.tokens.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	is, err := r.d.next(context.Background())
	if err != nil || is != nil {
		t.Fatalf("next = %+v, %v; want the page-2 label event to disqualify the issue", is, err)
	}
	for i := 0; i < maxPages*pageSize; i++ {
		r.gh.events[1] = append(r.gh.events[1], ghEvent{Event: "mentioned", Actor: &ghUser{Login: "someone"}})
	}
	if _, err := r.d.next(context.Background()); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("an over-long history: %v, want an error", err)
	}
}

// P3: a restart after the PR opened but before the state file recorded it
// adopts the PR instead of failing on a duplicate and stopping the issue.
func TestPublishAdoptsAnAlreadyOpenPR(t *testing.T) {
	r := newRig(t, commitAs())
	r.gh.seedIssue(1)
	r.gh.mu.Lock()
	r.gh.openPull(55, "agent/issue-1")
	r.gh.mu.Unlock()
	out, err := runOnce(t, r)
	if err != nil || out.PR != 55 || len(r.gh.createdPRs()) != 0 {
		t.Fatalf("outcome %+v, err %v, created %d; want PR 55 adopted", out, err, len(r.gh.createdPRs()))
	}
}

// P3: a signal ends --once without its PR, which is not success.
func TestOnceInterruptedIsAFailure(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if exitCode(context.Canceled, true, log) != 1 || exitCode(context.Canceled, false, log) != 0 || exitCode(nil, true, log) != 0 {
		t.Fatal("exit codes")
	}
}
