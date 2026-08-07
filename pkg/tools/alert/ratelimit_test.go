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

package alert

import (
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock so rate-limit tests are
// deterministic (no sleeps, no wall-clock flake).
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.t = c.t.Add(d)
}

func TestRateLimiter_Unlimited(t *testing.T) {
	t.Parallel()
	rl, err := newRateLimiter("", nil)
	if err != nil {
		t.Fatalf("newRateLimiter: %v", err)
	}
	for i := 0; i < 100; i++ {
		if !rl.allow("a") {
			t.Fatalf("empty spec should never rate-limit; blocked at %d", i)
		}
	}
}

func TestRateLimiter_OnePerWindow(t *testing.T) {
	t.Parallel()
	// A fixed base time; the harness forbids time.Now() but a literal is fine.
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	rl, err := newRateLimiter("1/30s", clk.now)
	if err != nil {
		t.Fatalf("newRateLimiter: %v", err)
	}
	if !rl.allow("a") {
		t.Fatal("first call should be allowed")
	}
	if rl.allow("a") {
		t.Fatal("second call within the window should be denied")
	}
	clk.add(29 * time.Second)
	if rl.allow("a") {
		t.Fatal("still within window at 29s should be denied")
	}
	clk.add(2 * time.Second) // now 31s elapsed → a token has refilled
	if !rl.allow("a") {
		t.Fatal("after the window elapses the call should be allowed")
	}
}

func TestRateLimiter_PerTargetIndependent(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	rl, err := newRateLimiter("1/30s", clk.now)
	if err != nil {
		t.Fatalf("newRateLimiter: %v", err)
	}
	// Exhaust target "a" — must not affect target "b".
	if !rl.allow("a") {
		t.Fatal("a first call allowed")
	}
	if rl.allow("a") {
		t.Fatal("a second call denied")
	}
	if !rl.allow("b") {
		t.Fatal("b has its own bucket and should be allowed")
	}
}

func TestRateLimiter_BurstAllowance(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	rl, err := newRateLimiter("3/min", clk.now)
	if err != nil {
		t.Fatalf("newRateLimiter: %v", err)
	}
	// A fresh bucket holds the full allowance (burst=3).
	for i := 0; i < 3; i++ {
		if !rl.allow("a") {
			t.Fatalf("burst call %d should be allowed", i)
		}
	}
	if rl.allow("a") {
		t.Fatal("4th call in the same instant should exceed the burst and be denied")
	}
}

func TestNewRateLimiter_BadSpec(t *testing.T) {
	t.Parallel()
	if _, err := newRateLimiter("banana", nil); err == nil {
		t.Error("bad spec should error")
	}
}
