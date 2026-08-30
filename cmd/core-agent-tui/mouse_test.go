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

import "testing"

// The default has to stay nil rather than a pointer to true: core-tui
// reads nil as "no host opinion" and applies its own default, so an
// operator who passed no mouse flag leaves that default free to move.
// Pinning it here would silently override a future core-tui change.
func TestMouseOptFromFlag_UnsetIsNil(t *testing.T) {
	t.Parallel()
	if got := mouseOptFromFlag(false); got != nil {
		t.Errorf("--no-mouse absent: got %v, want nil (core-tui default, capture on)", *got)
	}
}

func TestMouseOptFromFlag_SetDisablesCapture(t *testing.T) {
	t.Parallel()
	got := mouseOptFromFlag(true)
	if got == nil {
		t.Fatal("--no-mouse: got nil, want a pointer to false")
	}
	if *got {
		t.Error("--no-mouse: got true, want false (capture disabled)")
	}
}

// --no-mouse is a bool flag, so permuteFlags must treat it as
// self-contained. If it were ever mistaken for a value-taking flag it
// would swallow the attach URL, and the operator would get the bare-
// invocation URL prompt instead of a connection — the same class of
// silent arg-eating that motivated permuteFlags in the first place.
func TestPermuteFlags_NoMouseDoesNotConsumeURL(t *testing.T) {
	t.Parallel()
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, []string{"--no-mouse", "http://localhost:7777"})
	if len(positionals) != 1 || positionals[0] != "http://localhost:7777" {
		t.Errorf("positionals = %v, want [http://localhost:7777]", positionals)
	}
	if len(flags) != 1 || flags[0] != "--no-mouse" {
		t.Errorf("flags = %v, want [--no-mouse]", flags)
	}
}

// The trailing-flag form is the one the permute logic exists for.
func TestPermuteFlags_NoMouseAfterURL(t *testing.T) {
	t.Parallel()
	fs := buildTestFlagSet()
	flags, positionals := permuteFlags(fs, []string{"http://localhost:7777", "--no-mouse"})
	if len(positionals) != 1 || positionals[0] != "http://localhost:7777" {
		t.Errorf("positionals = %v, want [http://localhost:7777]", positionals)
	}
	if len(flags) != 1 || flags[0] != "--no-mouse" {
		t.Errorf("flags = %v, want [--no-mouse]", flags)
	}
}
