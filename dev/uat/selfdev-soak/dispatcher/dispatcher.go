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
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// dispatcher runs A7's steps 0–3 and 7 of docs/selfdev-soak-design.md.
// Steps 4–6 (CI, review and rebase re-wakes) and 8 (the nightly harvest)
// are not here; see docs/selfdev-soak-dispatcher-design.md for where each
// one attaches.
type dispatcher struct {
	cfg    config
	gh     *ghClient
	tokens tokenSource
	daemon *daemonClient
	git    *gitOps
	log    *slog.Logger
	st     *state

	// skipped remembers which ineligible issues were already logged, so a
	// poll every two minutes does not repeat the same refusal forever.
	skipped map[int]string
}

// outcome is what processing one issue came to.
type outcome struct {
	Issue   int
	PR      int
	PRURL   string
	Stopped bool
	Reason  string
}

// loop polls until ctx ends, or — with --once — until one issue has been
// processed end to end.
func (d *dispatcher) loop(ctx context.Context) (outcome, error) {
	for {
		out, did, err := d.tick(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return out, ctx.Err()
			}
			d.log.Error("poll failed", "err", err)
		}
		if did && d.cfg.Once {
			return out, err
		}
		select {
		case <-ctx.Done():
			return outcome{}, ctx.Err()
		case <-time.After(d.cfg.Poll):
		}
	}
}

// tick is one poll. did reports whether an issue was processed to an end
// (a PR or a stop) during it.
func (d *dispatcher) tick(ctx context.Context) (out outcome, did bool, err error) {
	d.reap(ctx)
	d.flushComments(ctx)
	if paused, err := d.paused(ctx); err != nil || paused {
		if paused {
			d.log.Info("paused: an open issue carries " + labelPause)
		}
		return outcome{}, false, err
	}
	if d.st.Active != nil {
		out, err := d.resume(ctx)
		return out, out.Issue != 0, err
	}
	open, err := d.openAgentPRs(ctx)
	if err != nil {
		return outcome{}, false, err
	}
	if open >= d.cfg.MaxOpenPRs {
		d.log.Info("at the open agent PR cap; not taking new work", "open", open, "cap", d.cfg.MaxOpenPRs)
		return outcome{}, false, nil
	}
	is, err := d.next(ctx)
	if err != nil || is == nil {
		return outcome{}, false, err
	}
	out, err = d.process(ctx, *is)
	return out, true, err
}

// paused is step 0: any open issue labeled soak:pause holds the
// dispatcher. Deliberately not provenance-checked — a pause is the safe
// direction, and the soft kill switch should be easy to pull.
func (d *dispatcher) paused(ctx context.Context) (bool, error) {
	issues, err := d.gh.openIssues(ctx, labelPause)
	if err != nil {
		return false, fmt.Errorf("check %s: %w", labelPause, err)
	}
	return len(issues) > 0, nil
}

// waitUnpaused blocks while the dispatcher is paused, so a pause that
// lands mid-issue holds the push as well as the next claim.
func (d *dispatcher) waitUnpaused(ctx context.Context) error {
	for {
		paused, err := d.paused(ctx)
		if err == nil && !paused {
			return nil
		}
		if err != nil {
			d.log.Warn("pause check failed; holding", "err", err)
		} else {
			d.log.Info("paused before pushing: an open issue carries " + labelPause)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.cfg.Poll):
		}
	}
}

// openAgentPRs counts the open PRs from the mirror's own agent/ branches:
// the parallel-PR cap of step 1.
func (d *dispatcher) openAgentPRs(ctx context.Context) (int, error) {
	pulls, err := d.gh.openPulls(ctx)
	if err != nil {
		return 0, fmt.Errorf("list open PRs: %w", err)
	}
	n := 0
	for _, p := range pulls {
		if strings.HasPrefix(p.Head.Ref, "agent/") && p.Head.Repo != nil && strings.EqualFold(p.Head.Repo.FullName, d.cfg.Owner+"/"+d.cfg.Repo) {
			n++
		}
	}
	return n, nil
}

