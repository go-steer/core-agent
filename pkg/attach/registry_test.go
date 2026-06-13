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

package attach

import (
	"context"
	"errors"
	"testing"

	"github.com/go-steer/core-agent/pkg/auth"
	"github.com/go-steer/core-agent/pkg/eventlog"
)

// stubRegistrant is a minimal Registrant for registry tests. It
// implements the interface methods with controllable return values;
// we never actually invoke Inject / RequestWake from registry tests.
type stubRegistrant struct {
	app, user, sid string
	log            *eventlog.Handle
	injected       []string
	injectedAs     []injectedRecord
	wakes          int
}

// injectedRecord captures the per-message caller for InjectAs so tests
// can assert that the right identity threaded through the handlers.
type injectedRecord struct {
	message string
	caller  auth.Caller
}

func (s *stubRegistrant) AppName() string            { return s.app }
func (s *stubRegistrant) UserID() string             { return s.user }
func (s *stubRegistrant) SessionID() string          { return s.sid }
func (s *stubRegistrant) EventLog() *eventlog.Handle { return s.log }
func (s *stubRegistrant) Inject(m string) error {
	return s.InjectAs(m, auth.Caller{})
}
func (s *stubRegistrant) InjectAs(m string, c auth.Caller) error {
	s.injected = append(s.injected, m)
	s.injectedAs = append(s.injectedAs, injectedRecord{message: m, caller: c})
	return nil
}
func (s *stubRegistrant) RequestWake() { s.wakes++ }

func TestRegistry_RegisterAndLookup(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "core-agent", user: "u", sid: "s1"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := reg.Len(); got != 1 {
		t.Errorf("Len = %d, want 1", got)
	}
	e, err := reg.Lookup(context.Background(), "core-agent", "s1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if e.Agent != ag {
		t.Errorf("Lookup returned different agent")
	}
}

func TestRegistry_RegisterRejectsDuplicate(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag1 := &stubRegistrant{app: "a", user: "u", sid: "s"}
	ag2 := &stubRegistrant{app: "a", user: "u", sid: "s"}
	if _, err := reg.Register(ag1); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	_, err := reg.Register(ag2)
	if !errors.Is(err, ErrSessionExists) {
		t.Errorf("second Register: want ErrSessionExists, got %v", err)
	}
}

func TestRegistry_RegisterRequiresAppAndSession(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	cases := []*stubRegistrant{
		{app: "", user: "u", sid: "s"},
		{app: "a", user: "u", sid: ""},
	}
	for _, c := range cases {
		if _, err := reg.Register(c); err == nil {
			t.Errorf("expected error for app=%q sid=%q", c.app, c.sid)
		}
	}
}

func TestRegistry_LookupNotFound(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	_, err := reg.Lookup(context.Background(), "nope", "nope")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("want ErrSessionNotFound, got %v", err)
	}
}

func TestRegistry_LookupSingle_Unambiguous(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "a", user: "u", sid: "unique"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	e, err := reg.LookupSingle(context.Background(), "unique")
	if err != nil {
		t.Fatalf("LookupSingle: %v", err)
	}
	if e.AppName != "a" {
		t.Errorf("got app=%q, want a", e.AppName)
	}
}

func TestRegistry_LookupSingle_Ambiguous(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag1 := &stubRegistrant{app: "app1", user: "u", sid: "shared"}
	ag2 := &stubRegistrant{app: "app2", user: "u", sid: "shared"}
	if _, err := reg.Register(ag1); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Register(ag2); err != nil {
		t.Fatal(err)
	}
	_, err := reg.LookupSingle(context.Background(), "shared")
	if !errors.Is(err, ErrAmbiguousSession) {
		t.Errorf("want ErrAmbiguousSession, got %v", err)
	}
}

func TestRegistry_Unregister(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	ag := &stubRegistrant{app: "a", user: "u", sid: "s"}
	if _, err := reg.Register(ag); err != nil {
		t.Fatal(err)
	}
	reg.Unregister("a", "u", "s")
	if reg.Len() != 0 {
		t.Errorf("after Unregister Len = %d, want 0", reg.Len())
	}
	// Idempotent: second Unregister is a no-op.
	reg.Unregister("a", "u", "s")
}

func TestRegistry_List_Sorted(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	for _, in := range []*stubRegistrant{
		{app: "z", user: "u", sid: "1"},
		{app: "a", user: "u", sid: "2"},
		{app: "a", user: "u", sid: "1"},
		{app: "m", user: "u", sid: "1"},
	} {
		if _, err := reg.Register(in); err != nil {
			t.Fatal(err)
		}
	}
	got := reg.List()
	want := []struct{ app, sid string }{
		{"a", "1"}, {"a", "2"}, {"m", "1"}, {"z", "1"},
	}
	if len(got) != len(want) {
		t.Fatalf("List len = %d, want %d", len(got), len(want))
	}
	for i, e := range got {
		if e.AppName != want[i].app || e.SessionID != want[i].sid {
			t.Errorf("List[%d] = %s/%s, want %s/%s",
				i, e.AppName, e.SessionID, want[i].app, want[i].sid)
		}
	}
}

func TestAgentRegistrarAdapter_TypeAssertion(t *testing.T) {
	t.Parallel()
	reg := NewSessionRegistry()
	adapter := NewAgentRegistrarAdapter(reg)

	ag := &stubRegistrant{app: "a", user: "u", sid: "s"}
	if _, err := adapter.Register(ag); err != nil {
		t.Errorf("Register(stubRegistrant) failed: %v", err)
	}

	// Something that doesn't implement Registrant.
	_, err := adapter.Register("not a registrant")
	var nrErr *ErrNotRegistrant
	if !errors.As(err, &nrErr) {
		t.Errorf("Register(string) want ErrNotRegistrant, got %v", err)
	}
}
