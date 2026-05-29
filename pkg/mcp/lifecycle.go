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
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/mcptoolset"

	"github.com/go-steer/core-agent/pkg/permissions"
	coretools "github.com/go-steer/core-agent/pkg/tools"
)

// Status values surfaced via the per-server records.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// implementationName is sent in the MCP handshake. Override via
// SetImplementationName when embedding in a host that wants its own
// identity.
var implementationName = "core-agent"

// SetImplementationName overrides the name reported during the MCP
// client handshake. Useful for hosts that want to identify themselves
// to the server. Call before Build.
func SetImplementationName(name string) {
	if name != "" {
		implementationName = name
	}
}

// Server is one configured MCP server's runtime state.
type Server struct {
	Name      string
	Status    string
	Tools     []string   // tool names exposed; populated lazily by Toolset
	ToolInfos []ToolInfo // name + description pairs, parallel to Tools
	Err       error      // non-nil when Status == StatusError
	toolset   tool.Toolset
	cmd       *exec.Cmd // stdio child; nil for http transports
}

// ToolInfo is a name + description pair for one exposed MCP tool.
// Surfaced so the TUI's /mcp command can render the same rich
// "name + description" format /tools uses, without each consumer
// re-enumerating the toolset (which requires constructing a stub
// ReadonlyContext). Sorted by Name to match Tools' ordering.
type ToolInfo struct {
	Name        string
	Description string
}

// Toolset returns the MCP toolset, or nil for failed servers.
func (s *Server) Toolset() tool.Toolset { return s.toolset }

// Close terminates any child process this server owns. For HTTP
// transports there's no process to kill — Close is a no-op.
//
// Termination strategy: SIGTERM, give the process up to 3 seconds to
// exit gracefully, then SIGKILL.
func (s *Server) Close() {
	if s == nil || s.cmd == nil || s.cmd.Process == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = s.cmd.Process.Wait()
		close(done)
	}()
	_ = s.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
}

