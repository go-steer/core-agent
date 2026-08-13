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

// Cross-restart durability for the hub's peer registry (#595, Gap 1
// of docs/kube-agents-platform-fit.md).
//
// Without it, a hub restart drops every registration and each peer
// stays invisible until its next heartbeat fails and it re-registers
// — a 20-60s blackout during which "who's in the fleet?" answers
// wrong rather than answering slowly. Peers recover on their own, so
// this is an availability optimization, not a correctness one; what
// makes it worth writing carefully is the state it carries.
//
// The record below is deliberately NOT the Peer struct. Peer's JSON
// tags are the *wire* shape, and they redact: Owner is `json:"-"`
// because it's hub-side authorization state that discovery responses
// must not leak. Persisting Peer directly would therefore reload
// every registration as ownerless, which silently undoes the #384
// hardening — canManage collapses to `c.Admin || c.Identity == ""`,
// so in single-user mode (empty caller identity) every caller would
// see and be able to delete every peer's registration ID, and in
// multi-session mode the real owner would lose access to its own.
// A file format and a wire format want different things; they get
// different structs.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// peerStateFileMode is 0600 because the file contains registration
// IDs, and a registration ID is a capability: whoever holds it can
// deregister the peer. Same reasoning as a token file.
const peerStateFileMode fs.FileMode = 0o600

// peerRecord is the on-disk form of a Peer — the registry's own
// state, including the fields the wire shape withholds. Field names
// mirror Peer's JSON tags where they exist so the file stays readable
// next to a `GET /peers` response; `owner` is the deliberate addition.
type peerRecord struct {
	RegistrationID string            `json:"registration_id"`
	Name           string            `json:"name"`
	Endpoint       string            `json:"endpoint"`
	Labels         map[string]string `json:"labels,omitempty"`
	RegisteredAt   time.Time         `json:"registered_at"`
	LastHeartbeat  time.Time         `json:"last_heartbeat"`
	LeaseExpiresAt time.Time         `json:"lease_expires_at"`
	Owner          string            `json:"owner,omitempty"`
}

func recordOf(p *Peer) peerRecord {
	return peerRecord{
		RegistrationID: p.RegistrationID,
		Name:           p.Name,
		Endpoint:       p.Endpoint,
		Labels:         copyLabels(p.Labels),
		RegisteredAt:   p.RegisteredAt,
		LastHeartbeat:  p.LastHeartbeat,
		LeaseExpiresAt: p.LeaseExpiresAt,
		Owner:          p.Owner,
	}
}

func (rec peerRecord) peer() *Peer {
	return &Peer{
		RegistrationID: rec.RegistrationID,
		Name:           rec.Name,
		Endpoint:       rec.Endpoint,
		Labels:         copyLabels(rec.Labels),
		RegisteredAt:   rec.RegisteredAt,
		LastHeartbeat:  rec.LastHeartbeat,
		LeaseExpiresAt: rec.LeaseExpiresAt,
		Owner:          rec.Owner,
	}
}

// peerPersister owns the state file. Writes are whole-file snapshots
// via temp+rename rather than appends: the registry is small (a
// fleet, not a log), and rename is the only way to guarantee a reader
// — including our own next startup — never sees a half-written file.
//
// seq serializes snapshots. A snapshot is stamped under the
// registry's write lock, so stamps order the same way the mutations
// did; the persister then drops any snapshot older than the last one
// it wrote. Without that, two mutations racing to the file could land
// out of order and leave the file describing a state that never
// existed.
type peerPersister struct {
	path string

	mu      sync.Mutex
	written uint64
	// lastErr is the most recent write failure, so repeats of the same
	// failure stay quiet — see noteResult.
	lastErr string
}

// peerSnapshot is one pending write: the full registry contents plus the
// stamp that orders it against other pending writes.
type peerSnapshot struct {
	seq     uint64
	records []peerRecord
}

// write persists snap unless a newer snapshot already landed.
func (p *peerPersister) write(snap peerSnapshot) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if snap.seq <= p.written {
		return nil
	}
	if err := p.writeFile(snap.records); err != nil {
		return err
	}
	p.written = snap.seq
	return nil
}

func (p *peerPersister) writeFile(records []peerRecord) error {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	for i := range records {
		if err := enc.Encode(records[i]); err != nil {
			return fmt.Errorf("attach: encode peer record %q: %w", records[i].Name, err)
		}
	}

	// Alongside the target, not in the system temp dir: rename is only
	// atomic within a filesystem, and /tmp is routinely a different
	// one. The name is fixed rather than randomized (os.CreateTemp)
	// for two reasons — a stale temp file from a crash gets truncated
	// and reused instead of accumulating, and the error text stays
	// identical across retries, which is what lets noteResult
	// recognize a repeat of the same failure instead of logging a
	// fresh path every heartbeat. Writes are serialized by p.mu, so
	// there is never more than one in flight.
	tmpName := p.path + ".tmp"
	tmp, err := os.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, peerStateFileMode) //nolint:gosec // operator-supplied state path
	if err != nil {
		return fmt.Errorf("attach: create temp peer state %s: %w", tmpName, err)
	}
	defer func() {
		// No-op once the rename succeeded; cleans up on every failure
		// path below.
		_ = os.Remove(tmpName)
	}()

	// O_CREATE honors the mode only for a file that didn't exist. A
	// leftover temp file from a crash keeps whatever mode it had, so
	// set it explicitly — the mode is a security property here, not a
	// convenience.
	if err := tmp.Chmod(peerStateFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("attach: chmod temp peer state: %w", err)
	}
	if _, err := tmp.WriteString(buf.String()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("attach: write temp peer state: %w", err)
	}
	// fsync before rename: rename is atomic with respect to *readers*,
	// but on a crash an un-synced payload can land after the rename,
	// leaving a valid-looking empty file where the old contents were.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("attach: sync temp peer state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("attach: close temp peer state: %w", err)
	}
	if err := os.Rename(tmpName, p.path); err != nil {
		return fmt.Errorf("attach: rename peer state into place: %w", err)
	}
	return nil
}

