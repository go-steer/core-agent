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
	"strings"
	"testing"
	"time"

	lipgloss "charm.land/lipgloss/v2"
)

// tableRows is the shape that broke the old fixed-width layout: a
// 36-column UUIDv7 ID next to an email user ID, both past what the
// %-30s / %-20s columns reserved.
//
// Deliberately asymmetric on Title — one titled row, one untitled — so
// the dash fallback is exercised by the same fixture as the layout.
func tableRows() []pickerEntry {
	return []pickerEntry{
		{Kind: kindCreate, Origin: "local"},
		{SessionID: "019ffc11-155a-759d-b149-08d0c1a5e54c", App: "core-agent", User: "platform-oncall@example.com", Origin: "local", Title: "Drain the west cluster"},
		{SessionID: "default", App: "core-agent", User: "local", Origin: "peer-west"},
	}
}

func TestFitColumnsWideTerminal(t *testing.T) {
	rows := tableRows()
	cols, widths := fitColumns(pickerColumns(), rows, time.Now(), 120)
	if len(cols) != len(pickerColumns()) {
		t.Fatalf("wide terminal dropped columns: %d of %d left", len(cols), len(pickerColumns()))
	}
	// Every cell has to fit un-elided, which is the whole point.
	for i, c := range cols {
		want := lipgloss.Width(c.header)
		for _, r := range rows {
			if r.Kind == kindCreate {
				continue
			}
			if w := lipgloss.Width(c.value(r, time.Now())); w > want {
				want = w
			}
		}
		if widths[i] != want {
			t.Errorf("column %s width = %d, want %d (content width)", c.header, widths[i], want)
		}
	}
}

func TestFitColumnsSqueezesToWidth(t *testing.T) {
	rows := tableRows()
	for _, avail := range []int{100, 76, 60, 48, 40, 30, 20} {
		cols, widths := fitColumns(pickerColumns(), rows, time.Now(), avail)
		if got := tableWidth(widths); got > avail {
			t.Errorf("avail=%d: table width %d overflows", avail, got)
		}
		natural := naturalWidths(cols, rows, time.Now())
		for i, c := range cols {
			// The min is a floor on what squeeze may TAKE, not a width
			// to pad up to, so it only binds a column that started
			// above it. Phrased that way the assertion stays
			// falsifiable — a squeeze that decremented past the min
			// would trip it — instead of restating the implementation.
			if natural[i] >= c.min && widths[i] < c.min && len(cols) > 1 {
				t.Errorf("avail=%d: column %s squeezed to %d, below min %d", avail, c.header, widths[i], c.min)
			}
		}
		// SESSION is never dropped — it's the only column that
		// identifies the row.
		if !hasColumn(cols, "SESSION") {
			t.Errorf("avail=%d: SESSION dropped, left with %s", avail, headerList(cols))
		}
	}
}

func TestFitColumnsDropOrder(t *testing.T) {
	rows := tableRows()
	// Narrow enough that not every column fits at its minimum: APP is
	// the least informative (usually identical on every row) so it
	// goes first, and the two columns an operator picks a row with —
	// what the session is called and which session it is — survive.
	cols, _ := fitColumns(pickerColumns(), rows, time.Now(), 42)
	if hasColumn(cols, "APP") {
		t.Errorf("APP should be the first column dropped, got %s", headerList(cols))
	}
	for _, want := range []string{"SESSION", "TITLE"} {
		if !hasColumn(cols, want) {
			t.Errorf("%s must survive the squeeze, got %s", want, headerList(cols))
		}
	}
}

// TestFitColumnsKeepsIdentityOverTitle pins the two places the title
// loses: a terminal too narrow to hold it alongside the addressable
// identity, and one too narrow to hold it alongside the evidence for
// the row order.
func TestFitColumnsKeepsIdentityOverTitle(t *testing.T) {
	cols, widths := fitColumns(pickerColumns(), tableRows(), time.Now(), 20)
	if hasColumn(cols, "TITLE") {
		t.Errorf("at avail=20 got %s, want the title gone", headerList(cols))
	}
	// And what's left is what the picker showed before there was a
	// title at all — the narrow terminal must not come out worse off
	// for the feature.
	if headerList(cols) != "SESSION,AGE" {
		t.Errorf("at avail=20 got %s, want SESSION,AGE", headerList(cols))
	}
	if got := tableWidth(widths); got > 20 {
		t.Errorf("at avail=20 table width %d overflows", got)
	}
}

