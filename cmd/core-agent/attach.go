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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/runner"
)

// runAttachSubcommand handles `core-agent attach <url> [flags]`.
// Connects to a remote attach-mode listener, prints the chat-style
// event stream, and reads stdin lines for inject (line-mode for v1;
// tmux-style raw-mode `:` deferred to a polish PR).
//
// URL forms:
//
//	http(s)://host:port/sessions/<app>/<sid>     qualified
//	http(s)://host:port/sessions/<sid>           shortcut
//	unix:///path/to/socket/sessions/<app>/<sid>  Unix domain socket
//
// Bearer token via --token (env var name) for matching the listener's
// --attach-token. mTLS is not yet wired in the client (TODO follow-on).
func runAttachSubcommand(args []string) int {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	tokenEnv := fs.String("token", "", "env var holding the bearer token (e.g. CORE_AGENT_ATTACH_TOKEN)")
	wakeFlag := fs.Bool("wake", false, "send POST /wake instead of streaming (one-shot)")
	noStdin := fs.Bool("no-stdin", false, "don't read stdin for inject; pure passive watch")
	if err := fs.Parse(args); err != nil {
		return runner.ExitConfigError
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "core-agent attach: URL is required")
		fmt.Fprintln(os.Stderr, "usage: core-agent attach <url> [--token=ENVVAR] [--wake] [--no-stdin]")
		return runner.ExitConfigError
	}
	rawURL := fs.Arg(0)
	token := ""
	if *tokenEnv != "" {
		token = os.Getenv(*tokenEnv)
	}

	parsed, err := parseAttachURL(rawURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent attach: %v\n", err)
		return runner.ExitConfigError
	}

	if *wakeFlag {
		return doWake(parsed, token)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Goroutine reads stdin lines and POSTs each as inject. Closing
	// stdin (Ctrl+D) or canceling ctx stops it.
	if !*noStdin {
		go runStdinInjector(ctx, parsed, token)
	}

	return streamEvents(ctx, parsed, token)
}

