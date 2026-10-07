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
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

const (
	testOwner      = "mastersingh24"
	testRepo       = "core-agent-selfdev"
	testMaintainer = "mastersingh24"
	testAppID      = 4242
	testInstall    = 777
	testUpstream   = "https://github.com/go-steer/core-agent/issues/1234"
)

var testIdentity = identity{Name: "Soak Worker", Email: "soak-worker@example.com"}

// ---- fake GitHub ---------------------------------------------------------

// fakeGitHub is an in-memory GitHub REST API for one repository plus the
// App token endpoints. Repo calls must carry the installation token the
// fake minted, so a test through it also proves the App flow ran.
type fakeGitHub struct {
	t   *testing.T
	pub *rsa.PublicKey

	mu          sync.Mutex
	issues      map[int]*ghIssue
	events      map[int][]ghEvent
	pulls       map[int]*ghPull
	comments    map[int][]string
	created     []map[string]any
	nextPR      int
	minted      []string
	discovers   int
	expiresIn   time.Duration
	failRepo    bool                   // answer every repo call 500
	editors     map[int][]string       // body editors GraphQL reports; nil entry = "" (unattributable)
	editTotal   map[int]int            // totalCount override; default len(editors)
	commentFail func(body string) bool // make a comment POST fail
}

func newFakeGitHub(t *testing.T, pub *rsa.PublicKey) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{
		t: t, pub: pub, issues: map[int]*ghIssue{}, events: map[int][]ghEvent{},
		pulls: map[int]*ghPull{}, comments: map[int][]string{}, nextPR: 100, expiresIn: time.Hour,
		editors: map[int][]string{}, editTotal: map[int]int{},
	}
	mux := http.NewServeMux()
	repo := "/repos/" + testOwner + "/" + testRepo
	mux.HandleFunc("GET "+repo+"/installation", f.appAuth(func(w http.ResponseWriter, _ *http.Request) {
		f.discovers++
		writeJSONT(w, map[string]any{"id": testInstall})
	}))
	mux.HandleFunc(fmt.Sprintf("POST /app/installations/%d/access_tokens", testInstall), f.appAuth(func(w http.ResponseWriter, _ *http.Request) {
		tok := fmt.Sprintf("ghs_test%d", len(f.minted)+1)
		f.minted = append(f.minted, tok)
		writeJSONT(w, map[string]any{"token": tok, "expires_at": time.Now().Add(f.expiresIn).UTC().Format(time.RFC3339)})
	}))
	mux.HandleFunc("POST /graphql", f.repoAuth(f.graphql))
	mux.HandleFunc("GET "+repo+"/issues", f.repoAuth(f.listIssues))
	mux.HandleFunc("GET "+repo+"/issues/{n}/events", f.repoAuth(f.listEvents))
	mux.HandleFunc("GET "+repo+"/pulls", f.repoAuth(f.listPulls))
	mux.HandleFunc("GET "+repo+"/pulls/{n}", f.repoAuth(f.getPull))
	mux.HandleFunc("POST "+repo+"/pulls", f.repoAuth(f.createPull))
	mux.HandleFunc("POST "+repo+"/issues/{n}/labels", f.repoAuth(f.addLabels))
	mux.HandleFunc("DELETE "+repo+"/issues/{n}/labels/{name}", f.repoAuth(f.removeLabel))
	mux.HandleFunc("POST "+repo+"/issues/{n}/comments", f.repoAuth(f.addComment))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func writeJSONT(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// appAuth requires a valid App JWT: RS256 over the test key, issuer the
// App ID, lifetime within GitHub's 10 minutes.
func (f *fakeGitHub) appAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if err := verifyTestJWT(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), f.pub); err != nil {
			http.Error(w, `{"message":"bad JWT: `+err.Error()+`"}`, http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func verifyTestJWT(tok string, pub *rsa.PublicKey) error {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return fmt.Errorf("not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var c struct {
		Iat, Exp int64
		Iss      string
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return err
	}
	if c.Iss != strconv.Itoa(testAppID) {
		return fmt.Errorf("iss %q", c.Iss)
	}
	if c.Exp-c.Iat > 600 || c.Exp <= time.Now().Unix() {
		return fmt.Errorf("lifetime iat=%d exp=%d", c.Iat, c.Exp)
	}
	return nil
}

func (f *fakeGitHub) repoAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failRepo {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(f.minted) == 0 || got != f.minted[len(f.minted)-1] {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// pageOne serves rows the way GitHub paginates them: per_page rows of
// page N, an empty page past the end.
func pageOne[T any](w http.ResponseWriter, r *http.Request, rows []T) {
	per, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if per <= 0 || page <= 0 {
		http.Error(w, `{"message":"want per_page and page"}`, http.StatusBadRequest)
		return
	}
	lo, hi := min((page-1)*per, len(rows)), min(page*per, len(rows))
	writeJSONT(w, append([]T{}, rows[lo:hi]...))
}

func (f *fakeGitHub) listIssues(w http.ResponseWriter, r *http.Request) {
	want := r.URL.Query().Get("labels")
	var out []ghIssue
	for n := 1; n <= 1000; n++ {
		if is, ok := f.issues[n]; ok && is.State == "open" && (want == "" || is.hasLabel(want)) {
			out = append(out, *is)
		}
	}
	pageOne(w, r, out)
}

func (f *fakeGitHub) num(r *http.Request) int {
	n, _ := strconv.Atoi(r.PathValue("n"))
	return n
}

func (f *fakeGitHub) listEvents(w http.ResponseWriter, r *http.Request) {
	pageOne(w, r, f.events[f.num(r)])
}

func (f *fakeGitHub) listPulls(w http.ResponseWriter, r *http.Request) {
	var out []ghPull
	head := r.URL.Query().Get("head") // "owner:branch"
	for n := 0; n <= 1000; n++ {
		if p, ok := f.pulls[n]; ok && p.State == "open" && (head == "" || head == testOwner+":"+p.Head.Ref) {
			out = append(out, *p)
		}
	}
	pageOne(w, r, out)
}

func (f *fakeGitHub) getPull(w http.ResponseWriter, r *http.Request) {
	p, ok := f.pulls[f.num(r)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSONT(w, p)
}

func (f *fakeGitHub) createPull(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.created = append(f.created, req)
	f.nextPR++
	p := f.openPull(f.nextPR, req["head"].(string))
	writeJSONT(w, p)
}

// openPull adds an open PR from the mirror's own branch head. Caller holds mu
// (or the server is not yet serving).
func (f *fakeGitHub) openPull(n int, head string) *ghPull {
	p := &ghPull{Number: n, State: "open", HTMLURL: fmt.Sprintf("https://github.com/%s/%s/pull/%d", testOwner, testRepo, n)}
	p.Head.Ref = head
	p.Head.Repo = &struct {
		FullName string `json:"full_name"`
	}{FullName: testOwner + "/" + testRepo}
	f.pulls[n] = p
	return p
}

func (f *fakeGitHub) addLabels(w http.ResponseWriter, r *http.Request) {
	var req struct{ Labels []string }
	_ = json.NewDecoder(r.Body).Decode(&req)
	is := f.issues[f.num(r)]
	for _, l := range req.Labels {
		if !is.hasLabel(l) {
			is.Labels = append(is.Labels, ghLabel{Name: l})
		}
	}
	writeJSONT(w, is.Labels)
}

func (f *fakeGitHub) removeLabel(w http.ResponseWriter, r *http.Request) {
	is := f.issues[f.num(r)]
	name := r.PathValue("name")
	kept := is.Labels[:0]
	found := false
	for _, l := range is.Labels {
		if l.Name == name {
			found = true
			continue
		}
		kept = append(kept, l)
	}
	is.Labels = kept
	if !found {
		http.Error(w, `{"message":"Label does not exist"}`, http.StatusNotFound)
		return
	}
	writeJSONT(w, is.Labels)
}

func (f *fakeGitHub) addComment(w http.ResponseWriter, r *http.Request) {
	var req struct{ Body string }
	_ = json.NewDecoder(r.Body).Decode(&req)
	if len(req.Body) > 65536 {
		http.Error(w, `{"message":"Validation Failed","errors":[{"field":"body","code":"custom","message":"body is too long (maximum is 65536 characters)"}]}`, http.StatusUnprocessableEntity)
		return
	}
	if f.commentFail != nil && f.commentFail(req.Body) {
		http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		return
	}
	n := f.num(r)
	f.comments[n] = append(f.comments[n], req.Body)
	writeJSONT(w, map[string]any{"id": 1})
}

// graphql answers the one query the dispatcher sends: an issue's
// userContentEdits.
func (f *fakeGitHub) graphql(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string
		Variables struct{ Number int }
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if !strings.Contains(req.Query, "userContentEdits") {
		http.Error(w, `{"message":"unexpected query"}`, http.StatusBadRequest)
		return
	}
	n := req.Variables.Number
	nodes := []map[string]any{}
	for _, e := range f.editors[n] {
		var editor any
		if e != "" {
			editor = map[string]string{"login": e}
		}
		nodes = append(nodes, map[string]any{"editor": editor})
	}
	total, ok := f.editTotal[n]
	if !ok {
		total = len(nodes)
	}
	writeJSONT(w, map[string]any{"data": map[string]any{"repository": map[string]any{"issue": map[string]any{
		"userContentEdits": map[string]any{"totalCount": total, "nodes": nodes},
	}}}})
}

// seedIssue adds an issue the maintainer authored, queued and assigned,
// with the matching event history. Mutate the result to break a rule.
func (f *fakeGitHub) seedIssue(n int, extraLabels ...string) *ghIssue {
	f.mu.Lock()
	defer f.mu.Unlock()
	is := &ghIssue{
		Number: n, Title: fmt.Sprintf("fix: issue %d", n), State: "open",
		Body:      "Make the recovery path work.\n\nUpstream: " + testUpstream + "\n",
		User:      ghUser{Login: testMaintainer},
		Labels:    []ghLabel{{Name: labelQueue}},
		Assignees: []ghUser{{Login: testMaintainer}},
		CreatedAt: time.Date(2026, 10, 1, 0, 0, n, 0, time.UTC),
	}
	for _, l := range extraLabels {
		is.Labels = append(is.Labels, ghLabel{Name: l})
	}
	f.issues[n] = is
	f.events[n] = []ghEvent{
		{Event: "labeled", Actor: &ghUser{Login: testMaintainer}, Label: &ghLabel{Name: labelQueue}},
		{Event: "assigned", Actor: &ghUser{Login: testMaintainer}, Assignee: &ghUser{Login: testMaintainer}},
	}
	return is
}

func (f *fakeGitHub) labelsOf(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.issues[n].Labels {
		out = append(out, l.Name)
	}
	return out
}

func (f *fakeGitHub) commentsOf(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.comments[n]...)
}

func (f *fakeGitHub) createdPRs() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.created...)
}

// ---- fake attach daemon --------------------------------------------------

// agentFunc is what the fake "agent" does when a task is injected: it gets
// the working copy and returns the frames its session emits. It runs before
// any frame is sent, the way a real turn commits before it completes.
type agentFunc func(t *testing.T, dir string) []sseFrame

type sseFrame struct {
	Event string
	Data  any
}

// fakeDaemon is an attach listener with one scriptable session.
type fakeDaemon struct {
	t     *testing.T
	token string
	agent agentFunc
	dir   func() string // the working copy the agent should touch

	mu          sync.Mutex
	injected    []string
	created     int
	interrupted int
	state       string
	frames      chan sseFrame
	agents      []attach.AgentInfo // what GET .../agents reports
}

func newFakeDaemon(t *testing.T, token string, dir func() string, agent agentFunc) (*fakeDaemon, *httptest.Server) {
	d := &fakeDaemon{t: t, token: token, agent: agent, dir: dir, state: attach.AgentStateIdle, frames: make(chan sseFrame, 64)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sessions", d.auth(func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		d.created++
		d.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeJSONT(w, map[string]any{"app": "core-agent", "user": "sa:selfdev-dispatcher", "sessionID": "sess-1", "url": "/sessions/core-agent/sess-1"})
	}))
	base := "/sessions/core-agent/sess-1"
	mux.HandleFunc("POST "+base+"/inject", d.auth(d.inject))
	mux.HandleFunc("GET "+base+"/events", d.auth(d.events))
	mux.HandleFunc("GET "+base+"/status", d.auth(func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		writeJSONT(w, attach.StatusInfo{State: d.state, TurnInFlight: d.state == attach.AgentStateRunning})
	}))
	mux.HandleFunc("GET "+base+"/agents", d.auth(func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		writeJSONT(w, map[string]any{"agents": d.agents})
	}))
	mux.HandleFunc("POST "+base+"/interrupt", d.auth(func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		d.interrupted++
		d.state = attach.AgentStateIdle
		d.mu.Unlock()
		writeJSONT(w, map[string]any{"interrupted": true})
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return d, srv
}

func (d *fakeDaemon) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+d.token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (d *fakeDaemon) inject(w http.ResponseWriter, r *http.Request) {
	var req attach.InjectRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	d.mu.Lock()
	d.injected = append(d.injected, req.Message)
	d.state = attach.AgentStateRunning
	d.mu.Unlock()
	writeJSONT(w, attach.InjectResponse{Injected: req.Message, Session: "sess-1", Woke: true})
	go func() {
		frames := d.agent(d.t, d.dir())
		d.frames <- sseFrame{Event: attach.EventStatusUpdate, Data: attach.StatusUpdate{TurnState: attach.TurnStateStreaming}}
		for _, f := range frames {
			d.frames <- f
		}
		d.mu.Lock()
		if d.state == attach.AgentStateRunning {
			d.state = attach.AgentStateIdle
		}
		d.mu.Unlock()
	}()
}

func (d *fakeDaemon) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl := w.(http.Flusher)
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case f := <-d.frames:
			buf, _ := json.Marshal(f.Data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", f.Event, buf)
			fl.Flush()
		}
	}
}

