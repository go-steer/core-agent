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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// ---- decision 6 ----------------------------------------------------------

func TestCheckProvenance(t *testing.T) {
	me := func() *ghUser { return &ghUser{Login: testMaintainer} }
	other := &ghUser{Login: "mallory"}
	queued := func(actor *ghUser) ghEvent {
		return ghEvent{Event: "labeled", Actor: actor, Label: &ghLabel{Name: labelQueue}}
	}
	assigned := func(actor *ghUser) ghEvent { return ghEvent{Event: "assigned", Actor: actor} }
	good := ghIssue{User: ghUser{Login: testMaintainer}, Assignees: []ghUser{{Login: testMaintainer}}}

	cases := []struct {
		name   string
		mutate func(*ghIssue)
		events []ghEvent
		want   string // "" = eligible; else a substring of the refusal
	}{
		{"maintainer did everything", nil, []ghEvent{queued(me()), assigned(me())}, ""},
		{"login case differs", nil, []ghEvent{queued(&ghUser{Login: "MasterSingh24"}), assigned(me())}, ""},
		{"authored by someone else", func(i *ghIssue) { i.User.Login = "mallory" }, []ghEvent{queued(me()), assigned(me())}, "authored by"},
		{"labeled by someone else", nil, []ghEvent{queued(other), assigned(me())}, "applied by \"mallory\""},
		{"labeled by someone else, then re-labeled by the maintainer", nil,
			[]ghEvent{queued(other), {Event: "unlabeled", Actor: me(), Label: &ghLabel{Name: labelQueue}}, queued(me()), assigned(me())}, "applied by \"mallory\""},
		{"assigned by someone else", nil, []ghEvent{queued(me()), assigned(other)}, "assigned by \"mallory\""},
		{"assigner field names someone else", nil,
			[]ghEvent{queued(me()), {Event: "assigned", Actor: me(), Assigner: other}}, "assigned by \"mallory\""},
		{"label name case differs, labeled by someone else", nil,
			[]ghEvent{{Event: "labeled", Actor: other, Label: &ghLabel{Name: "Soak:Queue"}}, queued(me()), assigned(me())}, "applied by \"mallory\""},
		{"title changed by someone else", nil, []ghEvent{queued(me()), assigned(me()), {Event: "renamed", Actor: other}}, "title changed"},
		{"label event from a deleted account", nil, []ghEvent{queued(nil), assigned(me())}, "applied by \"\""},
		{"no label event at all", nil, []ghEvent{assigned(me())}, "no soak:queue labeled event"},
		{"never assigned", func(i *ghIssue) { i.Assignees = nil }, []ghEvent{queued(me())}, "not assigned"},
		{"assignee present but no assigned event", nil, []ghEvent{queued(me())}, "no assigned event"},
		{"another label by someone else is fine", nil,
			[]ghEvent{queued(me()), assigned(me()), {Event: "labeled", Actor: other, Label: &ghLabel{Name: "bug"}}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			is := good
			if tc.mutate != nil {
				tc.mutate(&is)
			}
			err := checkProvenance(is, tc.events, testMaintainer)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
}

func TestClaimable(t *testing.T) {
	base := ghIssue{State: "open", Labels: []ghLabel{{Name: labelQueue}}}
	if !claimable(base) {
		t.Fatal("a queued open issue is not claimable")
	}
	for name, is := range map[string]ghIssue{
		"active":   {State: "open", Labels: []ghLabel{{Name: labelQueue}, {Name: labelActive}}},
		"stopped":  {State: "open", Labels: []ghLabel{{Name: labelQueue}, {Name: labelStopped}}},
		"closed":   {State: "closed", Labels: []ghLabel{{Name: labelQueue}}},
		"PR":       {State: "open", Labels: []ghLabel{{Name: labelQueue}}, PullRequest: &struct{}{}},
		"unqueued": {State: "open"},
	} {
		if claimable(is) {
			t.Errorf("%s issue is claimable", name)
		}
	}
}

func TestUpstreamLink(t *testing.T) {
	body := "See https://github.com/other/repo/issues/9 and https://github.com/go-steer/core-agent/issues/1234 too."
	if got := upstreamLink(body, "go-steer/core-agent"); got != "https://github.com/go-steer/core-agent/issues/1234" {
		t.Errorf("got %q", got)
	}
	if got := upstreamLink("https://github.com/go-steer/core-agentx/issues/1", "go-steer/core-agent"); got != "" {
		t.Errorf("matched a different repo: %q", got)
	}
}

// ---- decision 16 ---------------------------------------------------------

func TestVerifyCommits(t *testing.T) {
	ok := commit{SHA: "abc", AuthorName: testIdentity.Name, AuthorEmail: testIdentity.Email,
		CommitterName: testIdentity.Name, CommitterEmail: testIdentity.Email,
		Message: "fix: x\n\nbody\n\nSigned-off-by: Soak Worker <soak-worker@example.com>\n"}
	cases := []struct {
		name   string
		mutate func(*commit)
		want   string
	}{
		{"soak identity throughout", nil, ""},
		{"email case differs", func(c *commit) { c.AuthorEmail = "Soak-Worker@Example.com" }, ""},
		{"maintainer as author", func(c *commit) { c.AuthorName, c.AuthorEmail = "Pat Maintainer", "pat@example.com" }, "author is Pat Maintainer"},
		{"bot noreply committer", func(c *commit) { c.CommitterEmail = "41898282+github-actions[bot]@users.noreply.github.com" }, "bot or noreply"},
		{"plain noreply author", func(c *commit) { c.AuthorEmail = "soak@users.noreply.github.com" }, "bot or noreply"},
		{"no sign-off", func(c *commit) { c.Message = "fix: x\n" }, "no Signed-off-by"},
		{"sign-off by someone else", func(c *commit) { c.Message += "Signed-off-by: Pat Maintainer <pat@example.com>\n" }, "signed off by Pat"},
		{"co-author trailer", func(c *commit) { c.Message += "Co-authored-by: Claude <noreply@anthropic.com>\n" }, "Co-authored-by trailer"},
		{"indented co-author trailer", func(c *commit) { c.Message += "  co-authored-by: Someone <s@example.com>\n" }, "trailer"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ok
			if tc.mutate != nil {
				tc.mutate(&c)
			}
			err := verifyCommits([]commit{ok, c}, testIdentity)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want a refusal containing %q", err, tc.want)
			}
		})
	}
	if err := verifyCommits(nil, testIdentity); err == nil {
		t.Error("an empty branch verified")
	}
}

func TestValidateIdentity(t *testing.T) {
	for _, bad := range []identity{
		{Name: "", Email: "x@example.com"},
		{Name: "Soak", Email: "not-an-email"},
		{Name: "writer[bot]", Email: "w@example.com"},
		{Name: "Soak", Email: "123+writer[bot]@users.noreply.github.com"},
		{Name: "Soak", Email: "noreply@example.com"},
	} {
		if validateIdentity(bad) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if err := validateIdentity(testIdentity); err != nil {
		t.Errorf("refused the soak identity: %v", err)
	}
}

// ---- App auth ------------------------------------------------------------

func TestAppTokenExchange(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	gh, srv := newFakeGitHub(t, &key.PublicKey)
	now := time.Now()
	src := &appTokenSource{api: srv.URL, appID: testAppID, key: key, owner: testOwner, repo: testRepo,
		http: srv.Client(), now: func() time.Time { return now }}

	tok, err := src.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_test1" || gh.discovers != 1 {
		t.Fatalf("token %q after %d discovers; want ghs_test1 after 1", tok, gh.discovers)
	}
	if again, _ := src.Token(context.Background()); again != tok || len(gh.minted) != 1 {
		t.Fatalf("second call minted again (%d mints)", len(gh.minted))
	}
	now = now.Add(56 * time.Minute) // inside the refresh margin of a 1h token
	if fresh, _ := src.Token(context.Background()); fresh != "ghs_test2" || gh.discovers != 1 {
		t.Fatalf("near expiry: token %q, discovers %d; want a new token and no re-discovery", fresh, gh.discovers)
	}

	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	bad := &appTokenSource{api: srv.URL, appID: testAppID, key: other, owner: testOwner, repo: testRepo, http: srv.Client(), now: time.Now}
	if _, err := bad.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a JWT signed by the wrong key: %v, want a 401", err)
	}
}

