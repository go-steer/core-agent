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
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Default TTL bounds. See docs/peer-registration-design.md "TTL +
// heartbeat policy" for the rationale.
const (
	DefaultHeartbeatTTL = 60 * time.Second
	defaultMaxTTL       = 5 * time.Minute
	pruneTick           = 5 * time.Second
)

// Peer is one entry in the hub's PeerRegistry. Carries the
// registration identity, the peer's reachable endpoint, opaque labels
// for filtering, and the lease state for liveness tracking.
type Peer struct {
	RegistrationID string            `json:"registration_id"`
	Name           string            `json:"name"`
	Endpoint       string            `json:"endpoint"`
	Labels         map[string]string `json:"labels,omitempty"`
	RegisteredAt   time.Time         `json:"registered_at"`
	LastHeartbeat  time.Time         `json:"last_heartbeat"`
	LeaseExpiresAt time.Time         `json:"lease_expires_at"`
}

// RegisterRequest is the body the peer POSTs to /peers. Validated
// inside PeerRegistry.Register; bad values surface as errors the
// handler maps to 400.
type RegisterRequest struct {
	Name            string            `json:"name"`
	Endpoint        string            `json:"endpoint"`
	Labels          map[string]string `json:"labels,omitempty"`
	HeartbeatTTLSec int               `json:"heartbeat_ttl_sec,omitempty"`
}

// PeerRegistry is the hub-side state. Independent from SessionRegistry
// — sessions and peers are orthogonal: a peer's endpoint may itself
// host its own sessions.
type PeerRegistry struct {
	maxTTL time.Duration
	now    func() time.Time // injectable clock for tests

	mu     sync.RWMutex
	byID   map[string]*Peer
	byName map[string]*Peer

	pruneCancel context.CancelFunc
	pruneStop   chan struct{}
}

// PeerRegistryOption configures NewPeerRegistry.
type PeerRegistryOption func(*PeerRegistry)

// WithMaxTTL caps how long a peer-requested heartbeat TTL can run.
// Defaults to 5 minutes. Peers asking for longer get clamped.
func WithMaxTTL(d time.Duration) PeerRegistryOption {
	return func(r *PeerRegistry) { r.maxTTL = d }
}

// withClock is a test helper — injectable clock so prune behavior is
// deterministic without sleeping real wall-clock time.
func withClock(now func() time.Time) PeerRegistryOption {
	return func(r *PeerRegistry) { r.now = now }
}

// NewPeerRegistry returns an empty hub registry plus a started prune
// goroutine that drops expired leases every pruneTick. Call Close to
// stop the goroutine.
func NewPeerRegistry(opts ...PeerRegistryOption) *PeerRegistry {
	r := &PeerRegistry{
		maxTTL: defaultMaxTTL,
		now:    time.Now,
		byID:   make(map[string]*Peer),
		byName: make(map[string]*Peer),
	}
	for _, opt := range opts {
		opt(r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.pruneCancel = cancel
	r.pruneStop = make(chan struct{})
	go r.pruneLoop(ctx)
	return r
}

// Close stops the prune goroutine. Idempotent.
func (r *PeerRegistry) Close() error {
	r.mu.Lock()
	cancel := r.pruneCancel
	r.pruneCancel = nil
	r.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	<-r.pruneStop
	return nil
}

// ErrPeerNameRequired is returned when RegisterRequest.Name is empty.
var ErrPeerNameRequired = errors.New("attach: peer Name is required")

// ErrPeerEndpointRequired is returned when RegisterRequest.Endpoint is empty.
var ErrPeerEndpointRequired = errors.New("attach: peer Endpoint is required")

// ErrPeerNotFound is returned when Lookup / Heartbeat / Deregister
// can't find the registration ID.
var ErrPeerNotFound = errors.New("attach: peer registration not found")

// Register adds (or upserts on Name match) a peer. Returns the
// assigned RegistrationID + lease expiry. Name-based upsert avoids
// orphaned entries when a peer restarts.
func (r *PeerRegistry) Register(req RegisterRequest) (*Peer, error) {
	if req.Name == "" {
		return nil, ErrPeerNameRequired
	}
	if req.Endpoint == "" {
		return nil, ErrPeerEndpointRequired
	}
	ttl := time.Duration(req.HeartbeatTTLSec) * time.Second
	if ttl <= 0 {
		ttl = DefaultHeartbeatTTL
	}
	if ttl > r.maxTTL {
		ttl = r.maxTTL
	}
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()

	// Upsert on Name: if a peer with this name already exists, drop
	// the old registration ID and issue a fresh one. Keeps the
	// registry clean across peer restarts.
	if existing, ok := r.byName[req.Name]; ok {
		delete(r.byID, existing.RegistrationID)
	}

	id, err := newRegistrationID()
	if err != nil {
		return nil, err
	}
	p := &Peer{
		RegistrationID: id,
		Name:           req.Name,
		Endpoint:       req.Endpoint,
		Labels:         copyLabels(req.Labels),
		RegisteredAt:   now,
		LastHeartbeat:  now,
		LeaseExpiresAt: now.Add(ttl),
	}
	r.byID[id] = p
	r.byName[req.Name] = p
	return p, nil
}

// Heartbeat extends the lease on the named registration. Returns the
// new lease expiry. ErrPeerNotFound when id is unknown (peer should
// re-Register on this error).
func (r *PeerRegistry) Heartbeat(id string) (*Peer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrPeerNotFound, id)
	}
	now := r.now()
	ttl := p.LeaseExpiresAt.Sub(p.LastHeartbeat)
	p.LastHeartbeat = now
	p.LeaseExpiresAt = now.Add(ttl)
	return p, nil
}

// Deregister removes the peer by ID. No-op on unknown id — keeps
// graceful shutdown paths idempotent.
func (r *PeerRegistry) Deregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok {
		return
	}
	delete(r.byID, id)
	delete(r.byName, p.Name)
}

// List returns a sorted snapshot of every live peer. labelMatch, if
// non-empty, filters to peers whose Labels contain every k=v in the
// match map. Returns a defensive copy of each Peer so callers can't
// mutate registry state.
func (r *PeerRegistry) List(labelMatch map[string]string) []*Peer {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Peer, 0, len(r.byID))
	for _, p := range r.byID {
		if !matchLabels(p.Labels, labelMatch) {
			continue
		}
		cp := *p
		cp.Labels = copyLabels(p.Labels)
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

// Len returns the count of live peers (post-prune).
func (r *PeerRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byID)
}

// Prune drops every peer whose lease has expired. Returns the count
// pruned. Called from the background goroutine on a tick; exposed for
// tests + manual triggering.
func (r *PeerRegistry) Prune() int {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	pruned := 0
	for id, p := range r.byID {
		if p.LeaseExpiresAt.Before(now) {
			delete(r.byID, id)
			delete(r.byName, p.Name)
			pruned++
		}
	}
	return pruned
}

func (r *PeerRegistry) pruneLoop(ctx context.Context) {
	defer close(r.pruneStop)
	t := time.NewTicker(pruneTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Prune()
		}
	}
}

func newRegistrationID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("attach: generate registration id: %w", err)
	}
	return "reg-" + hex.EncodeToString(b[:]), nil
}

func copyLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func matchLabels(have, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
