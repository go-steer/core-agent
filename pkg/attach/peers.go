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
	"net/url"
	"sort"
	"sync"
	"time"
)

// Default TTL bounds. See docs/peer-registration-design.md "TTL +
// heartbeat policy" for the rationale.
const (
	defaultHeartbeatTTL = 60 * time.Second
	defaultMaxTTL       = 5 * time.Minute
	pruneTick           = 5 * time.Second
)

// Peer is one entry in the hub's PeerRegistry. Carries the
// registration identity, the peer's reachable endpoint, opaque labels
// for filtering, and the lease state for liveness tracking.
type Peer struct {
	// RegistrationID is the capability handle for heartbeat +
	// deregister. omitempty because GET /peers redacts it for
	// callers other than the registration's owner or an admin
	// (#384 — enumerate-then-delete hardening).
	RegistrationID string            `json:"registration_id,omitempty"`
	Name           string            `json:"name"`
	Endpoint       string            `json:"endpoint"`
	Labels         map[string]string `json:"labels,omitempty"`
	RegisteredAt   time.Time         `json:"registered_at"`
	LastHeartbeat  time.Time         `json:"last_heartbeat"`
	LeaseExpiresAt time.Time         `json:"lease_expires_at"`

	// Owner is the authenticated caller identity that registered
	// this peer (#384). Deregistration requires the same owner or an
	// admin. Never serialized — it's hub-side authorization state,
	// not discovery data.
	Owner string `json:"-"`
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

	// persister is nil unless the hub was built with a state file
	// (#595). persistSeq orders snapshots — see snapshotLocked.
	persister  *peerPersister
	persistSeq uint64

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

// NewPeerRegistryWithState is NewPeerRegistry backed by a durable
// state file (#595): registrations are snapshotted to path on every
// mutation and reloaded on startup, so a hub restart doesn't blank
// the fleet for a heartbeat interval.
//
// Returns an error when path exists but can't be read, or when the
// first snapshot can't be written — a missing file is a first start,
// not a failure. This is why the sketch in #595 (a WithPersistFile
// option) isn't what shipped: an option can't report that loading
// failed, and a hub that silently comes up empty while configured
// for durability is exactly the class of claimed-but-unenforced
// property this milestone is closing. Malformed individual lines are
// a softer failure and are skipped with a warning; see loadPeerState.
//
// The initial write is deliberate rather than lazy: it surfaces an
// unwritable directory at startup instead of at the first
// registration, when the only witness would be a log line.
func NewPeerRegistryWithState(path string, opts ...PeerRegistryOption) (*PeerRegistry, error) {
	if path == "" {
		return nil, fmt.Errorf("attach: peer state file path is required")
	}
	r := NewPeerRegistry(opts...)
	loaded, err := loadPeerState(path, r.now())
	if err != nil {
		_ = r.Close()
		return nil, err
	}
	r.mu.Lock()
	for _, p := range loaded {
		// Name collisions can only come from a hand-edited file;
		// last-line-wins matches the in-memory upsert-on-name rule.
		if existing, ok := r.byName[p.Name]; ok {
			delete(r.byID, existing.RegistrationID)
		}
		r.byID[p.RegistrationID] = p
		r.byName[p.Name] = p
	}
	r.persister = &peerPersister{path: path}
	snap := r.snapshotLocked()
	r.mu.Unlock()

	if err := r.persister.write(snap); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
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

// ErrPeerEndpointInvalid is returned when RegisterRequest.Endpoint is
// not an absolute http/https URL with a host (#384). Unvalidated
// endpoints let a hostile registrant publish javascript:/file:/
// relative junk that downstream TUIs would then dial with operator
// credentials.
var ErrPeerEndpointInvalid = errors.New("attach: peer Endpoint must be an absolute http or https URL with a host")

// validatePeerEndpoint enforces the #384 endpoint policy: absolute
// URL, http or https scheme, non-empty host. Everything else —
// javascript:, ftp:, scheme-less, relative paths, host-less URLs —
// is rejected.
func validatePeerEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%w: %q: %v", ErrPeerEndpointInvalid, endpoint, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: %q", ErrPeerEndpointInvalid, endpoint)
	}
	return nil
}

