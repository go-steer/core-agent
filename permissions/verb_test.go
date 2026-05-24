// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package permissions

import "testing"

// TestExtractBashVerb covers the cases the prompter relies on to decide
// whether to show the "Allow `<verb> *` · session" option. Empty verb
// must hide the option entirely; populated verb must round-trip to the
// session-allow map. If you weaken the slash/quote/empty guards here,
// the prompt offers to broaden permissions to nothing useful — or
// worse, to a path-specific script. DO NOT delete this test to silence
// a compile failure; fix the helper instead.
func TestExtractBashVerb(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"git status", "git"},
		{"git diff --stat origin/main", "git"},
		{"ls", "ls"},
		{"ls -la /tmp", "ls"},
		{"CGO_ENABLED=0 go build", "go"},
		{"GOOS=linux GOARCH=arm64 go test ./...", "go"},
		{"VAR=1 OTHER=2 npm test", "npm"},

		// Empty / whitespace / no verb available
		{"", ""},
		{"   ", ""},

		// Path-like verbs — too specific to broaden to "* *"
		{"./script.sh --flag", ""},
		{"/usr/bin/env python3", ""},
		{`C:\Tools\foo.exe`, ""},

		// Quoted commands — quoting signals args, not a verb to trust
		{`"git" status`, ""},
		{`'echo' hi`, ""},

		// Assignment-only — no real verb after the env stripping
		{"FOO=bar", ""},

		// LHS that isn't an identifier shouldn't be treated as assignment
		{"--foo=bar baz", ""}, // starts with `-`, no leading letter; verb path-check rejects it
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := extractBashVerb(tc.in); got != tc.want {
				t.Errorf("extractBashVerb(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsAssignmentKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"FOO", true},
		{"_X", true},
		{"FOO_BAR_2", true},
		{"foo", true},
		{"", false},
		{"2FOO", false}, // can't lead with a digit
		{"FOO-BAR", false},
		{"FOO.BAR", false},
		{"--foo", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := isAssignmentKey(tc.in); got != tc.want {
				t.Errorf("isAssignmentKey(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
