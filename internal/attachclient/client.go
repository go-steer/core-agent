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

	"github.com/go-steer/core-agent/pkg/attach"
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
	URL   *ParsedURL
	Token string

	http       *http.Client
	streamHTTP *http.Client
}

// New builds a Client. ParseURL the rawURL first; Token may be empty.
// timeout governs short-lived RPC calls. SSE streams ignore it
// (caller's ctx is the cancel signal). Zero timeout falls back to 30 s
// for RPCs.
func New(parsed *ParsedURL, token string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		URL:        parsed,
		Token:      token,
		http:       newHTTPClient(parsed, timeout),
		streamHTTP: newHTTPClient(parsed, 0), // no timeout — SSE is long-lived
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

// auth applies the bearer token (if any) to req.
func (c *Client) auth(req *http.Request) {
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
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
	c.auth(req)
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("stream: status %d: %s", resp.StatusCode, body)
	}

	out := make(chan attach.Frame, 32)
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
			var frame attach.Frame
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
		return fmt.Errorf("%s %s: status %d: %s", method, suffix, resp.StatusCode, b)
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
	c.auth(req)
	return c.http.Do(req)
}
