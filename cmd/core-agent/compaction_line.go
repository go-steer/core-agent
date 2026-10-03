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

package main

import (
	"fmt"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/usage"
)

// compactionBootLine states, in tokens, where automatic compaction fires
// for the daemon's configured model.
//
// A fraction alone is not enough to act on. "0.021" on a 1,048,576-token
// window is 22,020 tokens, and the 2026-10-03 A1 soak was first graded at
// 21,000 because whoever did the arithmetic assumed a 1,000,000-token
// window. The line also makes the effective threshold visible at all:
// before #1226 an operator's threshold was silently outranked by the
// per-tier defaults, and nothing anywhere said which one was in force.
//
// The threshold comes from the compactor's own ThresholdFor, the same
// path ShouldCompact resolves through, so the line cannot describe a
// threshold the daemon will not use. dev/uat/gke-drill/soak_verdict.py
// reads the token figure back out of the captured daemon log; change the
// wording and change its pattern too.
//
// The model is the one configured at boot. A session that switches model
// at runtime resolves its own threshold per turn; this line does not
// follow it.
func compactionBootLine(c *agent.DefaultCompactor, model string) string {
	thr := c.ThresholdFor(model)
	window := usage.ContextWindowSizeFor(model)
	if window <= 0 {
		// No catalogued window: compaction does not stop, it assumes one
		// (pkg/agent compactionWindowSize → AssumedContextWindowSize), so
		// the line states that figure rather than "unknown" — it is the
		// threshold the daemon will act on.
		return fmt.Sprintf("core-agent: compaction: fires at %.3g of %s's assumed %d-token window = %d tokens (no catalogued window size for this model)",
			thr, model, agent.AssumedContextWindowSize, int(thr*float64(agent.AssumedContextWindowSize)))
	}
	return fmt.Sprintf("core-agent: compaction: fires at %.3g of %s's %d-token window = %d tokens", thr, model, window, int(thr*float64(window)))
}