// ErrPeerNotFound is returned when Lookup / Heartbeat / Deregister
// can't find the registration ID.
var ErrPeerNotFound = errors.New("attach: peer registration not found")

// Register adds (or upserts on Name match) a peer with no recorded
// owner. Prefer RegisterOwned where a caller identity is available —
// ownerless registrations can only be deregistered by an admin or a
// caller with the same (empty) identity.
func (r *PeerRegistry) Register(req RegisterRequest) (*Peer, error) {
	return r.RegisterOwned(req, "")
}

// RegisterOwned adds (or upserts on Name match) a peer, recording
// owner (the authenticated caller identity) on the registration for
// the #384 deregistration/visibility checks. Returns the assigned
// RegistrationID + lease expiry. Name-based upsert avoids orphaned
// entries when a peer restarts.
func (r *PeerRegistry) RegisterOwned(req RegisterRequest, owner string) (*Peer, error) {
	if req.Name == "" {
		return nil, ErrPeerNameRequired
	}
	if req.Endpoint == "" {
		return nil, ErrPeerEndpointRequired
	}
	if err := validatePeerEndpoint(req.Endpoint); err != nil {
		return nil, err
	}
	ttl := time.Duration(req.HeartbeatTTLSec) * time.Second
	if ttl <= 0 {
		ttl = defaultHeartbeatTTL
	}
	if ttl > r.maxTTL {
		ttl = r.maxTTL
	}
	now := r.now()

	r.mu.Lock()

	// Upsert on Name: if a peer with this name already exists, drop
	// the old registration ID and issue a fresh one. Keeps the
	// registry clean across peer restarts.
	if existing, ok := r.byName[req.Name]; ok {
		delete(r.byID, existing.RegistrationID)
	}

	id, err := newRegistrationID()
	if err != nil {
		r.mu.Unlock()
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
		Owner:          owner,
	}
	r.byID[id] = p
	r.byName[req.Name] = p
	snap := r.snapshotLocked()
	r.mu.Unlock()

	r.persist(snap)
	return p, nil
}

// Heartbeat extends the lease on the named registration. Returns the
// new lease expiry. ErrPeerNotFound when id is unknown (peer should
// re-Register on this error).
func (r *PeerRegistry) Heartbeat(id string) (*Peer, error) {
	r.mu.Lock()
	p, ok := r.byID[id]
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrPeerNotFound, id)
	}
	now := r.now()
	ttl := p.LeaseExpiresAt.Sub(p.LastHeartbeat)
	p.LastHeartbeat = now
	p.LeaseExpiresAt = now.Add(ttl)
	// Heartbeats are persisted too, not just registrations: the
	// reload drops leases that have already expired, so a snapshot
	// with stale expiry times would discard exactly the peers that
	// have been alive the longest.
	snap := r.snapshotLocked()
	r.mu.Unlock()

	r.persist(snap)
	return p, nil
}

// Lookup returns a defensive copy of the peer registered under id,
// or (nil, false) when unknown. Used by the HTTP handlers for the
// #384 owner/admin deregistration check.
func (r *PeerRegistry) Lookup(id string) (*Peer, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.byID[id]
	if !ok {
		return nil, false
	}
	cp := *p
	cp.Labels = copyLabels(p.Labels)
	return &cp, true
}

// Deregister removes the peer by ID. No-op on unknown id — keeps
// graceful shutdown paths idempotent.
func (r *PeerRegistry) Deregister(id string) {
	r.mu.Lock()
	p, ok := r.byID[id]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(r.byID, id)
	delete(r.byName, p.Name)
	snap := r.snapshotLocked()
	r.mu.Unlock()

	r.persist(snap)
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
	pruned := 0
	for id, p := range r.byID {
		if p.LeaseExpiresAt.Before(now) {
			delete(r.byID, id)
			delete(r.byName, p.Name)
			pruned++
		}
	}
	// Only snapshot when something changed — the prune loop ticks
	// every 5s for the life of the daemon, and an idle hub shouldn't
	// rewrite its state file 17,000 times a day.
	var snap peerSnapshot
	if pruned > 0 {
		snap = r.snapshotLocked()
	}
	r.mu.Unlock()

	if pruned > 0 {
		r.persist(snap)
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