// loadPeerState reads the state file. A missing file is not an error
// — that's a first start. A file that exists but can't be read IS an
// error: the operator asked for durability, and starting empty while
// claiming to be durable is the failure mode this whole endpoint
// exists to avoid.
//
// Individual malformed lines are a different case: they're skipped
// with a warning rather than failing the daemon. Temp+rename means we
// never write a partial file, so a bad line is external damage, and
// the peers it described will re-register within a heartbeat anyway
// — refusing to boot would trade a recoverable degradation for an
// unrecoverable one.
func loadPeerState(path string, now time.Time) ([]*Peer, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied state path, same trust level as the config file naming it
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("attach: open peer state %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var out []*Peer
	// Registration IDs must stay unique: the registry keys byID on
	// them, and a duplicate would leave byName pointing at a peer that
	// byID no longer holds — a split view that Len() and List()
	// disagree about. Only a hand-edited file can produce one, so
	// first-line-wins and say so.
	seenIDs := make(map[string]bool)
	sc := bufio.NewScanner(f)
	// Endpoints and labels are small, but a single pathological line
	// shouldn't abort the scan with a bare "token too long".
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var rec peerRecord
		if err := json.Unmarshal([]byte(text), &rec); err != nil {
			log.Printf("attach: peer state %s line %d: skipping unparseable record: %v", path, line, err)
			continue
		}
		// Re-validate on the way in. The file is operator-writable and
		// survives across versions, so it is untrusted input in
		// exactly the way a request body is: a hand-edited endpoint
		// must not reach a TUI that will dial it with operator
		// credentials (#384).
		if rec.Name == "" || rec.RegistrationID == "" {
			log.Printf("attach: peer state %s line %d: skipping record with empty name or registration id", path, line)
			continue
		}
		if err := validatePeerEndpoint(rec.Endpoint); err != nil {
			log.Printf("attach: peer state %s line %d: skipping peer %q: %v", path, line, rec.Name, err)
			continue
		}
		if seenIDs[rec.RegistrationID] {
			log.Printf("attach: peer state %s line %d: skipping peer %q: duplicate registration id", path, line, rec.Name)
			continue
		}
		// An expired lease is stale by definition — the peer stopped
		// heartbeating before we came back. Dropping it here keeps the
		// restart path from resurrecting peers the prune loop would
		// delete seconds later, which would otherwise make a hub
		// briefly report a dead fleet member as live.
		if !rec.LeaseExpiresAt.After(now) {
			continue
		}
		seenIDs[rec.RegistrationID] = true
		out = append(out, rec.peer())
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("attach: read peer state %s: %w", path, err)
	}
	return out, nil
}

// snapshotLocked stamps and copies the current registry contents. The
// caller must hold r.mu (write lock) — the stamp has to be taken in
// the same critical section as the mutation it reflects, or two
// mutations could stamp in the opposite order to which they applied.
func (r *PeerRegistry) snapshotLocked() peerSnapshot {
	// Persistence is opt-in, and the overwhelmingly common hub has it
	// off; don't copy the registry for a write that will never happen.
	if r.persister == nil {
		return peerSnapshot{}
	}
	r.persistSeq++
	snap := peerSnapshot{seq: r.persistSeq, records: make([]peerRecord, 0, len(r.byID))}
	for _, p := range r.byID {
		snap.records = append(snap.records, recordOf(p))
	}
	return snap
}

// persist writes a snapshot taken by snapshotLocked. Call it AFTER
// releasing r.mu: the file write is slow relative to the critical
// section, and holding the registry lock across it would make every
// heartbeat wait on the disk.
//
// A write failure degrades durability but must not fail the
// registration that triggered it — the peer is live in memory and
// discovery keeps working. It is logged rather than swallowed:
// "durability configured but not happening" is precisely the kind of
// unenforced claim that needs to be visible.
func (r *PeerRegistry) persist(snap peerSnapshot) {
	if r.persister == nil {
		return
	}
	if msg := r.persister.noteResult(r.persister.write(snap)); msg != "" {
		log.Print(msg)
	}
}

// noteResult reports persistence failures on the edges rather than on
// every write, returning the line to log or "" for silence. A broken
// volume fails once per heartbeat, per peer, forever; a log line each
// time buries the transition that mattered under thousands of copies
// of itself. The first failure and the recovery are what's findable.
//
// It returns the message instead of logging it so the edge behavior
// is testable without hijacking the process-wide logger out from
// under every other parallel test.
func (p *peerPersister) noteResult(err error) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case err != nil && p.lastErr != err.Error():
		p.lastErr = err.Error()
		return fmt.Sprintf("attach: persist peer state: %v (registry stays live in memory; durability degraded until this clears)", err)
	case err == nil && p.lastErr != "":
		prev := p.lastErr
		p.lastErr = ""
		return fmt.Sprintf("attach: peer state writes recovered (previous failure: %s)", prev)
	}
	return ""
}
