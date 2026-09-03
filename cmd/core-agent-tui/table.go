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

import (
	"fmt"
	"strings"
	"time"

	lipgloss "charm.land/lipgloss/v2"

	"github.com/go-steer/core-agent/v2/internal/coretuiremote"
)

// Column layout for the session picker's table.
//
// The picker used to render fixed %-30s / %-20s columns, which fits
// nothing real: a UUIDv7 session ID is 36 columns and an email user ID
// routinely passes 20, so every long cell shoved the rest of its row
// right and the headers stopped describing the data underneath them.
// Here widths are measured from the rows actually being shown, then
// shrunk to the terminal, then columns are dropped entirely if even
// their minimums don't fit.

// colGap is the blank run between two columns. Two spaces, so a cell
// that was elided still reads as separate from its neighbour.
const colGap = 2

// pickerColumn is one table column: a header, how to pull the cell out
// of a row, how narrow it may get, and how it degrades.
type pickerColumn struct {
	header string
	value  func(pickerEntry, time.Time) string
	// min is the narrowest this column may be squeezed to before the
	// layout starts taking width from somewhere else.
	min int
	// elideMiddle keeps both ends of the cell. UUIDv7s share a long
	// leading run (the timestamp) and differ in the tail, so a
	// right-elided ID is unusable for telling two rows apart.
	elideMiddle bool
	// rightAlign is for the numeric-ish AGE column.
	rightAlign bool
	// dropRank orders removal when the terminal is too narrow to hold
	// every column at its minimum: highest rank goes first. Rank 0 is
	// never dropped.
	dropRank int
	// hasContent reports whether a row has anything real for this
	// column, as distinct from the placeholder value() renders. When
	// it's set and no row on screen answers true, the column is left
	// out of the layout entirely — see emptyColumnsRemoved.
	hasContent func(pickerEntry) bool
}

// pickerColumns is the table's schema, left to right.
func pickerColumns() []pickerColumn {
	return []pickerColumn{
		{
			// First column because it's the one an operator reads to
			// recognise a row: they know the session by what they asked
			// it to do, not by the tail of a UUID. Outlives every other
			// descriptive column, but goes before AGE (the evidence for
			// the row order) and before SESSION (the identity).
			header:     "TITLE",
			min:        12,
			dropRank:   2,
			value:      func(e pickerEntry, _ time.Time) string { return titleLabel(e.Title) },
			hasContent: func(e pickerEntry) bool { return titleLabel(e.Title) != noTitle },
		},
		{
			header:      "SESSION",
			min:         14,
			elideMiddle: true,
			value:       func(e pickerEntry, _ time.Time) string { return e.SessionID },
		},
		{
			// Usually the same app on every row, so it's the first
			// thing to go when the terminal is narrow.
			header:   "APP",
			min:      6,
			dropRank: 5,
			value:    func(e pickerEntry, _ time.Time) string { return e.App },
		},
		{
			header:   "USER",
			min:      8,
			dropRank: 4,
			value:    func(e pickerEntry, _ time.Time) string { return e.User },
		},
		{
			header:   "ORIGIN",
			min:      6,
			dropRank: 3,
			value:    func(e pickerEntry, _ time.Time) string { return e.Origin },
		},
		{
			// Three columns wide and it's the evidence for the row
			// order, so it survives longest. Cheap enough that giving
			// its space to the title buys a few more characters of one
			// cell and costs the whole column: at the widths where the
			// choice arises the title doesn't fit either, and dropping
			// AGE first left SESSION alone on a row that used to carry
			// both.
			header:     "AGE",
			min:        3,
			rightAlign: true,
			dropRank:   1,
			value:      func(e pickerEntry, now time.Time) string { return ageLabel(e.CreatedAt, now) },
		},
	}
}

// noTitle is what the TITLE cell shows for a session that has none.
const noTitle = "—"

// titleLabel renders a session's title cell. The raw wire string is
// sanitized with the same helper the /switch dialog uses — a title can
// arrive from a peer daemon this process didn't write, and a newline or
// a CSI sequence in it would break far more than one cell.
//
// An untitled session gets a dash rather than blank space: the column
// keeps its shape, and "no title yet" reads as a fact instead of a
// render that failed. The identity is in SESSION either way.
func titleLabel(title string) string {
	if t := coretuiremote.SessionTitleForDisplay(title); t != "" {
		return t
	}
	return noTitle
}

// fitColumns picks the visible columns and their widths for the given
// rows and available width. Four stages, in order: drop the columns
// with nothing in them, measure the content, squeeze the widest
// columns down toward their minimums, and only then drop columns by
// rank.
func fitColumns(cols []pickerColumn, rows []pickerEntry, now time.Time, avail int) ([]pickerColumn, []int) {
	visible := emptyColumnsRemoved(cols, rows)
	for {
		widths := naturalWidths(visible, rows, now)
		squeeze(visible, widths, avail)
		if tableWidth(widths) <= avail || len(visible) == 1 {
			return visible, widths
		}
		visible = dropColumn(visible)
	}
}

