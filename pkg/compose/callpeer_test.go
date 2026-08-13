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

package compose

import (
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

func registerPeer(t *testing.T, reg *attach.PeerRegistry, name, endpoint string) *attach.Peer {
	t.Helper()
	p, err := reg.Register(attach.RegisterRequest{Name: name, Endpoint: endpoint})
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return p
}

func TestPeerDirectory_ProjectsLivePeers(t *testing.T) {
	t.Parallel()
	reg := attach.NewPeerRegistry()
	t.Cleanup(func() { _ = reg.Close() })
	registerPeer(t, reg, "operator-prod-1", "https://prod-1.example.test")

	got := PeerDirectory(func() *attach.PeerRegistry { return reg }, nil).Peers()
	if len(got) != 1 {
		t.Fatalf("peers = %d, want 1", len(got))
	}
	if got[0].Name != "operator-prod-1" || got[0].Endpoint != "https://prod-1.example.test" {
		t.Errorf("peer = %+v, want the registered name + endpoint", got[0])
	}
}

// A peer that stopped heartbeating is presumed gone. Waiting for the
// registry's 5s prune tick would let call_peer burn its whole timeout
// dialing a pod that isn't there.
func TestPeerDirectory_FiltersExpiredLeases(t *testing.T) {
	t.Parallel()
	reg := attach.NewPeerRegistry()
	t.Cleanup(func() { _ = reg.Close() })
	p := registerPeer(t, reg, "gone", "https://gone.example.test")

	future := p.LeaseExpiresAt.Add(time.Second)
	got := PeerDirectory(func() *attach.PeerRegistry { return reg }, func() time.Time { return future }).Peers()
	if len(got) != 0 {
		t.Errorf("peers after the lease lapsed = %+v, want none", got)
	}
	// The registry itself still holds it — the filter is ours, not a
	// side effect of pruning.
	if reg.Len() != 1 {
		t.Errorf("registry Len = %d, want the entry still present (filtering must not mutate)", reg.Len())
	}
}

func TestPeerDirectory_NilRegistryIsAnEmptyRoster(t *testing.T) {
	t.Parallel()
	// The closure shape exists precisely because the registry is built
	// later in boot than the tool. Reading it early must be harmless.
	if got := PeerDirectory(func() *attach.PeerRegistry { return nil }, nil).Peers(); len(got) != 0 {
		t.Errorf("peers = %+v, want none for a nil registry", got)
	}
	if got := PeerDirectory(nil, nil).Peers(); len(got) != 0 {
		t.Errorf("peers = %+v, want none for a nil getter", got)
	}
}

func TestPeerDirectory_ReadsTheRegistryOnEveryCall(t *testing.T) {
	t.Parallel()
	reg := attach.NewPeerRegistry()
	t.Cleanup(func() { _ = reg.Close() })
	dir := PeerDirectory(func() *attach.PeerRegistry { return reg }, nil)

	if got := dir.Peers(); len(got) != 0 {
		t.Fatalf("peers before registration = %+v, want none", got)
	}
	registerPeer(t, reg, "late", "https://late.example.test")
	if got := dir.Peers(); len(got) != 1 {
		t.Errorf("peers after registration = %+v, want the newly registered peer", got)
	}
}

func TestBuildCallPeerTool_NamesTheToolAndSeesTheRegistry(t *testing.T) {
	t.Parallel()
	reg := attach.NewPeerRegistry()
	t.Cleanup(func() { _ = reg.Close() })
	registerPeer(t, reg, "ops", "https://ops.example.test")

	cfg := config.DefaultConfig()
	cfg.Tools.CallPeer = config.CallPeerConfig{Enabled: true}
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})

	tl, err := BuildCallPeerTool(gate, cfg, func() *attach.PeerRegistry { return reg })
	if err != nil {
		t.Fatalf("BuildCallPeerTool: %v", err)
	}
	if tl.Name() != "call_peer" {
		t.Errorf("tool name = %q, want call_peer", tl.Name())
	}
}
