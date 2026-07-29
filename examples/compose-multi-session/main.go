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

// Example: assemble a multi-session attach daemon from pkg/compose —
// the #386 story. Instead of re-implementing cmd/core-agent's wiring,
// a library consumer builds the same substrate from the composition
// helpers:
//
//   - compose.BuildMultiSessionAuthn — bearer-table users.json ->
//     per-request auth.Authenticator
//   - permissions.New + Gate.SetGrantStore(&permissions.ConfigGrantStore)
//     — an "allow always" prompt answer persists into
//     .agents/config.json (demonstrated with a scripted prompter)
//   - compose.SessionFactoryDeps + BuildSessionFactory /
//     BuildSessionResumer — fresh per-caller agents on POST /sessions,
//     resumable from the persisted ACL store after eviction/restart
//   - attach.NewServer with MultiSessionEnabled — per-session ACLs:
//     bob cannot even see alice's session (404, not 403, by design)
//
// The demo then drives the daemon over plain HTTP as two bearer
// identities (alice + bob) and shows creation, isolation, and inject.
//
//	go run ./examples/compose-multi-session
//
// Hermetic: echo mock model, loopback listener, temp dirs, throwaway
// tokens. No credentials, no network beyond 127.0.0.1.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/compose"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// Throwaway loopback-only demo tokens. Real deployments generate
// them (openssl rand -hex 32) and never commit them.
const (
	demoAliceToken = "tok-alice-local-demo" //nolint:gosec // demo token for the hermetic example users table
	demoBobToken   = "tok-bob-local-demo"   //nolint:gosec // demo token for the hermetic example users table
)

// allowAlwaysPrompter is a scripted permissions.Prompter that answers
// every approval prompt with "allow always" — standing in for the
// human at a TUI modal so the GrantStore persistence path runs
// non-interactively.
type allowAlwaysPrompter struct{}