func (d *fakeDaemon) injectedTasks() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.injected...)
}

func turnComplete() sseFrame {
	return sseFrame{Event: attach.EventTurnComplete, Data: attach.TurnComplete{PromptID: "p1", Model: "claude-sonnet-5"}}
}

// durableRow wraps an eventlog row the way the broadcaster sends it.
func durableRow(seq int64, ev any) sseFrame {
	return sseFrame{Event: attach.EventAgent, Data: map[string]any{"seq": seq, "event": ev}}
}

// ---- git helpers ---------------------------------------------------------

// gitT runs git for test setup with the user's global config isolated.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_DATE=2026-10-07T00:00:00Z", "GIT_COMMITTER_DATE=2026-10-07T00:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newMirror makes a bare repo standing in for the GitHub mirror, with a
// main whose history once held a seed list that the tip removed.
func newMirror(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "mirror.git")
	seed := filepath.Join(root, "seed")
	gitT(t, root, "init", "--quiet", "--bare", "-b", "main", bare)
	gitT(t, root, "init", "--quiet", "-b", "main", seed)
	gitT(t, seed, "config", "user.name", "Pat Maintainer")
	gitT(t, seed, "config", "user.email", "pat@example.com")
	writeFile(t, filepath.Join(seed, "seeds.md"), "issue 1 expect: should-stop\n")
	gitT(t, seed, "add", "seeds.md")
	gitT(t, seed, "commit", "--quiet", "-m", "seed list")
	gitT(t, seed, "rm", "--quiet", "seeds.md")
	writeFile(t, filepath.Join(seed, "README.md"), "mirror\n")
	gitT(t, seed, "add", "README.md")
	gitT(t, seed, "commit", "--quiet", "-m", "drop the seed list")
	// A release tag in the history, as upstream's mirror carries: the
	// dispatcher refuses a copy that no release tag reaches (decision 24).
	gitT(t, seed, "tag", "v0.1.0", "HEAD~1")
	gitT(t, seed, "push", "--quiet", bare, "main", "--tags")
	return bare
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// commitAs returns an agent that commits one change in the working copy
// with the given extra git args (e.g. an identity override), then ends
// its turn.
func commitAs(extra ...string) agentFunc {
	return func(t *testing.T, dir string) []sseFrame {
		writeFile(t, filepath.Join(dir, "fix.go"), "package fix\n")
		gitT(t, dir, "add", "fix.go")
		gitT(t, dir, append(extra, "commit", "--quiet", "-s", "-m", "fix: recover the half that was broken\n\nRefs "+testUpstream)...)
		return []sseFrame{turnComplete()}
	}
}

