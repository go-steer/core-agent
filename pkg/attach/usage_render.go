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
	"time"
)

// RenderUsage projects a UsageInfo into the plain-text block the TUI's
// custom /usage slash prints. Lives in package attach rather than in a
// TUI-specific package so both the embedded (cmd/core-agent) and remote
// (internal/coretuiremote) adapters render identical output — this is
// the operator-facing view of GET /sessions/<id>/usage.
//
// Layout (three sections; sections with nothing to say are omitted):
//
//	Session totals
//	  <turns> turns · <in>/<cached>/<uncached> in tokens · <out> out
//	  cost $<actual>  (uncached ref $<ref>  → cache saved $<delta>)
//
//	Per model
//	  <name>: <turns> turns · in <in> (<cached> cached) · $<cost>
//
//	Per turn
//	  #<n> <hh:mm:ss> <model> · in <in> (<cached> cached) · out <out> · $<cost>
//
// Numbers use grouping commas + fixed 4-decimal dollar amounts to
// match the existing /stats formatting so operators can eyeball the
// two side-by-side without unit-conversion friction.
func RenderUsage(info UsageInfo) string {
	var sb strings.Builder

	// Section 1: session totals.
	sb.WriteString("Session totals\n")
	if info.Overall.Turns == 0 {
		sb.WriteString("  (no turns recorded yet)\n")
	} else {
		fmt.Fprintf(&sb, "  %d turn(s) · in %s (%s cached / %s uncached) · out %s\n",
			info.Overall.Turns,
			commas(info.Overall.InputTokens),
			commas(info.Overall.InputTokensCached),
			commas(info.Overall.InputTokensUncached),
			commas(info.Overall.OutputTokens),
		)
		if info.Overall.ThoughtsTokens > 0 {
			fmt.Fprintf(&sb, "  thoughts: %s tokens\n", commas(info.Overall.ThoughtsTokens))
		}
		fmt.Fprintf(&sb, "  cost $%.4f", info.Overall.CostUSD)
		if info.Overall.CostUSDUncachedReference > info.Overall.CostUSD {
			saved := info.Overall.CostUSDUncachedReference - info.Overall.CostUSD
			fmt.Fprintf(&sb, "  (uncached ref $%.4f → cache saved $%.4f)",
				info.Overall.CostUSDUncachedReference, saved)
		}
		sb.WriteString("\n")
	}

	// Section 2: per-model breakdown (only when populated; matches
	// AttachUsage's rule of emitting PerModel only for mixed sessions).
	if len(info.PerModel) > 0 {
		sb.WriteString("\nPer model\n")
		for name, t := range info.PerModel {
			fmt.Fprintf(&sb, "  %s: %d turn(s) · in %s (%s cached) · $%.4f",
				name, t.Turns, commas(t.InputTokens), commas(t.InputTokensCached), t.CostUSD)
			if t.CostUSDUncachedReference > t.CostUSD {
				fmt.Fprintf(&sb, "  (ref $%.4f)", t.CostUSDUncachedReference)
			}
			sb.WriteString("\n")
		}
	}

	// Section 3: per-turn history. Cap at the tail for long sessions
	// — a 500-turn autonomous run would otherwise blow past the
	// scrollback. Operators who want the full array can curl the
	// endpoint directly.
	if n := len(info.PerTurn); n > 0 {
		sb.WriteString("\nPer turn")
		const maxRows = 20
		start := 0
		if n > maxRows {
			start = n - maxRows
			fmt.Fprintf(&sb, " (last %d of %d)", maxRows, n)
		}
		sb.WriteString("\n")
		for _, t := range info.PerTurn[start:] {
			fmt.Fprintf(&sb, "  #%d %s %s · in %s",
				t.Turn, t.At.Format(time.TimeOnly), abbrevModel(t.Model), commas(t.InputTokens))
			if t.InputTokensCached > 0 {
				fmt.Fprintf(&sb, " (%s cached)", commas(t.InputTokensCached))
			}
			fmt.Fprintf(&sb, " · out %s · $%.4f\n",
				commas(t.OutputTokens), t.CostUSD)
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

// commas renders an int64 with thousands separators. Small helper
// keeps RenderUsage's format strings readable.
func commas(n int64) string {
	if n < 0 {
		return "-" + commas(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	first := len(s) % 3
	if first > 0 {
		b.WriteString(s[:first])
		if len(s) > first {
			b.WriteByte(',')
		}
	}
	for i := first; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}

// abbrevModel drops the common gemini-3.1- prefix so per-turn rows
// stay narrow enough for typical terminal widths. Passes through
// anything shorter or that doesn't match the prefix.
func abbrevModel(name string) string {
	if name == "" {
		return "-"
	}
	const prefix = "gemini-"
	if strings.HasPrefix(name, prefix) {
		return name[len(prefix):]
	}
	return name
}
