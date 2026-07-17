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

package agent

import (
	"testing"

	"google.golang.org/adk/session"
)

// The dispatch mechanism itself is exercised in pkg/hooks; here we
// only need to know the option populates the two field slots the tap
// loop reads.

func TestWithEventHook_PopulatesBothCallbacks(t *testing.T) {
	t.Parallel()
	var o options
	onEvent := func(*session.Event) {}
	onTurnEnd := func() {}
	WithEventHook(onEvent, onTurnEnd)(&o)
	if o.onEvent == nil {
		t.Error("onEvent not populated")
	}
	if o.onTurnEnd == nil {
		t.Error("onTurnEnd not populated")
	}
}

func TestWithEventHook_AcceptsNil(t *testing.T) {
	t.Parallel()
	// Legal for either callback to be nil (e.g., an observer that
	// only cares about turn boundaries, or vice-versa). Setting both
	// nil is also legal — becomes a no-op option.
	var o options
	o.onEvent = func(*session.Event) {} // pre-set to see the option clears
	o.onTurnEnd = func() {}
	WithEventHook(nil, nil)(&o)
	if o.onEvent != nil {
		t.Error("WithEventHook(nil, nil) should clear onEvent")
	}
	if o.onTurnEnd != nil {
		t.Error("WithEventHook(nil, nil) should clear onTurnEnd")
	}
}

func TestWithEventHook_LatestWins(t *testing.T) {
	t.Parallel()
	// Single-slot semantics: a second call replaces the first.
	var o options
	firstCallCount := 0
	WithEventHook(func(*session.Event) { firstCallCount++ }, nil)(&o)
	secondCallCount := 0
	WithEventHook(func(*session.Event) { secondCallCount++ }, nil)(&o)
	o.onEvent(nil)
	if firstCallCount != 0 {
		t.Errorf("first callback fired %d times, want 0 (replaced)", firstCallCount)
	}
	if secondCallCount != 1 {
		t.Errorf("second callback fired %d times, want 1", secondCallCount)
	}
}
