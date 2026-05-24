// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import "github.com/charmbracelet/bubbles/key"

// KeyMap centralizes the keybindings the TUI handles directly. Bubble
// Tea's textarea consumes most other keys (typing, arrows, etc.).
type KeyMap struct {
	Submit     key.Binding
	Newline    key.Binding
	Cancel     key.Binding // Ctrl+C — interrupts current turn or exits
	ClearView  key.Binding // Ctrl+L — clears viewport (history preserved)
	ClearInput key.Binding // Ctrl+U — clears the textarea (shell convention)
	ScrollUp   key.Binding
	ScrollDown key.Binding
	LineUp     key.Binding // Up arrow — recalls history when input empty
	LineDown   key.Binding // Down arrow — moves forward through recall

	// Permission modal: y allow once, n deny, s allow for the session,
	// v allow this verb (e.g. `git *`) for the session, t allow this
	// tool for the session, a allow always (persisted).
	ConfirmAllowOnce        key.Binding
	ConfirmDeny             key.Binding
	ConfirmAllowSession     key.Binding
	ConfirmAllowSessionVerb key.Binding
	ConfirmAllowSessionTool key.Binding
	ConfirmAllowAlways      key.Binding
}

// DefaultKeyMap returns Cogo's V1 bindings.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Submit: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "send"),
		),
		Newline: key.NewBinding(
			key.WithKeys("shift+enter", "ctrl+j"),
			key.WithHelp("shift+enter", "newline"),
		),
		Cancel: key.NewBinding(
			key.WithKeys("ctrl+c"),
			key.WithHelp("ctrl+c", "cancel/exit"),
		),
		ClearView: key.NewBinding(
			key.WithKeys("ctrl+l"),
			key.WithHelp("ctrl+l", "clear viewport"),
		),
		ClearInput: key.NewBinding(
			key.WithKeys("ctrl+u"),
			key.WithHelp("ctrl+u", "clear input"),
		),
		ScrollUp: key.NewBinding(
			key.WithKeys("pgup"),
			key.WithHelp("pgup", "scroll up"),
		),
		ScrollDown: key.NewBinding(
			key.WithKeys("pgdown"),
			key.WithHelp("pgdown", "scroll down"),
		),
		LineUp: key.NewBinding(
			key.WithKeys("up"),
			key.WithHelp("↑", "recall previous prompt (when input empty)"),
		),
		LineDown: key.NewBinding(
			key.WithKeys("down"),
			key.WithHelp("↓", "recall next prompt (when navigating history)"),
		),
		ConfirmAllowOnce: key.NewBinding(
			key.WithKeys("y"),
			key.WithHelp("y", "allow once"),
		),
		ConfirmDeny: key.NewBinding(
			key.WithKeys("n", "esc"),
			key.WithHelp("n/esc", "deny"),
		),
		ConfirmAllowSession: key.NewBinding(
			key.WithKeys("s"),
			key.WithHelp("s", "allow this call for the session"),
		),
		ConfirmAllowSessionVerb: key.NewBinding(
			key.WithKeys("v"),
			key.WithHelp("v", "allow this verb (e.g. `git *`) for the session"),
		),
		ConfirmAllowSessionTool: key.NewBinding(
			key.WithKeys("t"),
			key.WithHelp("t", "allow this tool for the session"),
		),
		ConfirmAllowAlways: key.NewBinding(
			key.WithKeys("a"),
			key.WithHelp("a", "always allow (persist)"),
		),
	}
}
