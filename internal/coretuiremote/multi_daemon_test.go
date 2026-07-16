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

// Tests for multi-daemon /switch + /attach (issue #246):
// NewWithClientFactory, peer-fan-out Sessions(), cross-daemon
// SwitchToSession, and the /attach <url> [<sid>] escape hatch.

package coretuiremote

import (
	"context"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/internal/attachclient"
)

// newTestAdapterMulti builds a multi-daemon-capable Adapter — same
// as newTestAdapter but wires a ClientFactory that talks to any
// httptest URL with a fixed token. Peer daemons started in-test are
// reached through this factory.
func newTestAdapterMulti(t *testing.T, fs *sessionsServer, sessionPath string) *Adapter {
	t.Helper()
	parsed, err := attachclient.ParseURL(fs.URL + sessionPath)
	if err != nil {
		t.Fatalf("parseURL: %v", err)
	}
	factory := func(endpoint string) (*attachclient.Client, error) {
		p, err := attachclient.ParseURL(endpoint)
		if err != nil {
			return nil, err
		}
		return attachclient.New(p, "test-token", 0), nil
	}
	return NewWithClientFactory(attachclient.New(parsed, "test-token", 0), sessionPath, factory)
}

// TestSessions_EnumeratesLocalAndPeers — multi-daemon Sessions()
// returns local rows first, then peer rows tagged with the peer
// label. The endpointByID cache is populated correctly (verified
// indirectly through SwitchToSession — see TestSwitchToSession_PeerTarget
// below).
func TestSessions_EnumeratesLocalAndPeers(t *testing.T) {
	t.Parallel()
	peer := startSessionsServer(t)
	peer.list = []attachclient.SessionDescriptor{
		{App: "core-agent", SessionID: "peer-a"},
		{App: "core-agent", SessionID: "peer-b"},
	}
	local := startSessionsServer(t)
	local.list = []attachclient.SessionDescriptor{
		{App: "core-agent", SessionID: "local-one"},
	}
	local.peers = []attachclient.PeerDescriptor{
		{Name: "staging", Endpoint: peer.URL},
	}
	a := newTestAdapterMulti(t, local, "/sessions/core-agent/local-one")

	got := a.Sessions()
	if len(got) != 3 {
		t.Fatalf("Sessions() = %d rows, want 3 (1 local + 2 peer): %+v", len(got), got)
	}

	// Local row first.
	if got[0].ID != "local-one" || !got[0].Current {
		t.Errorf("row 0 = %+v, want local-one (current)", got[0])
	}
	if strings.Contains(got[0].Display, "peer:") {
		t.Errorf("local row Display should not carry a peer: tag, got %q", got[0].Display)
	}

	// Peer rows.
	for _, r := range got[1:] {
		if !strings.HasPrefix(r.Display, "[peer:staging]") {
			t.Errorf("peer row Display = %q, want [peer:staging] prefix", r.Display)
		}
		if r.Description != peer.URL {
			t.Errorf("peer row Description = %q, want %q (peer endpoint)", r.Description, peer.URL)
		}
		if r.Current {
			t.Errorf("peer row %s marked Current — only local session should be current", r.ID)
		}
	}
}

// TestSessions_PeerFanOutDropsFailures — a peer whose ListSessions
// errors is silently dropped from the enumeration; local rows and
// other-peer rows survive.
func TestSessions_PeerFanOutDropsFailures(t *testing.T) {
	t.Parallel()
	badPeer := startSessionsServer(t)
	badPeer.listStatus = 500 // ListSessions errors on this one
	goodPeer := startSessionsServer(t)
	goodPeer.list = []attachclient.SessionDescriptor{{SessionID: "good"}}

	local := startSessionsServer(t)
	local.list = []attachclient.SessionDescriptor{{SessionID: "here"}}
	local.peers = []attachclient.PeerDescriptor{
		{Name: "bad", Endpoint: badPeer.URL},
		{Name: "good", Endpoint: goodPeer.URL},
	}
	a := newTestAdapterMulti(t, local, "/sessions/here")

	got := a.Sessions()
	if len(got) != 2 {
		t.Fatalf("Sessions() = %d rows, want 2 (1 local + 1 good peer, bad peer dropped): %+v", len(got), got)
	}
	if got[0].ID != "here" {
		t.Errorf("row 0 = %q, want local 'here'", got[0].ID)
	}
	if got[1].ID != "good" {
		t.Errorf("row 1 = %q, want peer 'good'", got[1].ID)
	}
}

// TestSessions_SkipsPeersWhenNoFactory — bare New (single-daemon
// mode) does NOT enumerate peers even when the daemon advertises
// them. Ensures the /peers call isn't made unnecessarily and that
// v0.10.x behavior is preserved verbatim for single-daemon hosts.
func TestSessions_SkipsPeersWhenNoFactory(t *testing.T) {
	t.Parallel()
	peer := startSessionsServer(t)
	peer.list = []attachclient.SessionDescriptor{{SessionID: "should-not-appear"}}
	local := startSessionsServer(t)
	local.list = []attachclient.SessionDescriptor{{SessionID: "only-me"}}
	local.peers = []attachclient.PeerDescriptor{{Name: "ignored", Endpoint: peer.URL}}

	// newTestAdapter (bare New — no factory)
	a := newTestAdapter(t, local, "/sessions/only-me")

	got := a.Sessions()
	if len(got) != 1 || got[0].ID != "only-me" {
		t.Fatalf("Sessions() = %+v, want single local row", got)
	}
}

