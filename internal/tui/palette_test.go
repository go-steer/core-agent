// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import "testing"

func TestDetectPaletteTrigger(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		value      string
		cursor     int
		wantOk     bool
		wantKind   paletteKind
		wantFilter string
		wantPos    int
	}{
		{"empty", "", 0, false, 0, "", 0},
		{"plain text", "hello", 5, false, 0, "", 0},
		{"slash at start", "/", 1, true, paletteSlash, "", 0},
		{"slash with prefix", "/hel", 4, true, paletteSlash, "hel", 0},
		{"slash followed by space", "/help ", 6, false, 0, "", 0},
		{"slash mid-word", "use /help", 9, false, 0, "", 0},
		{"at at start", "@", 1, true, paletteFile, "", 0},
		{"at with prefix", "@docs", 5, true, paletteFile, "docs", 0},
		{"at after space", "show me @do", 11, true, paletteFile, "do", 8},
		{"at mid-word ignored", "user@host.com", 13, false, 0, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kind, pos, filter, ok := detectPaletteTrigger(tc.value, tc.cursor)
			if ok != tc.wantOk {
				t.Errorf("ok = %v, want %v", ok, tc.wantOk)
			}
			if !tc.wantOk {
				return
			}
			if kind != tc.wantKind {
				t.Errorf("kind = %v, want %v", kind, tc.wantKind)
			}
			if filter != tc.wantFilter {
				t.Errorf("filter = %q, want %q", filter, tc.wantFilter)
			}
			if pos != tc.wantPos {
				t.Errorf("pos = %d, want %d", pos, tc.wantPos)
			}
		})
	}
}

func TestFilterPaletteItems(t *testing.T) {
	t.Parallel()
	items := allSlashItems()

	all := filterPaletteItems(items, "")
	if len(all) != len(items) {
		t.Errorf("empty filter dropped items: %d vs %d", len(all), len(items))
	}

	got := filterPaletteItems(items, "qu")
	if len(got) == 0 || got[0].Display != "/quit" {
		t.Errorf("filter 'qu' should put /quit first; got %+v", got)
	}

	none := filterPaletteItems(items, "zzzzz")
	if len(none) != 0 {
		t.Errorf("nonsense filter should return nothing; got %+v", none)
	}
}

func TestTokenizeAtRefs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"hello", nil},
		{"@a.go", []string{"a.go"}},
		{"check @docs/x.md please", []string{"docs/x.md"}},
		{"@one and @two", []string{"one", "two"}},
		{"email like a@b.com is not a ref", nil},
		{"@", nil},
		{"line1\n@inner.txt\nline3", []string{"inner.txt"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := tokenizeAtRefs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestExpandAtRefs_InlinesContent(t *testing.T) {
	t.Parallel()
	reader := func(p string) ([]byte, error) {
		switch p {
		case "a.txt":
			return []byte("alpha\n"), nil
		case "b.md":
			return []byte("# beta\n"), nil
		}
		return nil, &fakeReadErr{p}
	}
	out, refs, diags := expandAtRefs("Compare @a.txt and @b.md please", reader)
	if len(refs) != 2 {
		t.Errorf("refs = %v", refs)
	}
	if len(diags) != 0 {
		t.Errorf("diags = %v", diags)
	}
	for _, want := range []string{"alpha", "# beta", "Referenced files:", "--- a.txt ---", "--- b.md ---"} {
		if !contains(out, want) {
			t.Errorf("expanded missing %q:\n%s", want, out)
		}
	}
}

func TestExpandAtRefs_DropsBadRefs(t *testing.T) {
	t.Parallel()
	reader := func(p string) ([]byte, error) {
		return nil, &fakeReadErr{p}
	}
	_, refs, diags := expandAtRefs("see @nope.txt", reader)
	if len(refs) != 0 {
		t.Errorf("expected no refs read, got %v", refs)
	}
	if len(diags) != 1 {
		t.Fatalf("expected 1 diagnostic, got %v", diags)
	}
}

type fakeReadErr struct{ path string }

func (e *fakeReadErr) Error() string { return "no such file: " + e.path }

func contains(haystack, needle string) bool {
	return len(needle) == 0 || indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
