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
// Sub-session isolation (issue #273): writes land in a derived
// session ID "<parent>:digest", NOT the parent's own row. ADK's
// database session service tracks last_update_time per row and
// rejects appends with a "stale session error" when an out-of-band
// writer bumps the row while another caller is holding a stale
// session.Session snapshot. Since digest.Process fires synchronously
// from the middle of a tool call (see pkg/mcp/digest_wrap.go), a
// direct Put against the parent row races the runner's own
// storedSession — which the runner captured at the start of the
// turn and holds through every AppendEvent that follows. Writing to
// a derived row sidesteps the race entirely; the same technique
// pkg/agent/subagent.go uses for the sub-runner isolation
// documented in docs/eventlog-decisions.md.
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
	handle           *eventlog.Handle
	appName          string
	userID           string
	sessionID        string // parent session — identifies the row tree we're associated with
	derivedSessionID string // <sessionID>:digest — where our Puts actually land

	// putMu serializes AppendEvent calls from this store. The
	// underlying service.AppendEvent already has its own write
	// mutex, but the atomic.Add-based ID counter can still yield
	// two "same-nanosecond+ different-counter" IDs that race the
	// SQLite UNIQUE index in edge cases. Serializing here is
	// zero-cost (retrieve_raw / Put are rare) and eliminates any
	// residual collision risk without spelunking into ADK's ID
	// generator.
	putMu sync.Mutex

	// ensureOnce guards the one-time Create of the derived session
	// row (see storeSubSessionSuffix). Subsequent Puts skip straight
	// to Get + AppendEvent. ensureErr caches a startup failure so
	// every Put after the first surfaces the same error rather than
	// silently missing.
	ensureOnce sync.Once
	ensureErr  error
}

// storeSubSessionSuffix is the suffix appended to the parent session
// ID to derive the row EventlogStore writes into. Chosen so audit
// tooling can split on ":" and recover the parent (matching the
// convention in pkg/agent/subagent.go's deriveSubagentSessionID).
const storeSubSessionSuffix = ":digest"

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
		handle:           handle,
		appName:          appName,
		userID:           userID,
		sessionID:        sessionID,
		derivedSessionID: sessionID + storeSubSessionSuffix,
	}, nil
}

// Put implements Store. Ensures the derived digest sub-session
// exists, fetches its session.Session, constructs a "digest.raw"
// event carrying the callID + base64-encoded payload, and appends
// it through the eventlog service.
//
// Writing to the derived <sessionID>:digest row (not the parent's)
// is what keeps digest.Process from tripping ADK's optimistic-
// concurrency check against the runner's mid-turn session snapshot
// (issue #273). See the EventlogStore godoc for the full rationale.
//
// Empty callID is rejected — same contract as FilesystemStore.
func (s *EventlogStore) Put(ctx context.Context, callID string, raw []byte) error {
	if callID == "" {
		return errors.New("digest: EventlogStore.Put: empty callID")
	}
	s.putMu.Lock()
	defer s.putMu.Unlock()
	if err := s.ensureDerivedSession(ctx); err != nil {
		return fmt.Errorf("digest: EventlogStore.Put: %w", err)
	}
	sess, err := s.fetchDerivedSession(ctx)
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
	// Scan the derived <sessionID>:digest row (where Put writes),
	// NOT the parent — see the EventlogStore godoc for the isolation
	// rationale. Anything in the parent row's events with
	// Author=="digest.raw" is either from a pre-issue-#273 daemon or
	// from a manual write and is intentionally ignored here.
	var latestB64 string
	for entry, err := range s.handle.Stream.Since(ctx, 0,
		eventlog.ForSession(s.appName, s.userID, s.derivedSessionID),
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

// ensureDerivedSession lazily creates the <sessionID>:digest row
// on the first Put. Subsequent Puts skip straight through. Uses
// a sync.Once + cached error so a startup failure surfaces on
// every subsequent Put rather than being silently absorbed. The
// Get path (via s.handle.Service.Get on a missing row) is what
// signals "session missing" here — Create errors surface directly.
func (s *EventlogStore) ensureDerivedSession(ctx context.Context) error {
	s.ensureOnce.Do(func() {
		// Get-first pattern: on daemon restart / session-resume the
		// derived row may already exist. Skip Create in that case so
		// we don't rely on the specific error shape Create returns
		// for a duplicate primary key across dialectors.
		_, err := s.handle.Service.Get(ctx, &session.GetRequest{
			AppName:   s.appName,
			UserID:    s.userID,
			SessionID: s.derivedSessionID,
		})
		if err == nil {
			return
		}
		_, err = s.handle.Service.Create(ctx, &session.CreateRequest{
			AppName:   s.appName,
			UserID:    s.userID,
			SessionID: s.derivedSessionID,
		})
		if err != nil {
			s.ensureErr = fmt.Errorf("create derived session %q: %w", s.derivedSessionID, err)
		}
	})
	return s.ensureErr
}

// fetchDerivedSession pulls the session.Session for the derived
// digest row. Extracted so Put stays readable — this is the only
// network / DB hop on the write path after ensureDerivedSession.
func (s *EventlogStore) fetchDerivedSession(ctx context.Context) (session.Session, error) {
	resp, err := s.handle.Service.Get(ctx, &session.GetRequest{
		AppName:   s.appName,
		UserID:    s.userID,
		SessionID: s.derivedSessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("session Get: %w", err)
	}
	if resp == nil || resp.Session == nil {
		return nil, fmt.Errorf("derived session %s/%s/%s not found", s.appName, s.userID, s.derivedSessionID)
	}
	return resp.Session, nil
}
