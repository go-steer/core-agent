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

// Package memory is an in-process eventlog.Transcript over ADK's
// in-memory session service. It is the reference implementation the
// conformance suite was written against and the backend tests use when
// they need a Transcript that is not SQL; it keeps nothing across a
// process, so it is not a durable store and the CLI never selects it.
package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"sort"
	"sync"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

// Transcript is the in-memory backend. Construct with New.
type Transcript struct {
	inner session.Service
	svc   *service

	mu      sync.Mutex
	trees   map[treeKey]*tree
	subs    eventlog.Subscribers
	closed  bool
	closeMu sync.Mutex
}

type treeKey struct{ app, user, root string }

// tree is one root session tree: its entries in seq order, the last
// seq it ever assigned (which survives deletion of the entries, so a
// re-created root does not reuse seqs a reader may hold a cursor
// past — SQL's AUTOINCREMENT gives the same guarantee), and every
// session ID created under it, appended to or not.
type tree struct {
	entries  []stored
	high     int64
	sessions map[string]struct{}
}

type stored struct {
	sessionID string
	entry     eventlog.Entry
}

// New returns an empty in-memory Transcript.
func New() *Transcript {
	t := &Transcript{inner: session.InMemoryService(), trees: make(map[treeKey]*tree)}
	t.svc = &service{t: t}
	return t
}

// Service implements eventlog.Transcript.
func (t *Transcript) Service() session.Service { return t.svc }

// Since implements eventlog.Transcript. Unscoped queries are refused
// with eventlog.ErrUnscopedQuery, as a store that cannot enumerate
// itself would; the suite relies on this backend to exercise that path.
func (t *Transcript) Since(ctx context.Context, fromSeq int64, opts ...eventlog.QueryOption) iter.Seq2[eventlog.Entry, error] {
	q := eventlog.ResolveQuery(opts...)
	return func(yield func(eventlog.Entry, error) bool) {
		if err := t.checkOpen(); err != nil {
			yield(eventlog.Entry{}, err)
			return
		}
		app, user, root, ok := q.Scope()
		if !ok {
			yield(eventlog.Entry{}, fmt.Errorf("memory: Since: %w", eventlog.ErrUnscopedQuery))
			return
		}
		// Snapshot under the lock, yield outside it: a consumer that
		// appends from inside the loop must not deadlock.
		t.mu.Lock()
		var snap []stored
		if tr := t.trees[treeKey{app, user, root}]; tr != nil {
			snap = append(snap, tr.entries...)
		}
		t.mu.Unlock()
		n := 0
		for _, s := range snap {
			if s.entry.Seq <= fromSeq {
				continue
			}
			if !q.Matches(app, user, s.sessionID, s.entry.Event) {
				continue
			}
			if ctx.Err() != nil {
				return
			}
			if !yield(s.entry, nil) {
				return
			}
			n++
			if q.Limit > 0 && n >= q.Limit {
				return
			}
		}
	}
}

