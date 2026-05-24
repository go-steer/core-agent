// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
)

// MarkdownRenderer wraps a Glamour TermRenderer to render assistant
// messages on TurnComplete. Streaming partial chunks bypass this
// renderer and display as plain text — Glamour can't gracefully render
// half-formed code fences or tables.
type MarkdownRenderer struct {
	r *glamour.TermRenderer
}

// NewMarkdownRenderer constructs a renderer with a fixed style name and
// word wrap at width characters. Width <= 0 disables wrap.
//
// styleName must be a recognized Glamour style ("dark", "light",
// "notty", etc.). We deliberately avoid glamour.WithAutoStyle() here:
// it issues an OSC-11 background-color query to the terminal every
// call, and once Bubble Tea is reading stdin, the terminal's response
// races into the textarea as input. The TUI detects light vs dark
// once at startup (before tea.NewProgram) and threads the result
// through this constructor.
//
// Returns a usable (no-op) renderer plus a non-nil error if Glamour
// initialization fails so the TUI can keep running with raw markdown
// rather than crashing.
func NewMarkdownRenderer(width int, styleName string) (*MarkdownRenderer, error) {
	if styleName == "" {
		styleName = "dark"
	}
	opts := []glamour.TermRendererOption{
		glamour.WithStyles(cogoStyleConfig(styleName)),
	}
	if width > 0 {
		opts = append(opts, glamour.WithWordWrap(width))
	}
	r, err := glamour.NewTermRenderer(opts...)
	if err != nil {
		return &MarkdownRenderer{}, err
	}
	return &MarkdownRenderer{r: r}, nil
}

// cogoStyleConfig starts from Glamour's bundled dark/light style and
// patches a couple of rough edges:
//
//  1. H2-H6 in the bundled styles render with the literal "##"/"###"
//     prefix in the output (e.g. "## Section" stays "## Section").
//     We strip the prefix and substitute bold + color so heading depth
//     is still visible without leaking raw markdown to the user. H1 is
//     left alone — its inverted banner block already strips the "#"
//     and reads as a heading on its own.
//
//  2. Code fences get static separator lines above and below so the
//     boundaries of a code block are visually obvious. Glamour doesn't
//     plumb the language tag through to the static prefix/suffix, so
//     the chrome is generic ("code") rather than per-language.
func cogoStyleConfig(styleName string) ansi.StyleConfig {
	cfg := styles.DarkStyleConfig
	if styleName == "light" {
		cfg = styles.LightStyleConfig
	}
	for level, h := range map[int]*ansi.StyleBlock{
		2: &cfg.H2,
		3: &cfg.H3,
		4: &cfg.H4,
		5: &cfg.H5,
		6: &cfg.H6,
	} {
		h.Prefix = ""
		h.Color = strPtr(headingColor(styleName, level))
		h.Bold = boolPtr(true)
	}
	cfg.CodeBlock.BlockPrefix = codeBlockTopBar
	cfg.CodeBlock.BlockSuffix = codeBlockBottomBar
	return cfg
}

// codeBlockTopBar / codeBlockBottomBar bracket fenced code blocks. The
// top bar carries a "code" label so the separator reads as a deliberate
// boundary rather than a horizontal rule. The exact glyph counts are
// arbitrary; they only need to look like a contained block at a glance.
const (
	codeBlockTopBar    = "──────── code ────────\n"
	codeBlockBottomBar = "──────────────────────"
)

// headingColor returns the 256-color index for heading level n (2-6).
// Cool-blue palette chosen so headings stay distinct from inline code
// and bold body text. Lighter shade per level so the visual hierarchy
// still reads even without indentation differences.
func headingColor(styleName string, level int) string {
	if styleName == "light" {
		switch level {
		case 2:
			return "27"
		case 3:
			return "33"
		case 4:
			return "61"
		default:
			return "67"
		}
	}
	switch level {
	case 2:
		return "75"
	case 3:
		return "39"
	case 4:
		return "147"
	default:
		return "110"
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// Render applies Glamour to markdown. If the renderer failed to
// initialize, it returns markdown unchanged so the user still sees
// something usable.
func (m *MarkdownRenderer) Render(markdown string) string {
	if m == nil || m.r == nil {
		return markdown
	}
	out, err := m.r.Render(markdown)
	if err != nil {
		return markdown
	}
	return out
}
