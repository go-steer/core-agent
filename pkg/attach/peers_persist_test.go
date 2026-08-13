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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/auth"
)

// statePath returns a path inside a fresh temp dir. The dir, not just
// the file, is per-test: the persister writes its temp file alongside
// the target, so a shared dir would let parallel tests see each
// other's in-flight writes.
func statePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "peers.jsonl")
}

func mustRegisterOwned(t *testing.T, r *PeerRegistry, name, endpoint, owner string, ttl int) *Peer {
	t.Helper()
	p, err := r.RegisterOwned(RegisterRequest{
		Name: name, Endpoint: endpoint, HeartbeatTTLSec: ttl,
		Labels: map[string]string{"cluster": name},
	}, owner)
	if err != nil {
		t.Fatalf("RegisterOwned(%s): %v", name, err)
	}
	return p
}

func mustReopen(t *testing.T, path string, opts ...PeerRegistryOption) *PeerRegistry {
	t.Helper()
	r, err := NewPeerRegistryWithState(path, opts...)
	if err != nil {
		t.Fatalf("NewPeerRegistryWithState: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// TestPeerState_SurvivesRestart is the whole point of #595: a hub
// that comes back up already knows its fleet instead of waiting a
// heartbeat interval for everyone to re-register.
func TestPeerState_SurvivesRestart(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	now, _ := clockFn(time.Now())

	first := mustReopen(t, path, withClock(now))
	a := mustRegisterOwned(t, first, "cluster-a", "https://10.0.0.1:7777", "ops@example.com", 120)
	mustRegisterOwned(t, first, "cluster-b", "https://10.0.0.2:7777", "ops@example.com", 120)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := mustReopen(t, path, withClock(now))
	if got := second.Len(); got != 2 {
		t.Fatalf("peers after restart = %d, want 2", got)
	}
	reloaded, ok := second.Lookup(a.RegistrationID)
	if !ok {
		t.Fatalf("registration id %s did not survive the restart", a.RegistrationID)
	}
	if reloaded.Endpoint != a.Endpoint || reloaded.Labels["cluster"] != "cluster-a" {
		t.Errorf("reloaded peer = %+v, want endpoint/labels preserved", reloaded)
	}
	if !reloaded.LeaseExpiresAt.Equal(a.LeaseExpiresAt) {
		t.Errorf("lease expiry = %v, want %v", reloaded.LeaseExpiresAt, a.LeaseExpiresAt)
	}
}

// TestPeerState_PreservesOwner is the security-relevant one, and the
// reason the on-disk record is a separate struct from Peer.
//
// Peer.Owner is `json:"-"` — it's hub-side authorization state that
// discovery responses deliberately withhold. Persist Peer directly
// (the obvious implementation) and every registration reloads
// ownerless, so canManage collapses to `c.Admin || c.Identity == ""`:
// the real owner loses control of its own registration, and in
// single-user mode (empty identity) every caller gains it. A restart
// would silently undo the #384 enumerate-then-delete hardening.
//
// Fails on pre-fix code: swap peerRecord for Peer in recordOf and
// this reports the owner as unable to manage its own peer.
func TestPeerState_PreservesOwner(t *testing.T) {
	t.Parallel()
	path := statePath(t)

	first := mustReopen(t, path)
	p := mustRegisterOwned(t, first, "cluster-a", "https://10.0.0.1:7777", "ops@example.com", 120)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second := mustReopen(t, path)
	reloaded, ok := second.Lookup(p.RegistrationID)
	if !ok {
		t.Fatalf("peer did not survive restart")
	}
	if reloaded.Owner != "ops@example.com" {
		t.Fatalf("Owner after restart = %q, want ops@example.com", reloaded.Owner)
	}
	if !canManage(auth.Caller{Identity: "ops@example.com"}, reloaded) {
		t.Error("owner cannot manage its own registration after a restart")
	}
	if canManage(auth.Caller{Identity: "intruder@example.com"}, reloaded) {
		t.Error("a non-owner can manage the registration after a restart")
	}
	// The single-user posture is where a dropped owner is worst: an
	// empty identity would match an empty Owner and hand every caller
	// the deregistration capability.
	if canManage(auth.Caller{Identity: ""}, reloaded) {
		t.Error("an anonymous caller can manage the registration after a restart")
	}
}

// TestPeerState_HeartbeatsArePersisted guards the reason heartbeats
// write to disk at all. Reload drops expired leases, so a file that
// only recorded the original registration would discard exactly the
// peers that have been alive longest.
//
// Fails on pre-fix code: drop the snapshot from Heartbeat and the
// reloaded registry is empty.
func TestPeerState_HeartbeatsArePersisted(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	now, advance := clockFn(time.Now())

	first := mustReopen(t, path, withClock(now))
	p := mustRegisterOwned(t, first, "cluster-a", "https://10.0.0.1:7777", "ops@example.com", 60)
	advance(45 * time.Second)
	if _, err := first.Heartbeat(p.RegistrationID); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// t+70s: past the original lease, inside the extended one.
	advance(25 * time.Second)
	second := mustReopen(t, path, withClock(now))
	if got := second.Len(); got != 1 {
		t.Fatalf("peers after restart = %d, want 1 (the heartbeat-extended lease)", got)
	}
}

// TestPeerState_ExpiredLeasesAreNotResurrected: a peer that stopped
// heartbeating before the hub came back is stale by definition, and
// reporting it as live — even for the few seconds until the prune
// loop ticks — is a worse answer than not reporting it.
func TestPeerState_ExpiredLeasesAreNotResurrected(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	now, advance := clockFn(time.Now())

	first := mustReopen(t, path, withClock(now))
	mustRegisterOwned(t, first, "gone", "https://10.0.0.1:7777", "ops@example.com", 30)
	mustRegisterOwned(t, first, "alive", "https://10.0.0.2:7777", "ops@example.com", 300)
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	advance(60 * time.Second)
	second := mustReopen(t, path, withClock(now))
	peers := second.List(nil)
	if len(peers) != 1 || peers[0].Name != "alive" {
		t.Fatalf("peers after restart = %v, want only [alive]", peerNames(peers))
	}
	// The expired entry should also be gone from the file, not just
	// filtered on read — otherwise it lingers forever.
	if body := readState(t, path); strings.Contains(body, `"gone"`) {
		t.Errorf("expired peer still in state file:\n%s", body)
	}
}

// TestPeerState_DeregisterAndPrunePersist: removals have to reach the
// file too, or a restart resurrects a peer the operator deleted.
func TestPeerState_DeregisterAndPrunePersist(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	now, advance := clockFn(time.Now())

	first := mustReopen(t, path, withClock(now))
	p := mustRegisterOwned(t, first, "deleted", "https://10.0.0.1:7777", "ops@example.com", 300)
	mustRegisterOwned(t, first, "pruned", "https://10.0.0.2:7777", "ops@example.com", 30)
	mustRegisterOwned(t, first, "kept", "https://10.0.0.3:7777", "ops@example.com", 300)
	first.Deregister(p.RegistrationID)
	advance(60 * time.Second)
	if got := first.Prune(); got != 1 {
		t.Fatalf("Prune removed %d, want 1", got)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	body := readState(t, path)
	if strings.Contains(body, `"deleted"`) || strings.Contains(body, `"pruned"`) {
		t.Errorf("removed peers still in state file:\n%s", body)
	}
	if !strings.Contains(body, `"kept"`) {
		t.Errorf("surviving peer missing from state file:\n%s", body)
	}
}

// TestPeerState_FileIsOwnerOnly: the file holds registration IDs, and
// a registration ID is the capability to deregister the peer. Group-
// or world-readable state on a shared volume hands that out.
func TestPeerState_FileIsOwnerOnly(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	r := mustReopen(t, path)
	mustRegisterOwned(t, r, "cluster-a", "https://10.0.0.1:7777", "ops@example.com", 120)

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := fi.Mode().Perm(); got != peerStateFileMode {
		t.Errorf("state file mode = %v, want %v (it contains deregistration capabilities)", got, peerStateFileMode)
	}
}

// TestPeerState_HandEditedRecordsAreRevalidated: the file is operator-
// writable and outlives any single binary, so it is untrusted input
// in the same way a request body is. A javascript:/file: endpoint
// must not reach a TUI that will dial it with operator credentials
// (#384) just because it arrived via disk instead of via POST.
func TestPeerState_HandEditedRecordsAreRevalidated(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	future := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	writeState(t, path, strings.Join([]string{
		`{"registration_id":"reg-1","name":"good","endpoint":"https://10.0.0.1:7777","lease_expires_at":"` + future + `"}`,
		`{"registration_id":"reg-2","name":"hostile","endpoint":"javascript:alert(1)","lease_expires_at":"` + future + `"}`,
		`{"registration_id":"reg-3","name":"relative","endpoint":"/peers","lease_expires_at":"` + future + `"}`,
		`{"registration_id":"","name":"anonymous","endpoint":"https://10.0.0.4:7777","lease_expires_at":"` + future + `"}`,
	}, "\n"))

	r := mustReopen(t, path)
	got := peerNames(r.List(nil))
	if len(got) != 1 || got[0] != "good" {
		t.Errorf("loaded peers = %v, want only [good]", got)
	}
}

// TestPeerState_MalformedLineIsSkippedNotFatal: temp+rename means we
// never write a partial file, so a bad line is external damage. The
// peers it described re-register within a heartbeat; refusing to boot
// would turn a recoverable degradation into an outage.
func TestPeerState_MalformedLineIsSkippedNotFatal(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	future := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	writeState(t, path, strings.Join([]string{
		`{"registration_id":"reg-1","name":"before","endpoint":"https://10.0.0.1:7777","lease_expires_at":"` + future + `"}`,
		`{"registration_id":"reg-2","name":"trunc`,
		``,
		`not json at all`,
		`{"registration_id":"reg-3","name":"after","endpoint":"https://10.0.0.3:7777","lease_expires_at":"` + future + `"}`,
	}, "\n"))

	r := mustReopen(t, path)
	got := peerNames(r.List(nil))
	if len(got) != 2 || got[0] != "after" || got[1] != "before" {
		t.Errorf("loaded peers = %v, want [after before] — good records around a bad line must survive", got)
	}
}

// TestPeerState_DuplicateRegistrationIDsAreRejected: the registry
// keys byID on the registration ID, so loading two records that share
// one leaves byName pointing at a peer byID no longer holds — a split
// view where Len() and List() disagree. Only a hand-edited file can
// produce it; first-line-wins.
func TestPeerState_DuplicateRegistrationIDsAreRejected(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	future := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	writeState(t, path, strings.Join([]string{
		`{"registration_id":"reg-dup","name":"first","endpoint":"https://10.0.0.1:7777","lease_expires_at":"` + future + `"}`,
		`{"registration_id":"reg-dup","name":"second","endpoint":"https://10.0.0.2:7777","lease_expires_at":"` + future + `"}`,
	}, "\n"))

	r := mustReopen(t, path)
	if got, want := r.Len(), 1; got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
	if got := peerNames(r.List(nil)); len(got) != 1 || got[0] != "first" {
		t.Errorf("loaded peers = %v, want [first]", got)
	}
}

// TestPeerState_RepeatedFailuresLogOnce: a broken volume fails once
// per heartbeat per peer, forever. Logging every failure buries the
// transition under thousands of copies of itself, so only the first
// failure and the recovery are reported.
func TestPeerState_RepeatedFailuresLogOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := &peerPersister{path: filepath.Join(dir, "missing-dir", "peers.jsonl")}

	var logged []string
	note := func() {
		seq := p.written + 1
		if msg := p.noteResult(p.write(peerSnapshot{seq: seq})); msg != "" {
			logged = append(logged, msg)
		}
	}
	note()
	note()
	note()
	// Same persister, now a writable path.
	p.path = filepath.Join(dir, "peers.jsonl")
	note()

	if len(logged) != 2 {
		t.Fatalf("logged %d lines, want 2 (one failure, one recovery):\n%v", len(logged), logged)
	}
	if !strings.Contains(logged[0], "durability degraded") {
		t.Errorf("first line = %q, want the failure notice", logged[0])
	}
	if !strings.Contains(logged[1], "writes recovered") {
		t.Errorf("second line = %q, want the recovery notice", logged[1])
	}
}

// TestPeerState_UnreadableFileIsFatal: the operator asked for
// durability. Coming up empty and pretending otherwise is the exact
// claimed-but-unenforced failure this milestone is closing, so an
// unreadable file fails the boot instead. A directory stands in for
// "can't be read" because it fails the same way for root, which CI
// often is; a chmod-000 file would not.
func TestPeerState_UnreadableFileIsFatal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := NewPeerRegistryWithState(dir); err == nil {
		t.Fatal("NewPeerRegistryWithState on an unreadable path returned no error")
	}
}

// TestPeerState_UnwritableDirIsFatalAtStartup: the first snapshot is
// written eagerly so a bad volume mount surfaces at boot, not at the
// first registration hours later where the only witness is a log line.
func TestPeerState_UnwritableDirIsFatalAtStartup(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "no-such-dir", "peers.jsonl")
	if _, err := NewPeerRegistryWithState(path); err == nil {
		t.Fatal("NewPeerRegistryWithState with a missing parent directory returned no error")
	}
}

func TestPeerState_EmptyPathIsError(t *testing.T) {
	t.Parallel()
	if _, err := NewPeerRegistryWithState(""); err == nil {
		t.Fatal("NewPeerRegistryWithState(\"\") returned no error")
	}
}

// TestPeerState_MissingFileIsAFirstStart: a hub's very first boot has
// no state file, and that is not a failure.
func TestPeerState_MissingFileIsAFirstStart(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	r := mustReopen(t, path)
	if r.Len() != 0 {
		t.Errorf("fresh registry has %d peers", r.Len())
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("state file not created on first start: %v", err)
	}
}

// TestPeerState_NoTempFilesLeftBehind: the temp+rename dance must not
// litter the volume, including on the paths where the write is
// skipped as stale.
func TestPeerState_NoTempFilesLeftBehind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "peers.jsonl")
	r := mustReopen(t, path)
	for i := range 5 {
		p := mustRegisterOwned(t, r, "peer-"+string(rune('a'+i)), "https://10.0.0.1:7777", "ops@example.com", 120)
		if _, err := r.Heartbeat(p.RegistrationID); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "peers.jsonl" {
			t.Errorf("leftover file in state dir: %s", e.Name())
		}
	}
}

