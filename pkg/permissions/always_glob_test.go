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

package permissions

import (
	"context"
	"testing"
)

// #1179: an "allow always" grant is for the call the operator saw. A
// glob metacharacter in that call's key is part of the call, so the
// installed pattern must match the key and nothing the metacharacter
// would otherwise reach. The derived session is the witness: its
// session-allow maps start empty, so only the shared policy can let a
// call through without a prompt.
func TestAllowAlways_KeyGlobCharsAreLiteral(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, key, pattern, other string
	}{
		{"trailing star", "files/report*", `fetch_url:files/report\*`, "files/report-of-everyone"},
		{"inner star", "a*b", `fetch_url:a\*b`, "a-anything-b"},
		{"question mark", "id=?", `fetch_url:id=\?`, "id=7"},
		{"class", "row[0-9]", `fetch_url:row\[0-9]`, "row5"},
		{"backslash", `C:\x*`, `fetch_url:C:\\x\*`, `C:\xyz`},
		// The escaped pattern spelled as a key is a different call:
		// in bash `x\;y` is one word, `x\\;y` ends a command at `;`.
		{"escaped spelling", `x\;y`, `fetch_url:x\\;y`, `x\\;y`},
		{"escaped star spelling", "a*", `fetch_url:a\*`, `a\*`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store := &fakeGrantStore{}
			p := &fakePrompter{decision: DecisionAllowAlways}
			g := New(Options{Prompter: p, GrantStore: store})
			if err := g.CheckGeneric(context.Background(), "fetch_url", tc.key); err != nil {
				t.Fatalf("CheckGeneric(%q) with AllowAlways: %v", tc.key, err)
			}
			got := store.all()
			if len(got) != 1 || got[0].Pattern != tc.pattern || got[0].Key != tc.key {
				t.Errorf("grants = %+v, want one with Pattern %q and Key %q", got, tc.pattern, tc.key)
			}

			sub := g.DeriveForSession("s2", &fakePrompter{decision: DecisionDeny})
			if err := sub.CheckGeneric(context.Background(), "fetch_url", tc.key); err != nil {
				t.Errorf("the approved key %q re-prompted on another session: %v", tc.key, err)
			}
			if err := sub.CheckGeneric(context.Background(), "fetch_url", tc.other); err == nil {
				t.Errorf("always-grant for %q also allowed %q without a prompt", tc.key, tc.other)
			}
		})
	}
}

// An unbalanced `[` used to make AddAllow reject the pattern, so the
// operator's "always" failed outright. Escaped, it installs.
func TestAllowAlways_UnbalancedBracketInstalls(t *testing.T) {
	t.Parallel()
	store := &fakeGrantStore{}
	g := New(Options{Prompter: &fakePrompter{decision: DecisionAllowAlways}, GrantStore: store})
	if err := g.CheckGeneric(context.Background(), "fetch_url", "q=[unclosed"); err != nil {
		t.Fatalf("CheckGeneric with an unbalanced [: %v", err)
	}
	if got := store.all(); len(got) != 1 || got[0].Pattern != `fetch_url:q=\[unclosed` {
		t.Fatalf("grants = %+v, want Pattern fetch_url:q=\\[unclosed", got)
	}
}

func TestEscapeGlob(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"":             "",
		"git status":   "git status",
		"rm *":         `rm \*`,
		"a?b[c]":       `a\?b\[c]`,
		`back\slash`:   `back\\slash`,
		`\*`:           `\\\*`,
		"ünïcødé*":     `ünïcødé\*`,
		"x]y-z^":       "x]y-z^",
		"two**stars?":  `two\*\*stars\?`,
		"bad\xffutf8*": "bad\xffutf8\\*",
	} {
		if got := escapeGlob(in); got != want {
			t.Errorf("escapeGlob(%q) = %q, want %q", in, got, want)
		}
		// Whatever the input, the escaped form matches it.
		if !matchGlob(escapeGlob(in), in) {
			t.Errorf("matchGlob(escapeGlob(%q), %q) = false", in, in)
		}
	}
}

// A trailing `*` is an open prefix only when it is not escaped, i.e.
// behind an even run of backslashes.
func TestIsOpenPrefixPattern_EscapedStar(t *testing.T) {
	t.Parallel()
	for pat, want := range map[string]bool{
		"git diff*": true,
		"*":         true,
		`rm \*`:     false,
		`\*`:        false,
		`a\\*`:      true,
		`a\\\*`:     false,
		"a*b":       false,
		"a?*":       false,
	} {
		if got := isOpenPrefixPattern(pat); got != want {
			t.Errorf("isOpenPrefixPattern(%q) = %v, want %v", pat, got, want)
		}
	}
	if matchGlob(`rm \*`, "rm -rf /") {
		t.Error(`matchGlob("rm \\*", "rm -rf /") = true; an escaped star is a literal`)
	}
}

// The exact-string shortcut is skipped only for a pattern carrying an
// escape escapeGlob writes. A hand-written allow whose backslash is
// something else keeps matching its own text exactly, as before #1179.
func TestMatchAllow_ExactShortcutKeptForOtherBackslashes(t *testing.T) {
	t.Parallel()
	p, err := NewPolicy([]string{`bash:echo a\b`, `bash:echo x\\;y`}, nil)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	for key, want := range map[string]Outcome{
		`echo a\b`:   OutcomeAllow,     // exact, as before
		`echo x\;y`:  OutcomeAllow,     // what the escaped pattern means
		`echo x\\;y`: OutcomeUnmatched, // its spelling is not that call
	} {
		if got := p.Match("bash", key); got != want {
			t.Errorf("Match(bash, %q) = %v, want %v", key, got, want)
		}
	}
}
