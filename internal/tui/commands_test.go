// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import "testing"

func TestParseSlash(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		input       string
		wantAction  SlashAction
		wantIsSlash bool
		wantCmd     string
	}{
		{"plain text", "hello world", "", false, ""},
		{"help", "/help", SlashHelp, true, "help"},
		{"help question mark", "/?", SlashHelp, true, "?"},
		{"quit", "/quit", SlashQuit, true, "quit"},
		{"exit alias", "/exit", SlashQuit, true, "exit"},
		{"q alias", "/q", SlashQuit, true, "q"},
		{"clear", "/clear", SlashClear, true, "clear"},
		{"case insensitive", "/HELP", SlashHelp, true, "help"},
		{"leading whitespace", "  /help", SlashHelp, true, "help"},
		{"trailing whitespace", "/help   ", SlashHelp, true, "help"},
		{"unknown", "/foo", SlashUnknown, true, "foo"},
		{"bare slash", "/", SlashUnknown, true, ""},
		{"slash with args ignored", "/help me out", SlashHelp, true, "help"},
		{"empty input", "", "", false, ""},
		{"just whitespace", "   ", "", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			action, cmd, _, isSlash := ParseSlash(tc.input)
			if isSlash != tc.wantIsSlash {
				t.Errorf("isSlash = %v, want %v", isSlash, tc.wantIsSlash)
			}
			if action != tc.wantAction {
				t.Errorf("action = %q, want %q", action, tc.wantAction)
			}
			if cmd != tc.wantCmd {
				t.Errorf("cmd = %q, want %q", cmd, tc.wantCmd)
			}
		})
	}
}

func TestHelpText_Nonempty(t *testing.T) {
	t.Parallel()
	if HelpText() == "" {
		t.Fatal("HelpText() returned empty string")
	}
}
