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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-steer/core-agent/v2/internal/subagentlog"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// Client is a thin HTTP client for one attach-mode endpoint. Holds
// the parsed URL, bearer token (empty for no auth), and a configured
// http.Client (Unix-socket-aware when the URL scheme is unix://).
// Safe for concurrent use.
//
// Three HTTP clients live inside: `http` for short-lived RPC calls
// with a request timeout, `slowHTTP` for the cost-bearing slash
// endpoints that block on a model call, and `streamHTTP` for SSE — no
// timeout, because the stream body stays open for as long as the agent
// runs and minutes can pass between frames. A single client with a
// Timeout would cut the SSE body mid-response on long model turns; the
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
	slowHTTP   *http.Client
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
	slow := slowRPCTimeout
	if timeout > slow {
		// An operator who asked for a longer RPC deadline than our slow
		// floor meant it; never shorten what they configured.
		slow = timeout
	}
	return &Client{
		URL:         parsed,
		Credentials: creds,
		http:        newHTTPClient(parsed, timeout, 0),
		slowHTTP:    newHTTPClient(parsed, slow, 0),
		// SSE is long-lived so it gets no whole-request Timeout — that
		// would cut the body mid-stream on a quiet session. But it DOES
		// get a response-header deadline (time-to-first-byte): without
		// it, a daemon that accepts the TCP connection but never sends a
		// response (its handler goroutine wedged) blocks the Stream /
		// PromptStream connect — and thus the TUI's reconnect loop —
		// forever. See streamResponseHeaderTimeout.
		streamHTTP: newHTTPClient(parsed, 0, streamResponseHeaderTimeout),
	}
}

// streamResponseHeaderTimeout bounds the wait between finishing the SSE
// request write and receiving the response headers. It does NOT limit
// reading the response body, so live SSE frames minutes apart are fine;
// it only guards the connect handshake so a wedged daemon can't hang the
// reconnect loop indefinitely (the observed "246 bytes stuck unread in
// the daemon's receive queue" failure mode). The caller's ctx remains
// the cooperative-cancel signal for a healthy long-lived stream.
const streamResponseHeaderTimeout = 30 * time.Second

// slowRPCTimeout is the whole-request deadline for the /slash/*
// endpoints. They are synchronous by design — the POST blocks while
// the daemon runs a model call — so the ordinary 30s RPC deadline is
// not a safety net for them, it's a bug: a side question over a long
// history on a thinking model routinely takes longer, and the client
// tearing the request down surfaces as "context deadline exceeded".
// That is the infra-error-instead-of-an-answer symptom /btw was
// reported for, and /compact and /done are slower still.
//
// Five minutes is a backstop against a wedged daemon, not a working
// deadline: the operator can already abandon an in-flight slash from
// the TUI (ESC cancels the request context), and that path is the one
// meant to be used.
const slowRPCTimeout = 5 * time.Minute

// newHTTPClient builds an http.Client for one attach endpoint. timeout
// is the whole-request deadline (0 = none, used for SSE). respHeaderT is
// the response-header (time-to-first-byte) deadline (0 = none); it's set
// on the SSE client so a stalled daemon can't block the connect forever
// while still allowing an unbounded body read afterward.
//
// The Transport is cloned from http.DefaultTransport so connection
// pooling, proxy handling, and HTTP/2 behavior match the stock client;
// a unix:// URL swaps in a socket-dialing DialContext and disables the
// inherited proxy (a socket can't be proxied).
func newHTTPClient(p *ParsedURL, timeout, respHeaderT time.Duration) *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if ok {
		transport = transport.Clone()
	} else {
		transport = &http.Transport{}
	}
	if respHeaderT > 0 {
		transport.ResponseHeaderTimeout = respHeaderT
	}
	if p.Scheme == "unix" {
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", p.SocketPath)
		}
		// A unix socket can't be proxied. Cloning DefaultTransport pulls in
		// Proxy: ProxyFromEnvironment, which — under a global HTTP_PROXY /
		// ALL_PROXY that doesn't NO_PROXY the literal "unix" host — would
		// reroute (or, for a socks5:// proxy, actively break) the socket
		// dial. Pre-clone this branch used a bare Transport with nil Proxy;
		// keep that behavior.
		transport.Proxy = nil
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
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
	// Status is "active" (live in the listener's registry) or "idle"
	// (known only from the persisted ACL store — attaching triggers a
	// lazy resume, so the first frame costs more).
	Status string `json:"status"`
	// LastTouchedAt is the server's last-activity stamp. Omitted by
	// listeners that don't track it, hence the zero-value check at
	// every read site.
	LastTouchedAt time.Time `json:"last_touched_at"`
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

