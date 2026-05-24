// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

// bottomPad reserves blank rows at the bottom of the alt-screen so the
// footer never sits flush against the terminal edge. handleResize
// subtracts the same value from the viewport height so total content
// still matches the screen.
const bottomPad = 1

// View renders the model as a single string. Layout (top to bottom):
//
//	Header
//	Viewport (scrollable history)
//	Palette (slash / file picker — only when active)
//	Input area (textarea inside a rounded border) — replaced by the
//	  permission modal when a request is pending
//	Footer (status hint or spinner)
//	bottomPad blank rows (breathing room)
func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		// Pre-resize: avoid drawing into 0×0.
		return "Loading…"
	}

	header := m.renderHeader()
	body := m.viewport.View()
	var input string
	switch {
	case m.pendingElicit != nil:
		input = m.renderElicitModal()
	case m.pendingConfirm != nil:
		input = m.renderConfirmModal()
	case m.modelPicker != nil:
		input = m.renderModelPicker()
	case m.permissionsPicker != nil:
		input = m.renderPermissionsPicker()
	default:
		input = m.renderInput()
	}
	footer := m.renderFooter()

	parts := []string{header, body}
	if m.palette != nil {
		parts = append(parts, m.renderPalette())
	}
	parts = append(parts, input, footer)
	// Append bottomPad EMPTY-STRING parts (not a newline string).
	// JoinVertical splits each part on "\n" — feeding it "\n" yields
	// two empty rows per newline, blowing the row budget by one and
	// scrolling the header off the top of the alt-screen. An empty
	// string contributes exactly one blank row, which is what we want.
	for i := 0; i < bottomPad; i++ {
		parts = append(parts, "")
	}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// renderPalette draws the palette overlay between the viewport and the
// input area. Cursor row is highlighted; non-cursor rows are muted.
func (m *Model) renderPalette() string {
	p := m.palette
	rows := len(p.items)
	if rows > MaxPaletteRows {
		rows = MaxPaletteRows
	}
	// Window items so the cursor stays in view.
	start := 0
	if p.cursor >= rows {
		start = p.cursor - rows + 1
	}
	end := start + rows
	if end > len(p.items) {
		end = len(p.items)
	}

	var lines []string
	for i := start; i < end; i++ {
		it := p.items[i]
		marker := "  "
		if i == p.cursor {
			marker = "▸ "
		}
		line := marker + it.Display
		if it.Hint != "" {
			line += "  " + it.Hint
		}
		if i == p.cursor {
			lines = append(lines, m.styles.HeaderAccent.Render(line))
		} else {
			lines = append(lines, m.styles.Footer.Render(line))
		}
	}
	header := "  " + paletteHeader(p)
	body := strings.Join(lines, "\n")
	return m.styles.InputBorder.Render(m.styles.System.Render(header) + "\n" + body)
}

func paletteHeader(p *paletteState) string {
	switch p.kind {
	case paletteSlash:
		return "Slash commands (↑/↓ select · enter run · esc cancel)"
	case paletteFile:
		return "Files (↑/↓ select · enter insert · esc cancel)"
	default:
		return ""
	}
}

// renderModelPicker draws the /model picker in place of the input.
// Same shape as the slash/file palette but with model IDs.
func (m *Model) renderModelPicker() string {
	p := m.modelPicker
	header := m.styles.System.Render("Model picker (↑/↓ select · enter switch · esc cancel)")
	var lines []string
	for i, id := range p.items {
		marker := "  "
		if i == p.cursor {
			marker = "▸ "
		}
		line := marker + id
		if id == m.cfg.Model.Name {
			line += "  (current)"
		}
		if i == p.cursor {
			lines = append(lines, m.styles.HeaderAccent.Render(line))
		} else {
			lines = append(lines, m.styles.Footer.Render(line))
		}
	}
	body := strings.Join(lines, "\n")
	return m.styles.InputBorder.Render(header + "\n" + body)
}