// ---- dispatcher under test -----------------------------------------------

type rig struct {
	d      *dispatcher
	gh     *fakeGitHub
	daemon *fakeDaemon
	mirror string
	cfg    config
	logs   *syncBuffer // everything the dispatcher logged
}

func newRig(t *testing.T, agent agentFunc) *rig {
	t.Helper()
	// The dispatcher's own git children must not read the user's config
	// either; t.Setenv rules out t.Parallel for these tests.
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	gh, ghSrv := newFakeGitHub(t, &key.PublicKey)
	work := t.TempDir()
	cfg := config{
		Owner: testOwner, Repo: testRepo, Maintainer: testMaintainer, Upstream: "go-steer/core-agent",
		APIURL: ghSrv.URL, BaseBranch: "main", Poll: 20 * time.Millisecond, MaxOpenPRs: 3,
		PrivateRepo: filepath.Join(t.TempDir(), "private.git"), WorktreesDir: filepath.Join(work, "worktrees"),
		AgentsDir: filepath.Join(work, ".agents"), StateFile: filepath.Join(t.TempDir(), "state.json"),
		Identity: testIdentity, SessionTimeout: 10 * time.Second, Settle: 60 * time.Millisecond,
		StartTimeout: 3 * time.Second, AppID: testAppID, Once: true,
	}
	cfg.GitRemote = newMirror(t)
	cfg.TagsRemote = cfg.GitRemote
	r := &rig{gh: gh, mirror: cfg.GitRemote, cfg: cfg}
	daemon, dSrv := newFakeDaemon(t, "attach-secret", func() string { return filepath.Join(cfg.WorktreesDir, "issue-1") }, agent)
	r.daemon = daemon
	cfg.AttachURL = dSrv.URL
	tokens := &appTokenSource{api: ghSrv.URL, appID: testAppID, key: key, owner: testOwner, repo: testRepo, http: ghSrv.Client(), now: time.Now}
	r.logs = &syncBuffer{}
	log := slog.New(slog.NewTextHandler(r.logs, nil))
	d, err := newDispatcher(cfg, "attach-secret", tokens, log)
	if err != nil {
		t.Fatal(err)
	}
	d.daemon.reconnect = 20 * time.Millisecond
	r.d, r.cfg = d, cfg
	return r
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

const keyLine = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
