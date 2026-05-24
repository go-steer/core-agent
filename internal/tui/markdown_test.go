// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"regexp"
	"strings"
	"testing"
)

// ansiEscape matches CSI sequences (color/style codes) emitted by
// Glamour. Stripping these makes string assertions resilient to the
// per-word styling Glamour wraps content in.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiEscape.ReplaceAllString(s, "") }

// TestMarkdownRenderer_HeadingMarkersStripped pins the heading
// rendering contract: H1-H6 must NOT leave the literal "#"/"##"/"###"
// prefix in the rendered output. Glamour's bundled "dark" / "light"
// styles ship with H2-H6 prefixes set to the marker itself; cogo's
// custom style overrides those (see cogoStyleConfig in markdown.go).
//
// DO NOT relax this assertion if it breaks. The user-visible bug is
// the same one that motivated the fix: assistant messages render with
// raw "## Section" instead of a styled heading. If a future Glamour
// upgrade or refactor changes how prefixes work, replace the regex
// with one that proves the new path still strips them — never delete
// the contract.
//
// TestMarkdownRenderer_CodeBlockSeparators pins the second contract:
// fenced code blocks must be bracketed by visible separator lines so
// the chat distinguishes code from prose. Same rule — if the test
// breaks, prove the new code path still produces visible chrome
// around code blocks; do not silence the assertion.
func TestMarkdownRenderer_HeadingMarkersStripped(t *testing.T) {
	t.Parallel()
	for _, style := range []string{"dark", "light"} {
		style := style
		t.Run(style, func(t *testing.T) {
			t.Parallel()
			mr, err := NewMarkdownRenderer(80, style)
			if err != nil {
				t.Fatalf("NewMarkdownRenderer(%q): %v", style, err)
			}
			src := strings.Join([]string{
				"# H1 line",
				"## H2 line",
				"### H3 line",
				"#### H4 line",
				"##### H5 line",
				"###### H6 line",
				"",
				"body paragraph",
			}, "\n")
			out := stripANSI(mr.Render(src))
			// Each leak pattern is unambiguous: real prose wouldn't
			// produce "## " at the start of a paragraph because the
			// renderer would have consumed the marker as a heading.
			for _, leak := range []string{"## H2", "### H3", "#### H4", "##### H5", "###### H6"} {
				if strings.Contains(out, leak) {
					t.Errorf("%s style: rendered output still contains %q\nfull output:\n%s", style, leak, out)
				}
			}
			// And the heading text itself must survive — make sure the
			// fix didn't strip the content along with the marker.
			for _, want := range []string{"H1 line", "H2 line", "H3 line", "H4 line", "H5 line", "H6 line"} {
				if !strings.Contains(out, want) {
					t.Errorf("%s style: rendered output missing %q\nfull output:\n%s", style, want, out)
				}
			}
		})
	}
}

func TestMarkdownRenderer_CodeBlockSeparators(t *testing.T) {
	t.Parallel()
	for _, style := range []string{"dark", "light"} {
		style := style
		t.Run(style, func(t *testing.T) {
			t.Parallel()
			mr, err := NewMarkdownRenderer(80, style)
			if err != nil {
				t.Fatalf("NewMarkdownRenderer(%q): %v", style, err)
			}
			src := "Some prose.\n\n```go\nfunc hi() { println(\"x\") }\n```\n\nMore prose.\n"
			out := stripANSI(mr.Render(src))
			// The bar glyphs must show up around the code block. We
			// check for substrings rather than the full bar to stay
			// resilient to width/padding tweaks.
			if !strings.Contains(out, "──── code ────") {
				t.Errorf("%s style: missing top separator with 'code' label\nfull output:\n%s", style, out)
			}
			if !strings.Contains(out, "──────────────────────") {
				t.Errorf("%s style: missing bottom separator bar\nfull output:\n%s", style, out)
			}
			// And the actual code content must still survive.
			if !strings.Contains(out, "func hi()") {
				t.Errorf("%s style: code content missing\nfull output:\n%s", style, out)
			}
		})
	}
}
