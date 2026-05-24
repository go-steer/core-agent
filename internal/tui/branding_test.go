// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/go-steer/core-agent/config"
)

// TestView_HeaderAlwaysShowsBrandAndStatus pins the persistent header
// contract: the very first row of View() output must carry both the
// brand wordmark (left) and the model + cwd + provider + mode status
// info (right), regardless of viewport state.
//
// DO NOT silence this test if it breaks. A failure here means users
// open the TUI and the top of the screen has no header — exactly the
// regression that motivated this test. If the layout legitimately
// changes, replace the assertions with ones that prove the new
// arrangement still shows brand + status; never delete the contract.
func TestView_HeaderAlwaysShowsBrandAndStatus(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	for _, size := range []struct {
		name          string
		width, height int
	}{
		{"tight", 80, 24},
		{"wide", 160, 40},
		{"very-narrow", 50, 24},
	} {
		size := size
		t.Run(size.name, func(t *testing.T) {
			t.Parallel()
			m := NewModel(cfg, nil, "dark")
			m.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			out := m.View()
			stripped := stripANSI(out)
			lines := strings.Split(stripped, "\n")
			if len(lines) == 0 {
				t.Fatalf("View() empty")
			}
			head := lines[0]
			// Brand wordmark must show.
			if !strings.Contains(head, "go-steer / c[o]go") {
				t.Errorf("first row missing brand wordmark; got %q", head)
			}
			// Model name must show on the same row. At very-narrow
			// widths the renderer is allowed to truncate with "…"
			// (long model IDs like gemini-3.1-pro-preview-customtools
			// don't fit alongside the brand + status badge in 50
			// cols); we accept either the full name or a prefix
			// followed by the ellipsis.
			if !strings.Contains(head, cfg.Model.Name) {
				// Take a prefix unambiguous enough to identify the
				// model family but short enough to fit in any
				// supported width.
				prefix := cfg.Model.Name
				if len(prefix) > 16 {
					prefix = prefix[:16]
				}
				if !strings.Contains(head, prefix) || !strings.Contains(head, "…") {
					t.Errorf("first row missing model name %q (or %q+…); got %q",
						cfg.Model.Name, prefix, head)
				}
			}
			// Permission mode badge must show on the same row.
			if !strings.Contains(head, "ask") {
				t.Errorf("first row missing permission mode; got %q", head)
			}
		})
	}
}

// TestSpinner_UsesBrandStyle pins the spinner-style wiring. The
// Styles.Spinner field exists for a reason: the streaming spinner
// glyph should render in brand cyan + bold, not bubble's default
// foreground. The bug was that we defined Styles.Spinner but never
// assigned it to spinner.Model.Style, so spinner.View() always
// rendered with the default style. DO NOT silence this test —
// failure means the spinner regressed to default colors and users
// stop seeing the brand affordance during streaming.
func TestSpinner_UsesBrandStyle(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	m := NewModel(cfg, nil, "dark")
	want := DefaultStyles().Spinner
	if m.spinner.Style.String() != want.String() {
		t.Errorf("spinner.Style not wired to Styles.Spinner; got %v want %v",
			m.spinner.Style, want)
	}
}

// TestHeaderBrand_NoControlCharLeaks guards against the cursor block
// or other styled spans emitting stray newlines into the brand line.
// A newline inside headerBrand silently breaks the JoinHorizontal in
// renderHeader and the whole status line collapses.
func TestHeaderBrand_NoControlCharLeaks(t *testing.T) {
	t.Parallel()
	out := headerBrand()
	if regexp.MustCompile(`\r|\n`).MatchString(out) {
		t.Errorf("headerBrand() contains a newline; would break renderHeader layout. Raw bytes: %q", out)
	}
	stripped := stripANSI(out)
	if !strings.Contains(stripped, "go-steer / c[o]go") {
		t.Errorf("headerBrand() missing wordmark after ANSI strip: %q", stripped)
	}
}

// TestView_RowCountFitsHeight pins the OTHER invariant that broke the
// header on a real terminal: View() must return EXACTLY m.height rows.
// One row over and Bubble Tea's alt-screen scrolls one line, sending
// the header off the top — exactly what users see when the bug fires.
// This is easy to break by accident: appending "\n" instead of "" to
// JoinVertical adds 2 blank rows per newline rather than 1.
func TestView_RowCountFitsHeight(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	for _, h := range []int{16, 24, 40, 60} {
		h := h
		t.Run("", func(t *testing.T) {
			t.Parallel()
			m := NewModel(cfg, nil, "dark")
			m.Update(tea.WindowSizeMsg{Width: 100, Height: h})
			out := m.View()
			got := strings.Count(out, "\n") + 1
			if got != h {
				t.Errorf("View() at height %d returned %d rows; want exactly %d (overflow scrolls the header off the top)", h, got, h)
			}
		})
	}
}

// TestRenderHeader_FitsWidth pins the structural invariant that broke
// the header on a real terminal: the rendered first row must be EXACTLY
// m.width columns wide. If it's even one column wider the terminal
// wraps it onto a second row, Bubble Tea's screen positioning loses
// track of the header, and the user opens the TUI with no header at
// all. DO NOT silence — fix the layout so the line fits.
func TestRenderHeader_FitsWidth(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	for _, w := range []int{40, 80, 100, 160, 200} {
		w := w
		t.Run("", func(t *testing.T) {
			t.Parallel()
			m := NewModel(cfg, nil, "dark")
			m.Update(tea.WindowSizeMsg{Width: w, Height: 24})
			head := m.renderHeader()
			got := lipgloss.Width(head)
			if got != w {
				t.Errorf("renderHeader at width %d returned a row of width %d; want exactly %d (overflow wraps and hides the header on real terminals)", w, got, w)
			}
		})
	}
}
