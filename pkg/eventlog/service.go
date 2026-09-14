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
	"google.golang.org/genai"
)

// FinishReasonMetadataKey is the CustomMetadata key under which
// AppendEvent records a model turn's genai FinishReason before persist.
// ADK's storage row has no FinishReason column
// (createEventFromStorageEvent drops it), but it DOES round-trip
// CustomMetadata — so stamping the reason here is what lets a reloaded
// event distinguish a MAX_TOKENS truncation (resume-able) from a normal
// STOP completion. Read by pkg/agent's auto-continue classifier (#582);
// mirrors the CompactionMetadataKey piggy-back the classifier already
// consults.
const FinishReasonMetadataKey = "finish_reason"

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

// Delete removes the session from ADK and then drops the matching
// overlay rows. Deleting only ADK's rows would leave orphaned overlay
// rows whose loadEvent fails forever, poisoning every unfiltered
// Watch/Since consumer (see gormStream.deleteSession).
func (s *service) Delete(ctx context.Context, req *session.DeleteRequest) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.inner.Delete(ctx, req); err != nil {
		return err
	}
	return s.stream.deleteSession(ctx, req.AppName, req.UserID, req.SessionID)
}

// AppendEvent writes the event through ADK and mirrors it into the
// overlay, where it picks up a monotonic seq. Errors from either layer
// surface to the caller.
//
// The two rows go in one transaction where they can (#976): the stashed
// record below is picked up by a GORM after-create callback running
// inside ADK's own transaction, so a crash between the writes can no
// longer leave an event that the overlay — the index every Since and
// Watch consumer reads — does not have. See atomic.go.
//
// Two paths still write after ADK rather than inside it, and both are
// deliberate. When the callback could not be registered, the fallback
// is exactly the behaviour that shipped before this: not atomic, but no
// worse than it was. And when ADK writes nothing at all, neither do we.
func (s *service) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	stampFinishReason(ev)

	var pending *overlayPending
	if s.stream != nil && s.stream.overlayInTx && sess != nil && ev != nil {
		pending = &overlayPending{sess: sess, ev: ev}
		ctx = withOverlayPending(ctx, pending)
	}
	if err := s.inner.AppendEvent(ctx, sess, ev); err != nil {
		return err
	}
	if pending != nil && pending.done {
		return nil
	}
	// ADK drops partial events without writing a row (runner.go filters
	// them too, so this is a direct-caller path). An overlay row for an
	// event ADK never persisted is the orphan shape deleteSession calls
	// poison: its seq is real, so every unfiltered Watch re-queries it
	// and re-fails to hydrate it, forever.
	if ev != nil && ev.Partial {
		return nil
	}
	if _, err := s.stream.Append(ctx, sess, ev); err != nil {
		return fmt.Errorf("eventlog: overlay write after ADK AppendEvent: %w", err)
	}
	return nil
}

// stampFinishReason mirrors a model turn's abnormal genai FinishReason
// into the event's CustomMetadata so it survives ADK's persist (see
// FinishReasonMetadataKey). Only abnormal reasons are recorded: the zero
// value is "" (not the UNSPECIFIED constant) and a plain STOP completion
// needs no marker — the auto-continue classifier treats unstamped tails
// as completed — so stamping either would only add noise to the 99% of
// turns that finish normally. MAX_TOKENS, SAFETY, RECITATION, etc. are
// kept: MAX_TOKENS drives a continuation (#582) and the rest are useful
// audit signal.
func stampFinishReason(ev *session.Event) {
	if ev == nil {
		return
	}
	switch ev.FinishReason {
	case "", genai.FinishReasonUnspecified, genai.FinishReasonStop:
		return
	}
	if ev.CustomMetadata == nil {
		ev.CustomMetadata = map[string]any{}
	}
	ev.CustomMetadata[FinishReasonMetadataKey] = string(ev.FinishReason)
}
