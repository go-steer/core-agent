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
	"fmt"
	"sync"

	"google.golang.org/adk/session"
)

// service is the session.Service eventlog.Open returns. It wraps
// ADK's database.SessionService unchanged for Get/List and
// serializes all writes (Create / Delete / AppendEvent) behind a
// single write mutex so concurrent writers — typically a parent
// agent and its background subagents sharing the same eventlog —
// never race at the SQLite layer.
//
// Why the mutex (and not just SQLite's busy_timeout): SQLite's WAL
// mode allows concurrent readers + one writer, but if a second writer
// arrives mid-write the SQLite busy-wait is on a per-connection
// timer, and gorm opens its own internal connection pool for ADK
// that we can't tune. Serializing at the Go layer is durable across
// driver / dialector choices and makes the consistency model
// trivially obvious: writes happen in the order they're invoked.
//
// Reads (Get/List) intentionally do NOT take the mutex — SQLite WAL
// handles concurrent readers natively and serializing reads would
// defeat the purpose of having an eventlog for live-tail observers.
//
// Consistency model for AppendEvent: writes ADK's events table
// first (so the event has its assigned ID and storage row), then
// mirrors into the overlay so it picks up a monotonic seq. The
// overlay table has a unique index on event_id so a retry of the
// same event is a no-op rather than a duplicate. Eventual-consistency
// reconciliation across the two tables is out of scope for v1.
type service struct {
	inner  session.Service
	stream *gormStream

	writeMu sync.Mutex
}

// Get / List are pure pass-throughs — no mutex needed.

func (s *service) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	return s.inner.Get(ctx, req)
}

func (s *service) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	return s.inner.List(ctx, req)
}

// Create / Delete / AppendEvent serialize through writeMu so a
// parent agent and its background subagents don't race at the
// SQLite write lock.

func (s *service) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inner.Create(ctx, req)
}

func (s *service) Delete(ctx context.Context, req *session.DeleteRequest) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inner.Delete(ctx, req)
}

// AppendEvent writes the event through ADK first (so the events row
// exists), then mirrors it into the overlay so it picks up a
// monotonic seq. Errors from either layer surface to the caller.
func (s *service) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.inner.AppendEvent(ctx, sess, ev); err != nil {
		return err
	}
	if _, err := s.stream.Append(ctx, sess, ev); err != nil {
		return fmt.Errorf("eventlog: overlay write after ADK AppendEvent: %w", err)
	}
	return nil
}