func (allowAlwaysPrompter) AskApproval(_ context.Context, req permissions.PromptRequest) (permissions.Decision, error) {
	fmt.Printf("  prompt: %s wants %q -> answering \"allow always\"\n", req.ToolName, req.Detail)
	return permissions.DecisionAllowAlways, nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir, err := os.MkdirTemp("", "compose-multi-session-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	agentsDir := filepath.Join(dir, ".agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return err
	}

	// --- authn: bearer users table -> Authenticator -------------------

	usersPath := filepath.Join(dir, "users.json")
	usersJSON := fmt.Sprintf(`{
  "version": 1,
  "users": [
    {"identity": "alice@example.com", "token": %q, "labels": {"team": "platform"}},
    {"identity": "bob@example.com",   "token": %q, "labels": {"team": "infra"}}
  ]
}`, demoAliceToken, demoBobToken)
	// 0600 is mandatory — the loader rejects laxer modes.
	if err := os.WriteFile(usersPath, []byte(usersJSON), 0o600); err != nil {
		return err
	}
	authn, fallback, err := compose.BuildMultiSessionAuthn(config.MultiSessionConfig{
		Enabled: true,
		Auth:    config.MultiSessionAuthConfig{Kind: "bearer_table", TableFile: usersPath},
	})
	if err != nil {
		return fmt.Errorf("BuildMultiSessionAuthn: %w", err)
	}
	fmt.Println("authn: bearer table loaded (alice, bob)")

	// --- permissions: template gate + persistent grant store ----------

	gate := permissions.New(permissions.Options{
		Mode:     permissions.ModeAsk,
		Prompter: allowAlwaysPrompter{},
	})
	gate.SetGrantStore(&permissions.ConfigGrantStore{AgentsDir: agentsDir})

	// Demonstrate the persistence contract: an ask-mode check prompts,
	// the scripted prompter answers "allow always", and the gate
	// writes the grant through the store into .agents/config.json —
	// so it survives a daemon restart.
	fmt.Println("\ngate: ask-mode bash check (unmatched -> prompt):")
	if err := gate.CheckBash(ctx, "git status"); err != nil {
		return fmt.Errorf("CheckBash: %w", err)
	}
	var persisted struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	raw, err := os.ReadFile(filepath.Join(agentsDir, "config.json"))
	if err != nil {
		return fmt.Errorf("read persisted config: %w", err)
	}
	if err := json.Unmarshal(raw, &persisted); err != nil {
		return err
	}
	fmt.Printf("  persisted to .agents/config.json: permissions.allow = %q\n", persisted.Permissions.Allow)

	// --- sessions: factory + resumer over a shared eventlog -----------

	handle, err := eventlog.Open(ctx, sqlite.Open(filepath.Join(dir, "session.db")))
	if err != nil {
		return fmt.Errorf("eventlog.Open: %w", err)
	}
	defer func() { _ = handle.Close() }()

	// The ACL store shares the eventlog DB; RegisterOwned writes
	// through it, the resumer reads from it on lookup miss.
	aclStore, err := attach.NewSessionACLStore(ctx, handle.DB)
	if err != nil {
		return fmt.Errorf("NewSessionACLStore: %w", err)
	}
	reg := attach.NewSessionRegistryWithStore(aclStore)

	llm, err := mock.NewEcho().Model(ctx, "echo")
	if err != nil {
		return err
	}
	deps := compose.SessionFactoryDeps{
		DaemonCtx:      ctx,
		Model:          llm,
		Template:       gate,
		EventlogHandle: handle,
		AgentsDir:      agentsDir,
		Registry:       reg,
		ACLStore:       aclStore,
	}

	srv, err := attach.NewServer(attach.Options{
		Registry:            reg,
		SessionFactory:      compose.BuildSessionFactory(deps),
		Resumer:             compose.BuildSessionResumer(deps),
		Authenticator:       authn,
		DefaultCaller:       fallback,
		MultiSessionEnabled: true,
		Addr:                "127.0.0.1:0",
		ShutdownTimeout:     2 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("NewServer: %w", err)
	}
	if err := srv.Bind(); err != nil {
		return fmt.Errorf("bind listener: %w", err)
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve() }()
	defer func() { _ = srv.Close() }()

	base := "http://" + srv.Addr()
	fmt.Printf("\ndaemon: listening on %s (multi-session, bearer auth)\n\n", base)

	// --- drive it over HTTP as alice and bob --------------------------

	// 1. Each identity creates its own session via POST /sessions.
	aliceSess, err := createSession(base, demoAliceToken)
	if err != nil {
		return err
	}
	fmt.Printf("alice: POST /sessions -> %s\n", aliceSess.SessionID)
	bobSess, err := createSession(base, demoBobToken)
	if err != nil {
		return err
	}
	fmt.Printf("bob:   POST /sessions -> %s\n", bobSess.SessionID)

	// 2. Per-identity listing: each caller sees only their own
	// sessions — the ACL filter hides everyone else's entirely.
	for _, who := range []struct{ name, token string }{
		{"alice", demoAliceToken}, {"bob", demoBobToken},
	} {
		sids, err := listSessions(base, who.token)
		if err != nil {
			return err
		}
		fmt.Printf("%s: GET /sessions -> %d session(s): %v\n", who.name, len(sids), sids)
		if len(sids) != 1 {
			return fmt.Errorf("%s should see exactly her/his own session, saw %v", who.name, sids)
		}
	}

	// 3. Cross-access is a 404 — not 403 — so a stranger can't even
	// learn the session exists (see docs/multi-session-design.md).
	code, err := getStatus(aliceSess.URL, demoBobToken)
	if err != nil {
		return err
	}
	fmt.Printf("bob:   GET alice's /status -> %d (denied reads as not-found)\n", code)
	if code != http.StatusNotFound {
		return fmt.Errorf("cross-access must 404, got %d", code)
	}
	code, err = getStatus(aliceSess.URL, demoAliceToken)
	if err != nil {
		return err
	}
	fmt.Printf("alice: GET her own /status -> %d\n", code)
	if code != http.StatusOK {
		return fmt.Errorf("owner status must 200, got %d", code)
	}

	// 4. Inject into alice's session as alice. The per-session wake
	// loop (spawned by the factory) drains it into an echo turn.
	if err := inject(aliceSess.URL, demoAliceToken, "hello from alice"); err != nil {
		return err
	}
	fmt.Println("alice: POST .../inject -> 200 (wake loop runs the turn)")
	if err := waitForEvents(ctx, handle, aliceSess.App, "alice@example.com", aliceSess.SessionID, 2, 10*time.Second); err != nil {
		return err
	}
	fmt.Println("alice: turn persisted to the shared eventlog")

	// 5. Clean shutdown: cancel the daemon ctx (ends every
	// per-session wake loop), then close the listener.
	cancel()
	if err := srv.Close(); err != nil {
		return fmt.Errorf("close server: %w", err)
	}
	if err := <-serveErr; err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	fmt.Println("\ndaemon: shut down cleanly")
	return nil
}

// sessionRef is the interesting half of the POST /sessions response.
type sessionRef struct {
	App       string `json:"app"`
	SessionID string `json:"sessionID"`
	URL       string `json:"url"`
}

func createSession(base, token string) (sessionRef, error) {
	var out sessionRef
	req, err := http.NewRequest(http.MethodPost, base+"/sessions", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	// State-changing attach endpoints require the JSON content type
	// even without a body (cross-site request forgery protection).
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return out, fmt.Errorf("POST /sessions: status %d: %s", resp.StatusCode, b)
	}
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func listSessions(base, token string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/sessions", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /sessions: status %d", resp.StatusCode)
	}
	var list struct {
		Sessions []struct {
			SessionID string `json:"sessionID"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	sids := make([]string, 0, len(list.Sessions))
	for _, s := range list.Sessions {
		sids = append(sids, s.SessionID)
	}
	return sids, nil
}

// getStatus returns the HTTP status code of GET <sessURL>/status.
func getStatus(sessURL, token string) (int, error) {
	req, err := http.NewRequest(http.MethodGet, sessURL+"/status", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func inject(sessURL, token, message string) error {
	body, _ := json.Marshal(map[string]string{"message": message})
	req, err := http.NewRequest(http.MethodPost, sessURL+"/inject", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST .../inject: status %d: %s", resp.StatusCode, b)
	}
	return nil
}

// waitForEvents polls the eventlog until the session has at least n
// rows, or times out.
func waitForEvents(ctx context.Context, handle *eventlog.Handle, app, user, sid string, n int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		count := 0
		for _, err := range handle.Stream.Since(ctx, 0, eventlog.ForSession(app, user, sid)) {
			if err != nil {
				return err
			}
			count++
		}
		if count >= n {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %d eventlog rows for session %s", n, sid)
}