// TestFitColumnsDropsTitleWhenNothingIsTitled is the H1 regression: a
// fleet where no session has a title (a listener older than attach
// protocol 1.6.0, or a host with session_title off) used to pay seven
// columns for a stack of dashes, and pay them out of ORIGIN and AGE —
// the TITLE column measures under its own min there, so the squeeze
// can't touch it and only the drop order can.
func TestFitColumnsDropsTitleWhenNothingIsTitled(t *testing.T) {
	rows := tableRows()
	for i := range rows {
		rows[i].Title = ""
	}
	for _, avail := range []int{120, 100, 76, 60, 50, 40, 30, 25, 20} {
		cols, widths := fitColumns(pickerColumns(), rows, time.Now(), avail)
		if hasColumn(cols, "TITLE") {
			t.Errorf("avail=%d: %s — an all-untitled list must not reserve a TITLE column", avail, headerList(cols))
		}
		// The layout has to be exactly what it was before the column
		// existed, not merely title-free.
		want, wantW := fitColumns(pickerColumns()[1:], rows, time.Now(), avail)
		if headerList(cols) != headerList(want) {
			t.Errorf("avail=%d: got %s, want %s (the pre-title layout)", avail, headerList(cols), headerList(want))
		}
		if tableWidth(widths) != tableWidth(wantW) {
			t.Errorf("avail=%d: width %d, want %d", avail, tableWidth(widths), tableWidth(wantW))
		}
	}
	// A title that sanitizes away to nothing is not a title either.
	for i := range rows {
		rows[i].Title = "\u200d\x1b"
	}
	if cols, _ := fitColumns(pickerColumns(), rows, time.Now(), 120); hasColumn(cols, "TITLE") {
		t.Errorf("got %s — titles that sanitize to nothing must not hold the column open", headerList(cols))
	}
}

// TestTitleColumnRendersTitle is the regression for the startup picker
// listing sessions by ID while /switch listed them by title: the column
// has to exist, and it has to carry the listener's title.
func TestTitleColumnRendersTitle(t *testing.T) {
	rows := tableRows()
	cols, widths := fitColumns(pickerColumns(), rows, time.Now(), 200)
	if !hasColumn(cols, "TITLE") {
		t.Fatalf("no TITLE column: %s", headerList(cols))
	}
	titled := renderRow(cols, widths, rowCells(cols, rows[1], time.Now()))
	if !strings.Contains(titled, "Drain the west cluster") {
		t.Errorf("titled row = %q, want the title in it", titled)
	}
	// An untitled session still shows its identity, with a dash where
	// the title would be — not a blank the operator has to interpret.
	untitled := renderRow(cols, widths, rowCells(cols, rows[2], time.Now()))
	if !strings.HasPrefix(strings.TrimSpace(untitled), "—") {
		t.Errorf("untitled row = %q, want it to open with the dash fallback", untitled)
	}
	if !strings.Contains(untitled, "default") {
		t.Errorf("untitled row = %q, want the session ID still in it", untitled)
	}
}

// TestTitleLabelSanitizes: the title is a wire field from another
// process — a peer daemon in fleet mode — so a newline or a CSI
// sequence in it must not escape its cell and repaint the picker.
func TestTitleLabelSanitizes(t *testing.T) {
	got := titleLabel("Debug\nthe \x1b[31mretries\x1b[0m")
	if strings.ContainsAny(got, "\n\r\x1b") {
		t.Errorf("titleLabel = %q, want no control characters", got)
	}
	// The newline becomes a separator rather than vanishing (dropping it
	// would weld "Debug" onto "the"); the escape byte just goes, leaving
	// its inert parameter bytes as ordinary text.
	if got != "Debug the [31mretries[0m" {
		t.Errorf("titleLabel = %q, want the words kept and the escape bytes gone", got)
	}
	if got := titleLabel("   \n\t "); got != noTitle {
		t.Errorf("all-whitespace title = %q, want the dash fallback", got)
	}
	// Combining marks are printable and survive the control-character
	// pass, but they draw nothing of their own — left in the cell they
	// graft onto the selection cursor drawn beside it.
	if got := titleLabel("\u0301\u0301\u0301"); got != noTitle {
		t.Errorf("combining-marks-only title = %q, want the dash fallback", got)
	}
	// A mark that has a base character to sit on is ordinary text.
	if got := titleLabel("e\u0301tat de l'art"); got != "e\u0301tat de l'art" {
		t.Errorf("accented title = %q, want the marks kept where they belong", got)
	}
}

// hasColumn / headerList are the vocabulary the drop-order assertions
// read in.
func hasColumn(cols []pickerColumn, header string) bool {
	for _, c := range cols {
		if c.header == header {
			return true
		}
	}
	return false
}

func headerList(cols []pickerColumn) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.header
	}
	return strings.Join(out, ",")
}

func TestRenderRowAlignment(t *testing.T) {
	rows := tableRows()
	now := time.Now()
	cols, widths := fitColumns(pickerColumns(), rows, now, 80)

	header := renderRow(cols, widths, headerCells(cols))
	// The ORIGIN header and every ORIGIN cell must start at the same
	// column — the misalignment in the bug report was exactly this.
	want := displayIndex(header, "ORIGIN")
	if want < 0 {
		t.Fatalf("ORIGIN not in header %q", header)
	}
	for _, r := range rows {
		if r.Kind == kindCreate {
			continue
		}
		line := renderRow(cols, widths, rowCells(cols, r, now))
		if got := displayIndex(line, r.Origin); got != want {
			t.Errorf("origin %q starts at column %d, header at %d\nheader: %q\nrow:    %q",
				r.Origin, got, want, header, line)
		}
	}
}

