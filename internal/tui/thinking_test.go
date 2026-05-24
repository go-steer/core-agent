// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/go-steer/core-agent/config"
)

// TestThinkingPhrase_WrapsAndAnchors pins two contracts at once:
//
//   - thinkingPhrase(0) MUST return the anchor phrase ("Thinking...")
//     so every turn begins with the unambiguous indicator before the
//     cheeky rotator wanders into the AI / sci-fi / CS jokes. Users
//     who don't get the joke still see "Thinking..." first.
//   - thinkingPhrase wraps cleanly past the end of the slice so the
//     rotator never panics on long-running turns.
//
// DO NOT silence this test if it breaks. A regression here either (a)
// removes the user-friendly anchor, leaving newcomers confused about
// what "Reticulating splines…" means, or (b) crashes the rotator on a
// long turn, which is exactly when users most need the indicator.
func TestThinkingPhrase_WrapsAndAnchors(t *testing.T) {
	t.Parallel()
	if got := thinkingPhrase(0); got != "Thinking..." {
		t.Errorf("thinkingPhrase(0) = %q, want anchor %q", got, "Thinking...")
	}
	n := len(thinkingPhrases)
	if got, want := thinkingPhrase(n), thinkingPhrases[0]; got != want {
		t.Errorf("thinkingPhrase(n) did not wrap to index 0; got %q want %q", got, want)
	}
	if got, want := thinkingPhrase(-1), thinkingPhrases[n-1]; got != want {
		t.Errorf("thinkingPhrase(-1) did not wrap to last index; got %q want %q", got, want)
	}
	if n < 10 {
		t.Errorf("thinkingPhrases has %d entries; the spec calls for 10-15 so the rotator stays interesting", n)
	}
}

// TestRenderMessage_StreamingShowsThinkingBetweenSegments pins the
// chat indicator contract: while the model is in StateStreaming AND
// the agent is between assistant segments (no in-progress message
// yet, OR a tool call just closed the previous segment), the chat
// MUST render the rotating thinking indicator at the bottom.
//
// DO NOT silence this test if it breaks. A failure means the user
// sends a prompt (or watches a tool call resolve) and stares at an
// empty space until the next chunk arrives — the exact "is anything
// happening?" UX gap this feature closes.
func TestRenderMessage_StreamingShowsThinkingBetweenSegments(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.history.Append(Message{Role: RoleUser, Text: "hello"})
	m.currentAssistantIdx = -1 // no in-progress assistant
	m.state = StateStreaming
	m.thinkingIdx = 0
	m.refreshViewport()

	// View() covers header + viewport + input + footer; the footer
	// ALSO says "Thinking..." in streaming mode, so pull the viewport
	// region directly to assert the chat-window indicator.
	body := stripANSI(m.viewport.View())
	if !strings.Contains(body, "Thinking...") {
		t.Errorf("expected anchor phrase 'Thinking...' in viewport while streaming with no in-progress assistant; got:\n%s", body)
	}

	// Rotate and verify the next phrase shows up after a refresh.
	m.thinkingIdx = 1
	m.refreshViewport()
	body = stripANSI(m.viewport.View())
	if !strings.Contains(body, thinkingPhrases[1]) {
		t.Errorf("expected rotated phrase %q in viewport; got:\n%s", thinkingPhrases[1], body)
	}

	// Once a chunk arrives the assistant message is created and the
	// indicator at the bottom of the chat must give way to the
	// response text.
	idx := m.history.Append(Message{Role: RoleAssistant, Text: "actual response text"})
	m.currentAssistantIdx = idx
	m.refreshViewport()
	body = stripANSI(m.viewport.View())
	if strings.Contains(body, "Thinking...") || strings.Contains(body, thinkingPhrases[1]) {
		t.Errorf("thinking indicator should be hidden once an assistant segment has content; got:\n%s", body)
	}
	if !strings.Contains(body, "actual response text") {
		t.Errorf("response text should be visible after first chunk; got:\n%s", body)
	}

	// Simulate a tool call closing the segment: clear currentAssistantIdx
	// and append a tool entry. The thinking indicator MUST come back at
	// the bottom while we wait for the next assistant segment.
	m.history.Append(Message{Role: RoleTool, Text: "bash · $ ls"})
	m.currentAssistantIdx = -1
	m.refreshViewport()
	body = stripANSI(m.viewport.View())
	if !strings.Contains(body, "Thinking...") && !strings.Contains(body, thinkingPhrases[1]) {
		t.Errorf("thinking indicator should reappear after a tool call closes the previous segment; got:\n%s", body)
	}
}

