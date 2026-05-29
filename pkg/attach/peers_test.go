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

package attach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// clockFn returns a closure that returns the current value of t and
// can be advanced via the returned setter. Test-only.
func clockFn(initial time.Time) (now func() time.Time, advance func(time.Duration)) {
	var v atomic.Pointer[time.Time]
	v.Store(&initial)
	return func() time.Time {
			return *v.Load()
		}, func(d time.Duration) {
			next := v.Load().Add(d)
			v.Store(&next)
		}
}

func TestPeerRegistry_RegisterAssignsIDAndLease(t *testing.T) {
	t.Parallel()
	now, _ := clockFn(time.Now())
	r := NewPeerRegistry(withClock(now))
	defer func() { _ = r.Close() }()

	p, err := r.Register(RegisterRequest{
		Name:            "monitor-a",
		Endpoint:        "https://10.0.0.1:7777",
		HeartbeatTTLSec: 30,
		Labels:          map[string]string{"role": "monitor"},
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if p.RegistrationID == "" {
		t.Errorf("RegistrationID should be assigned")
	}
	if p.LeaseExpiresAt.Sub(p.RegisteredAt) != 30*time.Second {
		t.Errorf("lease duration = %v, want 30s", p.LeaseExpiresAt.Sub(p.RegisteredAt))
	}
	if p.Labels["role"] != "monitor" {
		t.Errorf("Labels lost: %v", p.Labels)
	}
}

func TestPeerRegistry_RegisterValidation(t *testing.T) {
	t.Parallel()
	r := NewPeerRegistry()
	defer func() { _ = r.Close() }()

	if _, err := r.Register(RegisterRequest{Endpoint: "x"}); !errors.Is(err, ErrPeerNameRequired) {
		t.Errorf("missing name: want ErrPeerNameRequired, got %v", err)
	}
	if _, err := r.Register(RegisterRequest{Name: "n"}); !errors.Is(err, ErrPeerEndpointRequired) {
		t.Errorf("missing endpoint: want ErrPeerEndpointRequired, got %v", err)
	}
}

func TestPeerRegistry_ClampsAtMaxTTL(t *testing.T) {
	t.Parallel()
	now, _ := clockFn(time.Now())
	r := NewPeerRegistry(withClock(now), WithMaxTTL(2*time.Minute))
	defer func() { _ = r.Close() }()

	// Peer asks for an hour; should get clamped to 2 minutes.
	p, err := r.Register(RegisterRequest{
		Name: "n", Endpoint: "x", HeartbeatTTLSec: 3600,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if p.LeaseExpiresAt.Sub(p.RegisteredAt) != 2*time.Minute {
		t.Errorf("lease = %v, want clamped to 2m", p.LeaseExpiresAt.Sub(p.RegisteredAt))
	}
}

func TestPeerRegistry_RegisterUpsertsOnName(t *testing.T) {
	t.Parallel()
	r := NewPeerRegistry()
	defer func() { _ = r.Close() }()

	first, _ := r.Register(RegisterRequest{Name: "n", Endpoint: "v1"})
	second, _ := r.Register(RegisterRequest{Name: "n", Endpoint: "v2"})

	if first.RegistrationID == second.RegistrationID {
		t.Errorf("upsert should issue a fresh RegistrationID")
	}
	if r.Len() != 1 {
		t.Errorf("after upsert Len = %d, want 1", r.Len())
	}
	// Old ID should be gone.
	if _, err := r.Heartbeat(first.RegistrationID); !errors.Is(err, ErrPeerNotFound) {
		t.Errorf("old ID still resolvable: %v", err)
	}
}

func TestPeerRegistry_HeartbeatExtendsLease(t *testing.T) {
	t.Parallel()
	now, advance := clockFn(time.Now())
	r := NewPeerRegistry(withClock(now))
	defer func() { _ = r.Close() }()

	p, _ := r.Register(RegisterRequest{Name: "n", Endpoint: "x", HeartbeatTTLSec: 60})
	expiresBefore := p.LeaseExpiresAt
	advance(30 * time.Second)
	p2, err := r.Heartbeat(p.RegistrationID)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if !p2.LeaseExpiresAt.After(expiresBefore) {
		t.Errorf("heartbeat should extend lease; was %v, now %v", expiresBefore, p2.LeaseExpiresAt)
	}
}

func TestPeerRegistry_PruneExpired(t *testing.T) {
	t.Parallel()
	now, advance := clockFn(time.Now())
	r := NewPeerRegistry(withClock(now))
	defer func() { _ = r.Close() }()

	_, _ = r.Register(RegisterRequest{Name: "fast", Endpoint: "x", HeartbeatTTLSec: 5})
	_, _ = r.Register(RegisterRequest{Name: "slow", Endpoint: "y", HeartbeatTTLSec: 60})

	advance(10 * time.Second) // "fast" lease (5s) has expired; "slow" still live
	pruned := r.Prune()
	if pruned != 1 {
		t.Errorf("Prune count = %d, want 1", pruned)
	}
	if r.Len() != 1 {
		t.Errorf("after prune Len = %d, want 1 (slow remains)", r.Len())
	}
}

func TestPeerRegistry_ListSortedAndFiltered(t *testing.T) {
	t.Parallel()
	r := NewPeerRegistry()
	defer func() { _ = r.Close() }()

	_, _ = r.Register(RegisterRequest{Name: "z", Endpoint: "x", Labels: map[string]string{"role": "monitor"}})
	_, _ = r.Register(RegisterRequest{Name: "a", Endpoint: "x", Labels: map[string]string{"role": "monitor"}})
	_, _ = r.Register(RegisterRequest{Name: "m", Endpoint: "x", Labels: map[string]string{"role": "supervisor"}})

	all := r.List(nil)
	if len(all) != 3 || all[0].Name != "a" || all[1].Name != "m" || all[2].Name != "z" {
		t.Errorf("List should be sorted by name, got %v", peerNames(all))
	}
	monitors := r.List(map[string]string{"role": "monitor"})
	if len(monitors) != 2 || monitors[0].Name != "a" || monitors[1].Name != "z" {
		t.Errorf("filter by role=monitor: got %v", peerNames(monitors))
	}
}

func peerNames(ps []*Peer) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name
	}
	return out
}

func TestPeerRegistry_ListDefensiveCopy(t *testing.T) {
	t.Parallel()
	r := NewPeerRegistry()
	defer func() { _ = r.Close() }()
	_, _ = r.Register(RegisterRequest{Name: "n", Endpoint: "x", Labels: map[string]string{"a": "b"}})
	ps := r.List(nil)
	// Mutate the returned labels map. Should not affect the registry.
	ps[0].Labels["a"] = "tampered"
	ps2 := r.List(nil)
	if ps2[0].Labels["a"] != "b" {
		t.Errorf("registry leaked internal Labels map: got %q after caller mutation", ps2[0].Labels["a"])
	}
}

func TestIntegration_PeersHTTP(t *testing.T) {
	t.Parallel()
	sessionReg := NewSessionRegistry()
	peerReg := NewPeerRegistry()
	defer func() { _ = peerReg.Close() }()

	srv, err := NewServer(Options{
		Registry: sessionReg, PeerRegistry: peerReg, Addr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	defer func() {
		_ = srv.Close()
		<-errCh
	}()
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	base := "http://" + srv.Addr()

	// POST /peers — register
	regBody, _ := json.Marshal(RegisterRequest{
		Name: "monitor-a", Endpoint: "https://10.0.0.1:7777",
		Labels: map[string]string{"role": "monitor"}, HeartbeatTTLSec: 60,
	})
	resp, err := http.Post(base+"/peers", "application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatalf("POST /peers: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("POST /peers status %d: %s", resp.StatusCode, respBody)
	}
	var registered Peer
	if err := json.NewDecoder(resp.Body).Decode(&registered); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode register response: %v", err)
	}
	_ = resp.Body.Close()
	if registered.RegistrationID == "" {
		t.Fatal("RegistrationID empty")
	}

	// GET /peers — list
	resp, err = http.Get(base + "/peers")
	if err != nil {
		t.Fatalf("GET /peers: %v", err)
	}
	var listOut struct {
		Peers []Peer `json:"peers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listOut); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode list: %v", err)
	}
	_ = resp.Body.Close()
	if len(listOut.Peers) != 1 || listOut.Peers[0].Name != "monitor-a" {
		t.Errorf("GET /peers content = %+v", listOut.Peers)
	}

	// GET /peers?label=role=monitor — filter match
	resp, err = http.Get(base + "/peers?label=role=monitor")
	if err != nil {
		t.Fatalf("GET /peers filter: %v", err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&listOut); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode filter list: %v", err)
	}
	_ = resp.Body.Close()
	if len(listOut.Peers) != 1 {
		t.Errorf("filter should match 1, got %d", len(listOut.Peers))
	}

	// GET /peers?label=role=other — filter mismatch
	resp, err = http.Get(base + "/peers?label=role=other")
	if err != nil {
		t.Fatalf("GET /peers filter mismatch: %v", err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&listOut); err != nil {
		_ = resp.Body.Close()
		t.Fatalf("decode mismatch: %v", err)
	}
	_ = resp.Body.Close()
	if len(listOut.Peers) != 0 {
		t.Errorf("filter mismatch should match 0, got %d", len(listOut.Peers))
	}

	// POST /peers/<id>/heartbeat
	resp, err = http.Post(base+"/peers/"+registered.RegistrationID+"/heartbeat",
		"application/json", nil)
	if err != nil {
		t.Fatalf("POST heartbeat: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("heartbeat status %d: %s", resp.StatusCode, respBody)
	}
	_ = resp.Body.Close()

	// POST /peers/<bogus>/heartbeat → 404
	resp, err = http.Post(base+"/peers/bogus/heartbeat", "application/json", nil)
	if err != nil {
		t.Fatalf("POST heartbeat bogus: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Errorf("heartbeat bogus status %d, want 404. body: %s", resp.StatusCode, respBody)
	}
	_ = resp.Body.Close()

	// DELETE /peers/<id> → 204; subsequent list is empty.
	req, _ := http.NewRequest(http.MethodDelete, base+"/peers/"+registered.RegistrationID, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE peer: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("DELETE status %d: %s", resp.StatusCode, respBody)
	}
	_ = resp.Body.Close()
	if peerReg.Len() != 0 {
		t.Errorf("after DELETE Len = %d, want 0", peerReg.Len())
	}
}

func TestIntegration_PeerEndpointsDisabledWhenRegistryNil(t *testing.T) {
	t.Parallel()
	sessionReg := NewSessionRegistry()
	srv, err := NewServer(Options{Registry: sessionReg, Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	defer func() {
		_ = srv.Close()
		<-errCh
	}()
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	base := "http://" + srv.Addr()

	// Without a PeerRegistry, /peers should 404 from the mux.
	resp, err := http.Get(base + "/peers")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /peers without PeerRegistry: status %d, want 404", resp.StatusCode)
	}
}

func TestPeerClient_RegisterAndHeartbeatRoundTrip(t *testing.T) {
	t.Parallel()
	sessionReg := NewSessionRegistry()
	peerReg := NewPeerRegistry()
	defer func() { _ = peerReg.Close() }()

	srv, err := NewServer(Options{
		Registry: sessionReg, PeerRegistry: peerReg, Addr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	defer func() {
		_ = srv.Close()
		<-errCh
	}()
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	hub := "http://" + srv.Addr()

	client := NewPeerClient(hub)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	stop, err := client.RegisterAndHeartbeat(ctx, RegisterRequest{
		Name: "client-peer", Endpoint: "https://10.0.0.5:7777",
		Labels: map[string]string{"src": "test"}, HeartbeatTTLSec: 3,
	})
	if err != nil {
		t.Fatalf("RegisterAndHeartbeat: %v", err)
	}

	// Wait for at least one heartbeat to land (cadence = ttl/3 = 1s).
	time.Sleep(1500 * time.Millisecond)
	peers := peerReg.List(nil)
	if len(peers) != 1 {
		t.Fatalf("hub Len = %d, want 1", len(peers))
	}
	if peers[0].LastHeartbeat.Equal(peers[0].RegisteredAt) {
		t.Errorf("LastHeartbeat never advanced beyond RegisteredAt — heartbeat goroutine isn't firing")
	}

	stop()
	// Give the deregister a moment to round-trip.
	time.Sleep(100 * time.Millisecond)
	if peerReg.Len() != 0 {
		t.Errorf("after stop, hub Len = %d, want 0 (Deregister)", peerReg.Len())
	}
}
