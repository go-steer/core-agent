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

package vcs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
	"github.com/go-steer/core-vcs/providers"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/internal/testutil"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

type testGHServer struct {
	Server   *httptest.Server
	Host     string
	Port     int
	RepoPath string

	dir   string
	bare  *git.Repository
	store *filesystem.Storage

	mu        sync.Mutex
	token     string
	protected map[string]bool
	nextPR    int
	prs       map[int]map[string]any
	comments  map[int][]map[string]any
}

func newTestGHServer(t *testing.T, repoPath string, files map[string]string) *testGHServer {
	t.Helper()
	if repoPath == "" {
		repoPath = "acme/infra"
	}
	dir := filepath.Join(t.TempDir(), "bare.git")
	fs := osfs.New(dir)
	store := filesystem.NewStorage(fs, cache.NewObjectLRUDefault())
	bare, err := git.Init(store, nil)
	if err != nil {
		t.Fatalf("git.Init bare: %v", err)
	}
	s := &testGHServer{
		RepoPath:  repoPath,
		dir:       dir,
		bare:      bare,
		store:     store,
		token:     "ghp-test-token-12345",
		protected: map[string]bool{"main": true},
		nextPR:    1,
		prs:       make(map[int]map[string]any),
		comments:  make(map[int][]map[string]any),
	}
	if files == nil {
		files = map[string]string{
			"deploy/replicas.yaml": "replicas: 2\n",
			"README.md":            "# acme/infra\n",
		}
	}
	s.seedBranch(t, "main", "initial commit", files)
	if err := store.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatalf("set HEAD: %v", err)
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Server.Close)
	u, err := url.Parse(s.Server.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	s.Host = host
	s.Port = port
	return s
}

func (s *testGHServer) seedBranch(t *testing.T, branch, msg string, files map[string]string) string {
	t.Helper()
	memStore := memory.NewStorage()
	wtFS := memfs.New()
	work, err := git.Init(memStore, wtFS)
	if err != nil {
		t.Fatalf("git.Init mem: %v", err)
	}
	wt, err := work.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	for path, content := range files {
		if dir := filepath.Dir(path); dir != "." && dir != "/" {
			_ = wtFS.MkdirAll(dir, 0o755)
		}
		f, err := wtFS.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(content))
		_ = f.Close()
		if _, err := wt.Add(path); err != nil {
			t.Fatal(err)
		}
	}
	sig := &object.Signature{
		Name:  "seed",
		Email: "seed@acme.example",
		When:  time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC),
	}
	hash, err := wt.Commit(msg, &git.CommitOptions{
		Author:            sig,
		Committer:         sig,
		AllowEmptyCommits: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	iter, _ := memStore.IterEncodedObjects(plumbing.AnyObject)
	_ = iter.ForEach(func(obj plumbing.EncodedObject) error {
		_, err := s.store.SetEncodedObject(obj)
		return err
	})
	refName := plumbing.NewBranchReferenceName(branch)
	if err := s.store.SetReference(plumbing.NewHashReference(refName, hash)); err != nil {
		t.Fatal(err)
	}
	return hash.String()
}

func (s *testGHServer) branchHash(branch string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, err := s.bare.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return ""
	}
	return ref.Hash().String()
}

type singleRepoLoader struct{ s storer.Storer }

func (l singleRepoLoader) Load(*transport.Endpoint) (storer.Storer, error) { return l.s, nil }

func (s *testGHServer) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/v3/") {
		s.serveAPI(w, r)
		return
	}
	s.serveGit(w, r)
}

