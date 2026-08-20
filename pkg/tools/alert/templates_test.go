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

package alert

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTruncate pins the unit every service limit is expressed in.
// Slack, Discord and PagerDuty all document their caps in CHARACTERS, so
// a byte-counted truncation both cuts non-ASCII text that would have fit
// and can split a rune in half, putting invalid UTF-8 on the wire.
func TestTruncate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"under the limit", "hello", 10, "hello"},
		{"exactly the limit", "hello", 5, "hello"},
		{"multi-byte string that fits in runes but not in bytes", "héllo", 5, "héllo"},
		{"over the limit", "hello world", 5, "hell…"},
		{"multi-byte over the limit", "ünïcödé text", 6, "ünïcö…"},
		{"one rune of room", "hello", 1, "…"},
		{"no room at all", "hello", 0, ""},
		{"negative", "hello", -3, ""},
		{"empty input", "", 5, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := truncate(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if n := utf8.RuneCountInString(got); tc.max > 0 && n > tc.max {
				t.Errorf("truncate(%q, %d) is %d runes, want at most %d", tc.in, tc.max, n, tc.max)
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncate(%q, %d) = %q, which is not valid UTF-8", tc.in, tc.max, got)
			}
		})
	}
}

func TestDetailValue(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		in   any
		want string
	}{
		"string passes through unquoted": {"prod-us-east", "prod-us-east"},
		"number":                         {float64(3), "3"},
		"bool":                           {true, "true"},
		"nil":                            {nil, "null"},
		"nested map is compact JSON":     {map[string]any{"a": 1}, `{"a":1}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := detailValue(tc.in); got != tc.want {
				t.Errorf("detailValue(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSortedKeys is what keeps every template's body byte-identical run
// to run: a Go map's range order is randomised, and a body that differs
// between two identical alerts is one no receiver can diff.
func TestSortedKeys(t *testing.T) {
	t.Parallel()
	in := map[string]any{"zulu": 1, "alpha": 2, "mike": 3, "bravo": 4}
	want := "alpha,bravo,mike,zulu"
	for range 20 {
		if got := strings.Join(sortedKeys(in), ","); got != want {
			t.Fatalf("sortedKeys() = %q, want %q", got, want)
		}
	}
	if sortedKeys(nil) != nil {
		t.Errorf("sortedKeys(nil) = %v, want nil", sortedKeys(nil))
	}
}