// runLsSubcommand handles `core-agent ls <url> [flags]`. Single GET
// /sessions; prints the registered sessions in a stable order.
func runLsSubcommand(args []string) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	tokenEnv := fs.String("token", "", "env var holding the bearer token")
	if err := fs.Parse(args); err != nil {
		return runner.ExitConfigError
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "core-agent ls: URL is required")
		fmt.Fprintln(os.Stderr, "usage: core-agent ls <url> [--token=ENVVAR]")
		return runner.ExitConfigError
	}
	rawURL := fs.Arg(0)
	token := ""
	if *tokenEnv != "" {
		token = os.Getenv(*tokenEnv)
	}
	parsed, err := parseAttachURL(rawURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent ls: %v\n", err)
		return runner.ExitConfigError
	}
	parsed.session = "" // /sessions endpoint, not /sessions/<id>

	req, err := http.NewRequest(http.MethodGet, parsed.baseURL+"/sessions", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent ls: %v\n", err)
		return runner.ExitAgentError
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := buildHTTPClient(parsed)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent ls: %v\n", err)
		return runner.ExitAgentError
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "core-agent ls: status %d: %s\n", resp.StatusCode, body)
		return runner.ExitAgentError
	}
	var out struct {
		Sessions []struct {
			App         string `json:"app"`
			User        string `json:"user"`
			SessionID   string `json:"sessionID"`
			HasEventLog bool   `json:"has_event_log"`
		} `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent ls: decode: %v\n", err)
		return runner.ExitAgentError
	}
	if len(out.Sessions) == 0 {
		fmt.Println("(no sessions)")
	} else {
		fmt.Println("SESSIONS:")
		fmt.Printf("  %-30s %-20s %-30s %s\n", "APP", "USER", "SESSION", "EVENTLOG")
		for _, s := range out.Sessions {
			eventlogMark := "-"
			if s.HasEventLog {
				eventlogMark = "yes"
			}
			fmt.Printf("  %-30s %-20s %-30s %s\n", s.App, s.User, s.SessionID, eventlogMark)
		}
	}

	// Peer listing — best-effort, suppress 404s (the hub may not have
	// peer-registration enabled). Show as a second section under the
	// session list so the operator sees both at a glance.
	peerReq, _ := http.NewRequest(http.MethodGet, parsed.baseURL+"/peers", nil)
	if token != "" {
		peerReq.Header.Set("Authorization", "Bearer "+token)
	}
	peerResp, perr := client.Do(peerReq)
	if perr == nil {
		defer func() { _ = peerResp.Body.Close() }()
		if peerResp.StatusCode == http.StatusOK {
			var peerOut struct {
				Peers []struct {
					Name     string            `json:"name"`
					Endpoint string            `json:"endpoint"`
					Labels   map[string]string `json:"labels,omitempty"`
				} `json:"peers"`
			}
			if derr := json.NewDecoder(peerResp.Body).Decode(&peerOut); derr == nil && len(peerOut.Peers) > 0 {
				fmt.Println()
				fmt.Println("PEERS:")
				fmt.Printf("  %-30s %-40s %s\n", "NAME", "ENDPOINT", "LABELS")
				for _, p := range peerOut.Peers {
					labels := ""
					for k, v := range p.Labels {
						if labels != "" {
							labels += ","
						}
						labels += k + "=" + v
					}
					fmt.Printf("  %-30s %-40s %s\n", p.Name, p.Endpoint, labels)
				}
			}
		}
	}
	return runner.ExitOK
}

// parsedAttachURL is the parsed components of an attach URL. session
// is empty for /sessions list URLs (ls subcommand).
type parsedAttachURL struct {
	scheme     string // http, https, unix
	host       string // host:port (or empty for unix)
	socketPath string // for unix scheme
	baseURL    string // for HTTP client: http(s)://host:port OR http://unix
	session    string // /sessions/<id> path or empty
}

func parseAttachURL(raw string) (*parsedAttachURL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	out := &parsedAttachURL{scheme: u.Scheme}
	switch u.Scheme {
	case "http", "https":
		out.host = u.Host
		out.baseURL = u.Scheme + "://" + u.Host
	case "unix":
		// unix:///path/to/socket/sessions/<id>
		// u.Path = "/path/to/socket/sessions/<id>" ... we don't have
		// a clean way to split socket-from-resource. Use the convention:
		// the socket path is everything before "/sessions/", the rest is
		// the resource path.
		idx := strings.Index(u.Path, "/sessions")
		if idx < 0 {
			out.socketPath = u.Path
		} else {
			out.socketPath = u.Path[:idx]
		}
		out.baseURL = "http://unix" // placeholder; actual dial via custom Dialer
	default:
		return nil, fmt.Errorf("unsupported URL scheme %q (want http, https, or unix)", u.Scheme)
	}
	// Strip /sessions/<rest> into out.session.
	if u.Scheme == "unix" {
		if idx := strings.Index(u.Path, "/sessions"); idx >= 0 {
			out.session = u.Path[idx:]
		}
	} else {
		out.session = u.Path
	}
	return out, nil
}

func buildHTTPClient(p *parsedAttachURL) *http.Client {
	if p.scheme == "unix" {
		return &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", p.socketPath)
				},
			},
		}
	}
	// TODO follow-on: mTLS client cert/key on https URLs.
	return http.DefaultClient
}

// streamEvents subscribes to the SSE endpoint and prints each frame
// in a chat-style form. Returns runner.ExitOK on a clean ctx cancel,
// ExitTransport on connection failures.
func streamEvents(ctx context.Context, p *parsedAttachURL, token string) int {
	url := p.baseURL + p.session + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent attach: %v\n", err)
		return runner.ExitAgentError
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := buildHTTPClient(p)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "core-agent attach: %v\n", err)
		return runner.ExitAgentError
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "core-agent attach: status %d: %s\n", resp.StatusCode, body)
		return runner.ExitAgentError
	}
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
		printFrame(frame)
	}
	if err := scanner.Err(); err != nil && !isCanceledErr(err) {
		fmt.Fprintf(os.Stderr, "core-agent attach: stream: %v\n", err)
		return runner.ExitAgentError
	}
	return runner.ExitOK
}

// runStdinInjector reads stdin lines and POSTs each non-empty line to
// the /inject endpoint. Stops on stdin close, ctx cancel, or scanner
// error. Errors are surfaced to stderr but don't kill the run (the
// chat stream goroutine is the canonical "we're still alive" signal).
func runStdinInjector(ctx context.Context, p *parsedAttachURL, token string) {
	fmt.Fprintln(os.Stderr, "[input] type a message + Enter to inject. Ctrl+D ends stdin; Ctrl+C ends the attach.")
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
		if err := postJSON(ctx, p, token, "/inject", map[string]string{"message": line}); err != nil {
			fmt.Fprintf(os.Stderr, "[input] inject failed: %v\n", err)
			continue
		}
		fmt.Fprintf(os.Stderr, "[input] queued: %s\n", line)
	}
}

func doWake(p *parsedAttachURL, token string) int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := postJSON(ctx, p, token, "/wake", map[string]string{}); err != nil {
		fmt.Fprintf(os.Stderr, "core-agent attach --wake: %v\n", err)
		return runner.ExitAgentError
	}
	fmt.Println("woken")
	return runner.ExitOK
}

func postJSON(ctx context.Context, p *parsedAttachURL, token, suffix string, body any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := p.baseURL + p.session + suffix
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := buildHTTPClient(p)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, respBody)
	}
	return nil
}

// printFrame renders one frame to stdout in a chat-style form. For
// v1 we keep it minimal: function calls / responses on stderr with
// the same sigils runner.WriteEvents uses, partial text inline on
// stdout. A future polish PR can pipe through runner.WriteEvents
// proper (which today expects an iter.Seq2, not a per-event push).
func printFrame(f attach.Frame) {
	if f.Event == nil {
		return
	}
	ev := f.Event
	if ev.Content == nil {
		return
	}
	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		switch {
		case p.FunctionCall != nil:
			fmt.Fprintf(os.Stderr, "→ %s(...)\n", p.FunctionCall.Name)
		case p.FunctionResponse != nil:
			fmt.Fprintf(os.Stderr, "← %s(...)\n", p.FunctionResponse.Name)
		case p.Text != "" && ev.Partial:
			fmt.Print(p.Text)
		}
	}
}

func isCanceledErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "context canceled") ||
		strings.Contains(err.Error(), "use of closed network connection")
}

// Silence unused-import errors when this file is compiled in
// isolation; session import is for the Frame's Event type clarity.
var _ = session.NewEvent
