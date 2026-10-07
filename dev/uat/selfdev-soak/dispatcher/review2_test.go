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

// Regression tests for the independent review of PR #1272 (cdc84b07).

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// secretFile stands in for the App key: a file the dispatcher can read
// and the agent can name.
func secretFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "app-key.pem")
	writeFile(t, p, "-----BEGIN RSA PRIVATE KEY-----\n"+keyLine+"\n-----END RSA PRIVATE KEY-----\n")
	return p
}

// leakAgents plant, in the copy's .git, a file git echoes when it fails to
// parse it, pointed at the secret (reviewer's scripts a.sh, b.sh, d.sh).
func leakAgents(secret string) map[string]agentFunc {
	// breakObject forces the fetch to fail AFTER upload-pack has loaded
	// the object store (and so read alternates / grafts): the branch
	// stays, one of its objects goes. Deleting the branch instead fails
	// at ref lookup, before anything is read (checked with git 2.55).
	breakObject := func(t *testing.T, dir string) {
		blob := gitT(t, dir, "rev-parse", "HEAD:fix.go")
		if err := os.Remove(filepath.Join(dir, ".git/objects", blob[:2], blob[2:])); err != nil {
			t.Error(err)
		}
	}
	return map[string]agentFunc{
		"alternates": func(t *testing.T, dir string) []sseFrame {
			commitAs()(t, dir)
			breakObject(t, dir)
			if err := os.Symlink(secret, filepath.Join(dir, ".git/objects/info/alternates")); err != nil {
				t.Error(err)
			}
			return []sseFrame{turnComplete()}
		},
		"packed-refs": func(t *testing.T, dir string) []sseFrame {
			commitAs()(t, dir)
			gitT(t, dir, "pack-refs", "--all")
			_ = os.Remove(filepath.Join(dir, ".git/packed-refs"))
			if err := os.Symlink(secret, filepath.Join(dir, ".git/packed-refs")); err != nil {
				t.Error(err)
			}
			return []sseFrame{turnComplete()}
		},
		"grafts": func(t *testing.T, dir string) []sseFrame {
			commitAs()(t, dir)
			breakObject(t, dir)
			if err := os.MkdirAll(filepath.Join(dir, ".git/info"), 0o750); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(secret, filepath.Join(dir, ".git/info/grafts")); err != nil {
				t.Error(err)
			}
			return []sseFrame{turnComplete()}
		},
	}
}

func assertNoSecret(t *testing.T, r *rig, out outcome) {
	t.Helper()
	state, _ := os.ReadFile(r.cfg.StateFile)
	for where, text := range map[string]string{
		"stop reason": out.Reason, "comment": strings.Join(r.gh.commentsOf(1), "\n"),
		"log": r.logs.String(), "state file": string(state),
	} {
		if strings.Contains(text, keyLine) || strings.Contains(text, "PRIVATE KEY") {
			t.Errorf("the secret reached the %s:\n%s", where, text)
		}
	}
}

// P1 layer (a): with the copy check switched off, git's output — which
// echoes the planted file — still never reaches a message.
func TestAgentRepoGitOutputNeverPublished(t *testing.T) {
	secret := secretFile(t)
	for name, agent := range leakAgents(secret) {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, agent)
			r.d.git.inspect = func(string, string) error { return nil }
			r.gh.seedIssue(1)
			out, err := runOnce(t, r)
			if err != nil || !out.Stopped {
				t.Fatalf("outcome %+v, err %v; want a stop", out, err)
			}
			t.Logf("stop reason: %s", out.Reason)
			assertNoSecret(t, r, out)
			if !strings.Contains(out.Reason, "withheld") && !strings.Contains(out.Reason, "did not produce") {
				t.Errorf("reason %q does not say git's output was withheld", out.Reason)
			}
		})
	}
}

