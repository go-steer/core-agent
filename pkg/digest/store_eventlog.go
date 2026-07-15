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

package digest

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"

	"github.com/go-steer/core-agent/pkg/eventlog"
)

// eventlogAuthorRaw is the event Author string EventlogStore uses to
// mark raw-payload records. Callers should not construct or match
// this string elsewhere — it's an internal EventlogStore contract.
const eventlogAuthorRaw = "digest.raw"

// eventlogMetaCallID + eventlogMetaRaw are the CustomMetadata keys
// used to carry the callID + base64-encoded payload on stored
// events. Base64 avoids text-encoding issues (JSON marshalers may
// mangle non-UTF-8 bytes, and raw payloads often carry image /
// binary data pulled through MCP tools).
const (
	eventlogMetaCallID = "digest_call_id"
	eventlogMetaRaw    = "digest_raw_b64"
)

// eventCounter guarantees a per-process unique suffix on generated
// event IDs. UnixNano alone collides on rapid back-to-back Puts (two
// same-tick writes → same ID → UNIQUE constraint failure on the ADK
// events table). Composed with UnixNano the pair stays sortable
// while avoiding collisions.
var eventCounter atomic.Uint64

// newEventID returns a unique-enough event ID for an EventlogStore
// write. Format: "digest-<unix-nanos>-<counter>". Matches the
// convention in agent/compactor.go's newBoundaryEventID (prefix
// makes audit-log greps cheap).
func newEventID() string {
	return fmt.Sprintf("digest-%d-%d", time.Now().UnixNano(), eventCounter.Add(1))
}

// EventlogStore is a Store backed by pkg/eventlog. Persists raw
// payloads as dedicated session.Event records tagged with
// Author == "digest.raw" and the callID + payload in CustomMetadata.
// Get scans the session's events for a matching record and returns
// the decoded raw bytes.
//
// Why a dedicated event record rather than reusing the tool-response
// row: the #84 wrap-layer flow substitutes the model-facing tool
// response with a digest before Agent.Run persists it, so the
// tool-response row in the eventlog carries the digest, not the
// raw. Recording raw as its own event keeps the audit trail complete
// and gives retrieve_raw a stable key to look up by, without a new
// database table.
//
// Depends on --session-db: constructors take an *eventlog.Handle,
// which is nil when the operator hasn't enabled the session
// database. NewEventlogStore returns an error rather than falling
// back silently — callers who want a filesystem-only fallback wire
// FilesystemStore explicitly.
//
// Safe for concurrent use — all writes go through the eventlog
// service's write mutex; reads use the underlying Stream which is
// concurrent-safe by design.
type EventlogStore struct {
	handle    *eventlog.Handle
	appName   string
	userID    string
	sessionID string

	// putMu serializes AppendEvent calls from this store. The
	// underlying service.AppendEvent already has its own write
	// mutex, but the atomic.Add-based ID counter can still yield
	// two "same-nanosecond+ different-counter" IDs that race the
	// SQLite UNIQUE index in edge cases. Serializing here is
	// zero-cost (retrieve_raw / Put are rare) and eliminates any
	// residual collision risk without spelunking into ADK's ID
	// generator.
	putMu sync.Mutex
}

// NewEventlogStore constructs an EventlogStore for the given session.
// Returns an error when handle is nil (no --session-db) or when any
// of appName/userID/sessionID is empty — a session-scoped store
// with missing identity would silently write into the wrong session.
func NewEventlogStore(handle *eventlog.Handle, appName, userID, sessionID string) (*EventlogStore, error) {
	if handle == nil {
		return nil, errors.New("digest: NewEventlogStore: nil handle (is --session-db enabled?)")
	}
	if handle.Service == nil || handle.Stream == nil {
		return nil, errors.New("digest: NewEventlogStore: handle missing Service or Stream")
	}
	if appName == "" || userID == "" || sessionID == "" {
		return nil, fmt.Errorf("digest: NewEventlogStore: empty session identity (app=%q user=%q sid=%q)",
			appName, userID, sessionID)
	}
	return &EventlogStore{
		handle:    handle,
		appName:   appName,
		userID:    userID,
		sessionID: sessionID,
	}, nil
}

