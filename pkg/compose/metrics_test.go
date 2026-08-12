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

// agentRegistrant is a bareRegistrant that DOES expose a wrapped
// agent, with a caller-chosen identity triple — lets a test put one
// *agent.Agent behind two registry entries, which attachadapter
// can't do (its triple comes from the agent).
type agentRegistrant struct {
	bareRegistrant
	agent *agent.Agent
}

func (a *agentRegistrant) Agent() *agent.Agent { return a.agent }

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

// TestRegistryTrackerProvider_SharedTrackerLastWins pins the /model
// swap shape: two registry entries sharing one tracker (swap
// re-registers under a fresh UUIDv7 session id without unregistering
// the old entry) must yield ONE TrackedSession attributed to the
// LAST entry in registry order — the newest session id.
func TestRegistryTrackerProvider_SharedTrackerLastWins(t *testing.T) {
	t.Parallel()
	reg := attach.NewSessionRegistry()
	tr := usage.NewTracker()

	old := newTestAgentWithTracker(t, "app", "u", "0195-old", tr)
	if _, err := reg.Register(attachadapter.New(old)); err != nil {
		t.Fatalf("register old: %v", err)
	}
	swapped := newTestAgentWithTracker(t, "app", "u", "0196-new", tr)
	if _, err := reg.Register(attachadapter.New(swapped)); err != nil {
		t.Fatalf("register swapped: %v", err)
	}

	got := RegistryTrackerProvider(reg).Trackers()
	if len(got) != 1 {
		t.Fatalf("Trackers() len = %d, want 1 (shared tracker deduped)", len(got))
	}
	if got[0].SessionID != "0196-new" {
		t.Errorf("session = %q, want the newest entry 0196-new", got[0].SessionID)
	}
}

// TestRegistryAgents covers the sibling unwrapper that feeds
// agent.RegisterMetrics. Its failure mode is quiet in both
// directions: skip an agent and its metrics never get registered;
// return a duplicate and the same agent is instrumented twice.
func TestRegistryAgents(t *testing.T) {
	t.Parallel()

	reg := attach.NewSessionRegistry()
	a := newTestAgent(t, "app", "u", "sess-a")
	b := newTestAgent(t, "app", "u", "sess-b")
	if _, err := reg.Register(attachadapter.New(a)); err != nil {
		t.Fatalf("register a: %v", err)
	}
	if _, err := reg.Register(attachadapter.New(b)); err != nil {
		t.Fatalf("register b: %v", err)
	}
	// One agent behind two registry entries. The registry rejects a
	// duplicate identity triple, so this needs a registrant carrying
	// its own triple — the shape a host gets when it re-registers a
	// swapped agent under a fresh session id without unregistering
	// the old entry. Pointer dedup is what stops RegisterMetrics
	// instrumenting it twice.
	if _, err := reg.Register(&agentRegistrant{bareRegistrant: bareRegistrant{session: "sess-a-again"}, agent: a}); err != nil {
		t.Fatalf("re-register a under a fresh session: %v", err)
	}
	// A registrant that exposes no agent at all is skipped, not a
	// panic and not a nil entry in the result.
	if _, err := reg.Register(&bareRegistrant{session: "sess-bare"}); err != nil {
		t.Fatalf("register bare: %v", err)
	}

	got := RegistryAgents(reg)
	if len(got) != 2 {
		t.Fatalf("got %d agents, want 2 (deduped, bare registrant skipped)", len(got))
	}
	seen := map[*agent.Agent]int{}
	for _, x := range got {
		if x == nil {
			t.Fatal("nil agent in result")
		}
		seen[x]++
	}
	if seen[a] != 1 || seen[b] != 1 {
		t.Errorf("agent occurrences: a=%d b=%d, want 1 each", seen[a], seen[b])
	}
}

func TestRegistryAgents_NilRegistry(t *testing.T) {
	t.Parallel()
	if got := RegistryAgents(nil); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestRegistryAgents_EmptyRegistry(t *testing.T) {
	t.Parallel()
	if got := RegistryAgents(attach.NewSessionRegistry()); len(got) != 0 {
		t.Errorf("got %v, want no agents", got)
	}
}

// newTestAgentWithTracker is newTestAgent with a caller-supplied
// tracker (shared-tracker scenarios).
func newTestAgentWithTracker(t *testing.T, app, user, sess string, tr *usage.Tracker) *agent.Agent {
	t.Helper()
	prov := mock.NewEcho()
	llm, err := prov.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock model: %v", err)
	}
	a, err := agent.New(llm,
		agent.WithAppName(app),
		agent.WithSession(user, sess),
		agent.WithUsageTracker(tr),
	)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}
