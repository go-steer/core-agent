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
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

func runOnce(t *testing.T, r *rig) (outcome, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return r.d.loop(ctx)
}

// The A7 path end to end: a queued issue becomes a session, the agent's
// commit is pushed to the mirror by SHA, and the PR says what it fixes,
// where it came from, which session did it, and the session's plan.
func TestOnceOpensPRFromAgentCommits(t *testing.T) {
	r := newRig(t, commitAs())
	r.gh.seedIssue(1, "soak:expect-mergeable")
	plans := filepath.Join(r.cfg.AgentsDir, "plans")
	if err := os.MkdirAll(plans, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(plans, "plan-1.md"), "---\nplan: 1\nagent: \"core-agent\"\nsession: \"sess-1\"\n---\n\n1. Fix the recovery half.\n")

	out, err := runOnce(t, r)
	if err != nil || out.Stopped || out.PR == 0 {
		t.Fatalf("outcome %+v, err %v; want a PR", out, err)
	}
	tip := gitT(t, r.mirror, "rev-parse", "refs/heads/agent/issue-1")
	author := gitT(t, r.mirror, "log", "-1", "--format=%an <%ae>", tip)
	if author != testIdentity.String() {
		t.Errorf("pushed commit author %q, want %q", author, testIdentity)
	}
	prs := r.gh.createdPRs()
	if len(prs) != 1 {
		t.Fatalf("created %d PRs, want 1", len(prs))
	}
	body, _ := prs[0]["body"].(string)
	for _, want := range []string{"Fixes #1", testUpstream, "`sess-1`", "1. Fix the recovery half."} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "plan: 1") {
		t.Errorf("PR body carries the plan's frontmatter:\n%s", body)
	}
	if prs[0]["head"] != "agent/issue-1" || prs[0]["base"] != "main" {
		t.Errorf("PR head/base = %v/%v", prs[0]["head"], prs[0]["base"])
	}
	if prs[0]["title"] != "fix: recover the half that was broken" {
		t.Errorf("PR title %q, want the agent's commit subject", prs[0]["title"])
	}
	if !slices.Contains(r.gh.labelsOf(1), labelActive) {
		t.Errorf("issue labels %v, want %s kept while the PR is open", r.gh.labelsOf(1), labelActive)
	}
	if r.d.st.Active != nil || len(r.d.st.OpenPRs) != 1 {
		t.Errorf("state %+v, want no active issue and one open PR", r.d.st)
	}
}

// Decision 20: the task is the issue's title, body, number and upstream
// link — never its labels — and the working copy has no history to mine.
func TestTaskCarriesNoLabelsAndCloneIsShallow(t *testing.T) {
	var shallow, count, seedObj string
	agent := func(t *testing.T, dir string) []sseFrame {
		shallow = gitT(t, dir, "rev-parse", "--is-shallow-repository")
		count = gitT(t, dir, "rev-list", "--count", "--all")
		seedObj = gitT(t, dir, "log", "--all", "--format=%H", "--", "seeds.md")
		return commitAs()(t, dir)
	}
	r := newRig(t, agent)
	is := r.gh.seedIssue(1, "soak:expect-should-stop", "area/recovery")
	if _, err := runOnce(t, r); err != nil {
		t.Fatal(err)
	}
	tasks := r.daemon.injectedTasks()
	if len(tasks) != 1 {
		t.Fatalf("injected %d tasks, want 1", len(tasks))
	}
	task := tasks[0]
	for _, l := range is.Labels {
		if strings.Contains(task, l.Name) {
			t.Errorf("task names label %q:\n%s", l.Name, task)
		}
	}
	if strings.Contains(task, "soak:") || strings.Contains(task, "expect") {
		t.Errorf("task carries soak label text:\n%s", task)
	}
	for _, want := range []string{"#1", is.Title, "Make the recovery path work.", testUpstream, "agent/issue-1"} {
		if !strings.Contains(task, want) {
			t.Errorf("task lacks %q", want)
		}
	}
	if shallow != "true" || count != "1" || seedObj != "" {
		t.Errorf("working copy: shallow=%s commits=%s seed-history=%q; want a depth-1 clone with no seed list in reach", shallow, count, seedObj)
	}
}

// Decision 6, through the real poll: an issue someone else labeled, or
// someone else wrote, is never taken — even when it is the oldest.
func TestNeverForwardsAnIssueSomeoneElseLabeledOrWrote(t *testing.T) {
	r := newRig(t, commitAs())
	r.gh.seedIssue(1)
	r.gh.events[1][0].Actor = &ghUser{Login: "mallory"}
	wroteByOther := r.gh.seedIssue(2)
	wroteByOther.User = ghUser{Login: "mallory"}
	r.gh.seedIssue(3)
	r.daemon.dir = func() string { return filepath.Join(r.cfg.WorktreesDir, "issue-3") }

	out, err := runOnce(t, r)
	if err != nil || out.Issue != 3 || out.PR == 0 {
		t.Fatalf("outcome %+v, err %v; want issue 3 to get the PR", out, err)
	}
	for _, n := range []int{1, 2} {
		if got := r.gh.labelsOf(n); !slices.Equal(got, []string{labelQueue}) {
			t.Errorf("issue %d labels %v: it was touched", n, got)
		}
	}
}

