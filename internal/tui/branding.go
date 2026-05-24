// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import "github.com/charmbracelet/lipgloss"

// Brand colors. Fixed hex (not AdaptiveColor) because the wordmark is
// brand identity and shouldn't shift between light/dark terminals.
//
// Palette is a Dracula-inspired duotone with a green liveness cursor:
//   - brandViolet — the primary name color, calm and distinct from
//     terminal text. Reads well on both dark and light backgrounds.
//   - brandCyan   — accent separator, picks up subtle highlights.
//   - brandSlate  — muted prefix/suffix that doesn't compete with the
//     agent name for attention.
//   - brandGreen  — the cursor glyph; signals "alive, accepting input".
var (
	brandViolet = lipgloss.Color("#BD93F9")
	brandCyan   = lipgloss.Color("#5FD7FF")
	brandSlate  = lipgloss.Color("#6272A4")
	brandGreen  = lipgloss.Color("#50FA7B")
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
	cursor := lipgloss.NewStyle().Foreground(brandGreen).Bold(true).Render("█")
	if identity == "" || identity == "core-agent" {
		return wordmark + " " + cursor
	}
	sep := lipgloss.NewStyle().Foreground(brandSlate).Render(" · ")
	name := lipgloss.NewStyle().Foreground(brandCyan).Bold(true).Render(identity)
	return wordmark + sep + name + " " + cursor
}

// emptyStateHint is shown inside the viewport when the chat history is
// empty. The slate italic matches the brand palette without needing a
// full splash banner.
func emptyStateHint() string {
	return lipgloss.NewStyle().Foreground(brandSlate).Italic(true).
		Render("> Type a message and hit Enter. /help for commands.")
}
