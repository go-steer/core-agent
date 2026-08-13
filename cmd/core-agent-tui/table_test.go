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
func tableRows() []pickerEntry {
	return []pickerEntry{
		{Kind: kindCreate, Origin: "local"},
		{SessionID: "019ffc11-155a-759d-b149-08d0c1a5e54c", App: "core-agent", User: "platform-oncall@example.com", Origin: "local"},
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
		for i, c := range cols {
			if widths[i] < c.min && len(cols) > 1 {
				t.Errorf("avail=%d: column %s squeezed to %d, below min %d", avail, c.header, widths[i], c.min)
			}
		}
		// SESSION is never dropped — it's the only column that
		// identifies the row.
		if cols[0].header != "SESSION" {
			t.Errorf("avail=%d: first column = %s, want SESSION", avail, cols[0].header)
		}
	}
}

func TestFitColumnsDropOrder(t *testing.T) {
	rows := tableRows()
	// Narrow enough that not every column fits at its minimum: APP is
	// the least informative (usually identical on every row) so it
	// goes first, and AGE survives because it's the visible evidence
	// for the row ordering.
	cols, _ := fitColumns(pickerColumns(), rows, time.Now(), 42)
	var headers []string
	for _, c := range cols {
		headers = append(headers, c.header)
	}
	joined := strings.Join(headers, ",")
	if strings.Contains(joined, "APP") {
		t.Errorf("APP should be the first column dropped, got %s", joined)
	}
	if !strings.Contains(joined, "AGE") || !strings.Contains(joined, "SESSION") {
		t.Errorf("SESSION and AGE must survive the squeeze, got %s", joined)
	}
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
