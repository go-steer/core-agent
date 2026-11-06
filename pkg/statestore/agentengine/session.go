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

package agentengine

import (
	"iter"
	"maps"
	"strings"
	"sync"
	"time"

	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

// resident is a session this process holds in memory: the owner's view
// of its events and state. With one writer per session (the design's
// first settled decision) this view is authoritative once loaded, so
// Get and Since cost no RPC after the first load.
type resident struct {
	parent              string
	appName, userID, id string
	mu                  sync.RWMutex
	state               map[string]any
	entries             []eventlog.Entry // seq order
	updatedAt           time.Time
	// expireAt is the service's expiry for the session, refreshed on
	// append once less than half the configured TTL remains. Zero when
	// the service did not report one.
	expireAt time.Time
}

// high is the last seq this session holds, 0 when it has no events.
func (r *resident) high() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if n := len(r.entries); n > 0 {
		return r.entries[n-1].Seq
	}
	return 0
}

// apply records a stored entry: state delta folded in, event appended.
func (r *resident) apply(e eventlog.Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == nil {
		r.state = make(map[string]any)
	}
	if e.Event != nil {
		for k, v := range e.Event.Actions.StateDelta {
			if strings.HasPrefix(k, session.KeyPrefixTemp) {
				continue
			}
			r.state[k] = v
		}
		if e.Event.Timestamp.After(r.updatedAt) {
			r.updatedAt = e.Event.Timestamp
		}
	}
	r.entries = append(r.entries, e)
}

// snapshot returns a session.Session view for a Get response: a copy
// of the state and the event slice as it is now. Events are shared
// pointers (the stored copies are never mutated by this backend), the
// slice header is not.
func (r *resident) snapshot() *localSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	evs := make([]*session.Event, 0, len(r.entries))
	for _, e := range r.entries {
		evs = append(evs, e.Event)
	}
	return &localSession{
		parent:    r.parent,
		appName:   r.appName,
		userID:    r.userID,
		id:        r.id,
		state:     &localState{m: maps.Clone(r.state)},
		events:    localEvents(evs),
		updatedAt: r.updatedAt,
	}
}

// localSession is the session.Session this backend hands to ADK. ADK's
// runner holds it for a turn and passes it back to AppendEvent, which
// looks the resident up by key rather than trusting the object — but
// also appends to the object, because the runner reads its Events()
// again for the next model call of the same invocation.
type localSession struct {
	parent              string
	appName, userID, id string
	state               *localState
	mu                  sync.RWMutex
	events              localEvents
	updatedAt           time.Time
}

func (s *localSession) ID() string           { return s.id }
func (s *localSession) AppName() string      { return s.appName }
func (s *localSession) UserID() string       { return s.userID }
func (s *localSession) State() session.State { return s.state }

// VertexSessionResource implements memory.VertexSessionOrigin (by
// structural typing) so Memory Bank's ConsolidateConversation can pass
// a zero-copy VertexSessionSource when the memory bank and transcript
// share a ReasoningEngine parent.
func (s *localSession) VertexSessionResource() (reasoningEngineParent, sessionResource string, ok bool) {
	if s.parent == "" || s.id == "" {
		return "", "", false
	}
	return s.parent, s.parent + "/sessions/" + mapID(s.id), true
}
func (s *localSession) Events() session.Events {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return localEvents(append([]*session.Event(nil), s.events...))
}
func (s *localSession) LastUpdateTime() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.updatedAt
}

// append mirrors a persisted event into this in-turn view.
func (s *localSession) append(ev *session.Event) {
	for k, v := range ev.Actions.StateDelta {
		if !strings.HasPrefix(k, session.KeyPrefixTemp) {
			_ = s.state.Set(k, v)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	if ev.Timestamp.After(s.updatedAt) {
		s.updatedAt = ev.Timestamp
	}
}

type localState struct {
	mu sync.RWMutex
	m  map[string]any
}

func (s *localState) Get(key string) (any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return v, nil
}

func (s *localState) Set(key string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]any)
	}
	s.m[key] = value
	return nil
}

func (s *localState) All() iter.Seq2[string, any] {
	s.mu.RLock()
	snap := maps.Clone(s.m)
	s.mu.RUnlock()
	return func(yield func(string, any) bool) {
		for k, v := range snap {
			if !yield(k, v) {
				return
			}
		}
	}
}

type localEvents []*session.Event

func (e localEvents) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, ev := range e {
			if !yield(ev) {
				return
			}
		}
	}
}
func (e localEvents) Len() int                { return len(e) }
func (e localEvents) At(i int) *session.Event { return e[i] }
