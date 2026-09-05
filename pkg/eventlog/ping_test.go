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

package eventlog

import (
	"context"
	"errors"
	"testing"
)

// TestPingEmptyLog: a daemon that has served no events yet is ready.
// If Ping used First rather than Scan it would return
// ErrRecordNotFound here, and every cold-booted pod would sit
// not-ready until its first event.
func TestPingEmptyLog(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	if err := h.Ping(context.Background()); err != nil {
		t.Fatalf("Ping on an empty log: %v, want nil", err)
	}
}

// TestPingWithRows: the ordinary steady state.
func TestPingWithRows(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	sess := mustCreateSession(t, h, "app", "user", "s1")
	if err := h.Service.AppendEvent(context.Background(), sess, makeEvent("p-1", "x", "", "hi")); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := h.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v, want nil", err)
	}
}

// TestPingAfterClose is the case a readiness probe races on every
// shutdown: the listener is still draining while the event log has
// already gone. It must report an error, not panic and take the
// process down on its way out.
func TestPingAfterClose(t *testing.T) {
	t.Parallel()
	h, _ := openTestHandle(t)
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := h.Ping(context.Background())
	if err == nil {
		t.Fatal("Ping after Close returned nil — a closed log is not ready")
	}
	// The DB pool is closed but h.DB is still non-nil (Close nils only
	// the unexported alias), so this goes through the query path and
	// must surface the driver's error rather than crash.
	t.Logf("Ping after Close: %v", err)
}

// TestPingNilHandle: daemonHealthChecks declines to register a check
// for a nil handle, but the method must not panic if anything else
// ever holds one.
func TestPingNilHandle(t *testing.T) {
	t.Parallel()
	var h *Handle
	if err := h.Ping(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("nil Handle Ping = %v, want ErrClosed", err)
	}
}

// TestPingHonoursContext: the probe's budget has to actually bound the
// query, or a wedged database parks the handler goroutine.
func TestPingHonoursContext(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.Ping(ctx); err == nil {
		t.Error("Ping with a canceled context returned nil — the context is not reaching the driver")
	}
}

// TestPingReadsTheTableNotThePool is the discrimination that justifies
// this being a query at all: after the underlying pool is closed, a
// bare pool ping and a real read diverge in whether they notice. If a
// future change swaps the query for db.Ping(), this stays green only
// if the swap kept the same failure sensitivity.
func TestPingReadsTheTableNotThePool(t *testing.T) {
	t.Parallel()
	h, cleanup := openTestHandle(t)
	defer cleanup()
	// Drop the table out from under the handle — the closest thing to
	// "the volume went away" we can stage in-process. A connection
	// ping is unaffected by this; a read is not.
	if err := h.DB.Exec("DROP TABLE agent_eventlog").Error; err != nil {
		t.Fatalf("DROP TABLE: %v", err)
	}
	if err := h.Ping(context.Background()); err == nil {
		t.Fatal("Ping returned nil against a missing table — it is not reading anything")
	}
}
