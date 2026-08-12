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

package attach

import (
	"fmt"
	"strings"
)

// RenderGuardrails projects a GuardrailInfo into the plain-text block
// the /guardrail slash prints. Lives in package attach — not in a
// TUI-specific package — so the embedded (cmd/core-agent) and remote
// (core-tui) surfaces render identical output from the same struct,
// the same arrangement RenderUsage uses.
//
// Layout:
//
//	Guardrails: HALTED — cost_ceiling
//	  watchdog     enforce · ok
//	  cost ceiling turn $0.50 · session $8.42 of $10.00 · TRIPPED
//	    per-session cost ceiling exceeded: …
//	  Reset with: /guardrail reset +5   (a bare reset would re-trip)
//
// The trailing hint line is the point of the whole block: an operator
// who reads "TRIPPED" needs the next keystroke, not a status code.
func RenderGuardrails(info GuardrailInfo) string {
	var sb strings.Builder

	if info.Halted {
		var which []string
		if info.Watchdog.Tripped {
			which = append(which, GuardrailWatchdog)
		}
		if info.CostCeiling.Tripped {
			which = append(which, GuardrailCostCeiling)
		}
		fmt.Fprintf(&sb, "Guardrails: HALTED — %s\n", strings.Join(which, " + "))
	} else {
		sb.WriteString("Guardrails: running\n")
	}

	mode := info.Watchdog.Mode
	if mode == "" {
		mode = "off"
	}
	state := "ok"
	if info.Watchdog.Tripped {
		state = "TRIPPED"
	} else if mode == "off" {
		state = "not armed"
	}
	fmt.Fprintf(&sb, "  watchdog      %s · %s\n", mode, state)
	if info.Watchdog.Reason != "" {
		fmt.Fprintf(&sb, "    %s\n", info.Watchdog.Reason)
	}

	cc := info.CostCeiling
	var parts []string
	if cc.MaxTurnUSD > 0 {
		parts = append(parts, fmt.Sprintf("turn $%.2f", cc.MaxTurnUSD))
	}
	if cc.MaxSessionUSD > 0 {
		parts = append(parts, fmt.Sprintf("session $%.4f of $%.2f", cc.SessionCostUSD, cc.MaxSessionUSD))
	} else {
		parts = append(parts, fmt.Sprintf("session $%.4f spent, no cap", cc.SessionCostUSD))
	}
	if cc.Tripped {
		parts = append(parts, "TRIPPED")
	}
	fmt.Fprintf(&sb, "  cost ceiling  %s\n", strings.Join(parts, " · "))
	if cc.Reason != "" {
		fmt.Fprintf(&sb, "    %s\n", cc.Reason)
	}

	// The actionable line. Only rendered when something is actually
	// halted — telling a healthy session how to reset invites the
	// reflex reset that hides why a later halt happened.
	switch {
	case cc.Tripped && cc.WouldRetrip:
		fmt.Fprintf(&sb,
			"  Reset with: /guardrail reset +<usd>   (spend is already at the ceiling; a bare reset would re-trip)\n")
	case info.Halted:
		sb.WriteString("  Reset with: /guardrail reset\n")
	}
	return sb.String()
}