func TestPauseHoldsTheQueue(t *testing.T) {
	r := newRig(t, commitAs())
	r.gh.seedIssue(1)
	pause := r.gh.seedIssue(9)
	pause.Labels = []ghLabel{{Name: labelPause}}
	pause.User = ghUser{Login: "anyone"} // a pause needs no provenance
	_, did, err := r.d.tick(context.Background())
	if err != nil || did {
		t.Fatalf("tick did=%v err=%v; want a paused no-op", did, err)
	}
	if len(r.daemon.injectedTasks()) != 0 || slices.Contains(r.gh.labelsOf(1), labelActive) {
		t.Fatal("a paused dispatcher claimed an issue")
	}
	r.gh.mu.Lock()
	pause.State = "closed"
	r.gh.mu.Unlock()
	if out, err := runOnce(t, r); err != nil || out.PR == 0 {
		t.Fatalf("after unpause: %+v, %v", out, err)
	}
}

func TestOpenPRCapHoldsTheQueue(t *testing.T) {
	r := newRig(t, commitAs())
	r.gh.seedIssue(1)
	r.gh.mu.Lock()
	r.gh.openPull(11, "agent/issue-11")
	r.gh.openPull(12, "agent/issue-12")
	r.gh.openPull(13, "agent/issue-13")
	r.gh.openPull(14, "someone/else") // not an agent PR: does not count
	r.gh.mu.Unlock()
	_, did, err := r.d.tick(context.Background())
	if err != nil || did || len(r.daemon.injectedTasks()) != 0 {
		t.Fatalf("at the cap: did=%v err=%v injected=%d; want no new work", did, err, len(r.daemon.injectedTasks()))
	}
	r.gh.mu.Lock()
	r.gh.pulls[13].State = "closed"
	r.gh.mu.Unlock()
	if out, err := runOnce(t, r); err != nil || out.PR == 0 {
		t.Fatalf("below the cap: %+v, %v", out, err)
	}
}

// Decision 16: a branch carrying any identity but the soak's — here the
// writer App's own bot noreply — is refused, never pushed, and the issue
// stops with the reason on it.
func TestRefusesToPushCommitsWithAnotherIdentity(t *testing.T) {
	r := newRig(t, commitAs("-c", "user.name=writer[bot]", "-c", "user.email=123+writer[bot]@users.noreply.github.com"))
	r.gh.seedIssue(1)
	out, err := runOnce(t, r)
	if err != nil || !out.Stopped {
		t.Fatalf("outcome %+v, err %v; want a stop", out, err)
	}
	if !strings.Contains(out.Reason, "refusing to push") || !strings.Contains(out.Reason, "bot or noreply") {
		t.Errorf("reason %q", out.Reason)
	}
	if _, err := os.Stat(filepath.Join(r.mirror, "refs/heads/agent/issue-1")); err == nil {
		t.Error("the refused branch reached the mirror")
	}
	assertStopped(t, r, 1, "refusing to push")
}

func assertStopped(t *testing.T, r *rig, n int, reason string) {
	t.Helper()
	labels := r.gh.labelsOf(n)
	if !slices.Contains(labels, labelStopped) || slices.Contains(labels, labelActive) {
		t.Errorf("labels %v, want %s and not %s", labels, labelStopped, labelActive)
	}
	cs := r.gh.commentsOf(n)
	if len(cs) != 1 || !strings.Contains(cs[0], reason) || !strings.Contains(cs[0], "decision 17") {
		t.Errorf("comments %q, want one naming %q", cs, reason)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.WorktreesDir, "issue-1")); !os.IsNotExist(err) {
		t.Errorf("working copy still present after the stop (err %v)", err)
	}
	if len(r.gh.createdPRs()) != 0 {
		t.Error("a stopped issue got a PR")
	}
	if r.d.st.Active != nil {
		t.Error("state still holds the stopped issue")
	}
}

// Step 7: a guardrail trip read from the session's durable row (#1258)
// stops the issue even though the agent committed.
func TestGuardrailTripStopsTheIssue(t *testing.T) {
	agent := func(t *testing.T, dir string) []sseFrame {
		commitAs()(t, dir)
		row := attach.NewGuardrailTurnTripEvent("cost_ceiling", "per-turn cost ceiling $10.00 reached", true)
		return []sseFrame{durableRow(7, row), {Event: attach.EventTurnError, Data: attach.TurnError{Kind: attach.TurnErrorCanceled, Message: "turn canceled"}}}
	}
	r := newRig(t, agent)
	r.gh.seedIssue(1)
	out, err := runOnce(t, r)
	if err != nil || !out.Stopped {
		t.Fatalf("outcome %+v, err %v; want a stop", out, err)
	}
	assertStopped(t, r, 1, "per-turn cost ceiling $10.00 reached")
	if _, err := os.Stat(filepath.Join(r.mirror, "refs/heads/agent/issue-1")); err == nil {
		t.Error("a tripped session's branch was pushed")
	}
}