// next returns the oldest issue that is claimable and passes decision 6,
// or nil when there is none.
func (d *dispatcher) next(ctx context.Context) (*ghIssue, error) {
	issues, err := d.gh.openIssues(ctx, labelQueue)
	if err != nil {
		return nil, fmt.Errorf("list %s issues: %w", labelQueue, err)
	}
	for i := range issues {
		is := issues[i]
		if !claimable(is) {
			continue
		}
		events, err := d.gh.issueEvents(ctx, is.Number)
		if err != nil {
			return nil, fmt.Errorf("issue #%d events: %w", is.Number, err)
		}
		err = checkProvenance(is, events, d.cfg.Maintainer)
		if err == nil {
			editors, total, gerr := d.gh.contentEditors(ctx, is.Number)
			if gerr != nil {
				return nil, fmt.Errorf("issue #%d body edits: %w", is.Number, gerr)
			}
			err = checkEditors(editors, total, d.cfg.Maintainer)
		}
		if err != nil {
			if d.skipped[is.Number] != err.Error() {
				d.skipped[is.Number] = err.Error()
				d.log.Warn("not forwarding issue (decision 6)", "issue", is.Number, "why", err.Error())
			}
			continue
		}
		return &is, nil
	}
	return nil, nil
}

// process is steps 2, 3 and 7 for one freshly picked issue.
func (d *dispatcher) process(ctx context.Context, is ghIssue) (outcome, error) {
	log := d.log.With("issue", is.Number)
	a := &activeIssue{
		Number: is.Number, Title: is.Title, StartedAt: time.Now().UTC(),
		Upstream: upstreamLink(is.Body, d.cfg.Upstream),
	}
	// State first, then the label: a crash or a failed label call in
	// between leaves an un-injected active issue, which the next poll
	// releases. The other order could leave a labeled issue nothing
	// remembers, stuck as claimed forever.
	d.st.Active = a
	if err := d.save(); err != nil {
		d.st.Active = nil
		return outcome{}, err
	}
	if err := d.gh.addLabels(ctx, is.Number, labelActive); err != nil {
		return outcome{}, fmt.Errorf("claim issue #%d: %w", is.Number, err)
	}
	log.Info("claimed issue", "title", is.Title)
	if a.Upstream == "" {
		return d.stop(ctx, a, "the issue names no "+d.cfg.Upstream+" issue link, and every soak issue must carry one (decision 12)")
	}
	return d.start(ctx, a, is.Body)
}

// start prepares the working copy, opens the session, and runs the task.
func (d *dispatcher) start(ctx context.Context, a *activeIssue, body string) (outcome, error) {
	if err := d.prepare(ctx, a); err != nil {
		if ctx.Err() != nil {
			return outcome{}, ctx.Err()
		}
		return d.stop(ctx, a, "the dispatcher could not start the session: "+err.Error())
	}
	task := buildTask(taskInput{
		Number: a.Number, Title: a.Title, Body: body, Upstream: a.Upstream,
		Repo: d.cfg.Owner + "/" + d.cfg.Repo, Branch: branchFor(a.Number), Dir: a.Dir, BaseSHA: a.BaseSHA,
	})
	// Recorded before the inject: a restart after this point resumes the
	// watch instead of injecting the task twice.
	a.Injected = true
	if err := d.save(); err != nil {
		return outcome{}, err
	}
	d.log.Info("injecting task", "issue", a.Number, "session", a.SessionID, "dir", a.Dir, "base", a.BaseSHA)
	end, err := d.daemon.runTask(ctx, a.SessionPath, task, d.cfg.SessionTimeout)
	return d.finish(ctx, a, end, err)
}

func (d *dispatcher) prepare(ctx context.Context, a *activeIssue) error {
	tok, err := d.tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("mint the App token: %w", err)
	}
	if a.BaseSHA, err = d.git.fetchBase(ctx, tok); err != nil {
		return fmt.Errorf("fetch the mirror's %s: %w", d.cfg.BaseBranch, err)
	}
	if a.Dir, err = d.git.prepareClone(ctx, a.Number); err != nil {
		return fmt.Errorf("make the working copy: %w", err)
	}
	if a.SessionPath, a.SessionID, err = d.daemon.createSession(ctx); err != nil {
		return err
	}
	return d.save()
}

