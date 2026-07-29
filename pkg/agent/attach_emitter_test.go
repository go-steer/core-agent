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
	"math"
	"sync"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// TestSetOperatorEventEmitter_UsageUpdatePopulatesLastTurn is the pin for
// authoritative per-turn cost delivery. When tracker.Append fires
// the emit callback wired by SetOperatorEventEmitter, the resulting
// UsageUpdate must carry LastTurn with the just-committed cost — so
// the remote TUI's per-turn footer renders without depending on a
// separate client-side pricing lookup.
func TestSetOperatorEventEmitter_UsageUpdatePopulatesLastTurn(t *testing.T) {
	t.Parallel()
	tracker := usage.NewTracker()
	a := &Agent{tracker: tracker}

	// Capture whatever gets emitted so we can inspect it.
	var (
		mu       sync.Mutex
		captured []attach.UsageUpdate
	)
	a.SetOperatorEventEmitter(func(eventType string, payload any) {
		if eventType != attach.EventUsageUpdate {
			return
		}
		p, ok := payload.(attach.UsageUpdate)
		if !ok {
			t.Errorf("payload wrong type: %T", payload)
			return
		}
		mu.Lock()
		captured = append(captured, p)
		mu.Unlock()
	})

	p := usage.Pricing{InputPerMTok: 1.25, CachedInputPerMTok: 0.3125, OutputPerMTok: 5.00}
	tracker.AppendUsage("gemini-3.1-pro", usage.TurnUsage{
		InputTokens:       10_000,
		CachedInputTokens: 8_000,
		OutputTokens:      500,
	}, p)

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected exactly one UsageUpdate emission, got %d", len(captured))
	}
	got := captured[0]
	if got.LastTurn == nil {
		t.Fatal("LastTurn missing — remote TUI per-turn footer won't render")
	}
	if got.LastTurn.TokensIn != 10_000 || got.LastTurn.TokensOut != 500 {
		t.Errorf("LastTurn tokens = %+v, want in=10000 out=500", got.LastTurn)
	}
	if got.LastTurn.TokensInCached != 8_000 {
		t.Errorf("LastTurn.TokensInCached = %d, want 8_000", got.LastTurn.TokensInCached)
	}
	if got.LastTurn.Model != "gemini-3.1-pro" {
		t.Errorf("LastTurn.Model = %q, want gemini-3.1-pro", got.LastTurn.Model)
	}
	// Cost with cache discount: 2k uncached @ $1.25/M + 8k cached @ $0.3125/M + 500 @ $5/M.
	wantCost := 0.002*1.25 + 0.008*0.3125 + 0.0005*5.00
	if math.Abs(got.LastTurn.CostUSD-wantCost) > 1e-9 {
		t.Errorf("LastTurn.CostUSD = %v, want %v", got.LastTurn.CostUSD, wantCost)
	}
	// Cumulative totals still populated for the session-total path.
	if got.CostUSDTotal != got.LastTurn.CostUSD {
		t.Errorf("first-turn CostUSDTotal (%v) should equal LastTurn.CostUSD (%v)",
			got.CostUSDTotal, got.LastTurn.CostUSD)
	}
}
