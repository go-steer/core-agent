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

package background

import (
	"errors"
	"testing"

	coretools "github.com/go-steer/core-agent/v2/pkg/tools"
)

func TestManager_ResolveScheduler(t *testing.T) {
	t.Parallel()
	def := coretools.SleepScheduler()
	mgr := &Manager{defaultScheduler: def}

	cases := []struct {
		choice  string
		wantNil bool
	}{
		{"", false},              // default
		{"default", false},       // default
		{"sleep", false},         // SleepScheduler
		{"exit_on_defer", false}, // ExitOnDeferScheduler
		{"none", true},           // explicitly no scheduler
	}
	for _, tc := range cases {
		got, err := mgr.resolveScheduler(tc.choice)
		if err != nil {
			t.Errorf("choice=%q: unexpected error %v", tc.choice, err)
			continue
		}
		if tc.wantNil && got != nil {
			t.Errorf("choice=%q: want nil, got %T", tc.choice, got)
		}
		if !tc.wantNil && got == nil {
			t.Errorf("choice=%q: want non-nil scheduler, got nil", tc.choice)
		}
	}

	if _, err := mgr.resolveScheduler("bogus"); !errors.Is(err, ErrUnknownScheduler) {
		t.Errorf("unknown choice should return ErrUnknownScheduler, got %v", err)
	}
}

func TestManager_DefaultSchedulerNilWhenUnset(t *testing.T) {
	t.Parallel()
	mgr := &Manager{} // no defaultScheduler
	got, err := mgr.resolveScheduler("")
	if err != nil {
		t.Fatalf("resolveScheduler: %v", err)
	}
	if got != nil {
		t.Errorf("empty choice with no default should resolve to nil, got %T", got)
	}
}
