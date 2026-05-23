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
	"strings"
)

// usagePanel is the live status-bar footer state: cumulative input
// + output tokens this session, plus a computed cost estimate. Updates
// on every model event arrival via ingest(); rendered each View()
// pass via render().
//
// Pricing is intentionally not wired here in v1 — the operator sees
// raw token counts plus a placeholder for cost ($-/-). Wiring the
// usage.PriceFor helper requires the cfg.Pricing table that lives in
// the core-agent process, not here; a follow-on PR can pass it
// alongside model_name via /status if needed. For now the headline
// is in/out tokens, which is the more interpretable number.
type usagePanel struct {
	modelName string
	inTokens  int
	outTokens int
	totalCost float64 // 0 in v1; see comment above
}

// ingest reads token usage from a model event's CustomMetadata and
// adds it to the cumulative counters. The metadata shape mirrors
// what genai returns and what usage.Tracker consumes — UsageMetadata
// is preserved verbatim through the eventlog.
func (u *usagePanel) ingest(meta map[string]any) {
	if meta == nil {
		return
	}
	// CustomMetadata is shaped by recording/eventlog convention. We
	// look for nested usage info under either "usage" or top-level
	// "input_tokens" / "output_tokens" keys.
	if in, ok := readInt(meta, "input_tokens"); ok {
		u.inTokens += in
	}
	if out, ok := readInt(meta, "output_tokens"); ok {
		u.outTokens += out
	}
	if u2, ok := meta["usage"].(map[string]any); ok {
		if in, ok := readInt(u2, "input_tokens"); ok {
			u.inTokens += in
		}
		if out, ok := readInt(u2, "output_tokens"); ok {
			u.outTokens += out
		}
	}
}

func readInt(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// render formats the panel for the footer line. If reconnecting is
// true OR connectionMsg is non-empty, the panel is replaced with the
// reconnect indicator — one line, one purpose, per the design doc.
//
// width is reserved for future "fit to terminal width" trimming —
// once cost figures + reconnect timers grow into the panel, narrow
// terminals will need a compact form. Unused in v1.
func (u usagePanel) render(_ int, reconnecting bool, connectionMsg string) string {
	if reconnecting {
		return styleWarn.Render("auto-reconnecting…")
	}
	if connectionMsg != "" {
		return styleWarn.Render(connectionMsg)
	}
	parts := []string{"/help"}
	if u.modelName != "" {
		parts = append(parts, u.modelName)
	}
	parts = append(parts, fmt.Sprintf("in %s", siTokens(u.inTokens)))
	parts = append(parts, fmt.Sprintf("out %s", siTokens(u.outTokens)))
	parts = append(parts, costStr(u.totalCost))
	return strings.Join(parts, "  ·  ")
}

// siTokens formats an integer with a K/M suffix at sensible
// thresholds: <1000 raw, <1M one decimal K, >=1M one decimal M.
func siTokens(n int) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fK", float64(n)/1000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

func costStr(c float64) string {
	if c == 0 {
		return "$—"
	}
	if c < 0.01 {
		return fmt.Sprintf("$%.1e", c)
	}
	return fmt.Sprintf("$%.3f", c)
}
