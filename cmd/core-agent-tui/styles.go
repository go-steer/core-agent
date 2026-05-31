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

import "github.com/charmbracelet/lipgloss"

// Minimal lipgloss palette + styles needed by the session picker
// (the only bubble-tea screen left in this binary after the
// remote-TUI-on-core-tui flip; everything else now flows through
// coretui.Run which manages its own theme).
//
// Kept in a separate file so the picker reads cleanly and so the
// next reader sees there's no longer a sprawling style system —
// just enough to render a list with a header and a footer.

var (
	colorAccent     = lipgloss.Color("69")  // cyan-blue
	colorMuted      = lipgloss.Color("244") // grey
	colorErr        = lipgloss.Color("196") // red
	colorBackground = lipgloss.Color("236") // panel bg
)

var (
	styleStatusBar = lipgloss.NewStyle().
			Foreground(lipgloss.Color("255")).
			Background(colorBackground).
			Padding(0, 1)
	styleFooter = lipgloss.NewStyle().
			Foreground(colorMuted).
			Padding(0, 1)
	styleBubbleErr = lipgloss.NewStyle().
			Foreground(colorErr).
			Bold(true)
	styleHint = lipgloss.NewStyle().
			Foreground(colorMuted).
			Italic(true)
)
