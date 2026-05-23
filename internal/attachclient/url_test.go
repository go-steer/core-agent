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

import "testing"

func TestParseURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw         string
		wantScheme  string
		wantHost    string
		wantSocket  string
		wantBaseURL string
		wantSession string
		wantErr     bool
	}{
		{
			raw:         "http://localhost:7777",
			wantScheme:  "http",
			wantHost:    "localhost:7777",
			wantBaseURL: "http://localhost:7777",
			wantSession: "",
		},
		{
			raw:         "https://hub.svc:7777/sessions/core-agent/sess-1",
			wantScheme:  "https",
			wantHost:    "hub.svc:7777",
			wantBaseURL: "https://hub.svc:7777",
			wantSession: "/sessions/core-agent/sess-1",
		},
		{
			raw:         "http://localhost:7777/sessions/sess-1",
			wantScheme:  "http",
			wantHost:    "localhost:7777",
			wantBaseURL: "http://localhost:7777",
			wantSession: "/sessions/sess-1",
		},
		{
			raw:         "unix:///tmp/core-agent.sock",
			wantScheme:  "unix",
			wantSocket:  "/tmp/core-agent.sock",
			wantBaseURL: "http://unix",
			wantSession: "",
		},
		{
			raw:         "unix:///tmp/core-agent.sock/sessions/sess-1",
			wantScheme:  "unix",
			wantSocket:  "/tmp/core-agent.sock",
			wantBaseURL: "http://unix",
			wantSession: "/sessions/sess-1",
		},
		{
			raw:     "ftp://nope",
			wantErr: true,
		},
		{
			raw:     "://malformed",
			wantErr: true,
		},
	}
	for _, c := range cases {
		got, err := ParseURL(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error, got nil", c.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", c.raw, err)
			continue
		}
		if got.Scheme != c.wantScheme {
			t.Errorf("%q: scheme = %q, want %q", c.raw, got.Scheme, c.wantScheme)
		}
		if got.Host != c.wantHost {
			t.Errorf("%q: host = %q, want %q", c.raw, got.Host, c.wantHost)
		}
		if got.SocketPath != c.wantSocket {
			t.Errorf("%q: socketPath = %q, want %q", c.raw, got.SocketPath, c.wantSocket)
		}
		if got.BaseURL != c.wantBaseURL {
			t.Errorf("%q: baseURL = %q, want %q", c.raw, got.BaseURL, c.wantBaseURL)
		}
		if got.Session != c.wantSession {
			t.Errorf("%q: session = %q, want %q", c.raw, got.Session, c.wantSession)
		}
	}
}

func TestParseURL_IsHubURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw       string
		wantIsHub bool
	}{
		{"http://localhost:7777", true},
		{"http://localhost:7777/sessions", true},
		{"http://localhost:7777/sessions/sess-1", false},
		{"unix:///tmp/sock", true},
		{"unix:///tmp/sock/sessions/sess-1", false},
	}
	for _, c := range cases {
		p, err := ParseURL(c.raw)
		if err != nil {
			t.Fatalf("%q: %v", c.raw, err)
		}
		if got := p.IsHubURL(); got != c.wantIsHub {
			t.Errorf("%q: IsHubURL=%v want %v", c.raw, got, c.wantIsHub)
		}
	}
}