// HighestSeq implements eventlog.Transcript.
func (t *Transcript) HighestSeq(_ context.Context, appName, userID, root string) (int64, error) {
	if err := t.checkOpen(); err != nil {
		return 0, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	tr := t.trees[treeKey{appName, userID, root}]
	if tr == nil || len(tr.entries) == 0 {
		return 0, nil
	}
	return tr.entries[len(tr.entries)-1].entry.Seq, nil
}

// Subscribe implements eventlog.Transcript.
func (t *Transcript) Subscribe(appName, userID, root string) (<-chan struct{}, func()) {
	return t.subs.Subscribe(appName, userID, root)
}

// Close implements eventlog.Transcript.
func (t *Transcript) Close() error {
	t.closeMu.Lock()
	defer t.closeMu.Unlock()
	t.closed = true
	return nil
}

func (t *Transcript) checkOpen() error {
	t.closeMu.Lock()
	defer t.closeMu.Unlock()
	if t.closed {
		return eventlog.ErrClosed
	}
	return nil
}

// treeFor returns the tree for (appName, userID, root), creating it.
// Caller holds t.mu.
func (t *Transcript) treeFor(appName, userID, root string) *tree {
	k := treeKey{appName, userID, root}
	tr := t.trees[k]
	if tr == nil {
		tr = &tree{sessions: make(map[string]struct{})}
		t.trees[k] = tr
	}
	return tr
}

// remember records that sessionID exists under its tree, so a root
// Delete can remove children that were created but never appended to.
func (t *Transcript) remember(appName, userID, sessionID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.treeFor(appName, userID, eventlog.RootOf(sessionID)).sessions[sessionID] = struct{}{}
}

// append records a copy of ev under sess with the tree's next seq.
// Called after the inner service accepted the event. The copy matters:
// the caller keeps mutating its own event (ADK's runner does), and a
// store that handed the caller's pointer back from Since would make
// every reader see those mutations. A JSON round-trip is also exactly
// what a durable backend does, so what this backend returns is what
// one of those would.
func (t *Transcript) append(ctx context.Context, sess session.Session, ev *session.Event) (int64, error) {
	stored0, err := cloneEvent(ev)
	if err != nil {
		return 0, err
	}
	k := treeKey{sess.AppName(), sess.UserID(), eventlog.RootOf(sess.ID())}
	var md map[string]string
	if src := eventlog.EntryMetadataFrom(ctx); len(src) > 0 {
		md = maps.Clone(src)
	}
	t.mu.Lock()
	tr := t.treeFor(k.app, k.user, k.root)
	tr.sessions[sess.ID()] = struct{}{}
	tr.high++
	seq := tr.high
	tr.entries = append(tr.entries, stored{sessionID: sess.ID(), entry: eventlog.Entry{Seq: seq, Event: stored0, Metadata: md}})
	t.mu.Unlock()
	t.subs.Notify(k.app, k.user, k.root)
	return seq, nil
}

func cloneEvent(ev *session.Event) (*session.Event, error) {
	b, err := json.Marshal(ev)
	if err != nil {
		return nil, fmt.Errorf("memory: encode event %q: %w", ev.ID, err)
	}
	out := &session.Event{}
	if err := json.Unmarshal(b, out); err != nil {
		return nil, fmt.Errorf("memory: decode event %q: %w", ev.ID, err)
	}
	return out, nil
}

// deleteSession drops a session's entries from its tree; for a root,
// every entry and every remembered session. The tree's high-water
// seq is kept either way, so a cursor held by a reader stays
// meaningful across a delete and re-create of the same ID.
func (t *Transcript) deleteSession(appName, userID, sessionID string) {
	k := treeKey{appName, userID, eventlog.RootOf(sessionID)}
	t.mu.Lock()
	defer t.mu.Unlock()
	tr := t.trees[k]
	if tr == nil {
		return
	}
	if sessionID == k.root {
		tr.entries = nil
		tr.sessions = make(map[string]struct{})
		return
	}
	kept := tr.entries[:0]
	for _, s := range tr.entries {
		if s.sessionID != sessionID {
			kept = append(kept, s)
		}
	}
	tr.entries = kept
	delete(tr.sessions, sessionID)
}

// childSessions lists the ":sub:" descendants of root the tree knows
// of — created or appended to — so Delete of a root can delete them
// from the inner service too.
func (t *Transcript) childSessions(appName, userID, root string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr := t.trees[treeKey{appName, userID, root}]
	if tr == nil {
		return nil
	}
	ids := make([]string, 0, len(tr.sessions))
	for id := range tr.sessions {
		if id != root {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// service is the session.Service the Transcript exposes: ADK's
// in-memory service with seq assignment on AppendEvent and tree
// cleanup on Delete.
type service struct {
	t *Transcript
	// writeMu serialises AppendEvent so seq assignment and the inner
	// append cannot interleave between two writers of one tree.
	writeMu sync.Mutex
}

func (s *service) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	if err := s.t.checkOpen(); err != nil {
		return nil, err
	}
	resp, err := s.t.inner.Create(ctx, req)
	if err != nil {
		return nil, err
	}
	s.t.remember(resp.Session.AppName(), resp.Session.UserID(), resp.Session.ID())
	return resp, nil
}

func (s *service) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	if err := s.t.checkOpen(); err != nil {
		return nil, err
	}
	return s.t.inner.Get(ctx, req)
}

func (s *service) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	if err := s.t.checkOpen(); err != nil {
		return nil, err
	}
	return s.t.inner.List(ctx, req)
}

func (s *service) Delete(ctx context.Context, req *session.DeleteRequest) error {
	if err := s.t.checkOpen(); err != nil {
		return err
	}
	if req == nil {
		return errors.New("memory: Delete: request is required")
	}
	if err := s.t.inner.Delete(ctx, req); err != nil {
		return err
	}
	if req.SessionID == eventlog.RootOf(req.SessionID) {
		for _, child := range s.t.childSessions(req.AppName, req.UserID, req.SessionID) {
			if err := s.t.inner.Delete(ctx, &session.DeleteRequest{AppName: req.AppName, UserID: req.UserID, SessionID: child}); err != nil && !errors.Is(err, session.ErrNotFound) {
				return err
			}
		}
	}
	s.t.deleteSession(req.AppName, req.UserID, req.SessionID)
	return nil
}

func (s *service) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if err := s.t.checkOpen(); err != nil {
		return err
	}
	if sess == nil || ev == nil {
		return errors.New("memory: AppendEvent: session and event are required")
	}
	if ev.Partial {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.t.inner.AppendEvent(ctx, sess, ev); err != nil {
		return err
	}
	_, err := s.t.append(ctx, sess, ev)
	return err
}