// SubagentEvents calls GET <base>/sessions/<sid>/agents/<name>/events —
// one subagent's inner turns, paged from the seq cursor `since` (0 for
// the whole history). limit <= 0 leaves the page size to the server.
//
// A name the server can't resolve comes back as a
// *SubagentNotFoundError carrying the names that would have resolved,
// not as an empty page: "no such subagent" and "this subagent recorded
// no turns" are different answers, and collapsing them is the failure
// #694 fixed server-side. Any other non-2xx stays an httpStatusError.
func (c *Client) SubagentEvents(ctx context.Context, sessionPath, name string, since int64, limit int) (attach.SubagentEventsResponse, error) {
	// Validate before the name becomes a path segment. The server
	// rejects the same set with a 400, but a name carrying "/" or a
	// dot segment would reshape the URL on the way there — "..", for
	// one, walks the request off this session's subtree and into a
	// mux redirect. Cheaper and clearer to refuse it here.
	if err := subagentlog.ValidateName(name); err != nil {
		return attach.SubagentEventsResponse{}, err
	}
	q := url.Values{}
	if since > 0 {
		q.Set("since", strconv.FormatInt(since, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	suffix := sessionPath + "/agents/" + url.PathEscape(name) + "/events"
	if len(q) > 0 {
		suffix += "?" + q.Encode()
	}
	var out attach.SubagentEventsResponse
	if err := c.doJSON(ctx, http.MethodGet, suffix, nil, &out); err != nil {
		if nf, ok := subagentNotFound(name, err); ok {
			return attach.SubagentEventsResponse{}, nf
		}
		return attach.SubagentEventsResponse{}, err
	}
	return out, nil
}

// subagentNotFound recognises the 404 shape doSubagentEvents writes for
// an unresolvable name and projects it into a typed error.
//
// A 404 whose body ISN'T that shape is left alone: it means the session
// itself is gone (or something else answered), which is a transport
// condition the caller should keep treating as one — including the
// PermanentStreamErr classification httpStatusError carries.
func subagentNotFound(name string, err error) (*SubagentNotFoundError, bool) {
	var se *httpStatusError
	if !errors.As(err, &se) || se.statusCode != http.StatusNotFound {
		return nil, false
	}
	var body attach.SubagentNotFoundResponse
	if json.Unmarshal([]byte(se.body), &body) != nil || body.Agent == "" {
		return nil, false
	}
	return &SubagentNotFoundError{
		Name:      name,
		Available: body.Available,
		Message:   body.Error,
	}, true
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
//
// Returns the whole response rather than just the text: an answered
// call and an empty one are both 200s (protocol 1.5.0), and the caller
// needs Empty + Detail to tell the operator which one happened.
func (c *Client) SlashBtw(ctx context.Context, sessionPath, question string) (attach.SideQueryResponse, error) {
	var out attach.SideQueryResponse
	if err := c.doJSON(ctx, http.MethodPost, sessionPath+"/slash/btw",
		attach.SideQueryRequest{Question: question}, &out); err != nil {
		return attach.SideQueryResponse{}, err
	}
	return out, nil
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
// toast vs. "turn cancelled" rendering. Paused reports whether the
// loop is now parked (protocol v1.5.0).
//
// Alias rather than a second declaration: this used to be a hand-copy
// of the server shape, which is how it silently missed every field
// v1.5.0 added.
type InterruptResponse = attach.InterruptResponse

// Interrupt calls POST <base>/sessions/<sid>/interrupt to cancel the
// in-flight turn on that session and park the loop. The returned
// InterruptResponse reports whether something was actually cancelled
// and whether the agent is now paused.
//
// hold=false asks for the pre-v1.5.0 cancel-and-carry-on behavior
// (no park). stopSubagents additionally stops every running background
// subagent — off by default, since subagent runs aren't resumable.
func (c *Client) Interrupt(ctx context.Context, sessionPath string, hold, stopSubagents bool) (InterruptResponse, error) {
	var out InterruptResponse
	body := attach.InterruptRequest{Hold: &hold, StopSubagents: stopSubagents}
	if err := c.doJSON(ctx, http.MethodPost, sessionPath+"/interrupt", body, &out); err != nil {
		return InterruptResponse{}, err
	}
	return out, nil
}

// Pause calls POST <base>/sessions/<sid>/pause — park the loop without
// touching an in-flight turn ("stop after this one").
func (c *Client) Pause(ctx context.Context, sessionPath, reason string) (attach.PauseResponse, error) {
	var out attach.PauseResponse
	if err := c.doJSON(ctx, http.MethodPost, sessionPath+"/pause",
		attach.PauseRequest{Reason: reason}, &out); err != nil {
		return attach.PauseResponse{}, err
	}
	return out, nil
}

// Resume calls POST <base>/sessions/<sid>/resume with the operator's
// disposition. An empty req is a plain "carry on".
func (c *Client) Resume(ctx context.Context, sessionPath string, req attach.ResumeRequest) (attach.ResumeResponse, error) {
	var out attach.ResumeResponse
	if err := c.doJSON(ctx, http.MethodPost, sessionPath+"/resume", req, &out); err != nil {
		return attach.ResumeResponse{}, err
	}
	return out, nil
}

// StopAgent calls POST <base>/sessions/<sid>/agents/<name>/stop —
// stop one runaway background subagent, which interrupting the parent
// can't reach.
func (c *Client) StopAgent(ctx context.Context, sessionPath, name string) error {
	return c.doJSON(ctx, http.MethodPost,
		sessionPath+"/agents/"+url.PathEscape(name)+"/stop", map[string]string{}, nil)
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
	case attach.EventPause:
		var p attach.PauseEvent
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
		return asRateLimit(&httpStatusError{
			op:         fmt.Sprintf("%s %s", method, suffix),
			statusCode: resp.StatusCode,
			body:       string(b),
		}, resp.Header)
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
	// Stamp the JSON content type on every write, body or not — the
	// server requires it on state-changing endpoints as part of its
	// browser-CSRF protection (#383).
	if body != nil || (method != http.MethodGet && method != http.MethodHead) {
		req.Header.Set("Content-Type", "application/json")
	}
	if err := c.auth(req); err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	return c.httpFor(suffix).Do(req)
}

// httpFor picks the RPC client whose whole-request deadline suits the
// endpoint: the long one for /slash/* (each blocks on a model call —
// see slowRPCTimeout), the ordinary one for everything else.
//
// Keyed on the path segment rather than a per-method flag so a slash
// endpoint added later gets the right deadline without anyone
// remembering to opt it in. Reads and small mutations stay on the
// short deadline, where a hang IS a failure worth surfacing fast.
func (c *Client) httpFor(suffix string) *http.Client {
	if c.slowHTTP != nil && strings.Contains(suffix, "/slash/") {
		return c.slowHTTP
	}
	return c.http
}
