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
	"errors"
	"fmt"
	"testing"

	"google.golang.org/adk/session"
	"gorm.io/gorm"
)

// TestIsSessionNotFoundAgainstRealService is the load-bearing one: it
// asks the real ADK session service for a session that was never
// created and asserts we can tell that apart from a database fault.
//
// The whole point of #973's implied session DB is that a daemon's very
// first boot reads a session that does not exist yet. If ADK ever stops
// wrapping GORM's sentinel with %w — an upgrade away — this test goes
// red here rather than in production as a "database error while
// fetching session" line on every healthy cold start.
func TestIsSessionNotFoundAgainstRealService(t *testing.T) {
	t.Parallel()

	h, cleanup := openTestHandle(t)
	defer cleanup()

	_, err := h.Service.Get(context.Background(), &session.GetRequest{
		AppName:   "core-agent",
		UserID:    "user",
		SessionID: "never-created",
	})
	if err == nil {
		t.Skip("ADK returned no error for a missing session; nothing to classify")
	}
	if !IsSessionNotFound(err) {
		t.Fatalf("IsSessionNotFound(%v) = false, want true\n"+
			"a missing session must not read as a database fault", err)
	}
}

// TestIsSessionNotFoundDoesNotSwallowRealFailures guards the other
// direction. Classifying too broadly would silence exactly the errors
// the caller still needs to print — a corrupt file, a permissions
// problem, a closed handle.
func TestIsSessionNotFoundDoesNotSwallowRealFailures(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "unrelated error", err: errors.New("disk i/o error"), want: false},
		{
			// The message alone is not the signal: a genuine fault
			// can carry the same prose ADK puts in front of the
			// sentinel, and must still be reported.
			name: "same prose, no sentinel",
			err:  errors.New("database error while fetching session: connection refused"),
			want: false,
		},
		{name: "handle closed", err: ErrClosed, want: false},
		{
			name: "sentinel wrapped once",
			err:  fmt.Errorf("database error while fetching session: %w", gorm.ErrRecordNotFound),
			want: true,
		},
		{
			name: "sentinel wrapped twice",
			err:  fmt.Errorf("get session: %w", fmt.Errorf("db: %w", gorm.ErrRecordNotFound)),
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsSessionNotFound(tc.err); got != tc.want {
				t.Errorf("IsSessionNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