// resume continues the active issue after a dispatcher restart.
func (d *dispatcher) resume(ctx context.Context) (outcome, error) {
	a := d.st.Active
	switch {
	case a.StopReason != "":
		return d.stop(ctx, a, a.StopReason)
	case !a.Injected:
		// The task never reached a session, so nothing was attempted and
		// there is no stop to hide: give the issue back to the queue.
		d.log.Info("releasing an issue whose task was never delivered", "issue", a.Number)
		if err := d.gh.removeLabel(ctx, a.Number, labelActive); err != nil {
			return outcome{}, err
		}
		_ = d.git.removeClone(a.Number)
		d.st.Active = nil
		return outcome{}, d.save()
	}
	remaining := d.cfg.SessionTimeout - time.Since(a.StartedAt)
	if remaining < d.daemon.settle {
		remaining = d.daemon.settle
	}
	d.log.Info("resuming the watch on an active session", "issue", a.Number, "session", a.SessionID)
	end, err := d.daemon.runTask(ctx, a.SessionPath, "", remaining)
	return d.finish(ctx, a, end, err)
}

// finish is step 3 (or 7): the session is idle; push and open the PR, or
// stop with the reason.
func (d *dispatcher) finish(ctx context.Context, a *activeIssue, end sessionEnd, err error) (outcome, error) {
	if err != nil {
		if ctx.Err() != nil {
			return outcome{}, ctx.Err() // shutting down: the state file resumes it
		}
		return d.stop(ctx, a, "the dispatcher lost the session: "+err.Error())
	}
	if reason := end.stopReason(); reason != "" {
		return d.stop(ctx, a, reason)
	}
	if err := d.waitUnpaused(ctx); err != nil {
		return outcome{}, err
	}
	return d.publish(ctx, a)
}

// publish verifies the agent's commits, pushes them by SHA, and opens the
// PR.
func (d *dispatcher) publish(ctx context.Context, a *activeIssue) (outcome, error) {
	tip, commits, err := d.git.collect(ctx, a.Number, a.Dir, a.BaseSHA)
	if err != nil {
		return d.stop(ctx, a, "could not read the agent's branch: "+err.Error())
	}
	err = verifyCommits(commits, d.cfg.Identity)
	if err == nil {
		err = verifyCommitTexts(commits, a.Number, d.cfg.Owner+"/"+d.cfg.Repo)
	}
	if err != nil {
		return d.stop(ctx, a, "refusing to push "+branchFor(a.Number)+": "+err.Error())
	}
	tok, err := d.tokens.Token(ctx)
	if err == nil {
		err = d.git.push(ctx, a.Number, tip, tok)
	}
	if err != nil {
		return d.stop(ctx, a, "push failed: "+err.Error())
	}
	d.log.Info("pushed", "issue", a.Number, "branch", branchFor(a.Number), "tip", tip, "commits", len(commits))
	pr, err := d.openOrAdoptPR(ctx, a, commits)
	if err != nil {
		return d.stop(ctx, a, "pushed "+branchFor(a.Number)+" but could not open the PR: "+err.Error())
	}
	d.st.OpenPRs = append(d.st.OpenPRs, openPR{Issue: a.Number, PR: pr.Number, SessionPath: a.SessionPath, BaseSHA: a.BaseSHA})
	d.st.Active = nil
	if err := d.save(); err != nil {
		return outcome{}, err
	}
	d.log.Info("opened PR", "issue", a.Number, "pr", pr.Number, "url", pr.HTMLURL, "session", a.SessionID)
	return outcome{Issue: a.Number, PR: pr.Number, PRURL: pr.HTMLURL}, nil
}

// openOrAdoptPR opens the issue's PR, or adopts the open one already on
// its branch: a restart after the PR was opened but before the state file
// recorded it must not end in a duplicate-PR failure and a false stop.
func (d *dispatcher) openOrAdoptPR(ctx context.Context, a *activeIssue, commits []commit) (ghPull, error) {
	existing, err := d.gh.openPullForHead(ctx, branchFor(a.Number))
	if err != nil {
		return ghPull{}, err
	}
	if existing != nil {
		d.log.Info("adopting the PR already open on the branch", "issue", a.Number, "pr", existing.Number)
		return *existing, nil
	}
	plan := neutralizeClosers(planFor(d.cfg.AgentsDir, a.SessionID))
	if line := attributionLine(plan); line != "" {
		d.log.Warn("withholding the plan from the PR body: it carries agent attribution", "issue", a.Number)
		plan = "(The session's plan artifact was withheld: it carries text the attribution check fails.)"
	}
	body := prBody(a, d.cfg.Owner+"/"+d.cfg.Repo, plan)
	if line := attributionLine(body); line != "" {
		return ghPull{}, fmt.Errorf("the PR body fails the attribution check (%q)", truncate(line, 120))
	}
	return d.gh.createPull(ctx, prTitle(a, commits), branchFor(a.Number), d.cfg.BaseBranch, body)
}