// TestSwitchToSession_PeerTargetUsesFactory — SwitchToSession on a
// sid that Sessions() sourced from a peer builds a fresh Adapter
// against the peer's endpoint (verified by SessionPath pointing at
// the peer's URL after the switch).
func TestSwitchToSession_PeerTargetUsesFactory(t *testing.T) {
	t.Parallel()
	peer := startSessionsServer(t)
	peer.list = []attachclient.SessionDescriptor{
		{App: "core-agent", SessionID: "target"},
	}
	local := startSessionsServer(t)
	local.list = []attachclient.SessionDescriptor{{SessionID: "here"}}
	local.peers = []attachclient.PeerDescriptor{{Name: "p", Endpoint: peer.URL}}

	a := newTestAdapterMulti(t, local, "/sessions/here")
	_ = a.Sessions() // populate endpointByID cache

	tgt, err := a.SwitchToSession("target")
	if err != nil {
		t.Fatalf("SwitchToSession(target): %v", err)
	}
	next, ok := tgt.Agent.(*Adapter)
	if !ok {
		t.Fatalf("SwitchTo.Agent is %T, want *Adapter", tgt.Agent)
	}
	if next.SessionPath() != "/sessions/core-agent/target" {
		t.Errorf("new sessionPath = %q, want /sessions/core-agent/target", next.SessionPath())
	}
	if !strings.Contains(tgt.Note, "peer") {
		t.Errorf("Note = %q, want to mention peer origin", tgt.Note)
	}
	if next.clientFactory == nil {
		t.Errorf("new Adapter should inherit clientFactory so operator can hop again")
	}
}

// TestSwitchToSession_LocalAfterPeerEnum — after Sessions() populated
// endpointByID with peer rows, a local target must still resolve
// through the local path (not accidentally route to a peer).
func TestSwitchToSession_LocalAfterPeerEnum(t *testing.T) {
	t.Parallel()
	peer := startSessionsServer(t)
	peer.list = []attachclient.SessionDescriptor{{SessionID: "far"}}
	local := startSessionsServer(t)
	local.list = []attachclient.SessionDescriptor{
		{App: "core-agent", SessionID: "here"},
		{App: "core-agent", SessionID: "sibling"},
	}
	local.peers = []attachclient.PeerDescriptor{{Name: "p", Endpoint: peer.URL}}

	a := newTestAdapterMulti(t, local, "/sessions/core-agent/here")
	_ = a.Sessions()

	tgt, err := a.SwitchToSession("sibling")
	if err != nil {
		t.Fatalf("SwitchToSession(sibling): %v", err)
	}
	next := tgt.Agent.(*Adapter)
	if next.SessionPath() != "/sessions/core-agent/sibling" {
		t.Errorf("local sibling path = %q, want /sessions/core-agent/sibling", next.SessionPath())
	}
	// Should have reused the local client, not built a fresh one.
	if next.client != a.client {
		t.Errorf("local sibling switch should reuse the current daemon's client")
	}
}

// TestSwitchToSession_FactoryInheritedThroughChain — chained switches
// preserve the factory so the operator can hop again from the target.
func TestSwitchToSession_FactoryInheritedThroughChain(t *testing.T) {
	t.Parallel()
	peer := startSessionsServer(t)
	peer.list = []attachclient.SessionDescriptor{{SessionID: "peer-sid"}}
	local := startSessionsServer(t)
	local.list = []attachclient.SessionDescriptor{{SessionID: "here"}}
	local.peers = []attachclient.PeerDescriptor{{Endpoint: peer.URL}}

	a := newTestAdapterMulti(t, local, "/sessions/here")
	_ = a.Sessions()
	tgt, err := a.SwitchToSession("peer-sid")
	if err != nil {
		t.Fatalf("SwitchToSession: %v", err)
	}
	next := tgt.Agent.(*Adapter)
	if next.clientFactory == nil {
		t.Fatalf("chained switch dropped clientFactory")
	}
	// And confirm we can call Sessions() on next without panic.
	_ = next.Sessions()
}

// TestAttach_ListsSessions — /attach <url> returns a system message
// listing the peer's sessions; no SwitchTo, no chat wipe.
func TestAttach_ListsSessions(t *testing.T) {
	t.Parallel()
	peer := startSessionsServer(t)
	peer.list = []attachclient.SessionDescriptor{
		{App: "core-agent", SessionID: "one"},
		{App: "core-agent", SessionID: "two"},
	}
	local := startSessionsServer(t)
	local.list = []attachclient.SessionDescriptor{{SessionID: "here"}}

	a := newTestAdapterMulti(t, local, "/sessions/here")
	res, err := a.dispatchAttach(context.Background(), peer.URL)
	if err != nil {
		t.Fatalf("dispatchAttach: %v", err)
	}
	if res.SwitchTo != nil {
		t.Errorf("bare /attach should not switch, got SwitchTo = %+v", res.SwitchTo)
	}
	if !strings.Contains(res.SystemMessage, "one") || !strings.Contains(res.SystemMessage, "two") {
		t.Errorf("SystemMessage should list peer's sessions, got %q", res.SystemMessage)
	}
}

