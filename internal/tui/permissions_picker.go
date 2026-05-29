// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"fmt"
	"strings"

	"github.com/go-steer/core-agent/pkg/permissions"
)

// permissionsPicker is the overlay state for the /permissions slash
// command. It mirrors modelPickerState in shape (cursor + items) but
// adds a per-row toggle so the user can pick which recommendations to
// persist before pressing enter.
//
// Layout in the chat is similar to the model picker: a bordered box
// in place of the input area while the picker is open. Only the
// recommendation rows are interactive; the audit list below is
// read-only context.
type permissionsPicker struct {
	recs     []permissions.Recommendation
	selected []bool   // index-aligned with recs
	approved []string // formatted approval log lines, for context
	cursor   int
}

// newPermissionsPicker builds the picker state from the gate's session
// approval log. Returns nil when there's nothing to review (so the
// caller can show a friendly "no approvals yet" system message
// instead of opening an empty modal).
func newPermissionsPicker(approvals []permissions.ApprovalLog) *permissionsPicker {
	if len(approvals) == 0 {
		return nil
	}
	recs := permissions.Recommend(approvals)
	permissions.SortRecommendations(recs)

	approved := make([]string, 0, len(approvals))
	for _, a := range approvals {
		approved = append(approved, fmt.Sprintf("%-12s %s   (%s)", a.Tool, a.Key, a.Decision))
	}
	return &permissionsPicker{
		recs:     recs,
		selected: make([]bool, len(recs)),
		approved: approved,
	}
}

// chosenPatterns returns the patterns the user toggled on. Empty when
// nothing's selected; caller treats that as "user wants to bail".
func (p *permissionsPicker) chosenPatterns() []string {
	out := make([]string, 0, len(p.recs))
	for i, r := range p.recs {
		if p.selected[i] {
			out = append(out, r.Pattern)
		}
	}
	return out
}

// renderPermissionsPicker draws the permissions overlay in place of
// the input area while the picker is open. Layout:
//
//	Permissions review
//	  [x] read_file:internal/tui/**   3 paths under `internal/tui/` approved
//	    examples: internal/tui/model.go, internal/tui/view.go, ...
//	  [ ] bash:*                      3 distinct calls — broaden to all bash calls
//	  ...
//	Other approvals this session (4)
//	  read_file    go.mod   (allow-once)
//	  ...
//	[↑/↓] move  [space] toggle  [enter] persist checked  [esc] cancel
func (m *Model) renderPermissionsPicker() string {
	p := m.permissionsPicker
	if p == nil {
		return ""
	}
	var lines []string
	lines = append(lines, m.styles.System.Render("Permissions review (this session)"))

	if len(p.recs) == 0 {
		lines = append(lines, m.styles.Footer.Render("  no recommendations — every approval was a one-off"))
	} else {
		lines = append(lines, m.styles.Footer.Render(
			fmt.Sprintf("  %s", plural(len(p.recs), "candidate"))))
		for i, r := range p.recs {
			marker := "[ ]"
			if p.selected[i] {
				marker = "[x]"
			}
			cursor := "  "
			if i == p.cursor {
				cursor = "▸ "
			}
			row := fmt.Sprintf("%s%s %-30s  %s", cursor, marker, r.Pattern, r.Reason)
			if i == p.cursor {
				lines = append(lines, m.styles.HeaderAccent.Render(row))
			} else {
				lines = append(lines, m.styles.Footer.Render(row))
			}
			if len(r.Evidence) > 0 {
				ev := strings.Join(truncEvidence(r.Evidence, 3), ", ")
				lines = append(lines, m.styles.Footer.Render("      ‹ "+ev+" ›"))
			}
		}
	}

	if len(p.approved) > 0 {
		lines = append(lines, "")
		lines = append(lines, m.styles.System.Render(
			fmt.Sprintf("Other approvals this session (%d)", len(p.approved))))
		max := 6
		shown := p.approved
		if len(shown) > max {
			shown = shown[:max]
		}
		for _, a := range shown {
			lines = append(lines, m.styles.Footer.Render("  "+a))
		}
		if len(p.approved) > max {
			lines = append(lines, m.styles.Footer.Render(
				fmt.Sprintf("  … and %d more", len(p.approved)-max)))
		}
	}

	lines = append(lines, "")
	lines = append(lines, m.styles.Footer.Render(
		"[↑/↓] move  [space] toggle  [enter] persist checked  [esc] cancel"))
	return m.styles.InputBorder.Render(strings.Join(lines, "\n"))
}

// truncEvidence keeps the first n entries of evidence and adds an
// ellipsis suffix when there are more, so the picker stays compact.
func truncEvidence(ev []string, n int) []string {
	if len(ev) <= n {
		return ev
	}
	out := append([]string(nil), ev[:n]...)
	out = append(out, fmt.Sprintf("(+%d more)", len(ev)-n))
	return out
}

// plural is a tiny helper used by the picker copy; the permissions
// package has its own internal helper, but we can't reach unexported
// symbols across packages, so we keep this one here for the TUI side.
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