func TestRenderRowNoTrailingPad(t *testing.T) {
	rows := tableRows()
	now := time.Now()
	cols, widths := fitColumns(pickerColumns(), rows, now, 80)
	line := renderRow(cols, widths, rowCells(cols, rows[1], now))
	if strings.HasSuffix(line, " ") {
		// A right-aligned trailing column pads on the left only, so a
		// highlighted row doesn't paint blanks to the screen edge.
		t.Errorf("row has trailing padding: %q", line)
	}
}

// displayIndex is strings.Index in terminal columns rather than
// bytes — an elided cell carries a 3-byte "…" that would otherwise
// skew every offset to its right.
func displayIndex(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return lipgloss.Width(s[:i])
}

func TestElide(t *testing.T) {
	const id = "019ffc11-155a-759d-b149-08d0c1a5e54c"
	for _, tc := range []struct {
		name   string
		in     string
		w      int
		middle bool
		want   string
	}{
		{"fits", "abc", 5, false, "abc"},
		{"exact", "abcde", 5, false, "abcde"},
		{"right", "abcdef", 4, false, "abc…"},
		{"middle", "abcdefgh", 5, true, "ab…gh"},
		{"single column", "abcdef", 1, false, "…"},
		{"zero width", "abcdef", 0, false, ""},
		{"uuid keeps both ends", id, 17, true, "019ffc11…c1a5e54c"},
	} {
		if got := elide(tc.in, tc.w, tc.middle); got != tc.want {
			t.Errorf("%s: elide(%q, %d, %v) = %q, want %q", tc.name, tc.in, tc.w, tc.middle, got, tc.want)
		}
	}
	// Elided output never exceeds the budget it was given.
	for w := 1; w <= 40; w++ {
		for _, middle := range []bool{false, true} {
			if got := lipgloss.Width(elide(id, w, middle)); got > w {
				t.Fatalf("elide(id, %d, %v) width = %d, over budget", w, middle, got)
			}
		}
	}
}

// TestElideWideRunes pins the double-width case. elide budgets in
// display columns but walks runes, and a CJK or emoji cell has two
// columns per rune: trimming by rune count overflows the column (the
// same crooked table this file exists to fix) and, once the []rune
// conversion has no spare capacity, indexes past the end and panics
// the render.
func TestElideWideRunes(t *testing.T) {
	var subjects []string
	for _, unit := range []string{"東", "🚀", "한", "é"} {
		for n := 1; n <= 20; n++ {
			subjects = append(subjects,
				strings.Repeat(unit, n),
				"x"+strings.Repeat(unit, n),
				strings.Repeat(unit, n)+"@example.com",
			)
		}
	}
	for _, s := range subjects {
		for w := 1; w <= 44; w++ {
			for _, middle := range []bool{false, true} {
				got := elide(s, w, middle) // must not panic
				if gw := lipgloss.Width(got); gw > w {
					t.Fatalf("elide(%q, %d, %v) = %q, width %d over budget", s, w, middle, got, gw)
				}
				if strings.ContainsRune(got, 0) {
					t.Fatalf("elide(%q, %d, %v) = %q, read past the end of the string", s, w, middle, got)
				}
			}
		}
	}
}

// TestRenderRowAlignmentWideRunes is the same defect seen from the
// table: one row with a non-ASCII user ID or peer name must not run
// wider than its ASCII neighbour, or the columns stop lining up with
// their headers again.
func TestRenderRowAlignmentWideRunes(t *testing.T) {
	now := time.Now()
	rows := []pickerEntry{
		{SessionID: "019ffc11-155a-759d-b149-08d0c1a5e54c", App: "core-agent", User: "platform-oncall@example.com", Origin: "local"},
		{SessionID: "019ffc11-16eb-7a61-a670-d8275bb524af", App: "core-agent", User: "運用担当者@example.com", Origin: "peer-東"},
	}
	for _, avail := range []int{30, 40, 60, 76, 100, 140} {
		cols, widths := fitColumns(pickerColumns(), rows, now, avail)
		want := lipgloss.Width(renderRow(cols, widths, headerCells(cols)))
		for _, r := range rows {
			line := renderRow(cols, widths, rowCells(cols, r, now))
			if got := lipgloss.Width(line); got != want {
				t.Errorf("avail=%d: row width %d, header width %d\n%q", avail, got, want, line)
			}
		}
	}
}

func TestAgeLabel(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		in   time.Time
		want string
	}{
		{time.Time{}, "—"},
		{now.Add(30 * time.Second), "now"}, // clock skew: server slightly ahead
		{now.Add(-10 * time.Second), "now"},
		{now.Add(-42 * time.Minute), "42m"},
		{now.Add(-3 * time.Hour), "3h"},
		{now.Add(-12 * 24 * time.Hour), "12d"},
		{now.Add(-800 * 24 * time.Hour), "2y"},
	} {
		if got := ageLabel(tc.in, now); got != tc.want {
			t.Errorf("ageLabel(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