func TestLoadAppKey(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	dir := t.TempDir()
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	for name, raw := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, raw, 0o400); err != nil {
			t.Fatal(err)
		}
		got, err := loadAppKey(p)
		if err != nil || !got.Equal(key) {
			t.Errorf("%s: %v", name, err)
		}
	}
	loose := filepath.Join(dir, "loose")
	if err := os.WriteFile(loose, pkcs1, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAppKey(loose); err == nil || !strings.Contains(err.Error(), "other users") {
		t.Errorf("world-readable key: %v", err)
	}
	junk := filepath.Join(dir, "junk")
	if err := os.WriteFile(junk, []byte("SECRET-LOOKING-TEXT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAppKey(junk); err == nil || strings.Contains(err.Error(), "SECRET-LOOKING-TEXT") {
		t.Errorf("junk key file: %v (must fail without echoing content)", err)
	}
}

// ---- credentials never on argv ------------------------------------------

func TestCredentialArgsKeepTheTokenOffArgv(t *testing.T) {
	const tok = "ghs_SUPERSECRET123"
	args, cleanup, err := credentialArgs("https://github.com/o/r.git", tok)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, tok) {
		t.Fatalf("token on argv: %q", joined)
	}
	path := strings.TrimPrefix(args[len(args)-1], "credential.helper=store --file=")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("credential file mode %04o", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "https://x-access-token:"+tok+"@github.com\n" {
		t.Errorf("credential line %q", raw)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("credential dir survives cleanup: %v", err)
	}
	if args, _, err := credentialArgs("/srv/mirror.git", tok); err != nil || args != nil {
		t.Errorf("local remote got credentials: %v %v", args, err)
	}
	if _, _, err := credentialArgs("https://github.com/o/r.git", ""); err == nil {
		t.Error("an https remote with no token was accepted")
	}
}

// ---- idle detection ------------------------------------------------------

func TestWatcherIdleDetection(t *testing.T) {
	t0 := time.Unix(1_000_000, 0)
	settle, start := 30*time.Second, 10*time.Minute
	idle := attach.StatusInfo{State: attach.AgentStateIdle}
	running := attach.StatusInfo{State: attach.AgentStateRunning, TurnInFlight: true}

	w := newWatcher(t0)
	if w.done(idle, t0.Add(time.Minute), t0, settle, start) {
		t.Fatal("idle before any turn ran counted as done")
	}
	w.observe(attach.Frame{Type: attach.EventStatusUpdate, TypedData: &attach.StatusUpdate{TurnState: attach.TurnStateStreaming}}, t0.Add(2*time.Minute))
	w.observe(attach.Frame{Type: attach.EventTurnComplete, TypedData: &attach.TurnComplete{}}, t0.Add(3*time.Minute))
	if w.done(running, t0.Add(4*time.Minute), t0, settle, start) {
		t.Fatal("a running session (auto_continue re-drive) counted as done")
	}
	if w.done(idle, t0.Add(3*time.Minute+10*time.Second), t0, settle, start) {
		t.Fatal("done before the settle window")
	}
	if !w.done(idle, t0.Add(4*time.Minute), t0, settle, start) {
		t.Fatal("idle and settled after turn-complete, not done")
	}
	if w.end.stopReason() != "" {
		t.Fatalf("a clean finish has a stop reason: %q", w.end.stopReason())
	}

	never := newWatcher(t0)
	if !never.done(idle, t0.Add(11*time.Minute), t0, settle, start) || !never.end.NeverRan {
		t.Fatal("a session that never ran a turn was not given up on")
	}
}

func TestWatcherCountsATripOnceAcrossFrameAndRow(t *testing.T) {
	w := newWatcher(time.Now())
	row := attach.NewGuardrailHaltEvent("watchdog", "runaway loop", true)
	w.observe(attach.Frame{Type: attach.EventGuardrailTrip, TypedData: &attach.GuardrailTrip{Guardrail: "watchdog", Reason: "runaway loop", EventID: row.ID}}, time.Now())
	w.observe(attach.Frame{Seq: 4, Event: row}, time.Now())
	if len(w.end.Trips) != 1 || w.lastSeq != 4 {
		t.Fatalf("trips %v lastSeq %d; want one trip and seq 4", w.end.Trips, w.lastSeq)
	}
	te := attach.NewTurnErrorEvent(attach.TurnError{Kind: attach.TurnErrorTransientNet, Message: "reset"}, "p1", "")
	w.observe(attach.Frame{Seq: 5, Event: te}, time.Now())
	if w.end.TurnError == nil || w.end.TurnError.Kind != attach.TurnErrorTransientNet {
		t.Fatalf("durable turn-error row not read: %+v", w.end.TurnError)
	}
	if r := w.end.stopReason(); !strings.Contains(r, "watchdog: runaway loop") {
		t.Fatalf("stop reason %q", r)
	}
}

// ---- flags ---------------------------------------------------------------

func TestParseFlags(t *testing.T) {
	base := []string{"--worktrees-dir", "/w", "--attach-url", "http://d:7777", "--token-file", "/t",
		"--app-id", "1", "--app-key-file", "/k", "--state-file", "/s", "--commit-name", "Soak Worker", "--commit-email", "soak@example.com"}
	c, err := parseFlags(base, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Owner != "mastersingh24" || c.Repo != "core-agent-selfdev" || c.Maintainer != "mastersingh24" ||
		c.Poll != 2*time.Minute || c.MaxOpenPRs != 3 || c.GitRemote != "https://github.com/mastersingh24/core-agent-selfdev.git" {
		t.Errorf("defaults: %+v", c)
	}
	var stderr bytes.Buffer
	if _, err := parseFlags(base[2:], &stderr); err == nil || !strings.Contains(err.Error(), "--worktrees-dir") {
		t.Errorf("missing --worktrees-dir: %v", err)
	}
	if _, err := parseFlags(append(base, "--commit-email", "x[bot]@users.noreply.github.com"), io.Discard); err == nil {
		t.Error("a bot commit identity was accepted")
	}
	if _, err := parseFlags(append(base, "--repo", "nope"), io.Discard); err == nil {
		t.Error("a malformed --repo was accepted")
	}
	if code := run([]string{"--bogus"}, io.Discard); code != 2 {
		t.Errorf("usage error exit %d, want 2", code)
	}
}

func TestStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "state.json")
	st, err := loadState(p)
	if err != nil || st.Active != nil {
		t.Fatalf("missing file: %+v %v", st, err)
	}
	st.Active = &activeIssue{Number: 3, SessionID: "s", Injected: true}
	st.OpenPRs = []openPR{{Issue: 2, PR: 9}}
	if err := saveState(p, st); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(p)
	if err != nil || got.Active.Number != 3 || !got.Active.Injected || got.OpenPRs[0].PR != 9 {
		t.Fatalf("round trip: %+v %v", got, err)
	}
}