// emptyColumnsRemoved drops any column declaring a hasContent probe
// that no row on screen satisfies.
//
// This runs before the width fit rather than falling out of it,
// because a column of placeholders is not merely narrow — it is
// *immune* to the squeeze. TITLE against a fleet where nothing is
// titled measures 5 (its own header; the cells are one dash), which is
// under its 12-column minimum, so it has negative slack and squeeze
// never picks it as a victim. It would then sit there outranking
// ORIGIN and AGE for space while carrying no information at all: an
// operator on a narrow pane against a pre-1.6.0 listener would lose the
// column telling them which peer a session is on and get a stack of
// dashes for it.
func emptyColumnsRemoved(cols []pickerColumn, rows []pickerEntry) []pickerColumn {
	out := make([]pickerColumn, 0, len(cols))
	for _, c := range cols {
		if c.hasContent != nil && !anyRowHasContent(c, rows) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func anyRowHasContent(c pickerColumn, rows []pickerEntry) bool {
	for _, r := range rows {
		if r.Kind == kindCreate {
			continue // the sentinel spans the whole row, it fills nothing
		}
		if c.hasContent(r) {
			return true
		}
	}
	return false
}

// naturalWidths measures each column against its header and every cell
// it has to hold.
func naturalWidths(cols []pickerColumn, rows []pickerEntry, now time.Time) []int {
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = lipgloss.Width(c.header)
	}
	for _, r := range rows {
		if r.Kind == kindCreate {
			continue // the sentinel spans the whole row, it sizes nothing
		}
		for i, c := range cols {
			if w := lipgloss.Width(c.value(r, now)); w > widths[i] {
				widths[i] = w
			}
		}
	}
	return widths
}

// squeeze shaves the widest over-minimum column, one column at a time,
// until the table fits or nothing can give. Taking from the widest
// keeps the damage on the column with the most slack rather than
// spreading an elision across all of them.
func squeeze(cols []pickerColumn, widths []int, avail int) {
	for tableWidth(widths) > avail {
		victim, slack := -1, 0
		for i := range cols {
			if s := widths[i] - cols[i].min; s > slack {
				victim, slack = i, s
			}
		}
		if victim < 0 {
			return // everything is at its minimum
		}
		widths[victim]--
	}
}

// dropColumn removes the highest-ranked droppable column.
func dropColumn(cols []pickerColumn) []pickerColumn {
	victim, rank := -1, 0
	for i, c := range cols {
		if c.dropRank > rank {
			victim, rank = i, c.dropRank
		}
	}
	if victim < 0 {
		// Nothing droppable left, so keep the first column. Reachable
		// only with two or more rank-0 columns — SESSION is the sole
		// one today, and fitColumns returns before calling here once
		// it's alone. A second would have to be one that may lead the
		// table, because this picks the leftmost survivor.
		return cols[:1]
	}
	out := make([]pickerColumn, 0, len(cols)-1)
	out = append(out, cols[:victim]...)
	return append(out, cols[victim+1:]...)
}

// tableWidth is the rendered width of a row: cells plus the gaps.
func tableWidth(widths []int) int {
	if len(widths) == 0 {
		return 0
	}
	total := colGap * (len(widths) - 1)
	for _, w := range widths {
		total += w
	}
	return total
}

// renderRow lays one row's cells out at the given widths. The trailing
// cell isn't padded, so a highlight style doesn't paint a bar of
// trailing blanks across the terminal.
func renderRow(cols []pickerColumn, widths []int, cells []string) string {
	var b strings.Builder
	for i := range cols {
		cell := elide(cells[i], widths[i], cols[i].elideMiddle)
		last := i == len(cols)-1
		switch {
		case last && !cols[i].rightAlign:
			b.WriteString(cell)
		case cols[i].rightAlign:
			b.WriteString(pad(widths[i]-lipgloss.Width(cell)) + cell)
		default:
			b.WriteString(cell + pad(widths[i]-lipgloss.Width(cell)))
		}
		if !last {
			b.WriteString(pad(colGap))
		}
	}
	return b.String()
}

// headerCells / rowCells pull the raw (un-elided) strings for a row.
func headerCells(cols []pickerColumn) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.header
	}
	return out
}

func rowCells(cols []pickerColumn, e pickerEntry, now time.Time) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.value(e, now)
	}
	return out
}

// elide trims s to w display columns, marking the cut with "…".
// middle keeps the head and the tail (session IDs); otherwise the tail
// goes.
//
// The budget is display columns, not runes. A CJK user ID or an emoji
// in a peer name is two columns per rune, so slicing r[:w-1] both
// overflows the column — the exact defect this file exists to fix —
// and indexes past the end of the rune slice, which panics the render
// once the conversion has no spare capacity.
func elide(s string, w int, middle bool) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	r := []rune(s)
	budget := w - 1 // the "…" costs a column
	if !middle {
		return string(r[:fitPrefix(r, budget)]) + "…"
	}
	head := fitPrefix(r, budget/2)
	// Measure the tail against what the head actually spent and take
	// it from the runes the head didn't, so the two ends can't
	// overlap on a string of zero-width marks.
	tail := fitSuffix(r[head:], budget-lipgloss.Width(string(r[:head])))
	return string(r[:head]) + "…" + string(r[len(r)-tail:])
}

// fitPrefix / fitSuffix report how many runes of r fit in w display
// columns, counting from the front / the back. Measuring per rune with
// the same lipgloss.Width the layout uses keeps the trim and the column
// arithmetic from disagreeing.
func fitPrefix(r []rune, w int) int {
	used, n := 0, 0
	for ; n < len(r); n++ {
		cw := lipgloss.Width(string(r[n]))
		if used+cw > w {
			break
		}
		used += cw
	}
	return n
}

func fitSuffix(r []rune, w int) int {
	used, n := 0, 0
	for ; n < len(r); n++ {
		cw := lipgloss.Width(string(r[len(r)-1-n]))
		if used+cw > w {
			break
		}
		used += cw
	}
	return n
}

func pad(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(" ", n)
}

// ageLabel renders how long ago t was, in one unit and at most four
// columns ("now", "42m", "3h", "12d"). Zero t means the session ID
// carried no creation time.
func ageLabel(t, now time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < 0, d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dy", int(d.Hours()/24/365))
	}
}
