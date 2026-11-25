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

package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
	adkagent "google.golang.org/adk/v2/agent"

	"github.com/go-steer/core-agent/v3/pkg/agentenv"
	"github.com/go-steer/core-agent/v3/pkg/digest"
	"github.com/go-steer/core-agent/v3/pkg/permissions"
	coretools "github.com/go-steer/core-agent/v3/pkg/tools"
)

func TestServer_Close_NilSafe(t *testing.T) {
	t.Parallel()
	(*Server)(nil).Close()
	(&Server{}).Close()
	(&Server{cmd: exec.Command("/bin/true")}).Close()
}

// stubTokenSource is a minimal oauth2.TokenSource for testing.
type stubTokenSource struct {
	token *oauth2.Token
	err   error
}

func (s *stubTokenSource) Token() (*oauth2.Token, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.token, nil
}

// captureTransport records the last request seen and reports whether
// it was called at all. Useful for asserting both the request shape
// and whether an upstream RoundTripper short-circuited.
type captureTransport struct {
	called bool
	req    *http.Request
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.called = true
	c.req = req
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     http.Header{},
	}, nil
}

func TestGoogleAuthTransport_SetsAuthorizationHeader(t *testing.T) {
	t.Parallel()
	base := &captureTransport{}
	rt := &googleAuthTransport{
		base:   base,
		source: &stubTokenSource{token: &oauth2.Token{AccessToken: "test-token"}},
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/mcp", nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if !base.called {
		t.Fatal("base RoundTripper was not called")
	}
	if got, want := base.req.Header.Get("Authorization"), "Bearer test-token"; got != want {
		t.Errorf("Authorization header: got %q want %q", got, want)
	}
}

