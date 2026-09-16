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
	"strings"
	"sync"
	"syscall"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
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
	// Warnings are things wrong with this server's config that are not
	// bad enough to take its tools away. Distinct from Err for exactly
	// that reason: Status stays StatusOK and the toolset is live, so a
	// consumer that only checks Err would miss these — which is why
	// every renderer of a server list prints them (#1016).
	Warnings []string
	toolset  tool.Toolset
	cmd      *exec.Cmd // stdio child; nil for http transports
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

// CloseAll terminates every server's child process concurrently and
// waits for all of them. Concurrency matters at daemon shutdown: each
// stdio Close is worst-case SIGTERM + 3s grace + SIGKILL, and serial
// closes over N servers would multiply that into the supervisor's
// termination grace period (#538). Nil servers are skipped; safe on
// an empty or nil slice.
func CloseAll(servers []*Server) {
	var wg sync.WaitGroup
	for _, s := range servers {
		if s == nil {
			continue
		}
		wg.Add(1)
		go func(s *Server) {
			defer wg.Done()
			s.Close()
		}(s)
	}
	wg.Wait()
}

// Build reads mcp.json from the project agents dir plus (optionally)
// the user's home-agents dir, merges the resulting server maps
// (project wins on name collision), and starts every declared server
// in parallel. The send callback is plumbed into each server's
// elicitation handler (when no interactive elicitor is provided) so
// the host can surface elicitation requests in the right place.
//
// homeAgentsDir is the portable user-scope root (typically
// $HOME/.agents/); pass "" to skip. See LoadAll for the merge rules.
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
func Build(ctx context.Context, agentsDir, homeAgentsDir string, send func(string), gate *permissions.Gate, elicitor ElicitorFn, digestOpts *DigestOptions) ([]*Server, []tool.Toolset, error) {
	cfg, err := LoadAll(agentsDir, homeAgentsDir)
	if err != nil {
		return nil, nil, err
	}
	if len(cfg.Servers) == 0 {
		return nil, nil, nil
	}

	// The AgenticWrap toggle in mcp.json can kill the wrap layer
	// without the operator having to null the digestOpts pointer at
	// every Build call site — it's a per-project config knob layered
	// on top of the process-wide --no-mcp-digest flag. Both must be
	// on for wrapping to fire.
	if digestOpts != nil && !cfg.AgenticWrapEnabled() {
		digestOpts = nil
	}
	// Merge the per-project threshold into DigestOptions when the
	// caller didn't pin one explicitly. Preserves precedence: CLI /
	// caller-supplied threshold wins; mcp.json fills the default.
	if digestOpts != nil && digestOpts.Threshold <= 0 {
		digestOpts.Threshold = cfg.AgenticWrapThresholdBytes()
	}

	out := make([]*Server, 0, len(cfg.Servers))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, spec := range cfg.Servers {
		wg.Add(1)
		go func(name string, spec ServerSpec) {
			defer wg.Done()
			srv := startOne(ctx, name, spec, send, gate, elicitor, digestOpts)
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

// wrapServerToolset composes the wrapper stack one server's tools go
// through on their way to the model. Split out of startOne so the
// composition is testable without a live transport — every field of
// ServerSpec that changes model-visible behavior is decided here, so
// "the spec reached the wrap" is a claim worth a test rather than a
// code read (this is the seam #706 wished main.go had).
//
// Inner to outer:
//
//   - the ServerSpec.Tools allowlist, when the spec sets one. First,
//     because a tool dropped here is never named, never declared and
//     never gated — the point is that the model is not told about it
//     at all (#759), and every layer above this one exists to shape
//     something the model can see;
//   - namespace, so an MCP server's `read_file` doesn't collide with
//     the built-in one, and — when the server declared itself
//     ReadOnly — the dispatch class every tool from it carries;
//   - digest, unless digestOpts is nil, the server is in the
//     options-level NeverServers denylist, or the spec sets
//     AgenticNever. Two knobs on purpose: project-wide via
//     DigestOptions, per-server via mcp.json;
//   - the permission gate, so MCP calls take the same ask/allow/yolo
//     path as built-ins. Patterns use the "mcp" namespace, e.g.
//     "mcp:filesystem_read_file".
//
// ServerSpec.ToolNotes rides the namespace layer rather than a wrapper
// of its own, because that is already the layer that clones the
// declaration to rewrite the name — one clone, one place the
// model-visible declaration is assembled.
//
// The second return value is the allowlist handle, nil unless the spec
// set one. Only unmatchedToolAllowlist wants it: the warning has to
// compare the allowlist against what the server exposed before the
// filter ran, and that list does not survive the composition.
func wrapServerToolset(ts tool.Toolset, name string, spec ServerSpec, digestOpts *DigestOptions, gate *permissions.Gate) (tool.Toolset, *allowlistToolset) {
	optsForServer := digestOpts
	if optsForServer != nil && spec.AgenticNever {
		optsForServer = nil
	}
	filtered, allow := withToolAllowlist(ts, spec.Tools)
	wrapped := withNamespaceAndDigest(filtered, name, name, optsForServer, spec.ReadOnly, spec.ToolNotes)
	if gate != nil {
		wrapped = coretools.GateToolset(wrapped, gate, "mcp")
	}
	return wrapped, allow
}

// startOne instantiates one server. Errors are stored on the Server
// rather than returned so a single broken server doesn't prevent the
// rest of the registry from coming up.
func startOne(ctx context.Context, name string, spec ServerSpec, send func(string), gate *permissions.Gate, elicitor ElicitorFn, digestOpts *DigestOptions) *Server {
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
	ts, err := mcpToolsetWithHints(client, transport)
	if err != nil {
		srv.Status = StatusError
		srv.Err = fmt.Errorf("toolset: %w", err)
		return srv
	}
	wrapped, allow := wrapServerToolset(ts, name, spec, digestOpts, gate)
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
		// Allowlist warnings come first: a name that admitted nothing
		// means a tool the operator believes is mounted is absent, and
		// an unmatched tool_notes key is very often the same typo
		// showing up twice.
		srv.Warnings = append(unmatchedToolAllowlist(allow),
			unmatchedToolNotes(spec.ToolNotes, sanitizePrefix(name), names)...)
	}
	return srv
}

// Warnings collects every server's non-fatal config warnings, each
// prefixed with the server it belongs to, ready for one line apiece on
// stderr.
//
// Exported because the hosts are the only place a warning can actually
// reach a person, and there are two of them — the parent's mcp.Build
// in main.go and a rooted subagent's in subagents.go. The subagent one
// is not the afterthought: a content root is where a per-tool note is
// most likely to live, since that is the scope that owns the tools it
// is describing.
func Warnings(servers []*Server) []string {
	var out []string
	for _, s := range servers {
		if s == nil {
			continue
		}
		for _, w := range s.Warnings {
			out = append(out, s.Name+": "+w)
		}
	}
	return out
}

// unmatchedToolNotes reports ToolNotes keys that named no tool this
// server actually exposed.
//
// The whole point of #1016 is that a fact the model needs went
// missing without anything saying so, and a note keyed on a typo or on
// a tool the server has since renamed fails exactly that way: the
// config reads correct, the agent behaves as though the feature were
// never configured, and the only symptom is behaviour nobody thinks to
// connect back to a spelling. So the mismatch is named, with the
// names that WERE available, because "no such tool" without the list
// is half an error message.
//
// A warning and not an error: the server is working, its other tools
// are fine, and refusing a cluster's whole read surface over a
// misspelled note would be a worse outcome than the one being
// prevented. Keys are matched against the prefixed names because that
// is what the toolset reports; the caller writes the unprefixed form
// and this rejoins them.
func unmatchedToolNotes(notes map[string]string, prefix string, exposed []string) []string {
	if len(notes) == 0 {
		return nil
	}
	have := make(map[string]bool, len(exposed))
	for _, n := range exposed {
		have[n] = true
	}
	var missing []string
	for toolName := range notes {
		if !have[prefix+"_"+toolName] {
			missing = append(missing, toolName)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return []string{fmt.Sprintf(
		"tool_notes names %d tool(s) this server does not expose: %s. Exposed: %s",
		len(missing), strings.Join(missing, ", "), strings.Join(exposed, ", "))}
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

		// JSON-RPC error extraction wraps below OTel so the span records
		// the raw HTTP outcome, but above auth/headers so it sees the
		// server's real response. See #180: without this, the SDK
		// surfaces only http.StatusText and drops the JSON-RPC body
		// (which is where MCP servers put actionable messages like the
		// missing IAM permission name).
		rt = &jsonRPCErrorTransport{base: rt}

		// OTel wrap outermost so the span covers the full outbound
		// MCP call (including auth-token attachment + custom headers)
		// AND traceparent gets injected on every request. This closes
		// the daemon → MCP-server link in the trace chain started by
		// the incoming inject request (see pkg/attach/server.go). No-
		// op when the global tracer provider is noop (telemetry off).
		// See #217.
		rt = otelhttp.NewTransport(rt)

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
