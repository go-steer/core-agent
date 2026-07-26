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

package coretuiremote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
)

// Regression test for #384's credential-exfiltration arm: the TUI
// enumerated hub-advertised peers and, on /switch/enumeration,
// connected to each using the operator's SAME credentials — so a
// hostile registrant publishing an attacker endpoint captured the
// operator's bearer/OAuth token. peerClientFor now withholds
// credentials from untrusted peer hosts.

// recordingServer captures whether inbound requests carried an
// Authorization header — the credential we must not leak to
// untrusted peers.
type recordingServer struct {
	*httptest.Server
	mu       sync.Mutex
	sawAuth  bool
	authVals []string
}

func startRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		if a := r.Header.Get("Authorization"); a != "" {
			rs.sawAuth = true
			rs.authVals = append(rs.authVals, a)
		}
		rs.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessions": []attachclient.SessionDescriptor{{App: "core-agent", SessionID: "peer-sid"}},
		})
	})
	rs.Server = httptest.NewServer(mux)
	t.Cleanup(rs.Close)
	return rs
}

func (rs *recordingServer) sawAuthorization() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.sawAuth
}

// credentialedFactory mirrors cmd/core-agent-tui's factory: every
// Client it builds carries the operator's bearer token. peerClientFor
// decides whether to route through this (trusted) or build a
// credential-less client (untrusted).
func credentialedFactory() ClientFactory {
	return func(endpoint string) (*attachclient.Client, error) {
		p, err := attachclient.ParseURL(endpoint)
		if err != nil {
			return nil, err
		}
		return attachclient.New(p, "operator-secret-token", 0), nil
	}
}

// TestPeerCredentials_UntrustedPeerGetsNoAuth: an untrusted
// hub-advertised peer endpoint is enumerated WITHOUT the operator's
// Authorization header.
func TestPeerCredentials_UntrustedPeerGetsNoAuth(t *testing.T) {
	t.Parallel()
	peer := startRecordingServer(t)

	local := startSessionsServer(t)
	local.list = []attachclient.SessionDescriptor{{App: "core-agent", SessionID: "local-one"}}
	local.peers = []attachclient.PeerDescriptor{{Name: "hostile", Endpoint: peer.URL}}

	// httptest servers all bind 127.0.0.1, so give the hub client a
	// distinct advertised Host (still dialing local.URL via BaseURL)
	// — otherwise the peer would spuriously match the hub host and be
	// treated as trusted.
	hub := &attachclient.ParsedURL{
		Scheme:  "http",
		Host:    "hub.internal:7777",
		BaseURL: local.URL,
		Session: "/sessions/core-agent/local-one",
	}
	a := NewWithClientFactory(attachclient.New(hub, "operator-secret-token", 0),
		"/sessions/core-agent/local-one", credentialedFactory())
	// No trusted-peers configured: only the hub host (hub.internal) is
	// trusted, and the httptest peer is on 127.0.0.1.

	rows := a.Sessions()
	if len(rows) != 2 {
		t.Fatalf("Sessions() = %d rows, want 2 (local + peer): %+v", len(rows), rows)
	}
	if peer.sawAuthorization() {
		t.Fatalf("untrusted peer %s received the operator's Authorization header (credential exfiltration): %v",
			peer.URL, peer.authVals)
	}
}

// TestPeerCredentials_TrustedPeerGetsAuth: adding the peer's host to
// the trusted-peers list restores credentialed enumeration.
func TestPeerCredentials_TrustedPeerGetsAuth(t *testing.T) {
	t.Parallel()
	peer := startRecordingServer(t)

	local := startSessionsServer(t)
	local.list = []attachclient.SessionDescriptor{{App: "core-agent", SessionID: "local-one"}}
	local.peers = []attachclient.PeerDescriptor{{Name: "trusted", Endpoint: peer.URL}}

	parsed, err := attachclient.ParseURL(local.URL + "/sessions/core-agent/local-one")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	a := NewWithClientFactory(attachclient.New(parsed, "operator-secret-token", 0),
		"/sessions/core-agent/local-one", credentialedFactory())
	a.SetTrustedPeerHosts([]string{hostnameOnly(mustHost(t, peer.URL))})

	if rows := a.Sessions(); len(rows) != 2 {
		t.Fatalf("Sessions() = %d rows, want 2: %+v", len(rows), rows)
	}
	if !peer.sawAuthorization() {
		t.Fatalf("trusted peer %s should have received the operator's Authorization header", peer.URL)
	}
}

func mustHost(t *testing.T, rawURL string) string {
	t.Helper()
	p, err := attachclient.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("ParseURL(%q): %v", rawURL, err)
	}
	return p.Host
}

func TestTrustPeerEndpointHost(t *testing.T) {
	t.Parallel()
	// Hub client at hub.example.com:7777.
	parsed, err := attachclient.ParseURL("http://hub.example.com:7777/sessions/s")
	if err != nil {
		t.Fatalf("ParseURL: %v", err)
	}
	a := New(attachclient.New(parsed, "tok", 0), "/sessions/s")

	// Hub's own host (any port) trusted.
	if !a.trustPeerEndpointHost("hub.example.com:9999") {
		t.Error("hub host should be trusted regardless of port")
	}
	if !a.trustPeerEndpointHost("HUB.EXAMPLE.COM:7777") {
		t.Error("hub host match should be case-insensitive")
	}
	// Arbitrary peer host not trusted by default.
	if a.trustPeerEndpointHost("attacker.example.com:7777") {
		t.Error("arbitrary peer host must not be trusted by default")
	}
	// Added to the trusted list → trusted.
	a.SetTrustedPeerHosts([]string{"partner.example.com"})
	if !a.trustPeerEndpointHost("partner.example.com:7777") {
		t.Error("explicitly trusted host should be trusted")
	}
	if a.trustPeerEndpointHost("attacker.example.com:7777") {
		t.Error("non-listed host must stay untrusted")
	}
}
