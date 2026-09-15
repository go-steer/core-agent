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

package vertexcache

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/genai"
)

// belowMinimum is the verbatim shape observed on the cluster during the
// #647 approval-gate UAT, image main-95e5e22 on std-simian-test.
const belowMinimum = "Error 400, Message: The cached content is of 2373 tokens. The minimum token count to start explicit caching is 4096., Status: INVALID_ARGUMENT, Details: []"

func TestIsBelowCacheMinimum(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"the real shape, verbatim", errors.New(belowMinimum), true},

		// The number is not the discriminator: the minimum is per-model
		// and Google has moved it before.
		{"a different minimum on a different model",
			errors.New("Error 400, Message: The cached content is of 900 tokens. The minimum token count to start explicit caching is 1024., Status: INVALID_ARGUMENT"),
			true},

		// --- must NOT match: other things that fail a Create ---
		{"IAM propagation, the #707 case that MUST keep its retries",
			errors.New("rpc error: code = PermissionDenied desc = 403"), false},
		{"bare INVALID_ARGUMENT", errors.New("Error 400, Message: Request contains an invalid argument., Status: INVALID_ARGUMENT"), false},
		{"quota, which mentions tokens but is time-fixable",
			errors.New("Error 429, Message: Quota exceeded: 1000000 input tokens per minute, Status: RESOURCE_EXHAUSTED"), false},
		{"an eviction, which belongs to IsCacheGone",
			errors.New("Error 400, Message: Cache content 1 is expired., Status: INVALID_ARGUMENT"), false},

		// The phrase without the status is not enough — a future error
		// that merely quotes a minimum must not be read as this one.
		{"the phrase under a non-400 status",
			errors.New("Error 500, Message: minimum token count service unavailable, Status: INTERNAL"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsBelowCacheMinimum(tc.err); got != tc.want {
				t.Errorf("IsBelowCacheMinimum(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// syncBuffer is a log sink a test goroutine can read while doInit's
// goroutine writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestInit_BelowMinimumGivesUpImmediately is #1067. The retry schedule
// exists for a failure time can fix (#707's IAM propagation). This one
// cannot be: the cached content is the agent's system instruction plus
// its tool declarations, fixed for the life of the process, so the
// five retries are five doomed RPCs and — worse — five log lines that
// read like something an operator should act on, printed at the moment
// a turn starts its first model request.
//
// Pre-fix this fails on both counts: createCount reaches 2 and
// Snapshot().Failed is false after the first attempt.
func TestInit_BelowMinimumGivesUpImmediately(t *testing.T) {
	t.Parallel()
	var logs syncBuffer
	f := &fakeCaches{createErr: errors.New(belowMinimum)}
	clk := &fakeClock{t: time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)}
	m := NewManager(f, "gemini-3.7-flash", Options{Logger: log.New(&logs, "", 0)})
	m.now = clk.now
	m.retryBackoff = []time.Duration{15 * time.Second, 30 * time.Second}
	sys := &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}

	m.Init(context.Background(), sys, nil)
	waitFor(t, testWait, func() bool { return m.Snapshot().Failed })

	// No retry is pending — not after the backoff, not ever.
	clk.advance(time.Hour)
	m.Init(context.Background(), sys, nil)
	time.Sleep(20 * time.Millisecond)
	if got := f.createCount.Load(); got != 1 {
		t.Errorf("createCount = %d, want 1 (a prompt does not grow between attempts)", got)
	}

	line := logs.String()
	if strings.Count(line, "\n") != 1 {
		t.Errorf("want exactly one log line, got:\n%s", line)
	}
	// It has to retract the startup line's "context cache: enabled"
	// claim in the same words, and it has to say there is nothing to
	// act on — that is the diagnostic cost the issue is about.
	for _, want := range []string{"context cache: disabled", "gemini-3.7-flash", "retrying cannot change that", "Nothing to fix"} {
		if !strings.Contains(line, want) {
			t.Errorf("the disable line does not say %q: %s", want, line)
		}
	}
	// The provider's own numbers survive, so an operator who wants to
	// know how far under the floor they are can read it here.
	if !strings.Contains(line, "2373 tokens") {
		t.Errorf("the disable line dropped the provider's own explanation: %s", line)
	}
}

// TestInit_BelowMinimumDoesNotCaptureTheRetryingFailures guards the
// carve-out's blast radius from the other side: a 403 during IAM
// propagation must still get its full schedule. A predicate that
// over-matches here re-introduces #707, which cost a live GKE session
// full input price on all 365K of its billed input tokens.
func TestInit_BelowMinimumDoesNotCaptureTheRetryingFailures(t *testing.T) {
	t.Parallel()
	f := &fakeCaches{createErr: errors.New("rpc error: code = PermissionDenied desc = 403")}
	m, clk := newRetryManager(f)
	sys := &genai.Content{Parts: []*genai.Part{{Text: "sys"}}}

	m.Init(context.Background(), sys, nil)
	waitFor(t, testWait, func() bool { return f.createCount.Load() == 1 })
	if m.Snapshot().Failed {
		t.Fatal("a 403 went permanently failed on the first attempt; #707 is back")
	}
	clk.advance(16 * time.Second)
	m.Init(context.Background(), sys, nil)
	waitFor(t, testWait, func() bool { return f.createCount.Load() == 2 })
}
