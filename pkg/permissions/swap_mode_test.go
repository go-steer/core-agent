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

package permissions

import (
	"sync"
	"testing"
)

func TestSwapMode_ReturnsWhatItReplaced(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeAsk})
	if prev := g.SwapMode(ModeYolo); prev != ModeAsk {
		t.Errorf("first swap returned %q, want ask", prev)
	}
	if prev := g.SwapMode(ModePlan); prev != ModeYolo {
		t.Errorf("second swap returned %q, want yolo", prev)
	}
	if prev := g.SwapMode("nonsense"); prev != ModePlan || g.Mode() != ModePlan {
		t.Errorf("unknown mode: returned %q, mode now %q; want plan and unchanged", prev, g.Mode())
	}
}

// Concurrent swaps each report the transition they actually made: read
// and write happen under one lock, so the reported "from" values chain
// into a single history with no transition reported twice.
func TestSwapMode_ConcurrentSwapsReportDistinctTransitions(t *testing.T) {
	t.Parallel()
	g := New(Options{Mode: ModeAsk})
	modes := []Mode{ModeYolo, ModePlan, ModeAcceptEdits, ModeAllow}
	const n = 200
	prevs := make(chan Mode, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prevs <- g.SwapMode(modes[i%len(modes)])
		}()
	}
	wg.Wait()
	close(prevs)
	// Each "from" is either the initial ask or a value some swap wrote.
	// Counting them: exactly one swap saw the initial ask.
	asks := 0
	for p := range prevs {
		if p == ModeAsk {
			asks++
		}
	}
	if asks != 1 {
		t.Errorf("%d swaps reported replacing the initial ask, want exactly 1 (no mode is ever written back as ask)", asks)
	}
}
