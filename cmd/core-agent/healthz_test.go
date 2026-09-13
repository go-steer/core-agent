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

package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"

	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/runner"
)

// TestDaemonHealthChecksNilHandle: no handle, no check. Registering a
// check that cannot fail would make /healthz report a subsystem it
// never looked at — the coarse-signal failure the endpoint exists to
// fix, dressed up as a green light.
func TestDaemonHealthChecksNilHandle(t *testing.T) {
	t.Parallel()
	if got := daemonHealthChecks(nil, nil); len(got) != 0 {
		t.Errorf("daemonHealthChecks(nil, nil) = %v, want none", got)
	}
}

// TestDaemonHealthChecksSessionDB wires the real check against a real
// event log and confirms it tracks the log's actual state in both
// directions. A check that only ever returns nil is worse than no
// check at all.
func TestDaemonHealthChecksSessionDB(t *testing.T) {
	t.Parallel()
	dsn := filepath.Join(t.TempDir(), "eventlog.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	checks := daemonHealthChecks(h, nil)
	if len(checks) != 1 || checks[0].Name != "session_db" {
		t.Fatalf("checks = %+v, want exactly one named session_db", checks)
	}
	if err := checks[0].Check(context.Background()); err != nil {
		t.Errorf("healthy log reports %v, want nil", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := checks[0].Check(context.Background()); err == nil {
		t.Error("closed log reports healthy — the check is not reading anything")
	}
}

// TestDaemonHealthChecksWakeLoops: ONE aggregate check, whatever the
// session count (#978). One check per session would grow the list
// without bound under a kubelet re-probing on a fixed period, and would
// put session ids in front of an unauthenticated caller. What the
// aggregate reports for a partial versus a total outage is
// runner.LoopHealthSet.Err's contract, tested in pkg/runner against the
// unexported writers only a wake loop gets to call.
func TestDaemonHealthChecksWakeLoops(t *testing.T) {
	t.Parallel()
	var loops runner.LoopHealthSet
	for _, sid := range []string{"alpha", "bravo", "charlie"} {
		loops.Register(sid)
	}
	checks := daemonHealthChecks(nil, &loops)
	if len(checks) != 1 || checks[0].Name != "wake_loops" {
		t.Fatalf("checks = %+v, want exactly one named wake_loops", checks)
	}
	if err := checks[0].Check(context.Background()); err != nil {
		t.Errorf("three idle sessions report %v, want nil", err)
	}
}

// A daemon wired with both subsystems registers both, in a stable
// order. /healthz names each check in its body, and a field that moves
// between probes is a field operators learn to ignore.
func TestDaemonHealthChecksRegistersBothSubsystems(t *testing.T) {
	t.Parallel()
	dsn := filepath.Join(t.TempDir(), "eventlog.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dsn))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	checks := daemonHealthChecks(h, &runner.LoopHealthSet{})
	var names []string
	for _, c := range checks {
		names = append(names, c.Name)
	}
	if len(names) != 2 || names[0] != "session_db" || names[1] != "wake_loops" {
		t.Errorf("check names = %v, want [session_db wake_loops]", names)
	}
}