// TestPeerState_StaleSnapshotNeverOverwritesNewer pins the ordering
// rule directly. Snapshots are stamped under the registry's write
// lock; the persister drops any stamp it has already passed. Without
// that, two concurrent mutations could land out of order and leave
// the file describing a state that never existed.
func TestPeerState_StaleSnapshotNeverOverwritesNewer(t *testing.T) {
	t.Parallel()
	path := statePath(t)
	p := &peerPersister{path: path}

	newer := peerSnapshot{seq: 7, records: []peerRecord{{
		RegistrationID: "reg-new", Name: "new",
		Endpoint: "https://10.0.0.2:7777", LeaseExpiresAt: time.Now().Add(time.Hour),
	}}}
	older := peerSnapshot{seq: 3, records: []peerRecord{{
		RegistrationID: "reg-old", Name: "old",
		Endpoint: "https://10.0.0.1:7777", LeaseExpiresAt: time.Now().Add(time.Hour),
	}}}
	if err := p.write(newer); err != nil {
		t.Fatalf("write newer: %v", err)
	}
	if err := p.write(older); err != nil {
		t.Fatalf("write older: %v", err)
	}
	if body := readState(t, path); !strings.Contains(body, `"new"`) || strings.Contains(body, `"old"`) {
		t.Errorf("stale snapshot overwrote the newer one:\n%s", body)
	}
}

// TestPeerState_InMemoryRegistryWritesNothing: persistence is opt-in,
// and the default hub must not touch the filesystem.
func TestPeerState_InMemoryRegistryWritesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := NewPeerRegistry()
	defer func() { _ = r.Close() }()
	mustRegisterOwned(t, r, "cluster-a", "https://10.0.0.1:7777", "ops@example.com", 120)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("in-memory registry wrote %d files", len(entries))
	}
}

func readState(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return string(b)
}

func writeState(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body+"\n"), peerStateFileMode); err != nil {
		t.Fatalf("write state: %v", err)
	}
}
