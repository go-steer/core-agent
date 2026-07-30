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

package compose

import (
	"context"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/agent"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/attachadapter"
	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/eventlog"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// bareRegistrant satisfies attach.Registrant WITHOUT wrapping a
// *agent.Agent — the shape a custom host might register. The tracker
// adapter must skip it, not panic.
type bareRegistrant struct{ session string }

func (b *bareRegistrant) AppName() string                    { return "custom" }
func (b *bareRegistrant) UserID() string                     { return "u" }
func (b *bareRegistrant) SessionID() string                  { return b.session }
func (b *bareRegistrant) EventLog() *eventlog.Handle         { return nil }
func (b *bareRegistrant) Inject(string) error                { return nil }
func (b *bareRegistrant) InjectAs(string, auth.Caller) error { return nil }
func (b *bareRegistrant) RequestWake()                       {}

func TestRegistryTrackerProvider(t *testing.T) {
	t.Parallel()

	reg := attach.NewSessionRegistry()

	// Adapter-wrapped agent: contributes its tracker with the
	// ENTRY's identity triple.
	a := newTestAgent(t, "app-a", "user-a", "sess-a")
	if _, err := reg.Register(attachadapter.New(a)); err != nil {
		t.Fatalf("register adapter: %v", err)
	}
	// Bare registrant: silently skipped.
	if _, err := reg.Register(&bareRegistrant{session: "sess-bare"}); err != nil {
		t.Fatalf("register bare: %v", err)
	}

	tp := RegistryTrackerProvider(reg)
	got := tp.Trackers()
	if len(got) != 1 {
		t.Fatalf("Trackers() len = %d, want 1 (bare registrant skipped)", len(got))
	}
	ts := got[0]
	if ts.Tracker != a.Tracker() {
		t.Error("tracker is not the wrapped agent's tracker")
	}
	if ts.SessionID != "sess-a" || ts.AppName != "app-a" || ts.UserID != "user-a" {
		t.Errorf("identity = %+v, want the entry triple", ts)
	}
}

func TestRegistryTrackerProvider_NilRegistry(t *testing.T) {
	t.Parallel()
	if tp := RegistryTrackerProvider(nil); tp != nil {
		t.Error("nil registry must yield nil provider")
	}
}

var _ usage.TrackerProvider = (*registryTrackerProvider)(nil)

// newTestAgent builds a minimal live agent for adapter tests.
func newTestAgent(t *testing.T, app, user, sess string) *agent.Agent {
	t.Helper()
	prov := mock.NewEcho()
	llm, err := prov.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock model: %v", err)
	}
	a, err := agent.New(llm,
		agent.WithAppName(app),
		agent.WithSession(user, sess),
		agent.WithUsageTracker(usage.NewTracker()),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}
