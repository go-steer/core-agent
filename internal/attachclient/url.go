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

// Package attachclient is the shared client for attach-mode endpoints,
// used by both `core-agent attach`/`ls` (in cmd/core-agent) and
// `core-agent-tui` (in cmd/core-agent-tui). The package is internal/
// so the surface isn't part of the public API stability promise —
// it's a coordination point between two of our own binaries, not a
// SDK consumers should reach for.
package attachclient

import (
	"fmt"
	"net/url"
	"strings"
)

// ParsedURL holds the components of an attach-mode URL. Three schemes
// are accepted: http://, https://, unix:// (the last for Unix-socket
// listeners — convention is unix:///path/to/socket/sessions/<sid>).
//
// Session is non-empty when the URL targets a specific session (e.g.
// /sessions/<sid> or /sessions/<app>/<sid>). For listing endpoints
// (GET /sessions) Session is empty.
type ParsedURL struct {
	Scheme     string // http | https | unix
	Host       string // host:port (empty for unix)
	SocketPath string // for unix scheme
	BaseURL    string // ready-to-use for HTTP client: http(s)://host OR http://unix placeholder
	Session    string // /sessions/<...> path; empty for list endpoints
}

// ParseURL decodes raw into a ParsedURL. Returns a clear error for
// unsupported schemes so the caller can surface "want http, https, or
// unix" without digging into url.Parse internals.
func ParseURL(raw string) (*ParsedURL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	out := &ParsedURL{Scheme: u.Scheme}
	switch u.Scheme {
	case "http", "https":
		out.Host = u.Host
		out.BaseURL = u.Scheme + "://" + u.Host
	case "unix":
		// unix:///path/to/socket/sessions/<id>
		// Convention: socket path is everything before "/sessions/";
		// the rest is the resource path.
		idx := strings.Index(u.Path, "/sessions")
		if idx < 0 {
			out.SocketPath = u.Path
		} else {
			out.SocketPath = u.Path[:idx]
		}
		out.BaseURL = "http://unix" // placeholder; the HTTP client dials via custom net.Dialer
	default:
		return nil, fmt.Errorf("unsupported URL scheme %q (want http, https, or unix)", u.Scheme)
	}
	// Extract the session sub-path if present.
	if u.Scheme == "unix" {
		if idx := strings.Index(u.Path, "/sessions"); idx >= 0 {
			out.Session = u.Path[idx:]
		}
	} else {
		out.Session = u.Path
	}
	return out, nil
}

// IsHubURL is a heuristic: hub URLs target the root (no /sessions/<id>
// suffix). Used by the TUI to decide whether to enumerate peer
// sessions in the picker or just list this listener's sessions.
func (p *ParsedURL) IsHubURL() bool {
	return p.Session == "" || p.Session == "/sessions"
}
