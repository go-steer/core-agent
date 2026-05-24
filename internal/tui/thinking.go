// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

// thinkingTickInterval is how long each cheeky thinking phrase stays
// on screen before the next one rotates in. Three seconds is long
// enough to read without effort and short enough that the indicator
// still feels alive.
const thinkingTickInterval = 3 * 1000 // milliseconds — see update.go for the tea.Tick wiring

// thinkingPhrases is a curated set of "Thinking..." alternatives shown
// in the chat window while the model composes a response. The first
// entry — plain "Thinking..." — anchors the indicator: a fresh turn
// always starts there so the affordance is unambiguous before the
// rotator wanders into the AI / sci-fi / CS jokes.
var thinkingPhrases = []string{
	"Thinking...",
	"Consulting the latent space...",
	"Sampling from the distribution...",
	"Reticulating splines...",
	"Computing the answer to the ultimate question...",
	"Spinning up the attention heads...",
	"Asking Stack Overflow nicely...",
	"Untangling pointer chains...",
	"Bargaining with the loss function...",
	"Compiling a thoughtful response...",
	"Defragmenting cache lines...",
	"Negotiating with the Vogons...",
	"Brewing a fresh stack frame...",
	"Plotting a hyperspace course...",
	"Resolving promises...",
	"Eval'ing your prompt...",
}

// thinkingPhrase returns the phrase at idx, wrapping around the slice.
// Negative inputs are normalized to a positive index. Always returns a
// non-empty string so the indicator never flickers blank.
func thinkingPhrase(idx int) string {
	n := len(thinkingPhrases)
	if n == 0 {
		return "Thinking..."
	}
	i := idx % n
	if i < 0 {
		i += n
	}
	return thinkingPhrases[i]
}

// renderThinkingLine builds the in-chat thinking indicator. The
// rotating phrase is styled via the existing m.styles.System path
// (italic + a muted/system color via lipgloss.AdaptiveColor).
//
// Long history (see PR #65 thread): every fixed-color or raw-ANSI
// styling we tried got swallowed by the user's VS Code terminal
// renderer — italic+cyan via lipgloss, bold+cyan with a `▶` prefix,
// raw `\x1b[1;3;36m`, etc. The only thing that visibly worked on
// that host was the existing System / Assistant lipgloss styles
// (which use AdaptiveColor and apparently survive whatever path
// transformation the host is doing).
//
// "System" gives us italic + a system color out of the box, which
// matches the user's "color and maybe italics" ask without re-
// introducing the styling that consistently broke.
func (m *Model) renderThinkingLine() string {
	return m.styles.System.Render(thinkingPhrase(m.thinkingIdx))
}