// TestAttach_DirectJump — /attach <url> <sid> returns SwitchTo with
// a new Adapter pointing at the peer's specific session.
func TestAttach_DirectJump(t *testing.T) {
	t.Parallel()
	peer := startSessionsServer(t)
	peer.list = []attachclient.SessionDescriptor{
		{App: "core-agent", SessionID: "target"},
	}
	local := startSessionsServer(t)
	a := newTestAdapterMulti(t, local, "/sessions/here")

	res, err := a.dispatchAttach(context.Background(), peer.URL+" target")
	if err != nil {
		t.Fatalf("dispatchAttach: %v", err)
	}
	if res.SwitchTo == nil {
		t.Fatal("/attach <url> <sid> should return SwitchTo")
	}
	next := res.SwitchTo.Agent.(*Adapter)
	if next.SessionPath() != "/sessions/core-agent/target" {
		t.Errorf("path = %q, want /sessions/core-agent/target", next.SessionPath())
	}
	if next.clientFactory == nil {
		t.Errorf("new Adapter should inherit factory")
	}
	if !strings.Contains(res.SwitchTo.Note, "target") {
		t.Errorf("Note = %q, want to mention target sid", res.SwitchTo.Note)
	}
}

// TestAttach_NoFactoryHint — bare-New adapter running /attach returns
// a friendly SystemMessage pointing at #246 rather than crashing.
func TestAttach_NoFactoryHint(t *testing.T) {
	t.Parallel()
	local := startSessionsServer(t)
	// bare newTestAdapter (single-daemon, no factory)
	a := newTestAdapter(t, local, "/sessions/here")

	res, err := a.dispatchAttach(context.Background(), "http://any:1234")
	if err != nil {
		t.Fatalf("dispatchAttach on bare adapter: unexpected err %v", err)
	}
	if res.SwitchTo != nil {
		t.Errorf("bare adapter must not switch, got %+v", res.SwitchTo)
	}
	if !strings.Contains(res.SystemMessage, "multi-daemon") && !strings.Contains(res.SystemMessage, "#246") {
		t.Errorf("expected hint about multi-daemon mode / #246, got %q", res.SystemMessage)
	}
}

// TestAttach_MalformedURL — invalid URL yields a friendly parse
// error, no crash.
func TestAttach_MalformedURL(t *testing.T) {
	t.Parallel()
	local := startSessionsServer(t)
	a := newTestAdapterMulti(t, local, "/sessions/here")

	res, err := a.dispatchAttach(context.Background(), "::not-a-url::")
	if err != nil {
		t.Fatalf("dispatchAttach: unexpected err %v", err)
	}
	if !strings.Contains(res.SystemMessage, "parse URL") {
		t.Errorf("expected parse URL error, got %q", res.SystemMessage)
	}
}

// TestAttach_EmptyArgs — bare /attach with no URL shows usage.
func TestAttach_EmptyArgs(t *testing.T) {
	t.Parallel()
	local := startSessionsServer(t)
	a := newTestAdapterMulti(t, local, "/sessions/here")

	res, err := a.dispatchAttach(context.Background(), "")
	if err != nil {
		t.Fatalf("dispatchAttach: %v", err)
	}
	if !strings.Contains(res.SystemMessage, "usage") {
		t.Errorf("expected usage hint, got %q", res.SystemMessage)
	}
}

// TestAttach_EmptyPeer — /attach <url> to a peer with no sessions
// returns a helpful message pointing at POST /sessions rather than
// an empty listing.
func TestAttach_EmptyPeer(t *testing.T) {
	t.Parallel()
	peer := startSessionsServer(t) // list stays empty
	local := startSessionsServer(t)
	a := newTestAdapterMulti(t, local, "/sessions/here")

	res, err := a.dispatchAttach(context.Background(), peer.URL)
	if err != nil {
		t.Fatalf("dispatchAttach: %v", err)
	}
	if !strings.Contains(res.SystemMessage, "no sessions") {
		t.Errorf("expected 'no sessions' hint, got %q", res.SystemMessage)
	}
}

// TestSlashCommands_IncludesAttach — /attach must be advertised in
// the palette + /help.
func TestSlashCommands_IncludesAttach(t *testing.T) {
	t.Parallel()
	local := startSessionsServer(t)
	a := newTestAdapterMulti(t, local, "/sessions/here")
	for _, spec := range a.SlashCommands() {
		if spec.Name == "attach" {
			if spec.Description == "" {
				t.Errorf("/attach spec has empty Description")
			}
			return
		}
	}
	t.Fatal("SlashCommands() does not advertise /attach")
}