// TestRenderThinkingLine_NoItalic pins the visibility contract for the
// in-chat indicator: the rotating phrase must NOT be styled with
// italic (SGR 3). VS Code's integrated terminal — among others —
// silently drops italic spans depending on the font, which surfaced as
// "I see the spinner but no text" in v0.1.2 dogfood. Bold + foreground
// color is the most portable visible styling.
//
// DO NOT silence this test if it breaks. Re-enabling italic on the
// indicator brings back a real bug that hides the affordance from a
// large chunk of the user base on dev-friendly terminal hosts.
// TestRenderThinkingLine_PhraseSurvives is the floor for the chat
// indicator: regardless of how the styling is wired (lipgloss System
// style, raw ANSI, plain text, or whatever future iteration), the
// human-readable phrase MUST appear in the output. We've burned
// several round-trips on stylings that rendered as "no text at all"
// on VS Code's terminal; the regression test guarantees that even
// when we tweak the styling, we don't ship a build whose chat
// indicator is invisible.
//
// DO NOT silence this test — failure means the chat thinking
// indicator regressed to invisible/empty for users.
func TestRenderThinkingLine_PhraseSurvives(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	m.thinkingIdx = 0
	out := m.renderThinkingLine()
	if !strings.Contains(stripANSI(out), "Thinking...") {
		t.Errorf("renderThinkingLine() lost the phrase; got: %q", out)
	}
}

// TestThinkingPhrases_AsciiOnly pins two related contracts:
//
//  1. Every phrase ends in literal "..." (three ASCII dots), not the
//     Unicode ellipsis "…" (U+2026). Reported in dogfood: VS Code's
//     terminal silently rendered "…" as zero-width on the user's font,
//     making the phrase look truncated.
//  2. No phrase contains non-ASCII characters at all. Same reason —
//     fancy glyphs are gambling against the user's installed font.
//
// DO NOT silence this test if it breaks. Re-introducing "…" or a
// stylized prefix glyph hides the indicator on a non-trivial slice of
// terminals and brings back the "I see nothing" bug.
func TestThinkingPhrases_AsciiOnly(t *testing.T) {
	t.Parallel()
	for i, p := range thinkingPhrases {
		if !strings.HasSuffix(p, "...") {
			t.Errorf("phrase[%d] = %q; must end in ASCII '...' not Unicode '…'", i, p)
		}
		for _, r := range p {
			if r > 127 {
				t.Errorf("phrase[%d] = %q contains non-ASCII rune %q (U+%04X); use ASCII only for portability", i, p, r, r)
			}
		}
	}
}

// TestRenderFooter_ContainsThinkingText pins that the streaming
// footer literally includes "Thinking" — without making any
// styling claim. We rolled back the brand-cyan wrap on a dogfood
// report that the styled footer dropped the word entirely on VS
// Code's terminal. This test guards the floor (the word is there)
// without re-introducing the regression of asserting the styling.
func TestRenderFooter_ContainsThinkingText(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.state = StateStreaming
	if !strings.Contains(stripANSI(m.renderFooter()), "Thinking") {
		t.Errorf("streaming footer missing literal 'Thinking' word; got: %q", m.renderFooter())
	}
}

// TestRenderMessage_IdleAssistantNoThinking guards against the inverse
// regression: when the agent is idle (e.g. a stale assistant message
// whose render somehow gets re-evaluated), the thinking indicator must
// NOT appear. Otherwise users see "Thinking..." forever after a turn
// finishes — worse than no indicator at all.
func TestRenderMessage_IdleAssistantNoThinking(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.history.Append(Message{Role: RoleUser, Text: "hi"})
	m.history.Append(Message{Role: RoleAssistant}) // empty assistant
	m.state = StateIdle                            // not streaming
	m.refreshViewport()

	body := stripANSI(m.viewport.View())
	if strings.Contains(body, "Thinking...") {
		t.Errorf("thinking indicator should NOT appear in chat while idle; got:\n%s", body)
	}
}
