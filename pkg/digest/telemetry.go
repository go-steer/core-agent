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

package digest

import (
	"sync"
)

// Package-level telemetry counters. Kept minimal for #128 step 5:
// per-method call count + cumulative bytes saved. Full per-tool /
// per-session rollup is a follow-up; this surface gives operators
// day-one visibility into "how often did structural fire vs
// passthrough" via the /usage endpoint.
//
// Global rather than tracker-scoped because Process itself is
// called from every consumer (MCP wrap, agentic wrappers) without
// a tracker reference. Callers that need per-session isolation
// can snapshot before + after a session and diff.
var (
	telemetryMu sync.Mutex
	telemetry   = TelemetrySnapshot{
		MethodCounts: map[string]int64{},
		BytesSaved:   map[string]int64{},
	}
)

// TelemetrySnapshot is a point-in-time view of the digest telemetry
// counters. Copy-by-value so consumers can read without holding the
// package mutex.
//
// MethodCounts is calls-per-method (MethodPassthrough / MethodStructuralJSON /
// MethodLLMFallback). BytesSaved is the cumulative reduction — for
// each call, (raw_bytes - digest_bytes) accrued to the call's method.
// Passthrough always contributes 0 (raw == digest by definition);
// operators reading the surface should treat the delta between the
// two structural fields as the compression win.
type TelemetrySnapshot struct {
	MethodCounts map[string]int64
	BytesSaved   map[string]int64
}

// recordTelemetry updates the package counters for one Process call.
// Called from Process after the Result is finalized. Thread-safe.
func recordTelemetry(method string, rawBytes, digestBytes int) {
	if method == "" {
		return
	}
	saved := rawBytes - digestBytes
	if saved < 0 {
		// Digest is larger than raw — happens on tiny inputs where
		// the JSON marshaler adds whitespace, or on pruner
		// annotations that expand a short list. Don't credit
		// negative "savings" — bucket at zero.
		saved = 0
	}
	telemetryMu.Lock()
	defer telemetryMu.Unlock()
	telemetry.MethodCounts[method]++
	telemetry.BytesSaved[method] += int64(saved)
}

// Telemetry returns a defensive copy of the current counter state.
// Safe for concurrent readers; callers get an isolated snapshot they
// can walk without racing writers.
func Telemetry() TelemetrySnapshot {
	telemetryMu.Lock()
	defer telemetryMu.Unlock()
	out := TelemetrySnapshot{
		MethodCounts: make(map[string]int64, len(telemetry.MethodCounts)),
		BytesSaved:   make(map[string]int64, len(telemetry.BytesSaved)),
	}
	for k, v := range telemetry.MethodCounts {
		out.MethodCounts[k] = v
	}
	for k, v := range telemetry.BytesSaved {
		out.BytesSaved[k] = v
	}
	return out
}

// ResetTelemetry zeroes the package counters. Test-only helper —
// production consumers snapshot + diff rather than reset, since the
// counter is process-wide and other observers might be reading it.
func ResetTelemetry() {
	telemetryMu.Lock()
	defer telemetryMu.Unlock()
	telemetry = TelemetrySnapshot{
		MethodCounts: map[string]int64{},
		BytesSaved:   map[string]int64{},
	}
}
