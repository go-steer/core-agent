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

// #975: context-window utilization was the previous model call's input
// token count and nothing else, so everything appended between two calls
// — which is where a large tool result lands — was invisible to the
// number compaction and the UI both read.

package usage_test

import (
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/usage"
)

func TestContextWindowUsed_IncludesTheUnmeasuredTail(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	tr.Append("gemini-3.5-flash", 1000, 50, usage.Pricing{})

	if got := tr.ContextWindowUsed(); got != 1000 {
		t.Fatalf("ContextWindowUsed with nothing appended = %d, want 1000", got)
	}

	// A tool result lands. Pre-#975 this changed nothing at all, which
	// is the entire defect: the next request carries it and the number
	// the threshold check reads does not.
	tr.AddPendingContextBytes(9000)

	want := 1000 + usage.EstimateTokensFromBytes(9000)
	if got := tr.ContextWindowUsed(); got != want {
		t.Errorf("ContextWindowUsed after a 9000-byte tool result = %d, want %d", got, want)
	}
}

func TestContextWindowUsed_TailIsClearedByTheNextMeasurement(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	tr.Append("gemini-3.5-flash", 1000, 50, usage.Pricing{})
	tr.AddPendingContextBytes(9000)

	// The next call's InputTokens is a real measurement of the whole
	// conversation including that result, so continuing to add the
	// estimate would double-count it.
	tr.Append("gemini-3.5-flash", 4200, 50, usage.Pricing{})

	if got := tr.PendingContextBytes(); got != 0 {
		t.Errorf("PendingContextBytes after a new measurement = %d, want 0", got)
	}
	if got := tr.ContextWindowUsed(); got != 4200 {
		t.Errorf("ContextWindowUsed after a new measurement = %d, want the measured 4200", got)
	}
}

func TestContextWindowUsedEstimated_SaysWhichKindOfNumberItIs(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	tr.Append("gemini-3.5-flash", 1000, 50, usage.Pricing{})

	if used, estimated := tr.ContextWindowUsedEstimated(); used != 1000 || estimated {
		t.Errorf("straight after a measurement = (%d, %v), want (1000, false)", used, estimated)
	}
	tr.AddPendingContextBytes(600)
	if _, estimated := tr.ContextWindowUsedEstimated(); !estimated {
		t.Errorf("with an unmeasured tail, estimated = false; a surface would render a guess as a measurement")
	}
}

// Bytes appended before any turn has landed are deliberately not
// reported: there is no measured floor to add them to, so the estimate
// on its own is a number with no relationship to the window. 0 keeps the
// established "unknown, suppress the segment" contract.
func TestContextWindowUsed_NoTurnYetStaysUnknown(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	tr.AddPendingContextBytes(50_000)

	used, estimated := tr.ContextWindowUsedEstimated()
	if used != 0 || estimated {
		t.Errorf("ContextWindowUsedEstimated before any turn = (%d, %v), want (0, false)", used, estimated)
	}
}

// The ratio is a deliberate over-count, and the direction is the point:
// under-counting means the threshold does not fire and the defect
// survives, over-counting means compaction fires early and costs a call.
func TestEstimateTokensFromBytes_OverCountsRatherThanUnder(t *testing.T) {
	t.Parallel()
	const prose = 4 // the usual English rule of thumb
	if usage.BytesPerTokenEstimate >= prose {
		t.Fatalf("BytesPerTokenEstimate = %d; must be below the %d-byte prose ratio, because what this measures is JSON, YAML and log output",
			usage.BytesPerTokenEstimate, prose)
	}
	if got, proseGot := usage.EstimateTokensFromBytes(12_000), 12_000/prose; got <= proseGot {
		t.Errorf("EstimateTokensFromBytes(12000) = %d, want more than the prose estimate %d", got, proseGot)
	}
	for _, n := range []int{0, -1, -9999} {
		if got := usage.EstimateTokensFromBytes(n); got != 0 {
			t.Errorf("EstimateTokensFromBytes(%d) = %d, want 0", n, got)
		}
	}
}

func TestAddPendingContextBytes_IgnoresNonPositive(t *testing.T) {
	t.Parallel()
	tr := usage.NewTracker()
	tr.Append("gemini-3.5-flash", 1000, 50, usage.Pricing{})
	tr.AddPendingContextBytes(0)
	tr.AddPendingContextBytes(-5000)
	if got := tr.PendingContextBytes(); got != 0 {
		t.Errorf("PendingContextBytes = %d, want 0; a negative must not shrink the estimate", got)
	}
}
