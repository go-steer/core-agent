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
)

func TestParseAllowPathSpec_SimpleRW(t *testing.T) {
	got, err := parseAllowPathSpec("/home/me/sibling:rw")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/home/me/sibling" || got.Access != "rw" {
		t.Errorf("got {%q,%q}, want {/home/me/sibling, rw}", got.Path, got.Access)
	}
}

func TestParseAllowPathSpec_ReadOnly(t *testing.T) {
	got, err := parseAllowPathSpec("/tmp:r")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/tmp" || got.Access != "r" {
		t.Errorf("got {%q,%q}, want {/tmp, r}", got.Path, got.Access)
	}
}

func TestParseAllowPathSpec_NoSuffixFails(t *testing.T) {
	_, err := parseAllowPathSpec("/tmp")
	if err == nil {
		t.Fatal("expected error when no :ACCESS suffix")
	}
	// Error should hint at the explicit-access requirement and show
	// the kind of spec we want.
	if !strings.Contains(err.Error(), "explicit access") {
		t.Errorf("error should mention 'explicit access', got: %v", err)
	}
	if !strings.Contains(err.Error(), ":rw") {
		t.Errorf("error should suggest the canonical form, got: %v", err)
	}
}

func TestParseAllowPathSpec_EmptyAccessFails(t *testing.T) {
	_, err := parseAllowPathSpec("/tmp:")
	if err == nil {
		t.Fatal("expected error for empty access")
	}
	if !strings.Contains(err.Error(), "access empty") {
		t.Errorf("error should mention empty access, got: %v", err)
	}
}

func TestParseAllowPathSpec_BadAccessFails(t *testing.T) {
	_, err := parseAllowPathSpec("/tmp:bogus")
	if err == nil {
		t.Fatal("expected error for invalid access spec")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should mention the bad value, got: %v", err)
	}
}

func TestParseAllowPathSpec_EmptyFails(t *testing.T) {
	_, err := parseAllowPathSpec("")
	if err == nil {
		t.Fatal("expected error for empty spec")
	}
	if _, err2 := parseAllowPathSpec("   "); err2 == nil {
		t.Errorf("expected error for whitespace-only spec")
	}
}

func TestParseAllowPathSpec_LastColonWins(t *testing.T) {
	// Pathological but legal: a path containing a colon. The split
	// MUST be at the LAST colon so "/foo:bar:rw" resolves cleanly.
	got, err := parseAllowPathSpec("/foo:bar:rw")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/foo:bar" || got.Access != "rw" {
		t.Errorf("got {%q,%q}, want {/foo:bar, rw}", got.Path, got.Access)
	}
}

func TestParseAllowPathSpec_PathEmptyFails(t *testing.T) {
	_, err := parseAllowPathSpec(":rw")
	if err == nil {
		t.Fatal("expected error for empty path")
	}
	if !strings.Contains(err.Error(), "path empty") {
		t.Errorf("error should mention empty path, got: %v", err)
	}
}

func TestParseAllowPathSpec_TrimsWhitespace(t *testing.T) {
	got, err := parseAllowPathSpec("  /tmp:rw  ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "/tmp" || got.Access != "rw" {
		t.Errorf("got {%q,%q}, want {/tmp, rw}", got.Path, got.Access)
	}
}