func TestGoogleAuthTransport_TokenErrorShortCircuits(t *testing.T) {
	t.Parallel()
	base := &captureTransport{}
	sentinel := errors.New("synthetic-token-error")
	rt := &googleAuthTransport{
		base:   base,
		source: &stubTokenSource{err: sentinel},
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/mcp", nil)
	_, err := rt.RoundTrip(req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain should wrap sentinel: got %v", err)
	}
	if base.called {
		t.Error("base RoundTripper must not be called when token fetch fails")
	}
}

func TestRoundTripperChain_AuthWinsOverStaticAuthorization(t *testing.T) {
	t.Parallel()
	// Wire the same composition transportFor builds: OAuth innermost,
	// headers outermost. The static headers map intentionally tries to
	// set Authorization (a misconfiguration); OAuth should overwrite it,
	// while a non-conflicting X-Custom header passes through.
	base := &captureTransport{}
	authRT := &googleAuthTransport{
		base:   base,
		source: &stubTokenSource{token: &oauth2.Token{AccessToken: "oauth-wins"}},
	}
	chain := &headerTransport{
		base: authRT,
		headers: map[string]string{
			"Authorization": "Bearer should-be-overwritten",
			"X-Custom":      "preserved",
		},
	}
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/mcp", nil)
	if _, err := chain.RoundTrip(req); err != nil {
		t.Fatalf("chain.RoundTrip: %v", err)
	}
	if got, want := base.req.Header.Get("Authorization"), "Bearer oauth-wins"; got != want {
		t.Errorf("OAuth must win over static Authorization: got %q want %q", got, want)
	}
	if got, want := base.req.Header.Get("X-Custom"), "preserved"; got != want {
		t.Errorf("non-conflicting static header should pass through: got %q want %q", got, want)
	}
}

func TestCloseAll_ClosesConcurrentlyAndTolerateNils(t *testing.T) {
	t.Parallel()
	var servers []*Server
	for i := 0; i < 3; i++ {
		cmd := exec.Command("/bin/sh", "-c", "sleep 60")
		if err := cmd.Start(); err != nil {
			t.Skipf("can't spawn child: %v", err)
		}
		// Reap on any exit path (including the Skipf above on a later
		// iteration); double-kill after CloseAll is a harmless error.
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		servers = append(servers, &Server{cmd: cmd})
	}
	// Nil entry and HTTP-transport server (no cmd) must be tolerated.
	servers = append(servers, nil, &Server{})

	done := make(chan struct{})
	go func() {
		CloseAll(servers)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		for _, s := range servers {
			if s != nil && s.cmd != nil && s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
		}
		t.Fatal("CloseAll did not return within 5s (children should terminate on SIGTERM well before the 3s per-server grace)")
	}
}

func TestServer_Close_ReapsStartedProcess(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("/bin/sh", "-c", "sleep 60")
	if err := cmd.Start(); err != nil {
		t.Skipf("can't spawn child: %v", err)
	}
	srv := &Server{cmd: cmd}

	done := make(chan struct{})
	go func() {
		srv.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("Server.Close did not return within 5s")
	}
}

// TestTransportFor_InjectedGoogleAuthSendsPlaceholder verifies that
// CORE_AGENT_GOOGLE_AUTH=injected skips google.FindDefaultCredentials
// and attaches "Authorization: Bearer placeholder" on HTTP MCP servers
// configured with auth.google_oauth. Not parallel: t.Setenv.
func TestTransportFor_InjectedGoogleAuthSendsPlaceholder(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", t.TempDir()+"/missing.json")
	t.Setenv(agentenv.EnvGoogleAuth, agentenv.GoogleAuthInjected)

	var (
		mu   sync.Mutex
		auth string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tr, _, err := transportFor(context.Background(), "gke", ServerSpec{
		Transport: "http",
		URL:       srv.URL,
		Auth: &AuthSpec{
			GoogleOAuth: &GoogleOAuthAuth{Scopes: []string{"https://www.googleapis.com/auth/container.read-only"}},
		},
	})
	if err != nil {
		t.Fatalf("transportFor with injected auth: %v", err)
	}
	st, ok := tr.(*mcpsdk.StreamableClientTransport)
	if !ok || st.HTTPClient == nil {
		t.Fatalf("transport = %#v, want *mcpsdk.StreamableClientTransport with HTTPClient", tr)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := st.HTTPClient.Do(req)
	if err != nil {
		t.Fatalf("HTTPClient.Do: %v", err)
	}
	_ = resp.Body.Close()

	mu.Lock()
	gotAuth := auth
	mu.Unlock()
	if want := "Bearer " + agentenv.InjectedGooglePlaceholder; gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

// A mistyped CORE_AGENT_GOOGLE_AUTH on a google_oauth server is refused
// by name, not run through FindDefaultCredentials to fail about ADC
// (#1419). Not parallel: t.Setenv.
func TestTransportFor_RejectsMistypedGoogleAuthEnv(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", t.TempDir()+"/missing.json")
	t.Setenv(agentenv.EnvGoogleAuth, "Injected")

	_, _, err := transportFor(context.Background(), "gke", ServerSpec{
		Transport: "http",
		URL:       "http://127.0.0.1:9",
		Auth: &AuthSpec{
			GoogleOAuth: &GoogleOAuthAuth{Scopes: []string{"https://www.googleapis.com/auth/container.read-only"}},
		},
	})
	if err == nil {
		t.Fatal("transportFor with CORE_AGENT_GOOGLE_AUTH=Injected returned nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), agentenv.EnvGoogleAuth) || !strings.Contains(err.Error(), `"Injected"`) {
		t.Fatalf("error %q should name the variable and the bad value, not ADC", err)
	}
	if strings.Contains(err.Error(), "default credentials") {
		t.Fatalf("error %q is about ADC; the variable check must run first", err)
	}
}

func TestServer_UndigestedToolset_NilSafeAndFallback(t *testing.T) {
	t.Parallel()
	if got := (*Server)(nil).UndigestedToolset(); got != nil {
		t.Errorf("(*Server)(nil).UndigestedToolset() = %v, want nil", got)
	}
	if got := (&Server{}).UndigestedToolset(); got != nil {
		t.Errorf("(&Server{}).UndigestedToolset() = %v, want nil", got)
	}
	inner := newInMemoryToolset(t)
	srv := &Server{toolset: inner}
	if got := srv.UndigestedToolset(); got != inner {
		t.Errorf("fallback UndigestedToolset() = %v, want Toolset()", got)
	}
}

func TestWrapServerToolsets_UndigestedSkipsDigestAndPreservesGateAndHints(t *testing.T) {
	t.Parallel()
	store := &spyStore{}
	yoloGate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	spec := ServerSpec{
		Transport: "http",
		URL:       "http://127.0.0.1:1",
		ReadOnly:  true,
		ToolNotes: map[string]string{"echo": "echo operator note"},
	}
	digested, undigested, _ := wrapServerToolsets(
		newInMemoryToolset(t), "demo", spec,
		&DigestOptions{Threshold: 100, Store: store},
		yoloGate,
	)
	if digested == undigested {
		t.Fatal("expected distinct undigested toolset when digesting is active")
	}

	// Both preserve ReadOnlyHint and ToolNotes.
	digTools := toolsOf(t, digested)
	rawTools := toolsOf(t, undigested)
	digEcho := digTools["demo_echo"]
	rawEcho := rawTools["demo_echo"]
	if digEcho == nil || rawEcho == nil {
		t.Fatalf("missing demo_echo: digested=%v undigested=%v", digEcho != nil, rawEcho != nil)
	}
	if !coretools.IsReadOnlyTool(rawEcho) {
		t.Error("undigested MCP tool lost ReadOnlyHint classification")
	}
	if !strings.Contains(rawEcho.Description(), "echo operator note") {
		t.Errorf("undigested MCP tool description = %q, want operator note", rawEcho.Description())
	}

	bigMsg := "HEAD_" + strings.Repeat("payload-", 400) + "_TAIL_MARKER"
	callCtx := &stubToolCtx{StrictContextMock: adkagent.NewStrictContextMock(context.Background()), callID: "call-1604"}

	digRes, err := digEcho.(runnable).Run(callCtx, map[string]any{"msg": bigMsg})
	if err != nil {
		t.Fatalf("digested Run: %v", err)
	}
	if got, _ := digRes["method"].(string); got != digest.MethodStructuralJSON {
		t.Errorf("digested method = %q, want %q", got, digest.MethodStructuralJSON)
	}
	if got, _ := digRes["call_id"].(string); got != "call-1604" {
		t.Errorf("digested call_id = %q, want call-1604", got)
	}

	rawRes, err := rawEcho.(runnable).Run(callCtx, map[string]any{"msg": bigMsg})
	if err != nil {
		t.Fatalf("undigested Run: %v", err)
	}
	if _, hasDigest := rawRes["digest"]; hasDigest {
		t.Errorf("undigested response unexpectedly carried digest key: %+v", rawRes)
	}
	if _, hasCallID := rawRes["call_id"]; hasCallID {
		t.Errorf("undigested response unexpectedly carried call_id key: %+v", rawRes)
	}
	if !strings.Contains(fmt.Sprint(rawRes), "_TAIL_MARKER") {
		t.Errorf("undigested response truncated payload: %+v", rawRes)
	}

	// Permission gate must still bind the undigested toolset.
	denyPolicy, err := permissions.NewPolicy(nil, []string{"mcp:*"})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	denyGate := permissions.New(permissions.Options{Mode: permissions.ModeAllow, Policy: denyPolicy})
	_, gatedUndigested, _ := wrapServerToolsets(
		newInMemoryToolset(t), "demo", spec,
		&DigestOptions{Threshold: 100, Store: store},
		denyGate,
	)
	deniedEcho := toolsOf(t, gatedUndigested)["demo_echo"]
	if _, err := deniedEcho.(runnable).Run(callCtx, map[string]any{"msg": "hi"}); err == nil {
		t.Error("undigested toolset bypassed permissions.Gate deny rule")
	}

	// When digesting is off (nil opts, AgenticNever, or NeverServers), no duplicate wrapper is allocated.
	specNever := spec
	specNever.AgenticNever = true
	w1, u1, _ := wrapServerToolsets(newInMemoryToolset(t), "demo", specNever, &DigestOptions{Threshold: 100}, yoloGate)
	if w1 != u1 {
		t.Error("AgenticNever server should reuse the single toolset for UndigestedToolset")
	}
	w2, u2, _ := wrapServerToolsets(newInMemoryToolset(t), "demo", spec, nil, yoloGate)
	if w2 != u2 {
		t.Error("nil DigestOptions server should reuse the single toolset for UndigestedToolset")
	}
	w3, u3, _ := wrapServerToolsets(newInMemoryToolset(t), "demo", spec, &DigestOptions{
		Threshold:    100,
		NeverServers: map[string]bool{"demo": true},
	}, yoloGate)
	if w3 != u3 {
		t.Error("NeverServers server should reuse the single toolset for UndigestedToolset")
	}
}

func TestBuild_DoesNotMutateCallerDigestOptionsThreshold(t *testing.T) {
	t.Parallel()
	mcpSrv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "v1"}, nil)
	type in struct{}
	type out struct {
		Body string `json:"body"`
	}
	// ~2000 bytes: strictly above mcp.json's 1234 threshold and strictly below
	// DefaultAgenticWrapThreshold (8000), proving the per-project threshold was
	// actually enforced without mutating the caller's DigestOptions.
	mcpsdk.AddTool(mcpSrv, &mcpsdk.Tool{
		Name:        "payload",
		Description: "Returns a ~2000-byte payload",
	}, func(_ context.Context, _ *mcpsdk.CallToolRequest, _ in) (*mcpsdk.CallToolResult, out, error) {
		return nil, out{Body: strings.Repeat("x", 2000)}, nil
	})
	httpSrv := httptest.NewServer(mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return mcpSrv
	}, &mcpsdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer func() {
		httpSrv.CloseClientConnections()
		httpSrv.Close()
	}()

	dir := t.TempDir()
	cfgJSON := fmt.Sprintf(`{
		"version": 1,
		"agentic_wrap": true,
		"agentic_wrap_threshold": 1234,
		"servers": {
			"demo": {"transport": "http", "url": %q}
		}
	}`, httpSrv.URL)
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("write mcp.json: %v", err)
	}
	opts := &DigestOptions{Store: &spyStore{}}
	servers, _, err := Build(WithTrustedAgentsDir(context.Background(), dir), dir, "", nil, nil, nil, opts)
	defer CloseAll(servers)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if opts.Threshold != 0 {
		t.Errorf("Build mutated caller DigestOptions.Threshold to %d, want 0", opts.Threshold)
	}
	if len(servers) != 1 || servers[0].Toolset() == nil {
		t.Fatalf("unexpected servers: %+v", servers)
	}
	pl := toolsOf(t, servers[0].Toolset())["demo_payload"]
	if pl == nil {
		t.Fatal("missing demo_payload tool")
	}
	callCtx := &stubToolCtx{StrictContextMock: adkagent.NewStrictContextMock(context.Background()), callID: "call-thr"}
	res, err := pl.(runnable).Run(callCtx, map[string]any{})
	if err != nil {
		t.Fatalf("demo_payload Run: %v", err)
	}
	if got, _ := res["method"].(string); got != digest.MethodStructuralJSON {
		t.Errorf("method = %q, want %q (mcp.json threshold 1234 was not applied to 2000-byte payload)", got, digest.MethodStructuralJSON)
	}
}