func (s *testGHServer) serveGit(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, pass, ok := r.BasicAuth()
	if !ok || pass != s.token {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	prefix := "/" + s.RepoPath + ".git"
	if !strings.HasPrefix(r.URL.Path, prefix+"/") {
		http.NotFound(w, r)
		return
	}
	sub := strings.TrimPrefix(r.URL.Path, prefix)
	srv := server.NewServer(singleRepoLoader{s: s.store})
	ep := &transport.Endpoint{Path: s.dir}

	switch {
	case sub == "/info/refs" && r.Method == http.MethodGet:
		svc := r.URL.Query().Get("service")
		var sess transport.Session
		var err error
		switch svc {
		case transport.UploadPackServiceName:
			sess, err = srv.NewUploadPackSession(ep, nil)
		case transport.ReceivePackServiceName:
			sess, err = srv.NewReceivePackSession(ep, nil)
		default:
			http.Error(w, "unsupported", http.StatusForbidden)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = sess.Close() }()
		ar, err := sess.AdvertisedReferencesContext(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if headRef, err := s.store.Reference(plumbing.HEAD); err == nil && headRef.Type() == plumbing.SymbolicReference {
			if ar.Capabilities != nil && len(ar.Capabilities.Get(capability.SymRef)) == 0 {
				_ = ar.Capabilities.Add(capability.SymRef, "HEAD:"+headRef.Target().String())
			}
		}
		ar.Prefix = [][]byte{[]byte("# service=" + svc), pktline.Flush}
		w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
		_ = ar.Encode(w)

	case sub == "/"+transport.UploadPackServiceName && r.Method == http.MethodPost:
		sess, err := srv.NewUploadPackSession(ep, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = sess.Close() }()
		upReq := packp.NewUploadPackRequest()
		if err := upReq.Decode(r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := sess.UploadPack(r.Context(), upReq)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = resp.Close() }()
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		_ = resp.Encode(w)

	case sub == "/"+transport.ReceivePackServiceName && r.Method == http.MethodPost:
		sess, err := srv.NewReceivePackSession(ep, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = sess.Close() }()
		raw, _ := io.ReadAll(r.Body)
		upReq := packp.NewReferenceUpdateRequest()
		if err := upReq.Decode(bytes.NewReader(raw)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		status, _ := sess.ReceivePack(r.Context(), upReq)
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		if status != nil {
			_ = status.Encode(w)
		}
	default:
		http.NotFound(w, r)
	}
}

func (s *testGHServer) serveAPI(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer "+s.token {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Bad credentials"})
		return
	}

	route := strings.TrimPrefix(r.URL.Path, "/api/v3")
	repoPrefix := "/repos/" + s.RepoPath
	w.Header().Set("Content-Type", "application/json")

	switch {
	case route == repoPrefix && r.Method == http.MethodGet:
		_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})

	case strings.HasPrefix(route, repoPrefix+"/branches/") && r.Method == http.MethodGet:
		branch, _ := url.PathUnescape(strings.TrimPrefix(route, repoPrefix+"/branches/"))
		prot, exists := s.protected[branch]
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Branch not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"protected": prot})

	case route == repoPrefix+"/pulls" && r.Method == http.MethodPost:
		var in struct {
			Title string `json:"title"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Body  string `json:"body"`
			Draft bool   `json:"draft"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		num := s.nextPR
		s.nextPR++
		sha := ""
		if ref, err := s.bare.Reference(plumbing.NewBranchReferenceName(in.Head), true); err == nil {
			sha = ref.Hash().String()
		}
		pr := map[string]any{
			"number":     num,
			"html_url":   s.Server.URL + "/" + s.RepoPath + "/pull/" + strconv.Itoa(num),
			"title":      in.Title,
			"body":       in.Body,
			"state":      "open",
			"draft":      in.Draft,
			"user":       map[string]any{"login": "platform-agent[bot]", "type": "Bot"},
			"head":       map[string]any{"ref": in.Head, "sha": sha},
			"base":       map[string]any{"ref": in.Base},
			"labels":     []map[string]any{},
			"created_at": "2026-11-06T12:00:00Z",
			"updated_at": "2026-11-06T12:00:00Z",
		}
		s.prs[num] = pr
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(pr)

	case route == repoPrefix+"/pulls" && r.Method == http.MethodGet:
		var out []map[string]any
		for i := 1; i < s.nextPR; i++ {
			if pr, ok := s.prs[i]; ok {
				out = append(out, pr)
			}
		}
		_ = json.NewEncoder(w).Encode(out)

	case strings.HasPrefix(route, repoPrefix+"/pulls/") && r.Method == http.MethodGet:
		rest := strings.TrimPrefix(route, repoPrefix+"/pulls/")
		switch {
		case strings.HasSuffix(rest, "/reviews") || strings.HasSuffix(rest, "/comments"):
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			num, _ := strconv.Atoi(rest)
			pr, ok := s.prs[num]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"})
				return
			}
			_ = json.NewEncoder(w).Encode(pr)
		}

	case strings.HasPrefix(route, repoPrefix+"/issues/") && strings.HasSuffix(route, "/labels") && r.Method == http.MethodPost:
		rest := strings.TrimSuffix(strings.TrimPrefix(route, repoPrefix+"/issues/"), "/labels")
		num, _ := strconv.Atoi(rest)
		var in struct {
			Labels []string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		var lbls []map[string]any
		for _, l := range in.Labels {
			lbls = append(lbls, map[string]any{"name": l})
		}
		if pr, ok := s.prs[num]; ok {
			pr["labels"] = lbls
		}
		_ = json.NewEncoder(w).Encode(lbls)

	case strings.HasPrefix(route, repoPrefix+"/issues/") && strings.HasSuffix(route, "/comments"):
		rest := strings.TrimSuffix(strings.TrimPrefix(route, repoPrefix+"/issues/"), "/comments")
		num, _ := strconv.Atoi(rest)
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(s.comments[num])
			return
		}
		var in struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		id := len(s.comments[num]) + 1
		c := map[string]any{
			"id":         id,
			"body":       in.Body,
			"html_url":   s.Server.URL + "/" + s.RepoPath + "/pull/" + strconv.Itoa(num) + "#issuecomment-" + strconv.Itoa(id),
			"user":       map[string]any{"login": "platform-agent[bot]", "type": "Bot"},
			"created_at": "2026-11-06T12:05:00Z",
		}
		s.comments[num] = append(s.comments[num], c)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(c)

	default:
		http.NotFound(w, r)
	}
}

