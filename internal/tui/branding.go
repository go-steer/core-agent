// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import "github.com/charmbracelet/lipgloss"

// Brand colors. Fixed hex (not AdaptiveColor) because the wordmark is
// brand identity and shouldn't shift between light/dark terminals.
//
// Palette is a magenta-family monochromatic — wordmark + identity +
// cursor all sit in the violet→pink range so the brand line reads as
// one coherent visual rather than three competing colors:
//
//   - brandViolet — the wordmark ("core-agent"). Saturated purple
//     with a magenta lean; reads as "magenta" on most terminals.
//   - brandPink   — the agent identity (DisplayName / AppName). Sister
//     hue, sits next to violet on the color wheel for a calm gradient.
//   - brandPinkBright — the cursor glyph. Brighter pink "pings"
//     attention without breaking the family. Still signals "alive,
//     accepting input" without the previous green's discord.
//   - brandSlate  — muted separator/prefix that doesn't compete.
//   - brandCyan   — retained for tests and spinner/accent fallbacks
//     elsewhere in the TUI; no longer in the brand line itself.
var (
	brandViolet     = lipgloss.Color("#BD93F9")
	brandPink       = lipgloss.Color("#FF79C6")
	brandPinkBright = lipgloss.Color("#FFB6E1")
	brandSlate      = lipgloss.Color("#6272A4")
	brandCyan       = lipgloss.Color("#5FD7FF")
)

// headerBrand renders the persistent brand line shown on the left of
// the status header. Format:
//
//	core-agent · <identity> █
//
// `<identity>` is either the agent's DisplayName (when configured via
// agent.display_name in .agents/config.json) or its AppName ("core-agent"
// by default). When identity equals "core-agent" the bare wordmark
// shows without the redundant "· core-agent" suffix.
//
// Operators see the cursor color (green) flash as input lands; the
// violet identity is the visual anchor for "which agent am I talking
// to" in multi-window setups.
func headerBrand(identity string) string {
	wordmark := lipgloss.NewStyle().Foreground(brandViolet).Bold(true).Render("core-agent")
	cursor := lipgloss.NewStyle().Foreground(brandPinkBright).Bold(true).Render("█")
	if identity == "" || identity == "core-agent" {
		return wordmark + " " + cursor
	}
	sep := lipgloss.NewStyle().Foreground(brandSlate).Render(" · ")
	name := lipgloss.NewStyle().Foreground(brandPink).Bold(true).Render(identity)
	return wordmark + sep + name + " " + cursor
}

// emptyStateHint is shown inside the viewport when the chat history is
// empty. The slate italic matches the brand palette without needing a
// full splash banner.
func emptyStateHint() string {
	return lipgloss.NewStyle().Foreground(brandSlate).Italic(true).
		Render("> Type a message and hit Enter. /help for commands.")
}
