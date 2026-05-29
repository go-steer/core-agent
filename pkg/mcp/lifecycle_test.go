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
	"errors"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
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