func TestNoCommitsStopsTheIssue(t *testing.T) {
	r := newRig(t, func(*testing.T, string) []sseFrame { return []sseFrame{turnComplete()} })
	r.gh.seedIssue(1)
	out, err := runOnce(t, r)
	if err != nil || !out.Stopped || !strings.Contains(out.Reason, "no commits past the base") {
		t.Fatalf("outcome %+v, err %v", out, err)
	}
	assertStopped(t, r, 1, "no commits past the base")
}

func TestMissingUpstreamLinkStopsWithoutASession(t *testing.T) {
	r := newRig(t, commitAs())
	is := r.gh.seedIssue(1)
	is.Body = "no link here"
	out, err := runOnce(t, r)
	if err != nil || !out.Stopped || !strings.Contains(out.Reason, "decision 12") {
		t.Fatalf("outcome %+v, err %v", out, err)
	}
	if r.daemon.created != 0 {
		t.Error("a session was created for an issue with no upstream link")
	}
}

// --once maps a stop to a non-zero exit and a PR to zero.
func TestRunLoopOnceExitSemantics(t *testing.T) {
	r := newRig(t, func(*testing.T, string) []sseFrame { return []sseFrame{turnComplete()} })
	r.gh.seedIssue(1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := r.d.runLoop(ctx); !errors.Is(err, errStopped) {
		t.Fatalf("runLoop on a stopped issue = %v, want errStopped", err)
	}
	r2 := newRig(t, commitAs())
	r2.gh.seedIssue(1)
	if err := r2.d.runLoop(ctx); err != nil {
		t.Fatalf("runLoop on a PR = %v, want nil", err)
	}
}

// A stop whose GitHub calls fail is kept and retried, not forgotten.
func TestStopIsRetriedAfterAGitHubFailure(t *testing.T) {
	r := newRig(t, commitAs())
	r.gh.seedIssue(1)
	if _, err := r.d.tokens.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := &activeIssue{Number: 1, Title: "x", StartedAt: time.Now()}
	r.d.st.Active = a
	r.gh.mu.Lock()
	r.gh.failRepo = true
	r.gh.mu.Unlock()
	if _, err := r.d.stop(context.Background(), a, "watchdog: runaway"); err == nil {
		t.Fatal("stop reported success while GitHub failed")
	}
	if r.d.st.Active == nil || r.d.st.Active.StopReason == "" {
		t.Fatal("the stop was forgotten")
	}
	r.gh.mu.Lock()
	r.gh.failRepo = false
	r.gh.mu.Unlock()
	out, did, err := r.d.tick(context.Background())
	if err != nil || !did || !out.Stopped {
		t.Fatalf("retry: %+v did=%v err=%v", out, did, err)
	}
	assertStopped(t, r, 1, "watchdog: runaway")
}

// A dispatcher restart between the claim and the inject gives the issue
// back instead of stopping it: nothing was attempted.
func TestRestartBeforeInjectReleasesTheClaim(t *testing.T) {
	r := newRig(t, commitAs())
	r.gh.seedIssue(1, labelActive)
	if _, err := r.d.tokens.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.d.st.Active = &activeIssue{Number: 1, StartedAt: time.Now()}
	if _, _, err := r.d.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(r.gh.labelsOf(1), labelActive) || r.d.st.Active != nil {
		t.Fatalf("labels %v active %+v; want the claim released", r.gh.labelsOf(1), r.d.st.Active)
	}
	if out, err := runOnce(t, r); err != nil || out.PR == 0 {
		t.Fatalf("re-run after release: %+v, %v", out, err)
	}
}

// Step 7's cleanup: a working copy outlives its open PR and goes when the
// PR merges.
func TestReapRemovesTheWorkingCopyWhenThePRCloses(t *testing.T) {
	r := newRig(t, commitAs())
	r.gh.seedIssue(1)
	out, err := runOnce(t, r)
	if err != nil || out.PR == 0 {
		t.Fatalf("%+v %v", out, err)
	}
	dir := filepath.Join(r.cfg.WorktreesDir, "issue-1")
	r.d.reap(context.Background())
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("working copy removed while its PR is open: %v", err)
	}
	r.gh.mu.Lock()
	merged := time.Now()
	r.gh.pulls[out.PR].State, r.gh.pulls[out.PR].MergedAt = "closed", &merged
	r.gh.mu.Unlock()
	r.d.reap(context.Background())
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("working copy still present after merge (err %v)", err)
	}
	if len(r.d.st.OpenPRs) != 0 {
		t.Errorf("open PRs %v after merge", r.d.st.OpenPRs)
	}
}