// renderElicitModal draws the MCP elicitation modal. URL mode shows
// the URL prominently with open/accept/decline keys. Form mode lists
// each field with the active one highlighted, plus a key legend and
// any validation error.
func (m *Model) renderElicitModal() string {
	st := m.pendingElicit
	if st == nil {
		return ""
	}
	header := m.styles.Confirm.Render(fmt.Sprintf("MCP %s — input requested", st.ServerName))
	if st.Mode == elicitURL {
		body := header + "\n"
		if st.Message != "" {
			body += m.styles.System.Render(st.Message) + "\n"
		}
		body += m.styles.HeaderAccent.Render(st.URL) + "\n" +
			m.styles.Footer.Render("[o] open in browser   [a/enter] accept   [n] decline   [esc] cancel")
		return m.styles.InputBorder.Render(body)
	}

	lines := []string{header}
	if st.Message != "" {
		lines = append(lines, m.styles.System.Render(st.Message))
	}
	for i, f := range st.Fields {
		marker := "  "
		if i == st.Active {
			marker = "▸ "
		}
		label := marker + f.Name
		if f.Required {
			label += " *"
		}
		if f.Description != "" {
			label += "  " + m.styles.Footer.Render("("+f.Description+")")
		}
		var value string
		switch f.Kind {
		case fieldString, fieldNumber, fieldInteger:
			value = f.input.View()
		case fieldEnum, fieldBoolean:
			value = renderChoiceCycler(f, i == st.Active, m.styles)
		}
		row := label + "\n    " + value
		if i == st.Active {
			lines = append(lines, m.styles.HeaderAccent.Render(row))
		} else {
			lines = append(lines, m.styles.Footer.Render(row))
		}
	}
	if st.Err != "" {
		lines = append(lines, m.styles.Error.Render("⚠ "+st.Err))
	}
	lines = append(lines, m.styles.Footer.Render(
		"[tab/↓] next   [shift+tab/↑] prev   [enter] submit   [esc] cancel"))
	return m.styles.InputBorder.Render(strings.Join(lines, "\n"))
}

// renderChoiceCycler shows the enum/boolean choice as the current
// value flanked by '<' and '>' to hint at left/right cycling.
func renderChoiceCycler(f elicitField, active bool, st Styles) string {
	if len(f.Choices) == 0 {
		return "(no choices)"
	}
	cur := f.choice
	if cur < 0 || cur >= len(f.Choices) {
		cur = 0
	}
	val := f.Choices[cur]
	if active {
		return st.HeaderAccent.Render("‹ " + val + " ›")
	}
	return val
}

// renderConfirmModal draws the permission request modal in place of
// the input area. The verb middle option ([v] this verb · session) is
// inserted between "this call" and "this tool" when the gate populated
// req.Verb — that's the only signal that broadening to `<verb> *` is
// safe to offer.
func (m *Model) renderConfirmModal() string {
	req := m.pendingConfirm.Req
	kindLabel := map[int]string{
		0: "Bash command",
		1: "File write",
		2: "Path scope",
		3: "Tool",
	}[int(req.Kind)]
	if kindLabel == "" {
		kindLabel = "Tool"
	}
	footer := "[y] once  [s] this call · session  "
	if req.Verb != "" {
		footer += "[v] `" + req.Verb + " *` · session  "
	}
	footer += "[t] this tool · session  [a] always (persist)  [n/esc] deny"
	body := m.styles.Confirm.Render(kindLabel+": "+req.Detail) + "\n" +
		m.styles.Footer.Render(footer)
	return m.styles.InputBorder.Render(body)
}