type runnableTool interface {
	Declaration() *genai.FunctionDeclaration
	Run(ctx adkagent.Context, args any) (result map[string]any, err error)
}

type testToolCtx struct {
	adkagent.StrictContextMock
	ctx context.Context
}

func (c *testToolCtx) Deadline() (time.Time, bool) { return c.ctx.Deadline() }
func (c *testToolCtx) Done() <-chan struct{}       { return c.ctx.Done() }
func (c *testToolCtx) Err() error                  { return c.ctx.Err() }
func (c *testToolCtx) Value(key any) any           { return c.ctx.Value(key) }
func (c *testToolCtx) AppName() string             { return "test-app" }
func (c *testToolCtx) UserID() string              { return "test-user" }
func (c *testToolCtx) SessionID() string           { return "test-session" }
func (c *testToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation {
	return nil
}

func newTestToolCtx() adkagent.Context {
	base := testutil.ApproverContext(context.Background())
	return &testToolCtx{
		StrictContextMock: adkagent.NewStrictContextMock(base),
		ctx:               base,
	}
}

func invokeTool(t *testing.T, toolsByName map[string]tool.Tool, name string, args map[string]any) (map[string]any, error) {
	t.Helper()
	tl, ok := toolsByName[name]
	if !ok {
		t.Fatalf("tool %q not found in built suite", name)
	}
	ft, ok := tl.(runnableTool)
	if !ok {
		t.Fatalf("tool %q does not implement runnableTool", name)
	}
	if args == nil {
		args = map[string]any{}
	}
	return ft.Run(newTestToolCtx(), args)
}

func indexTools(tools []tool.Tool) map[string]tool.Tool {
	m := make(map[string]tool.Tool, len(tools))
	for _, tl := range tools {
		m[tl.Name()] = tl
	}
	return m
}

func TestBuild_EmptyConfigRegistersNothing(t *testing.T) {
	t.Parallel()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	got, err := Build(config.DefaultConfig(), gate, t.TempDir())
	if err != nil {
		t.Fatalf("Build(default): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Build(default) returned %d tools, want 0", len(got))
	}
	gotNil, err := Build(nil, gate, t.TempDir())
	if err != nil || len(gotNil) != 0 {
		t.Fatalf("Build(nil) = (%v, %v), want (nil, nil)", gotNil, err)
	}
}

func TestBuild_DisableListFiltersTools(t *testing.T) {
	t.Parallel()
	srv := newTestGHServer(t, "acme/infra", nil)
	cfg := config.DefaultConfig()
	cfg.Tools.Disable = []string{"vcs_publish", "vcs_propose"}
	cfg.VCS = config.VCSConfig{
		Forges: []config.VCSForgeConfig{{
			Name:     "ghes",
			Provider: config.VCSProviderGitHub,
			Host:     srv.Host,
			Scheme:   "http",
			Port:     srv.Port,
			TokenEnv: "GH_TOKEN",
		}},
		Repositories: []config.VCSRepositoryConfig{{
			Forge:      "ghes",
			Repository: "acme/infra",
			Role:       config.VCSRoleGitOps,
			BaseBranch: "main",
		}},
	}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	built, err := Build(cfg, gate, t.TempDir(), WithGetenv(func(string) string { return srv.token }))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	byName := indexTools(built)
	if _, ok := byName["vcs_publish"]; ok {
		t.Error("vcs_publish should be omitted when listed in tools.disable")
	}
	if _, ok := byName["vcs_propose"]; ok {
		t.Error("vcs_propose should be omitted when listed in tools.disable")
	}
	if _, ok := byName["vcs_clone"]; !ok {
		t.Error("vcs_clone should remain enabled")
	}
}

func TestSuite_EndToEndGitOpsWorkflow(t *testing.T) {
	t.Parallel()
	srv := newTestGHServer(t, "acme/infra", map[string]string{
		"deploy/replicas.yaml": "replicas: 2\n",
		"README.md":            "# acme/infra\n",
	})
	cwd := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "gh-token")
	if err := os.WriteFile(tokenFile, []byte(srv.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	cfg.VCS = config.VCSConfig{
		WorkspaceDir: filepath.Join(cwd, ".vcs"),
		BranchPrefix: "agent/",
		Forges: []config.VCSForgeConfig{{
			Name:      "ghes",
			Provider:  config.VCSProviderGitHub,
			Host:      srv.Host,
			Scheme:    "http",
			Port:      srv.Port,
			TokenFile: tokenFile,
			CommitIdentity: &config.VCSCommitIdentityConfig{
				Name:  "platform-agent[bot]",
				Email: "platform-agent@acme.example",
			},
		}},
		Repositories: []config.VCSRepositoryConfig{{
			Forge:      "ghes",
			Repository: "acme/infra",
			Role:       config.VCSRoleGitOps,
			BaseBranch: "main",
		}},
	}
	gate, err := permissions.FromConfig(cfg, cwd, "", nil)
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	gate.SetMode(permissions.ModeYolo)

	var events []Event
	built, err := Build(cfg, gate, cwd,
		WithRegistry(providers.NewRegistry()),
		WithEventSink(func(_ context.Context, ev Event) {
			events = append(events, ev)
		}),
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	byName := indexTools(built)

	// 1. vcs_capabilities
	capOut, err := invokeTool(t, byName, "vcs_capabilities", nil)
	if err != nil {
		t.Fatalf("vcs_capabilities: %v", err)
	}
	if capOut["base_branch"] != "main" || capOut["branch_prefix"] != "agent/" {
		t.Errorf("vcs_capabilities = %v, want base_branch=main branch_prefix=agent/", capOut)
	}

	// 2. vcs_clone
	cloneOut, err := invokeTool(t, byName, "vcs_clone", nil)
	if err != nil {
		t.Fatalf("vcs_clone: %v", err)
	}
	wsPath, _ := cloneOut["path"].(string)
	if wsPath == "" {
		t.Fatalf("vcs_clone returned empty path: %v", cloneOut)
	}

	// 3. Edit a file inside the cloned workspace.
	if err := os.WriteFile(filepath.Join(wsPath, "deploy/replicas.yaml"), []byte("replicas: 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 4. vcs_status
	statusOut, err := invokeTool(t, byName, "vcs_status", nil)
	if err != nil {
		t.Fatalf("vcs_status: %v", err)
	}
	stMap, _ := statusOut["status"].(map[string]any)
	if clean, _ := stMap["clean"].(bool); clean {
		t.Errorf("vcs_status clean = %v, want false (status=%v)", stMap["clean"], stMap)
	}

	// 5. vcs_diff
	diffOut, err := invokeTool(t, byName, "vcs_diff", nil)
	if err != nil {
		t.Fatalf("vcs_diff: %v", err)
	}
	diffStr, _ := diffOut["diff"].(string)
	if !strings.Contains(diffStr, "+replicas: 4") {
		t.Errorf("vcs_diff = %q, want +replicas: 4", diffStr)
	}

	// 6. vcs_commit on topic branch
	commitOut, err := invokeTool(t, byName, "vcs_commit", map[string]any{
		"branch":  "agent/scale-replicas",
		"message": "fix: scale replicas from 2 to 4 for peak load",
	})
	if err != nil {
		t.Fatalf("vcs_commit: %v", err)
	}
	if commitOut["branch"] != "agent/scale-replicas" {
		t.Errorf("vcs_commit branch = %v, want agent/scale-replicas", commitOut["branch"])
	}

	// 7. vcs_log
	logOut, err := invokeTool(t, byName, "vcs_log", map[string]any{"limit": 5})
	if err != nil {
		t.Fatalf("vcs_log: %v", err)
	}
	commits, _ := logOut["commits"].([]any)
	if len(commits) != 2 {
		t.Errorf("vcs_log len(commits) = %d, want 2", len(commits))
	}

	// 8. vcs_publish
	pubOut, err := invokeTool(t, byName, "vcs_publish", map[string]any{
		"branch": "agent/scale-replicas",
	})
	if err != nil {
		t.Fatalf("vcs_publish: %v", err)
	}
	resMap, _ := pubOut["result"].(map[string]any)
	if resMap["Branch"] != "agent/scale-replicas" && resMap["branch"] != "agent/scale-replicas" {
		t.Errorf("vcs_publish result = %v", pubOut)
	}
	if srv.branchHash("agent/scale-replicas") == "" {
		t.Error("remote branch agent/scale-replicas was not pushed")
	}

	// 9. vcs_propose
	propOut, err := invokeTool(t, byName, "vcs_propose", map[string]any{
		"title":         "fix: scale replicas to 4",
		"body":          "Root cause: CPU saturation. Evidence: p99 latency. Change: replicas 2->4. Rollback: revert PR.",
		"source_branch": "agent/scale-replicas",
		"labels":        []string{"gitops"},
	})
	if err != nil {
		t.Fatalf("vcs_propose: %v", err)
	}
	propMap, _ := propOut["proposal"].(map[string]any)
	if propMap["Number"] != float64(1) && propMap["number"] != float64(1) {
		t.Errorf("vcs_propose number = %v, want 1", propMap)
	}

	// 10. vcs_proposal_comment
	_, err = invokeTool(t, byName, "vcs_proposal_comment", map[string]any{
		"number": 1,
		"body":   "Verified rollout in staging.",
	})
	if err != nil {
		t.Fatalf("vcs_proposal_comment: %v", err)
	}

	// 11. vcs_proposal_view
	viewOut, err := invokeTool(t, byName, "vcs_proposal_view", map[string]any{"number": 1})
	if err != nil {
		t.Fatalf("vcs_proposal_view: %v", err)
	}
	commentsList, _ := viewOut["comments"].([]any)
	if len(commentsList) != 1 {
		t.Errorf("vcs_proposal_view comments = %v, want 1 comment", commentsList)
	}

	// 12. vcs_proposal_list
	listOut, err := invokeTool(t, byName, "vcs_proposal_list", map[string]any{"state": "open"})
	if err != nil {
		t.Fatalf("vcs_proposal_list: %v", err)
	}
	proposalsList, _ := listOut["proposals"].([]any)
	if len(proposalsList) != 1 {
		t.Errorf("vcs_proposal_list proposals = %v, want 1", proposalsList)
	}

	// 13. Verify EventSink received vcs.published and vcs.proposed.
	if len(events) != 2 || events[0].Kind != EventKindPublished || events[1].Kind != EventKindProposed {
		t.Errorf("events = %+v, want [vcs.published, vcs.proposed]", events)
	}
}

func TestSuite_GuardsAndCredentialRefusal(t *testing.T) {
	t.Parallel()
	srv := newTestGHServer(t, "acme/infra", nil)
	cwd := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "secret-vcs-token")
	if err := os.WriteFile(tokenFile, []byte(srv.token), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultConfig()
	cfg.Permissions.PlanMode = config.PlanModeRequired
	cfg.VCS = config.VCSConfig{
		WorkspaceDir: filepath.Join(cwd, ".vcs"),
		BranchPrefix: "agent/",
		Forges: []config.VCSForgeConfig{{
			Name:      "ghes",
			Provider:  config.VCSProviderGitHub,
			Host:      srv.Host,
			Scheme:    "http",
			Port:      srv.Port,
			TokenFile: tokenFile,
		}},
		Repositories: []config.VCSRepositoryConfig{{
			Forge:      "ghes",
			Repository: "acme/infra",
			Role:       config.VCSRoleGitOps,
			BaseBranch: "main",
		}},
	}
	gate, err := permissions.FromConfig(cfg, cwd, "", nil)
	if err != nil {
		t.Fatalf("FromConfig: %v", err)
	}
	gate.SetMode(permissions.ModeYolo)

	built, err := Build(cfg, gate, cwd)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	byName := indexTools(built)

	// 1. Credential file mention is refused on every vcs_* tool before any network call.
	_, err = invokeTool(t, byName, "vcs_capabilities", map[string]any{"repository": tokenFile})
	if !errors.Is(err, permissions.ErrCredentialFile) {
		t.Errorf("vcs_capabilities(tokenFile) err = %v, want ErrCredentialFile", err)
	}

	// 2. Plan-exempt read tools work before record_plan.
	cloneOut, err := invokeTool(t, byName, "vcs_clone", nil)
	if err != nil {
		t.Fatalf("vcs_clone before plan should succeed (plan-exempt), got: %v", err)
	}
	wsPath, _ := cloneOut["path"].(string)
	if err := os.WriteFile(filepath.Join(wsPath, "README.md"), []byte("# updated\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. Mutating VCS tools are denied before MarkPlanRecorded.
	_, err = invokeTool(t, byName, "vcs_commit", map[string]any{
		"branch":  "agent/update-readme",
		"message": "docs: update readme heading",
	})
	if err == nil || !strings.Contains(err.Error(), "record_plan") {
		t.Errorf("vcs_commit before plan err = %v, want plan-first refusal", err)
	}

	// 4. Mark plan recorded; mutating VCS tools now proceed and enforce core-vcs guards.
	gate.MarkPlanRecorded()

	// TARGET_IS_BRANCH when attempting to branch or publish "main".
	_, err = invokeTool(t, byName, "vcs_commit", map[string]any{
		"branch":  "main",
		"message": "docs: try to commit on main",
	})
	if err == nil || !strings.Contains(err.Error(), "TARGET_IS_BRANCH") {
		t.Errorf("vcs_commit(main) err = %v, want TARGET_IS_BRANCH", err)
	}

	// BRANCH_NOT_OURS when branch lacks branch_prefix.
	_, err = invokeTool(t, byName, "vcs_commit", map[string]any{
		"branch":  "feature/no-prefix",
		"message": "docs: try bad prefix",
	})
	if err == nil || !strings.Contains(err.Error(), "BRANCH_NOT_OURS") {
		t.Errorf("vcs_commit(feature/no-prefix) err = %v, want BRANCH_NOT_OURS", err)
	}
}

func TestSuite_ApproverReceivesDiffSummary(t *testing.T) {
	t.Parallel()
	srv := newTestGHServer(t, "acme/infra", nil)
	cwd := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.VCS = config.VCSConfig{
		WorkspaceDir: filepath.Join(cwd, ".vcs"),
		BranchPrefix: "agent/",
		Forges: []config.VCSForgeConfig{{
			Name:     "ghes",
			Provider: config.VCSProviderGitHub,
			Host:     srv.Host,
			Scheme:   "http",
			Port:     srv.Port,
			TokenEnv: "GH_TOKEN",
		}},
		Repositories: []config.VCSRepositoryConfig{{
			Forge:      "ghes",
			Repository: "acme/infra",
			Role:       config.VCSRoleGitOps,
			BaseBranch: "main",
		}},
	}
	gate, probe := testutil.AutoGateInScope(t, cwd, "vcs_commit:*", "vcs_publish:*")
	built, err := Build(cfg, gate, cwd, WithGetenv(func(string) string { return srv.token }))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	byName := indexTools(built)

	cloneOut, err := invokeTool(t, byName, "vcs_clone", nil)
	if err != nil {
		t.Fatalf("vcs_clone: %v", err)
	}
	wsPath, _ := cloneOut["path"].(string)
	if err := os.WriteFile(filepath.Join(wsPath, "deploy/replicas.yaml"), []byte("replicas: 9\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	commitTool := byName["vcs_commit"].(runnableTool)
	_, err = commitTool.Run(newTestToolCtx(), map[string]any{
		"branch":  "agent/approver-check",
		"message": "fix: bump replicas to 9",
	})
	if err == nil {
		t.Fatal("vcs_commit succeeded, want probe deny")
	}
	probe.RequireArgs(t, "vcs_commit", "+replicas: 9")
}