// Build reads .agents/mcp.json and starts every declared server in
// parallel. The send callback is plumbed into each server's
// elicitation handler (when no interactive elicitor is provided) so
// the host can surface elicitation requests in the right place.
//
// gate (optional) gates each MCP tool call through the permission
// system so MCP tools are subject to the same ask/allow/yolo rules
// as built-in tools. Pass nil to skip gating.
//
// elicitor (optional) is the interactive bridge for elicitation
// requests. Headless callers leave it nil and fall back to the
// decline-with-notice stub.
//
// Servers that fail to start come back with Status==StatusError so
// they're visible without breaking the rest of the agent.
func Build(ctx context.Context, agentsDir string, send func(string), gate *permissions.Gate, elicitor ElicitorFn) ([]*Server, []tool.Toolset, error) {
	cfg, err := Load(agentsDir)
	if err != nil {
		return nil, nil, err
	}
	if len(cfg.Servers) == 0 {
		return nil, nil, nil
	}

	out := make([]*Server, 0, len(cfg.Servers))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, spec := range cfg.Servers {
		wg.Add(1)
		go func(name string, spec ServerSpec) {
			defer wg.Done()
			srv := startOne(ctx, name, spec, send, gate, elicitor)
			mu.Lock()
			out = append(out, srv)
			mu.Unlock()
		}(name, spec)
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	toolsets := make([]tool.Toolset, 0, len(out))
	for _, s := range out {
		if s.toolset != nil {
			toolsets = append(toolsets, s.toolset)
		}
	}
	return out, toolsets, nil
}

// startOne instantiates one server. Errors are stored on the Server
// rather than returned so a single broken server doesn't prevent the
// rest of the registry from coming up.
func startOne(ctx context.Context, name string, spec ServerSpec, send func(string), gate *permissions.Gate, elicitor ElicitorFn) *Server {
	srv := &Server{Name: name}

	transport, cmd, err := transportFor(ctx, name, spec)
	if err != nil {
		srv.Status = StatusError
		srv.Err = err
		return srv
	}
	srv.cmd = cmd

	client := mcpsdk.NewClient(
		&mcpsdk.Implementation{Name: implementationName, Version: "0.1.0"},
		&mcpsdk.ClientOptions{ElicitationHandler: handlerFor(name, send, elicitor)},
	)
	ts, err := mcptoolset.New(mcptoolset.Config{
		Client:    client,
		Transport: transport,
	})
	if err != nil {
		srv.Status = StatusError
		srv.Err = fmt.Errorf("toolset: %w", err)
		return srv
	}
	// Wrap with our own namespace so an MCP server's `read_file` (for
	// example) doesn't collide with a built-in `read_file`.
	wrapped := withNamespace(ts, name)
	// Then wrap with the permission gate so MCP tool calls go through
	// the same ask/allow/yolo flow as built-in tools. Allowlist
	// patterns use the "mcp" namespace, e.g. "mcp:filesystem_read_file".
	if gate != nil {
		wrapped = coretools.GateToolset(wrapped, gate, "mcp")
	}
	srv.toolset = wrapped
	srv.Status = StatusOK
	if tools, err := wrapped.Tools(asReadonly(ctx)); err == nil {
		infos := make([]ToolInfo, 0, len(tools))
		for _, t := range tools {
			infos = append(infos, ToolInfo{
				Name:        t.Name(),
				Description: t.Description(),
			})
		}
		sort.Slice(infos, func(i, j int) bool {
			return infos[i].Name < infos[j].Name
		})
		// Materialize the names-only slice in lockstep so existing
		// consumers (Server.Tools) keep working.
		names := make([]string, len(infos))
		for i, info := range infos {
			names[i] = info.Name
		}
		srv.Tools = names
		srv.ToolInfos = infos
	}
	return srv
}

// transportFor builds the appropriate mcp.Transport for the spec.
// For stdio it also returns the *exec.Cmd so the Server can hold a
// reference for shutdown; for http the cmd is nil. ctx is used by
// auth strategies that resolve credentials at construction time
// (e.g. google.FindDefaultCredentials); name is used to scope error
// messages back to the misconfigured server.
func transportFor(ctx context.Context, name string, spec ServerSpec) (mcpsdk.Transport, *exec.Cmd, error) {
	switch spec.Transport {
	case "stdio":
		// Spec is sourced from the user's own .agents/mcp.json; spawning
		// the configured command is the contract.
		cmd := exec.Command(spec.Command, spec.Args...) // #nosec G204
		env := InterpolateMap(spec.Env)
		if len(env) > 0 {
			cmd.Env = append(cmd.Env, append([]string{}, parentEnv()...)...)
			for k, v := range env {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
		}
		return &mcpsdk.CommandTransport{Command: cmd}, cmd, nil
	case "http":
		rt := http.DefaultTransport

		// Auth wraps innermost (closest to the wire) so the static
		// header layer above can't accidentally overwrite Authorization
		// via misconfiguration. Net effect: auth-set Authorization
		// always wins over a header-set one.
		if spec.Auth != nil && spec.Auth.GoogleOAuth != nil {
			creds, err := google.FindDefaultCredentials(ctx, spec.Auth.GoogleOAuth.Scopes...)
			if err != nil {
				return nil, nil, fmt.Errorf(
					"mcp: %q: load Google default credentials: %w "+
						"(run `gcloud auth application-default login` or "+
						"ensure metadata server is reachable)", name, err)
			}
			// Fail-fast: pre-fetch a token so misconfig (no ADC,
			// missing scopes grant, etc.) surfaces at server-init
			// time instead of on the first tool call.
			if _, err := creds.TokenSource.Token(); err != nil {
				return nil, nil, fmt.Errorf(
					"mcp: %q: initial Google OAuth token fetch: %w", name, err)
			}
			rt = &googleAuthTransport{base: rt, source: creds.TokenSource}
		}

		headers := InterpolateMap(spec.Headers)
		if len(headers) > 0 {
			rt = &headerTransport{base: rt, headers: headers}
		}

		return &mcpsdk.StreamableClientTransport{
			Endpoint:   spec.URL,
			HTTPClient: &http.Client{Transport: rt},
		}, nil, nil
	default:
		return nil, nil, fmt.Errorf("unknown transport %q", spec.Transport)
	}
}

// headerTransport injects custom headers into every outgoing request.
// Used for MCP HTTP servers that authenticate via headers.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for k, v := range t.headers {
		if v != "" {
			clone.Header.Set(k, v)
		}
	}
	return t.base.RoundTrip(clone)
}

// googleAuthTransport injects "Authorization: Bearer <token>" from an
// oauth2.TokenSource on every request. Generic over the source so the
// type can later back both OAuth access tokens
// (google.FindDefaultCredentials) and OIDC ID tokens
// (idtoken.NewTokenSource) — both return oauth2.TokenSource.
type googleAuthTransport struct {
	base   http.RoundTripper
	source oauth2.TokenSource
}

func (t *googleAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok, err := t.source.Token()
	if err != nil {
		return nil, fmt.Errorf("mcp: fetch Google auth token: %w", err)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	return t.base.RoundTrip(clone)
}

func parentEnv() []string {
	return osEnviron()
}