func (m *Model) renderHeader() string {
	provider := m.cfg.Model.Provider
	if provider == "" {
		provider = "auto"
	}
	mode := m.cfg.Permissions.Mode
	if mode == "" {
		mode = "ask"
	}
	cwd := shortDir(m.projectRoot)

	// Header layout: 1-char gutter on each edge + brand on the left +
	// flexible gap + status on the right. Crucially the assembled line
	// must end up EXACTLY m.width columns wide — one column over and
	// the terminal wraps the row, which (because Bubble Tea's screen
	// positioning assumes a single-row header) scrolls the whole header
	// off the top of the alt-screen and the user opens the TUI to a
	// missing header. The Header style has no Padding for the same
	// reason: it would invisibly add 2 cols on top of our budget.
	left := headerBrand(m.brandIdentity())
	// Build the right side incrementally, appending each segment only
	// if it still fits. Mode badge is the floor (security-critical: a
	// user must always see whether they're in yolo); the model name is
	// truncated to make room for it when the terminal is too narrow.
	const gutter = 1
	budget := m.width - 2*gutter - lipgloss.Width(left) - 1 // -1 for min gap
	badge := " · " + modeBadge(mode, m.styles)
	// Reserve room for the badge before we lay down the model name, so
	// the badge is guaranteed to fit even at very-narrow widths.
	modelBudget := budget - lipgloss.Width(badge)
	modelDisplay := m.cfg.Model.Name
	if modelBudget < lipgloss.Width(modelDisplay) && modelBudget > 1 {
		modelDisplay = ansi.Truncate(modelDisplay, modelBudget, "…")
	}
	right := m.styles.HeaderAccent.Render(modelDisplay) + badge
	tryAppend := func(s string) {
		if lipgloss.Width(right)+lipgloss.Width(s) <= budget {
			right += s
		}
	}
	tryAppend(" · " + cwd)
	tryAppend(" · " + provider)
	if m.usage != nil {
		tot := m.usage.Totals()
		tryAppend(fmt.Sprintf(" · σ ↑%d/↓%d/$%s",
			tot.InputTokens, tot.OutputTokens, formatCost(tot.CostUSD)))
	}
	gap := m.width - 2*gutter - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	pad := strings.Repeat(" ", gutter)
	line := pad + left + strings.Repeat(" ", gap) + right + pad
	// Belt and suspenders: truncate if the math still produced a row
	// wider than the terminal (e.g., a model name longer than the whole
	// terminal width). ansi.Truncate is ANSI-aware so it doesn't break
	// the styled brand spans.
	if lipgloss.Width(line) > m.width {
		line = ansi.Truncate(line, m.width, "…")
	}
	return m.styles.Header.Render(line)
}

// shortDir returns the basename of dir prefixed with "~/" when dir is
// inside the user's home; otherwise it falls back to the absolute
// basename. Empty dir → "?".
func shortDir(dir string) string {
	if dir == "" {
		return "?"
	}
	home := homeDir()
	if home != "" && (dir == home || strings.HasPrefix(dir, home+"/")) {
		rel := strings.TrimPrefix(dir, home)
		return "~" + rel
	}
	return dir
}

// modeBadge styles the permission mode so "yolo" stands out — landing
// in yolo without realizing it should be visually obvious. The yolo
// label gets a leading ⚠ glyph so it's recognizable even on a quick
// glance at the corner of the header.
func modeBadge(mode string, st Styles) string {
	switch mode {
	case "yolo":
		return st.Error.Render("⚠ " + mode)
	case "ask":
		return st.HeaderAccent.Render(mode)
	default:
		return mode
	}
}

func (m *Model) renderInput() string {
	return m.styles.InputBorder.Render(m.textarea.View())
}

func (m *Model) renderFooter() string {
	switch {
	case m.pendingElicit != nil:
		return m.styles.Footer.Render("MCP elicitation in progress — see the modal above")
	case m.pendingConfirm != nil:
		return m.styles.Footer.Render("Permission required — choose one of the keys above")
	case m.state == StateStreaming:
		// Plain footer string. Earlier attempts to brand-cyan the
		// "Thinking..." word reported "no Thinking in the footer" on
		// VS Code's integrated terminal; the styled multi-span line was
		// breaking the bubble tea renderer's diff on that host. Until
		// we understand the host bug, fall back to a single Footer
		// wrap with no nested styles.
		return m.styles.Footer.Render(m.spinner.View() + " Thinking... (Ctrl+C to cancel)")
	case m.confirmingClear:
		return m.styles.Confirm.Render("Confirm clear: type y / yes / anything else")
	default:
		return m.styles.Footer.Render("/help · /quit · Ctrl+C to exit")
	}
}
