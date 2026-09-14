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

// The agent side of the context-reduction failure row (#908) — see
// pkg/attach/context_reduction_events.go for the row itself and for why
// an eventlog append is the mechanism.

package agent

import (
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// recordContextReductionFailure writes the durable row for an automatic
// compaction or checkpoint that failed and was swallowed.
//
// Called from the pending-drain paths AFTER they have logged, so the two
// surfaces always say the same thing and neither is load-bearing for the
// other. Both drains run before the turn's cancel is registered, so
// queueOutOfBandEvent flushes straight through rather than parking the
// row until the turn ends.
//
// Silent no-op without an eventlog (the queue drops it), which is the
// same deal the guardrail rows make: no log, nowhere to write, and the
// in-memory behavior is exactly what it was before.
//
// consecutiveFailures and cooldownTurns describe the compaction path's
// backoff; pass 0/0 from the checkpoint path, which has none.
func (a *Agent) recordContextReductionFailure(operation string, err error, consecutiveFailures, cooldownTurns int) {
	if a == nil || err == nil {
		return
	}
	a.queueOutOfBandEvent(attach.NewContextReductionFailedEvent(
		operation, err.Error(), consecutiveFailures, cooldownTurns))
}

// recordContextReductionDegraded writes the durable row for a context
// reduction that is still happening but on an assumption or a fallback
// rather than on the real thing (#974) — an unknown context window, or a
// compaction done by mechanical truncation because the summarizer was
// unavailable.
//
// Separate from the failure path above for the reason the row's own
// documentation gives: "did not run" and "ran, worse" are opposite
// readings during an incident. Callers own the at-most-once discipline;
// this writes whatever it is handed.
func (a *Agent) recordContextReductionDegraded(operation, detail string) {
	if a == nil || operation == "" {
		return
	}
	a.queueOutOfBandEvent(attach.NewContextReductionDegradedEvent(operation, detail))
}
