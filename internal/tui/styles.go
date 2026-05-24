// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import "github.com/charmbracelet/lipgloss"

// Styles centralizes Lip Gloss styling so the View has a single source
// of truth for colors, padding, and borders. AdaptiveColor pairs let
// each style render correctly on light and dark terminals.
type Styles struct {
	Header       lipgloss.Style
	HeaderAccent lipgloss.Style
	UserPrefix   lipgloss.Style
	UserText     lipgloss.Style
	Assistant    lipgloss.Style
	System       lipgloss.Style
	Error        lipgloss.Style
	InputBorder  lipgloss.Style
	Spinner      lipgloss.Style
	Footer       lipgloss.Style
	Confirm      lipgloss.Style
}

// DefaultStyles returns the Slice 2 visual identity. Explicit theme
// switching (light/dark forcing) is deferred to a later slice; for now
// every color is adaptive.
func DefaultStyles() Styles {
	muted := lipgloss.AdaptiveColor{Light: "#6c6c6c", Dark: "#9a9a9a"}
	accent := lipgloss.AdaptiveColor{Light: "#005f87", Dark: "#5fafff"}
	user := lipgloss.AdaptiveColor{Light: "#0050a0", Dark: "#87afff"}
	assistant := lipgloss.AdaptiveColor{Light: "#1e1e1e", Dark: "#d0d0d0"}
	systemColor := lipgloss.AdaptiveColor{Light: "#5f5f5f", Dark: "#a8a8a8"}
	errorc := lipgloss.AdaptiveColor{Light: "#af0000", Dark: "#ff5f5f"}
	border := lipgloss.AdaptiveColor{Light: "#bcbcbc", Dark: "#5f5f5f"}

	return Styles{
		Header: lipgloss.NewStyle().
			Foreground(muted),
		HeaderAccent: lipgloss.NewStyle().
			Foreground(accent).
			Bold(true),
		UserPrefix: lipgloss.NewStyle().
			Foreground(user).
			Bold(true),
		UserText: lipgloss.NewStyle().
			Foreground(user),
		Assistant: lipgloss.NewStyle().
			Foreground(assistant),
		System: lipgloss.NewStyle().
			Foreground(systemColor).
			Italic(true),
		Error: lipgloss.NewStyle().
			Foreground(errorc).
			Bold(true),
		InputBorder: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(border).
			Padding(0, 1),
		Spinner: lipgloss.NewStyle().
			Foreground(brandCyan).
			Bold(true),
		Footer: lipgloss.NewStyle().
			Foreground(muted),
		Confirm: lipgloss.NewStyle().
			Foreground(accent).
			Italic(true),
	}
}
