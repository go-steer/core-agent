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
)

// TestDaemonHealthChecksNilHandle: no handle, no check. Registering a
// check that cannot fail would make /healthz report a subsystem it
// never looked at — the coarse-signal failure the endpoint exists to
// fix, dressed up as a green light.
func TestDaemonHealthChecksNilHandle(t *testing.T) {
	t.Parallel()
	if got := daemonHealthChecks(nil); len(got) != 0 {
		t.Errorf("daemonHealthChecks(nil) = %v, want none", got)
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
	checks := daemonHealthChecks(h)
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
