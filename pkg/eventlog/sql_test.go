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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
)

func dsnOf(t *testing.T, d interface{}) string {
	t.Helper()
	dd, ok := d.(*sqlite.Dialector)
	if !ok {
		t.Fatalf("dialector is %T, want *sqlite.Dialector", d)
	}
	return dd.DSN
}

func TestInjectSQLiteDSNParam(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, key, val, want string
	}{
		{"no query", "file.db", "_txlock", "immediate", "file.db?_txlock=immediate"},
		{
			"existing query", "file.db?_pragma=busy_timeout(5000)", "_txlock", "immediate",
			"file.db?_pragma=busy_timeout(5000)&_txlock=immediate",
		},
		{"already set is left alone", "file.db?_txlock=deferred", "_txlock", "immediate", "file.db?_txlock=deferred"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := sqlite.Open(c.in)
			injectSQLiteDSNParam(d, c.key, c.val)
			if got := dsnOf(t, d); got != c.want {
				t.Fatalf("DSN = %q, want %q", got, c.want)
			}
		})
	}
}

// TestOpen_InjectsImmediateTxlock pins the DSN Open hands the driver:
// both the busy_timeout pragma and _txlock=immediate must be present so
// every read-write transaction begins with BEGIN IMMEDIATE.
func TestOpen_InjectsImmediateTxlock(t *testing.T) {
	t.Parallel()
	d := sqlite.Open(filepath.Join(t.TempDir(), "eventlog.db"))
	h, err := Open(context.Background(), d)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = h.Close() }()

	dsn := dsnOf(t, d)
	if !strings.Contains(dsn, "_txlock=immediate") {
		t.Errorf("DSN %q missing _txlock=immediate", dsn)
	}
	if !strings.Contains(dsn, "_pragma=busy_timeout(5000)") {
		t.Errorf("DSN %q missing busy_timeout pragma", dsn)
	}
}

// TestOpen_AppendEventWaitsForHeldWriteLock reproduces the #575 race:
// an unrelated connection (standing in for the boot-scan's overlay-pool
// lock/bootlog writes) holds the SQLite write lock while a continuation
// turn's AppendEvent — a read-then-write transaction on the ADK pool —
// tries to commit.
//
// Under the default deferred BEGIN, ADK's transaction takes a read
// snapshot, then the first write attempts a snapshot→write upgrade,
// which SQLite refuses with an *immediate* SQLITE_BUSY (busy_timeout
// never retries an upgrade). With _txlock=immediate the transaction
// begins with BEGIN IMMEDIATE and instead waits on busy_timeout for the
// lock. Releasing the holder well within busy_timeout must let the
// AppendEvent succeed rather than fail.
//
// This test fails on pre-fix code (AppendEvent returns "database is
// locked") and passes once Open injects _txlock=immediate.
func TestOpen_AppendEventWaitsForHeldWriteLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h, cleanup := openTestHandle(t)
	defer cleanup()

	sess := mustCreateSession(t, h, "app", "user", "sess")

	// A scratch table on the same database file; writing to any table
	// takes the single database-wide write lock.
	if err := h.DB.WithContext(ctx).Exec("CREATE TABLE lockhold (id INTEGER)").Error; err != nil {
		t.Fatalf("create scratch table: %v", err)
	}

	sqlDB, err := h.DB.DB()
	if err != nil {
		t.Fatalf("underlying *sql.DB: %v", err)
	}

	lockAcquired := make(chan struct{})
	release := make(chan struct{})
	holdErr := make(chan error, 1)
	go func() {
		conn, err := sqlDB.Conn(ctx)
		if err != nil {
			holdErr <- err
			close(lockAcquired)
			return
		}
		defer func() { _ = conn.Close() }()
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			holdErr <- err
			close(lockAcquired)
			return
		}
		// Grab the write lock unconditionally with a real write.
		if _, err := tx.ExecContext(ctx, "INSERT INTO lockhold (id) VALUES (1)"); err != nil {
			_ = tx.Rollback()
			holdErr <- err
			close(lockAcquired)
			return
		}
		close(lockAcquired)
		<-release
		holdErr <- tx.Rollback()
	}()

	<-lockAcquired
	select {
	case err := <-holdErr:
		t.Fatalf("lock holder failed before AppendEvent: %v", err)
	default:
	}

	appendErr := make(chan error, 1)
	go func() {
		appendErr <- h.Service.AppendEvent(ctx, sess, makeEvent("ev-contended", "assistant", "", "resume"))
	}()

	// Let AppendEvent reach its BEGIN IMMEDIATE and block on the lock,
	// then release it — comfortably inside the 5s busy_timeout.
	time.Sleep(300 * time.Millisecond)
	close(release)

	select {
	case err := <-appendErr:
		if err != nil {
			t.Fatalf("AppendEvent under a held write lock failed instead of waiting: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AppendEvent did not complete after the write lock was released")
	}
	if err := <-holdErr; err != nil {
		t.Logf("lock holder rollback: %v", err)
	}
}