// P1 layer (b): the copy check refuses each planted file before any git
// runs against the copy, and still leaks nothing.
func TestCopyCheckRefusesPlantedGitFiles(t *testing.T) {
	secret := secretFile(t)
	for name, agent := range leakAgents(secret) {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, agent)
			r.gh.seedIssue(1)
			out, err := runOnce(t, r)
			if err != nil || !out.Stopped || !strings.Contains(out.Reason, "refusing to read the working copy") {
				t.Fatalf("outcome %+v, err %v; want the copy refused", out, err)
			}
			assertNoSecret(t, r, out)
		})
	}
}

func TestInspectCopyRules(t *testing.T) {
	mk := func(t *testing.T) (string, string) {
		root := t.TempDir()
		gitT(t, root, "init", "--quiet", "-b", "main", "src")
		src := filepath.Join(root, "src")
		gitT(t, src, "-c", "user.name=A", "-c", "user.email=a@x", "commit", "--quiet", "--allow-empty", "-m", "base")
		gitT(t, root, "clone", "--quiet", "--depth=1", "file://"+src, "copy")
		copyDir := filepath.Join(root, "copy")
		return copyDir, gitT(t, copyDir, "rev-parse", "HEAD")
	}
	dir, base := mk(t)
	if err := inspectCopy(dir, base); err != nil {
		t.Fatalf("a fresh clone was refused: %v", err)
	}
	cases := map[string]func(t *testing.T, g string){
		"symlink anywhere": func(t *testing.T, g string) { _ = os.Symlink("/etc/hostname", filepath.Join(g, "refs/heads/x")) },
		"alternates":       func(t *testing.T, g string) { writeFile(t, filepath.Join(g, "objects/info/alternates"), "/tmp\n") },
		"http-alternates":  func(t *testing.T, g string) { writeFile(t, filepath.Join(g, "objects/info/http-alternates"), "x\n") },
		"grafts":           func(t *testing.T, g string) { writeFile(t, filepath.Join(g, "info/grafts"), "x\n") },
		"commondir":        func(t *testing.T, g string) { writeFile(t, filepath.Join(g, "commondir"), "/tmp\n") },
		"config include": func(t *testing.T, g string) {
			writeFile(t, filepath.Join(g, "config"), "[core]\n[Include]\n\tpath = /x\n")
		},
		"FIFO packed-refs": func(t *testing.T, g string) {
			_ = os.Remove(filepath.Join(g, "packed-refs"))
			if err := syscall.Mkfifo(filepath.Join(g, "packed-refs"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"shallow extra root": func(t *testing.T, g string) {
			writeFile(t, filepath.Join(g, "shallow"), base+"\n"+strings.Repeat("a", 40)+"\n")
		},
		"symlinked shallow": func(t *testing.T, g string) {
			_ = os.Remove(filepath.Join(g, "shallow"))
			_ = os.Symlink("/etc/hostname", filepath.Join(g, "shallow"))
		},
		".git is a gitfile": nil,
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			dir, _ := mk(t)
			g := filepath.Join(dir, ".git")
			if plant == nil {
				if err := os.RemoveAll(g); err != nil {
					t.Fatal(err)
				}
				writeFile(t, g, "gitdir: /elsewhere\n")
			} else {
				plant(t, g)
			}
			if err := inspectCopy(dir, base); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// P3: a fetch that declines to update a shallow root exits 0; a ref left
// from an earlier attempt must not be read as the new tip.
func TestCollectNeverReadsAStaleRef(t *testing.T) {
	r := newRig(t, commitAs())
	g := r.d.git
	g.inspect = func(string, string) error { return nil } // prove the ref layer alone
	ctx := context.Background()
	base, err := g.fetchBase(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := g.prepareClone(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	commitAs()(t, dir)
	if _, _, err := g.collect(ctx, 1, dir, base); err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	dir, err = g.prepareClone(ctx, 1) // a person's retry: a fresh copy
	if err != nil {
		t.Fatal(err)
	}
	commitAs("-c", "user.name=Evil", "-c", "user.email=evil@example.com")(t, dir)
	writeFile(t, filepath.Join(dir, "more.go"), "package fix\n")
	gitT(t, dir, "add", "more.go")
	gitT(t, dir, "commit", "--quiet", "-s", "-m", "fix: more")
	tip := gitT(t, dir, "rev-parse", "HEAD")
	shallow := filepath.Join(dir, ".git/shallow")
	old, _ := os.ReadFile(shallow)
	writeFile(t, shallow, string(old)+tip+"\n")
	if got, _, err := g.collect(ctx, 1, dir, base); err == nil {
		t.Fatalf("collect returned tip %s from a fetch that never updated the ref", got)
	}
}

// P2: an agent-controlled reason of any length can't wedge the queue on
// a comment GitHub refuses.
func TestHugeStopReasonIsCappedAndPosted(t *testing.T) {
	r := newRig(t, func(*testing.T, string) []sseFrame {
		return []sseFrame{{Event: attach.EventTurnError, Data: attach.TurnError{Kind: attach.TurnErrorUnknown, Message: strings.Repeat("x", 100_000)}}}
	})
	r.gh.seedIssue(1)
	out, err := runOnce(t, r)
	if err != nil || !out.Stopped {
		t.Fatalf("outcome %+v err %v", out, err)
	}
	cs := r.gh.commentsOf(1)
	if len(cs) != 1 || len(cs[0]) > 4000 || !strings.Contains(cs[0], "[truncated]") {
		t.Fatalf("comment not capped: %d comments, first %d bytes", len(cs), len(cs[0]))
	}
	assertStopped(t, r, 1, "[truncated]")
}

// P2: a comment that can't post never holds the labels or the next poll.
func TestCommentFailureDoesNotWedgeTheStop(t *testing.T) {
	r := newRig(t, func(*testing.T, string) []sseFrame { return []sseFrame{turnComplete()} })
	r.gh.seedIssue(1)
	r.gh.mu.Lock()
	r.gh.commentFail = func(string) bool { return true }
	r.gh.mu.Unlock()
	out, err := runOnce(t, r)
	if err != nil || !out.Stopped {
		t.Fatalf("outcome %+v err %v", out, err)
	}
	if labels := r.gh.labelsOf(1); !slices.Contains(labels, labelStopped) || slices.Contains(labels, labelActive) {
		t.Fatalf("labels %v", labels)
	}
	if r.d.st.Active != nil || len(r.d.st.PendingComments) != 1 {
		t.Fatalf("state %+v; want no active issue and one owed comment", r.d.st)
	}
	r.gh.mu.Lock()
	r.gh.commentFail = nil
	r.gh.mu.Unlock()
	if _, _, err := r.d.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.gh.commentsOf(1)) != 1 || len(r.d.st.PendingComments) != 0 {
		t.Fatalf("owed comment not delivered: %v / %v", r.gh.commentsOf(1), r.d.st.PendingComments)
	}
}

// P2: a background subagent still running keeps the session from
// counting as done, and the parent turn it triggers on finishing is
// waited for.
func TestRunningSubagentHoldsThePush(t *testing.T) {
	var r *rig
	var later sync.WaitGroup
	agent := func(t *testing.T, dir string) []sseFrame {
		commitAs()(t, dir)
		later.Add(1)
		go func() {
			defer later.Done()
			time.Sleep(500 * time.Millisecond) // well past the 60ms settle
			writeFile(t, filepath.Join(dir, "later.go"), "package fix\n")
			gitT(t, dir, "add", "later.go")
			gitT(t, dir, "commit", "--quiet", "-s", "-m", "fix: what the reviewer subagent found")
			r.daemon.mu.Lock()
			r.daemon.agents = []attach.AgentInfo{{ID: "a1", Name: "reviewer", Status: attach.AgentStatusCompleted}}
			r.daemon.mu.Unlock()
		}()
		return []sseFrame{turnComplete()}
	}
	r = newRig(t, agent)
	r.daemon.agents = []attach.AgentInfo{{ID: "a1", Name: "reviewer", Status: attach.AgentStatusRunning}}
	r.gh.seedIssue(1)
	out, err := runOnce(t, r)
	later.Wait() // the late commit lands whether or not the dispatcher waited for it
	if err != nil || out.PR == 0 {
		t.Fatalf("outcome %+v err %v", out, err)
	}
	if n := gitT(t, r.mirror, "rev-list", "--count", "main..agent/issue-1"); n != "2" {
		t.Fatalf("pushed %s commits; want 2 — the push went out while the subagent worked", n)
	}
}

// Item 4: decision-6 additions through the real poll.
func TestBodyEditedBySomeoneElseIsNeverTaken(t *testing.T) {
	for name, set := range map[string]func(f *fakeGitHub){
		"edited by someone else": func(f *fakeGitHub) { f.editors[1] = []string{testMaintainer, "mallory"} },
		"unattributable edit":    func(f *fakeGitHub) { f.editors[1] = []string{""} },
		"more edits than read":   func(f *fakeGitHub) { f.editors[1] = []string{testMaintainer}; f.editTotal[1] = 101 },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, commitAs())
			r.gh.seedIssue(1)
			set(r.gh)
			if _, err := r.d.tokens.Token(context.Background()); err != nil {
				t.Fatal(err)
			}
			if is, err := r.d.next(context.Background()); err != nil || is != nil {
				t.Fatalf("next = %+v, %v; want the issue refused", is, err)
			}
		})
	}
	r := newRig(t, commitAs())
	r.gh.seedIssue(1)
	r.gh.editors[1] = []string{testMaintainer, "MasterSingh24"}
	if out, err := runOnce(t, r); err != nil || out.PR == 0 {
		t.Fatalf("maintainer-only edits: %+v %v", out, err)
	}
}

func TestProvenanceAdditions(t *testing.T) {
	me, other := &ghUser{Login: testMaintainer}, &ghUser{Login: "mallory"}
	app := &ghApp{Slug: "some-app"}
	good := []ghEvent{{Event: "labeled", Actor: me, Label: &ghLabel{Name: labelQueue}}, {Event: "assigned", Actor: me}}
	is := ghIssue{User: ghUser{Login: testMaintainer}, Assignees: []ghUser{{Login: testMaintainer}}}
	for name, extra := range map[string]ghEvent{
		"reopened by someone else":        {Event: "reopened", Actor: other},
		"soak:stopped removed by another": {Event: "unlabeled", Actor: other, Label: &ghLabel{Name: labelStopped}},
		"soak:queue removed by another":   {Event: "unlabeled", Actor: other, Label: &ghLabel{Name: labelQueue}},
		"label applied through an App":    {Event: "labeled", Actor: me, Label: &ghLabel{Name: labelQueue}, ViaApp: app},
		"assignment performed via an App": {Event: "assigned", Actor: me, ViaApp: app},
	} {
		if err := checkProvenance(is, append(append([]ghEvent{}, good...), extra), testMaintainer); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, ok := range map[string]ghEvent{
		"reopened by the maintainer":           {Event: "reopened", Actor: me},
		"soak:stopped removed by maintainer":   {Event: "unlabeled", Actor: me, Label: &ghLabel{Name: labelStopped}},
		"dispatcher's own soak:active via App": {Event: "labeled", Actor: &ghUser{Login: "writer[bot]"}, Label: &ghLabel{Name: labelActive}, ViaApp: app},
	} {
		if err := checkProvenance(is, append(append([]ghEvent{}, good...), ok), testMaintainer); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
	viaApp := is
	viaApp.ViaApp = app
	if err := checkProvenance(viaApp, good, testMaintainer); err == nil {
		t.Error("an issue created through an App was accepted")
	}
}

// Item 6: every stop path interrupts the session before the copy goes.
func TestStopInterruptsTheSession(t *testing.T) {
	r := newRig(t, func(*testing.T, string) []sseFrame { return []sseFrame{turnComplete()} })
	r.gh.seedIssue(1)
	if out, err := runOnce(t, r); err != nil || !out.Stopped {
		t.Fatalf("%+v %v", out, err)
	}
	r.daemon.mu.Lock()
	defer r.daemon.mu.Unlock()
	if r.daemon.interrupted == 0 {
		t.Fatal("a stopped session was left running")
	}
}

// Item 6: closing keywords and attribution text.
func TestClosersAndAttribution(t *testing.T) {
	plan := "Fixes #7, closes other/repo#9, resolved https://github.com/x/y/issues/3. Refs #8 stays."
	got := neutralizeClosers(plan)
	if strings.Contains(strings.ToLower(got), "fixes #7") || strings.Contains(got, "closes") || strings.Contains(got, "resolved") || !strings.Contains(got, "refs #7") {
		t.Fatalf("neutralizeClosers = %q", got)
	}
	mirror := testOwner + "/" + testRepo
	for msg, bad := range map[string]bool{
		"fix: x\n\nFixes #1\n":                     false,
		"fix: x\n\nFixes #7\n":                     true,
		"fix: x\n\nCloses " + mirror + "#7\n":      true,
		"fix: x\n\nFixes " + testUpstream + "\n":   false,
		"fix: x\n\nGenerated with Claude Code\n":   true,
		"fix: x\n\n🤖 Generated with [Claude](x)\n": true,
		"fix: x\n\nSelf-Development-Run: 3\n":      true,
		"fix: x\n\nAI-Assisted: yes\n":             true,
		"fix: generate the code\n":                 false,
	} {
		if err := checkCommitText(msg, 1, mirror); (err != nil) != bad {
			t.Errorf("%q: err %v, want refused=%v", msg, err, bad)
		}
	}
	if validateIdentity(identity{Name: "core-agent", Email: "x@example.com"}) == nil {
		t.Error("an agent's name was accepted as the soak identity")
	}
}

func TestPRBodyNeutralizesPlanClosersAndWithholdsAttribution(t *testing.T) {
	for name, tc := range map[string]struct{ plan, want, never string }{
		"closer":      {"Fixes #7 and closes #9.", "refs #7", "closes #9"},
		"attribution": {"Do it.\n\n🤖 Generated with Claude Code", "withheld", "Generated with"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, commitAs())
			r.gh.seedIssue(1)
			plans := filepath.Join(r.cfg.AgentsDir, "plans")
			if err := os.MkdirAll(plans, 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(plans, "plan-1.md"), "---\nplan: 1\nsession: \"sess-1\"\n---\n\n"+tc.plan+"\n")
			if out, err := runOnce(t, r); err != nil || out.PR == 0 {
				t.Fatalf("%+v %v", out, err)
			}
			body, _ := r.gh.createdPRs()[0]["body"].(string)
			if !strings.Contains(body, tc.want) || strings.Contains(body, tc.never) || strings.Count(body, "Fixes #") != 1 {
				t.Fatalf("PR body:\n%s", body)
			}
		})
	}
}

// Item 6, at the publish call site: a commit carrying attribution text or
// closing another mirror issue is refused, never pushed.
func TestPublishRefusesCommitText(t *testing.T) {
	for name, msg := range map[string]string{
		"generated footer": "fix: x\n\nGenerated with Claude Code\n\nSigned-off-by: " + testIdentity.String(),
		"run trailer":      "fix: x\n\nSelf-Development-Run: 3\nSigned-off-by: " + testIdentity.String(),
		"foreign closer":   "fix: x\n\nFixes #7\n\nSigned-off-by: " + testIdentity.String(),
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, func(t *testing.T, dir string) []sseFrame {
				writeFile(t, filepath.Join(dir, "fix.go"), "package fix\n")
				gitT(t, dir, "add", "fix.go")
				gitT(t, dir, "commit", "--quiet", "-m", msg)
				return []sseFrame{turnComplete()}
			})
			r.gh.seedIssue(1)
			out, err := runOnce(t, r)
			if err != nil || !out.Stopped || !strings.Contains(out.Reason, "refusing to push") {
				t.Fatalf("outcome %+v err %v", out, err)
			}
		})
	}
}
