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

	"google.golang.org/adk/session"
)

// service is the session.Service eventlog.Open returns. It wraps
// ADK's database.SessionService unchanged for Create/Get/List/Delete
// and intercepts AppendEvent to mirror the event into our overlay
// table so it picks up a seq number.
//
// Consistency model: AppendEvent writes to ADK's events table first
// (so the event has its assigned ID and storage row), then to our
// overlay. If the overlay write fails after ADK's succeeded the
// caller sees an error. The overlay table has a unique index on
// event_id so a retry of the same event is a no-op rather than a
// duplicate. Eventual-consistency reconciliation across the two
// tables is out of scope for v1.
type service struct {
	inner  session.Service
	stream *gormStream
}

// Create / Get / List / Delete are pure pass-throughs — ADK owns
// the schema and lifecycle for these.

func (s *service) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	return s.inner.Create(ctx, req)
}

func (s *service) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	return s.inner.Get(ctx, req)
}

func (s *service) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	return s.inner.List(ctx, req)
}

func (s *service) Delete(ctx context.Context, req *session.DeleteRequest) error {
	return s.inner.Delete(ctx, req)
}

// AppendEvent writes the event through ADK first (so the events row
// exists), then mirrors it into the overlay so it picks up a
// monotonic seq. Errors from either layer surface to the caller.
func (s *service) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if err := s.inner.AppendEvent(ctx, sess, ev); err != nil {
		return err
	}
	if _, err := s.stream.Append(ctx, sess, ev); err != nil {
		return fmt.Errorf("eventlog: overlay write after ADK AppendEvent: %w", err)
	}
	return nil
}