// Put implements Store. Fetches the session from the underlying
// service, constructs a "digest.raw" event carrying the callID +
// base64-encoded payload, and appends it through the eventlog
// service (which handles ADK write + overlay seq atomically).
//
// Empty callID is rejected — same contract as FilesystemStore. A
// missing session is treated as a caller error (the caller should
// wire EventlogStore only for sessions they own).
func (s *EventlogStore) Put(ctx context.Context, callID string, raw []byte) error {
	if callID == "" {
		return errors.New("digest: EventlogStore.Put: empty callID")
	}
	s.putMu.Lock()
	defer s.putMu.Unlock()
	sess, err := s.fetchSession(ctx)
	if err != nil {
		return fmt.Errorf("digest: EventlogStore.Put: %w", err)
	}
	ev := &session.Event{
		ID:        newEventID(),
		Author:    eventlogAuthorRaw,
		Timestamp: time.Now(),
		LLMResponse: adkmodel.LLMResponse{
			CustomMetadata: map[string]any{
				eventlogMetaCallID: callID,
				eventlogMetaRaw:    base64.StdEncoding.EncodeToString(raw),
			},
		},
	}
	if err := s.handle.Service.AppendEvent(ctx, sess, ev); err != nil {
		return fmt.Errorf("digest: EventlogStore.Put: append: %w", err)
	}
	return nil
}

// Get implements Store. Scans the session's events via Stream.Since
// with the WithAuthor filter set to "digest.raw", returning the most
// recent match's decoded payload. Returns ErrNotFound when no event
// matches callID.
//
// Scan cost: O(events emitted with Author=="digest.raw" in this
// session). retrieve_raw is model-driven and rare, so the cost is
// acceptable; if telemetry shows it dominates, a follow-up patch
// can add an in-memory callID → seq index over Stream.Watch.
func (s *EventlogStore) Get(ctx context.Context, callID string) ([]byte, error) {
	if callID == "" {
		return nil, ErrNotFound
	}
	var latestB64 string
	for entry, err := range s.handle.Stream.Since(ctx, 0,
		eventlog.ForSession(s.appName, s.userID, s.sessionID),
		eventlog.WithAuthor(eventlogAuthorRaw)) {
		if err != nil {
			return nil, fmt.Errorf("digest: EventlogStore.Get: %w", err)
		}
		if entry.Event == nil {
			continue
		}
		meta := entry.Event.CustomMetadata
		if meta == nil {
			continue
		}
		gotID, _ := meta[eventlogMetaCallID].(string)
		if gotID != callID {
			continue
		}
		b64, _ := meta[eventlogMetaRaw].(string)
		latestB64 = b64
		// Keep scanning — later entries with the same callID
		// override earlier ones (matches FilesystemStore's
		// Put-overwrites semantics).
	}
	if latestB64 == "" {
		return nil, ErrNotFound
	}
	raw, err := base64.StdEncoding.DecodeString(latestB64)
	if err != nil {
		return nil, fmt.Errorf("digest: EventlogStore.Get: decode: %w", err)
	}
	return raw, nil
}

// fetchSession pulls the session.Session for the store's identity.
// Extracted so Put stays readable — this is the only network / DB
// hop on the write path.
func (s *EventlogStore) fetchSession(ctx context.Context) (session.Session, error) {
	resp, err := s.handle.Service.Get(ctx, &session.GetRequest{
		AppName:   s.appName,
		UserID:    s.userID,
		SessionID: s.sessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("session Get: %w", err)
	}
	if resp == nil || resp.Session == nil {
		return nil, fmt.Errorf("session %s/%s/%s not found", s.appName, s.userID, s.sessionID)
	}
	return resp.Session, nil
}
