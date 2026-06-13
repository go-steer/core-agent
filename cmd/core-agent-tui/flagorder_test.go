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
	"flag"
	"reflect"
	"testing"
)

// buildTestFlagSet mirrors the actual TUI flag set so the permute
// tests exercise the real arity decisions (bool vs value-taking).
// Kept in lockstep with main.go's flag definitions.
func buildTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("core-agent-tui-test", flag.ContinueOnError)
	fs.String("token", "", "")
	fs.String("auth", "", "")
	fs.String("theme", "", "")
	fs.String("alias", "", "")
	fs.Bool("new-session", false, "")
	return fs
}

func TestPermuteFlags_AllFlagsFirst(t *testing.T) {
	t.Parallel()
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, []string{
		"--token", "T", "--new-session", "http://host:7777",
	})
	wantFlags := []string{"--token", "T", "--new-session"}
	wantPositionals := []string{"http://host:7777"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags: got %v, want %v", flags, wantFlags)
	}
	if !reflect.DeepEqual(positionals, wantPositionals) {
		t.Errorf("positionals: got %v, want %v", positionals, wantPositionals)
	}
}

func TestPermuteFlags_FlagsAfterPositional(t *testing.T) {
	t.Parallel()
	// This is the bug case — without permuting, the standard flag
	// package stops at "http://host:7777" and silently drops
	// `--token T` and `--new-session`. The permuter pulls them
	// in front so fs.Parse sees them as flags.
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, []string{
		"http://host:7777", "--token", "T", "--new-session",
	})
	wantFlags := []string{"--token", "T", "--new-session"}
	wantPositionals := []string{"http://host:7777"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags: got %v, want %v", flags, wantFlags)
	}
	if !reflect.DeepEqual(positionals, wantPositionals) {
		t.Errorf("positionals: got %v, want %v", positionals, wantPositionals)
	}
}

func TestPermuteFlags_Interleaved(t *testing.T) {
	t.Parallel()
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, []string{
		"--token", "T", "http://host:7777", "--new-session", "extra",
	})
	wantFlags := []string{"--token", "T", "--new-session"}
	wantPositionals := []string{"http://host:7777", "extra"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags: got %v, want %v", flags, wantFlags)
	}
	if !reflect.DeepEqual(positionals, wantPositionals) {
		t.Errorf("positionals: got %v, want %v", positionals, wantPositionals)
	}
}

func TestPermuteFlags_EqualsForm(t *testing.T) {
	t.Parallel()
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, []string{
		"http://host:7777", "--token=T", "--new-session=true",
	})
	wantFlags := []string{"--token=T", "--new-session=true"}
	wantPositionals := []string{"http://host:7777"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags: got %v, want %v", flags, wantFlags)
	}
	if !reflect.DeepEqual(positionals, wantPositionals) {
		t.Errorf("positionals: got %v, want %v", positionals, wantPositionals)
	}
}

func TestPermuteFlags_BoolFlagDoesNotConsumeNextArg(t *testing.T) {
	t.Parallel()
	// --new-session is a bool — the next arg ("http://...") must NOT
	// be claimed as its value. Regression guard for accidentally
	// treating every flag as value-taking.
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, []string{
		"--new-session", "http://host:7777",
	})
	wantFlags := []string{"--new-session"}
	wantPositionals := []string{"http://host:7777"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags: got %v, want %v", flags, wantFlags)
	}
	if !reflect.DeepEqual(positionals, wantPositionals) {
		t.Errorf("positionals: got %v, want %v", positionals, wantPositionals)
	}
}

func TestPermuteFlags_StringFlagConsumesNextArg(t *testing.T) {
	t.Parallel()
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, []string{
		"--token", "MY_TOKEN", "http://host:7777",
	})
	wantFlags := []string{"--token", "MY_TOKEN"}
	wantPositionals := []string{"http://host:7777"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags: got %v, want %v", flags, wantFlags)
	}
	if !reflect.DeepEqual(positionals, wantPositionals) {
		t.Errorf("positionals: got %v, want %v", positionals, wantPositionals)
	}
}

func TestPermuteFlags_DoubleDashStopsPermutation(t *testing.T) {
	t.Parallel()
	// POSIX convention: -- ends flag parsing. Everything after is
	// positional, even if it looks like a flag.
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, []string{
		"--token", "T", "--", "--new-session", "http://host:7777",
	})
	wantFlags := []string{"--token", "T"}
	wantPositionals := []string{"--new-session", "http://host:7777"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags: got %v, want %v", flags, wantFlags)
	}
	if !reflect.DeepEqual(positionals, wantPositionals) {
		t.Errorf("positionals: got %v, want %v", positionals, wantPositionals)
	}
}

func TestPermuteFlags_UnknownFlagPassesThrough(t *testing.T) {
	t.Parallel()
	// Unknown flags are forwarded to fs.Parse so the standard
	// "flag provided but not defined" error fires. We don't try
	// to consume a value for unknown flags (we don't know if they
	// take one).
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, []string{
		"--bogus", "http://host:7777",
	})
	wantFlags := []string{"--bogus"}
	wantPositionals := []string{"http://host:7777"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags: got %v, want %v", flags, wantFlags)
	}
	if !reflect.DeepEqual(positionals, wantPositionals) {
		t.Errorf("positionals: got %v, want %v", positionals, wantPositionals)
	}
}

func TestPermuteFlags_RealWorldBugCase(t *testing.T) {
	t.Parallel()
	// The exact command from the bug report:
	//   core-agent-tui http://127.0.0.1:7777 --token ALICE_TOKEN --new-session
	// Without the permuter, --token + --new-session get silently
	// dropped → picker runs without auth → GET /sessions 401.
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, []string{
		"http://127.0.0.1:7777", "--token", "ALICE_TOKEN", "--new-session",
	})

	// fs.Parse should now succeed with the right flag values.
	if err := fs.Parse(flags); err != nil {
		t.Fatalf("fs.Parse(flags=%v): %v", flags, err)
	}
	if got := fs.Lookup("token").Value.String(); got != "ALICE_TOKEN" {
		t.Errorf("--token: got %q, want ALICE_TOKEN", got)
	}
	if got := fs.Lookup("new-session").Value.String(); got != "true" {
		t.Errorf("--new-session: got %q, want true", got)
	}
	wantPositionals := []string{"http://127.0.0.1:7777"}
	if !reflect.DeepEqual(positionals, wantPositionals) {
		t.Errorf("positionals: got %v, want %v", positionals, wantPositionals)
	}
}

func TestPermuteFlags_EmptyArgs(t *testing.T) {
	t.Parallel()
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, nil)
	if len(flags) != 0 {
		t.Errorf("flags: got %v, want empty", flags)
	}
	if len(positionals) != 0 {
		t.Errorf("positionals: got %v, want empty", positionals)
	}
}

func TestPermuteFlags_BareDashIsPositional(t *testing.T) {
	t.Parallel()
	// "-" alone is conventionally stdin. Treat as a positional;
	// also stop permuting (matches POSIX semantics for `--`).
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, []string{
		"--token", "T", "-", "--new-session",
	})
	wantFlags := []string{"--token", "T"}
	wantPositionals := []string{"--new-session"}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags: got %v, want %v", flags, wantFlags)
	}
	if !reflect.DeepEqual(positionals, wantPositionals) {
		t.Errorf("positionals: got %v, want %v", positionals, wantPositionals)
	}
}
