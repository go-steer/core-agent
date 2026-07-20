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

package attachclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// Client is a thin HTTP client for one attach-mode endpoint. Holds
// the parsed URL, bearer token (empty for no auth), and a configured
// http.Client (Unix-socket-aware when the URL scheme is unix://).
// Safe for concurrent use.
//
// Two HTTP clients live inside: `http` for short-lived RPC calls
// with a request timeout, and `streamHTTP` for SSE — no timeout,
// because the stream body stays open for as long as the agent runs
// and minutes can pass between frames. A single client with a Timeout
// would cut the SSE body mid-response on long model turns; the
// symptom is "stream ended: <nil>" reconnect-loops in the UI.
type Client struct {
	URL *ParsedURL

	// Token is kept for source-compat with the original constructor
	// (New stores its token here AND wraps it in a BearerCreds in
	// Credentials below). New code should prefer Credentials directly.
	// When both are set, Credentials wins — that's how callers opt
	// in to the gateway-fronted path while keeping the legacy field
	// for backward compatibility.
	Token       string
	Credentials Credentials

	http       *http.Client
	streamHTTP *http.Client
}

// New builds a Client wrapped in BearerCreds. ParseURL the rawURL
// first; Token may be empty (auth disabled — fine for Unix socket).
// timeout governs short-lived RPC calls. SSE streams ignore it
// (caller's ctx is the cancel signal). Zero timeout falls back to 30 s
// for RPCs.
//
// Use NewWithCredentials to construct a Client with a non-Bearer auth
// strategy (Cloud Run IAM, IAP, …).
func New(parsed *ParsedURL, token string, timeout time.Duration) *Client {
	c := NewWithCredentials(parsed, BearerCreds{Token: token}, timeout)
	c.Token = token
	return c
}

// NewWithCredentials builds a Client with an explicit Credentials
// implementation. Used by callers that need a non-Bearer auth path
// (e.g. cmd/core-agent-tui's --auth=google-id-token mode, which
// supplies a GoogleIDTokenCreds backed by ADC).
func NewWithCredentials(parsed *ParsedURL, creds Credentials, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		URL:         parsed,
		Credentials: creds,
		http:        newHTTPClient(parsed, timeout),
		streamHTTP:  newHTTPClient(parsed, 0), // no timeout — SSE is long-lived
	}
}

// newHTTPClient wires up a Unix-socket-aware Transport when the URL
// scheme is unix://. For http/https it returns a stock client.
func newHTTPClient(p *ParsedURL, timeout time.Duration) *http.Client {
	if p.Scheme == "unix" {
		return &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", p.SocketPath)
				},
			},
		}
	}
	return &http.Client{Timeout: timeout}
}