// stop is step 7: say why on the issue, label it soak:stopped, drop the
// claim and the working copy, and move on. The reason is persisted first
// so a GitHub failure here is retried on the next poll, never lost.
//
// The reason is capped: part of it can be agent-controlled (a turn
// error's message, a commit header), and a comment over GitHub's size
// limit is refused, which used to wedge the queue on a stop that could
// never complete. The comment is also posted last and retried on its
// own (flushComments), so a comment failure never holds the labels, the
// cleanup, or the next issue.
func (d *dispatcher) stop(ctx context.Context, a *activeIssue, reason string) (outcome, error) {
	reason = truncate(reason, maxReasonBytes)
	a.StopReason = reason
	if err := d.save(); err != nil {
		return outcome{}, err
	}
	d.log.Warn("stopping issue", "issue", a.Number, "session", a.SessionID, "reason", reason)
	if a.SessionPath != "" {
		// Before the copy goes: a session still working would otherwise
		// keep editing a directory that no longer exists.
		d.daemon.interrupt(ctx, a.SessionPath)
	}
	if err := d.gh.addLabels(ctx, a.Number, labelStopped); err != nil {
		return outcome{}, fmt.Errorf("label #%d %s: %w", a.Number, labelStopped, err)
	}
	if err := d.gh.removeLabel(ctx, a.Number, labelActive); err != nil {
		return outcome{}, fmt.Errorf("unlabel #%d %s: %w", a.Number, labelActive, err)
	}
	if err := d.git.removeClone(a.Number); err != nil {
		d.log.Warn("could not remove the working copy", "issue", a.Number, "err", err)
	}
	d.st.PendingComments = append(d.st.PendingComments, pendingComment{Issue: a.Number, Body: stopComment(a, reason)})
	d.st.Active = nil
	if err := d.save(); err != nil {
		return outcome{}, err
	}
	d.flushComments(ctx)
	return outcome{Issue: a.Number, Stopped: true, Reason: reason}, nil
}

// maxReasonBytes caps a stop reason. The comment around it stays far
// below GitHub's 65536-character body limit.
const maxReasonBytes = 2000

// maxCommentAttempts bounds the retries of one stop comment. The issue is
// already labeled soak:stopped, so a comment that can never post is
// logged and dropped rather than retried forever.
const maxCommentAttempts = 10

// flushComments posts the queued stop comments, keeping the ones that
// fail for the next poll.
func (d *dispatcher) flushComments(ctx context.Context) {
	if len(d.st.PendingComments) == 0 {
		return
	}
	var kept []pendingComment
	for _, p := range d.st.PendingComments {
		err := d.gh.comment(ctx, p.Issue, p.Body)
		if err == nil {
			continue
		}
		p.Attempts++
		if p.Attempts >= maxCommentAttempts {
			d.log.Error("giving up on a stop comment; the issue is labeled soak:stopped", "issue", p.Issue, "err", err)
			continue
		}
		d.log.Warn("stop comment failed; will retry", "issue", p.Issue, "attempt", p.Attempts, "err", err)
		kept = append(kept, p)
	}
	d.st.PendingComments = kept
	if err := d.save(); err != nil {
		d.log.Error("save state", "err", err)
	}
}

// reap removes the working copy of every PR that has merged or closed.
func (d *dispatcher) reap(ctx context.Context) {
	kept := d.st.OpenPRs[:0]
	for _, o := range d.st.OpenPRs {
		pr, err := d.gh.pull(ctx, o.PR)
		if err != nil || pr.State != "closed" {
			if err != nil {
				d.log.Warn("PR state check failed", "pr", o.PR, "err", err)
			}
			kept = append(kept, o)
			continue
		}
		if err := d.git.removeClone(o.Issue); err != nil {
			d.log.Warn("could not remove the working copy", "issue", o.Issue, "err", err)
		}
		d.log.Info("PR closed; removed its working copy", "issue", o.Issue, "pr", o.PR, "merged", pr.MergedAt != nil)
	}
	if len(kept) != len(d.st.OpenPRs) {
		d.st.OpenPRs = kept
		if err := d.save(); err != nil {
			d.log.Error("save state", "err", err)
		}
	}
}

func (d *dispatcher) save() error {
	if err := saveState(d.cfg.StateFile, d.st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// errStopped is returned by --once when its one issue stopped, so the
// exit code says the run did not open a PR.
var errStopped = errors.New("the issue stopped")