// auth stamps the wired Credentials' headers on req. Errors from
// the underlying credential source (e.g. ADC misconfig, metadata
// server unreachable) propagate so the caller can surface them
// instead of sending an unauthenticated request that would 401.
//
// Falls back to the legacy Token-based bearer path when no
// Credentials value was supplied — preserves the zero-value /
// direct-field-assignment construction patterns that pre-date the
// Credentials interface.
func (c *Client) auth(req *http.Request) error {
	if c.Credentials != nil {
		return c.Credentials.Apply(req)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return nil
}

// ---- /sessions list ----

// SessionDescriptor mirrors the attach server's GET /sessions row.
type SessionDescriptor struct {
	App         string `json:"app"`
	User        string `json:"user"`
	SessionID   string `json:"sessionID"`
	HasEventLog bool   `json:"has_event_log"`
}

// ListSessions calls GET <base>/sessions.
func (c *Client) ListSessions(ctx context.Context) ([]SessionDescriptor, error) {
	var out struct {
		Sessions []SessionDescriptor `json:"sessions"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/sessions", nil, &out); err != nil {
		return nil, err
	}
	return out.Sessions, nil
}

// NewSessionResponse mirrors the attach server's POST /sessions
// 201 body — the new session's triple plus the absolute URL the
// client should attach to (events / inject / status / etc. live
// underneath).
type NewSessionResponse struct {
	AppName   string `json:"app"`
	UserID    string `json:"user"`
	SessionID string `json:"sessionID"`
	URL       string `json:"url"`
}

// NewSession calls POST <base>/sessions to create a fresh session
// owned by the authenticated caller. Returns the new session's
// descriptor on success.
//
// Server-side behavior:
//   - 201: new session created, response carries the triple + URL
//   - 401: caller couldn't be resolved (anonymous request)
//   - 501: daemon doesn't have a SessionFactory configured
//   - 500: factory error
//   - 409: triple collision (factory's SessionID generator clashed)
//
// All non-2xx responses surface as errors.
func (c *Client) NewSession(ctx context.Context) (NewSessionResponse, error) {
	var out NewSessionResponse
	if err := c.doJSON(ctx, http.MethodPost, "/sessions", nil, &out); err != nil {
		return NewSessionResponse{}, err
	}
	return out, nil
}

// ---- /peers ----

// PeerDescriptor mirrors the attach server's GET /peers row.
type PeerDescriptor struct {
	RegistrationID string            `json:"registration_id"`
	Name           string            `json:"name"`
	Endpoint       string            `json:"endpoint"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// ListPeers calls GET <base>/peers. Returns nil (not an error) when
// the listener doesn't have peer-registration enabled (HTTP 404).
func (c *Client) ListPeers(ctx context.Context) ([]PeerDescriptor, error) {
	resp, err := c.do(ctx, http.MethodGet, "/peers", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list peers: status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Peers []PeerDescriptor `json:"peers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode peers: %w", err)
	}
	return out.Peers, nil
}

// ---- Session-scoped reads (/tools, /agents, /status) ----

// Tools calls GET <base>/sessions/<sid>/tools. Returns the parsed
// list; empty (not nil) if the session doesn't implement the provider.
func (c *Client) Tools(ctx context.Context, sessionPath string) ([]attach.ToolInfo, error) {
	var out struct {
		Tools []attach.ToolInfo `json:"tools"`
	}
	if err := c.doJSON(ctx, http.MethodGet, sessionPath+"/tools", nil, &out); err != nil {
		return nil, err
	}
	return out.Tools, nil
}

// Agents calls GET <base>/sessions/<sid>/agents.
func (c *Client) Agents(ctx context.Context, sessionPath string) ([]attach.AgentInfo, error) {
	var out struct {
		Agents []attach.AgentInfo `json:"agents"`
	}
	if err := c.doJSON(ctx, http.MethodGet, sessionPath+"/agents", nil, &out); err != nil {
		return nil, err
	}
	return out.Agents, nil
}

// Status calls GET <base>/sessions/<sid>/status.
func (c *Client) Status(ctx context.Context, sessionPath string) (attach.StatusInfo, error) {
	var out attach.StatusInfo
	if err := c.doJSON(ctx, http.MethodGet, sessionPath+"/status", nil, &out); err != nil {
		return attach.StatusInfo{}, err
	}
	return out, nil
}

// Usage calls GET <base>/sessions/<sid>/usage. Backs the remote
// TUI's /stats slash. Returns zero UsageInfo if the agent doesn't
// implement the capability (server returns 501).
func (c *Client) Usage(ctx context.Context, sessionPath string) (attach.UsageInfo, error) {
	var out attach.UsageInfo
	if err := c.doJSON(ctx, http.MethodGet, sessionPath+"/usage", nil, &out); err != nil {
		return attach.UsageInfo{}, err
	}
	return out, nil
}

// Context calls GET <base>/sessions/<sid>/context. Backs the remote
// TUI's /context slash. Returns zero ContextInfo on 501.
func (c *Client) Context(ctx context.Context, sessionPath string) (attach.ContextInfo, error) {
	var out attach.ContextInfo
	if err := c.doJSON(ctx, http.MethodGet, sessionPath+"/context", nil, &out); err != nil {
		return attach.ContextInfo{}, err
	}
	return out, nil
}

// Memory calls GET <base>/sessions/<sid>/memory. Backs the remote
// TUI's /memory slash. Returns empty slice (not nil) on 501.
func (c *Client) Memory(ctx context.Context, sessionPath string) ([]attach.MemorySource, error) {
	var out struct {
		Sources []attach.MemorySource `json:"sources"`
	}
	if err := c.doJSON(ctx, http.MethodGet, sessionPath+"/memory", nil, &out); err != nil {
		return nil, err
	}
	return out.Sources, nil
}

// Skills calls GET <base>/sessions/<sid>/skills. Backs the remote
// TUI's /skills slash.
func (c *Client) Skills(ctx context.Context, sessionPath string) ([]attach.SkillInfo, error) {
	var out struct {
		Skills []attach.SkillInfo `json:"skills"`
	}
	if err := c.doJSON(ctx, http.MethodGet, sessionPath+"/skills", nil, &out); err != nil {
		return nil, err
	}
	return out.Skills, nil
}

// MCP calls GET <base>/sessions/<sid>/mcp. Backs the remote TUI's
// /mcp slash. Returns zero MCPInfo on 501.
func (c *Client) MCP(ctx context.Context, sessionPath string) (attach.MCPInfo, error) {
	var out attach.MCPInfo
	if err := c.doJSON(ctx, http.MethodGet, sessionPath+"/mcp", nil, &out); err != nil {
		return attach.MCPInfo{}, err
	}
	return out, nil
}

// Pricing calls GET <base>/sessions/<sid>/pricing. Backs the remote
// TUI's /pricing slash. Returns zero PricingInfo on 501.
func (c *Client) Pricing(ctx context.Context, sessionPath string) (attach.PricingInfo, error) {
	var out attach.PricingInfo
	if err := c.doJSON(ctx, http.MethodGet, sessionPath+"/pricing", nil, &out); err != nil {
		return attach.PricingInfo{}, err
	}
	return out, nil
}

// Perms calls GET <base>/sessions/<sid>/perms. Backs the remote
// TUI's /permissions slash. Returns zero PermsInfo on 501.
func (c *Client) Perms(ctx context.Context, sessionPath string) (attach.PermsInfo, error) {
	var out attach.PermsInfo
	if err := c.doJSON(ctx, http.MethodGet, sessionPath+"/perms", nil, &out); err != nil {
		return attach.PermsInfo{}, err
	}
	return out, nil
}

// AllowPatterns calls POST <base>/sessions/<sid>/perms/allow with the
// given patterns. Backs the remote TUI's /allow slash. Returns nil
// on success (204), an error otherwise — including 501 when the
// agent doesn't implement PermsController and 400 when the gate
// rejects a pattern.
func (c *Client) AllowPatterns(ctx context.Context, sessionPath string, patterns []string) error {
	return c.doJSON(ctx, http.MethodPost, sessionPath+"/perms/allow",
		attach.PatternsRequest{Patterns: patterns}, nil)
}

// DenyPatterns calls POST <base>/sessions/<sid>/perms/deny. Backs
// the remote TUI's /deny slash.
func (c *Client) DenyPatterns(ctx context.Context, sessionPath string, patterns []string) error {
	return c.doJSON(ctx, http.MethodPost, sessionPath+"/perms/deny",
		attach.PatternsRequest{Patterns: patterns}, nil)
}

// RefreshPricing calls POST <base>/sessions/<sid>/pricing/refresh.
// Backs the remote TUI's /pricing refresh subcommand. Returns the
// outcome (whether the LiteLLM fetch actually pulled new data and
// the post-refresh model count) so the client can update its display.
func (c *Client) RefreshPricing(ctx context.Context, sessionPath string) (attach.PricingRefreshResponse, error) {
	var out attach.PricingRefreshResponse
	if err := c.doJSON(ctx, http.MethodPost, sessionPath+"/pricing/refresh", struct{}{}, &out); err != nil {
		return attach.PricingRefreshResponse{}, err
	}
	return out, nil
}

// SetManualPricing calls POST <base>/sessions/<sid>/pricing/set.
// Backs the remote TUI's /pricing set subcommand.
func (c *Client) SetManualPricing(ctx context.Context, sessionPath string, req attach.PricingSetRequest) error {
	return c.doJSON(ctx, http.MethodPost, sessionPath+"/pricing/set", req, nil)
}

// Reload calls POST <base>/sessions/<sid>/reload. Backs the remote
// TUI's /reload slash. Returns the per-surface success flags +
// any errors so the operator sees which parts (memory / skills /
// mcp) succeeded and which failed.
func (c *Client) Reload(ctx context.Context, sessionPath string) (attach.ReloadResponse, error) {
	var out attach.ReloadResponse
	if err := c.doJSON(ctx, http.MethodPost, sessionPath+"/reload", struct{}{}, &out); err != nil {
		return attach.ReloadResponse{}, err
	}
	return out, nil
}

// Replan calls POST <base>/sessions/<sid>/slash/replan. Backs the
// remote TUI's /replan slash. Reason is the optional free-text
// the operator typed after /replan; today it's surfaced in the
// archive's audit trail but doesn't drive any model-side behavior.
func (c *Client) Replan(ctx context.Context, sessionPath, reason string) (attach.ReplanResponse, error) {
	var out attach.ReplanResponse
	if err := c.doJSON(ctx, http.MethodPost, sessionPath+"/slash/replan",
		attach.ReplanRequest{Reason: reason}, &out); err != nil {
		return attach.ReplanResponse{}, err
	}
	return out, nil
}

// SlashCompact calls POST <base>/sessions/<sid>/slash/compact.
// Synchronous: blocks until the compaction summarizer completes
// (5–30s typical for real model calls). The remote TUI should
// render the in-chat preamble row at dispatch — this call does NOT
// emit a preamble itself.
func (c *Client) SlashCompact(ctx context.Context, sessionPath, focus string) (attach.CompactResponse, error) {
	var out attach.CompactResponse
	if err := c.doJSON(ctx, http.MethodPost, sessionPath+"/slash/compact",
		attach.CompactRequest{Focus: focus}, &out); err != nil {
		return attach.CompactResponse{}, err
	}
	return out, nil
}

// SlashDone calls POST <base>/sessions/<sid>/slash/done. Synchronous.
// Backs the remote TUI's /done slash.
func (c *Client) SlashDone(ctx context.Context, sessionPath, note string) (attach.CheckpointResponse, error) {
	var out attach.CheckpointResponse
	if err := c.doJSON(ctx, http.MethodPost, sessionPath+"/slash/done",
		attach.CheckpointRequest{Note: note}, &out); err != nil {
		return attach.CheckpointResponse{}, err
	}
	return out, nil
}

// SlashBtw calls POST <base>/sessions/<sid>/slash/btw. Synchronous.
// Backs the remote TUI's /btw slash. The answer renders as a
// dismissible overlay (no event-log persistence).
func (c *Client) SlashBtw(ctx context.Context, sessionPath, question string) (string, error) {
	var out attach.SideQueryResponse
	if err := c.doJSON(ctx, http.MethodPost, sessionPath+"/slash/btw",
		attach.SideQueryRequest{Question: question}, &out); err != nil {
		return "", err
	}
	return out.Answer, nil
}

// SlashSubagent calls POST <base>/sessions/<sid>/slash/subagent.
// Backs the remote TUI's /subagent slash. Returns the spawn
// confirmation (name + started_at); the subagent's events flow
// through the existing SSE stream under a branch label so the
// operator sees its turns alongside the parent's.
func (c *Client) SlashSubagent(ctx context.Context, sessionPath string, spec attach.SubagentSpec) (attach.SubagentSpawnResponse, error) {
	var out attach.SubagentSpawnResponse
	if err := c.doJSON(ctx, http.MethodPost, sessionPath+"/slash/subagent", spec, &out); err != nil {
		return attach.SubagentSpawnResponse{}, err
	}
	return out, nil
}

// ---- POSTs (/inject, /wake) ----

// Inject calls POST <base>/sessions/<sid>/inject with the given message.
// sessionPath is the /sessions/<sid> prefix (relative to BaseURL).
func (c *Client) Inject(ctx context.Context, sessionPath, message string) error {
	return c.doJSON(ctx, http.MethodPost, sessionPath+"/inject",
		map[string]string{"message": message}, nil)
}

// Wake calls POST <base>/sessions/<sid>/wake.
func (c *Client) Wake(ctx context.Context, sessionPath string) error {
	return c.doJSON(ctx, http.MethodPost, sessionPath+"/wake",
		map[string]string{}, nil)
}

// InterruptResponse is the parsed body of POST /sessions/<sid>/interrupt.
// Interrupted reports whether there was an in-flight turn to cancel
// (server-side); false means the agent was idle and the call was a
// no-op. The TUI distinguishes these for its "nothing to interrupt"
// toast vs. "turn cancelled" rendering.
type InterruptResponse struct {
	Interrupted bool   `json:"interrupted"`
	Session     string `json:"session"`
}

// Interrupt calls POST <base>/sessions/<sid>/interrupt to cancel the
// in-flight turn on that session. The returned InterruptResponse
// reports whether something was actually cancelled.
func (c *Client) Interrupt(ctx context.Context, sessionPath string) (InterruptResponse, error) {
	var out InterruptResponse
	if err := c.doJSON(ctx, http.MethodPost, sessionPath+"/interrupt",
		map[string]string{}, &out); err != nil {
		return InterruptResponse{}, err
	}
	return out, nil
}

// ---- SSE stream ----

// Stream connects to <base><sessionPath>/events?since=<since> and
// returns a channel of decoded frames. Closes the channel on ctx
// cancel, stream error, or upstream EOF. Errors that prevented the
// initial GET (network failure, non-200 status) are returned
// synchronously; downstream errors land in the returned channel's
// error field via the second return value being closed.
//
// The lossless-replay property of the protocol means that passing a
// non-zero since value asks the server to replay any frames since
// that sequence before resuming live tail.
func (c *Client) Stream(ctx context.Context, sessionPath string, since int64) (<-chan attach.Frame, error) {
	url := c.URL.BaseURL + sessionPath + "/events"
	if since > 0 {
		url = fmt.Sprintf("%s?since=%d", url, since)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if err := c.auth(req); err != nil {
		return nil, fmt.Errorf("stream: auth: %w", err)
	}
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, &httpStatusError{op: "stream", statusCode: resp.StatusCode, body: string(body)}
	}

	out := make(chan attach.Frame, 32)
	go func() {
		defer close(out)
		defer func() { _ = resp.Body.Close() }()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		// SSE wire format groups an event-type line (optional, defaults
		// to "message") with the data line(s) until a blank-line
		// separator. We track the in-progress event name and reset on
		// the boundary so each data line lands with its matching type.
		var eventType string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				eventType = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				raw := strings.TrimPrefix(line, "data: ")
				frame, ok := parseStreamFrame(eventType, raw)
				if !ok {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case out <- frame:
				}
			case line == "":
				eventType = ""
			}
		}
	}()
	return out, nil
}

// parseStreamFrame dispatches a single data block by SSE event type.
// Legacy frames ("agent" or empty event) unmarshal into the full
// attach.Frame shape (carries seq + ADK session.Event). Typed events
// (status-update / usage-update / inbox / turn-complete / turn-error /
// capabilities) unmarshal into the matching payload struct, which is
// stashed on attach.Frame.TypedData with Type set so consumers can
// dispatch downstream. Returns false for parse errors or unknown
// event types — the consumer (coretuiremote) tolerates either as
// no-op so unknown SSE event names don't crash the stream.
func parseStreamFrame(eventType, raw string) (attach.Frame, bool) {
	switch eventType {
	case "", attach.EventAgent:
		var frame attach.Frame
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			return attach.Frame{}, false
		}
		return frame, true
	case attach.EventCapabilities:
		var p attach.Capabilities
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return attach.Frame{}, false
		}
		return attach.Frame{Type: eventType, TypedData: &p}, true
	case attach.EventStatusUpdate:
		var p attach.StatusUpdate
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return attach.Frame{}, false
		}
		return attach.Frame{Type: eventType, TypedData: &p}, true
	case attach.EventUsageUpdate:
		var p attach.UsageUpdate
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return attach.Frame{}, false
		}
		return attach.Frame{Type: eventType, TypedData: &p}, true
	case attach.EventInbox:
		var p attach.InboxEvent
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return attach.Frame{}, false
		}
		return attach.Frame{Type: eventType, TypedData: &p}, true
	case attach.EventTurnComplete:
		var p attach.TurnComplete
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return attach.Frame{}, false
		}
		return attach.Frame{Type: eventType, TypedData: &p}, true
	case attach.EventTurnError:
		var p attach.TurnError
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			return attach.Frame{}, false
		}
		return attach.Frame{Type: eventType, TypedData: &p}, true
	default:
		// Unknown event type — tolerated per spec §3 (forward-compat
		// with future event names). Drop on the floor.
		return attach.Frame{}, false
	}
}

// PromptStream subscribes to <base><sessionPath>/perms/stream and
// returns a channel of PromptFrames. Closes the channel on ctx
// cancel, stream error, or upstream EOF. 501 (capability not
// registered — agent wasn't constructed with WithAttachPromptBroker)
// is returned synchronously so callers can fall back gracefully.
func (c *Client) PromptStream(ctx context.Context, sessionPath string) (<-chan attach.PromptFrame, error) {
	url := c.URL.BaseURL + sessionPath + "/perms/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if err := c.auth(req); err != nil {
		return nil, fmt.Errorf("perm stream: auth: %w", err)
	}
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, &httpStatusError{op: "perms/stream", statusCode: resp.StatusCode, body: string(body)}
	}

	out := make(chan attach.PromptFrame, 16)
	go func() {
		defer close(out)
		defer func() { _ = resp.Body.Close() }()
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			raw := strings.TrimPrefix(line, "data: ")
			var frame attach.PromptFrame
			if err := json.Unmarshal([]byte(raw), &frame); err != nil {
				continue
			}
			select {
			case <-ctx.Done():
				return
			case out <- frame:
			}
		}
	}()
	return out, nil
}

// RespondToPrompt POSTs the operator's decision to
// <base><sessionPath>/perms/respond. decision must be one of the
// wire-format strings (e.g. "allow-once"); see attach.DecisionFromWire
// for the mapping.
func (c *Client) RespondToPrompt(ctx context.Context, sessionPath, id, decision string) error {
	return c.doJSON(ctx, http.MethodPost, sessionPath+"/perms/respond", attach.PromptResponse{ID: id, Decision: decision}, nil)
}

// ---- helpers ----

// doJSON sends a request, optionally decodes a JSON body into out (nil
// to discard). 4xx/5xx are returned as errors with the response body
// in the message.
func (c *Client) doJSON(ctx context.Context, method, suffix string, body, out any) error {
	resp, err := c.do(ctx, method, suffix, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return &httpStatusError{op: fmt.Sprintf("%s %s", method, suffix), statusCode: resp.StatusCode, body: string(b)}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", suffix, err)
	}
	return nil
}

// do builds + sends a request. Caller is responsible for resp.Body.Close().
func (c *Client) do(ctx context.Context, method, suffix string, body any) (*http.Response, error) {
	url := c.URL.BaseURL + suffix
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.auth(req); err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	return c.http.Do(req)
}
