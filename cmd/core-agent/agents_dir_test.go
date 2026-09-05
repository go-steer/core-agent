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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveAgentsDir pins the precedence #945 asks for:
// --agents-dir > filepath.Dir(-c) > .agents/ discovered from cwd.
//
// The "no flag" rows are the compatibility half — the derivation stays
// the default, so an operator who does not pass the flag sees no
// change at all.
func TestResolveAgentsDir(t *testing.T) {
	t.Parallel()
	realDir := t.TempDir()

	cases := []struct {
		name       string
		flagValue  string
		cfgPath    string
		derived    string
		wantDir    string
		wantSource agentsDirSource
	}{
		{
			name:       "no flag, -c given: derivation stands",
			cfgPath:    "/etc/core-agent/.agents/config.json",
			derived:    "/etc/core-agent/.agents",
			wantDir:    "/etc/core-agent/.agents",
			wantSource: agentsDirFromCfg,
		},
		{
			name:       "no flag, no -c: discovery stands",
			derived:    "/proj/.agents",
			wantDir:    "/proj/.agents",
			wantSource: agentsDirDiscovery,
		},
		{
			name:       "no flag, nothing found",
			wantDir:    "",
			wantSource: agentsDirNone,
		},
		{
			// The case the issue is about: config.json in one place,
			// the tree it configures in another.
			name:       "flag beats the -c derivation",
			flagValue:  realDir,
			cfgPath:    "/etc/core-agent/config.json",
			derived:    "/etc/core-agent",
			wantDir:    realDir,
			wantSource: agentsDirExplicit,
		},
		{
			name:       "flag beats discovery",
			flagValue:  realDir,
			derived:    "/proj/.agents",
			wantDir:    realDir,
			wantSource: agentsDirExplicit,
		},
		{
			name:       "flag stands when nothing else was found",
			flagValue:  realDir,
			wantDir:    realDir,
			wantSource: agentsDirExplicit,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotDir, gotSource, err := resolveAgentsDir(tc.flagValue, tc.cfgPath, tc.derived)
			if err != nil {
				t.Fatalf("resolveAgentsDir: unexpected error %v", err)
			}
			if gotDir != tc.wantDir {
				t.Errorf("dir = %q, want %q", gotDir, tc.wantDir)
			}
			if gotSource != tc.wantSource {
				t.Errorf("source = %v (%s), want %v (%s)", gotSource, gotSource, tc.wantSource, tc.wantSource)
			}
		})
	}
}

// TestResolveAgentsDirRejectsBadPath: a typo'd --agents-dir must be
// fatal, not a warning. Everything downstream of this value degrades
// *silently* when it points nowhere — no MCP servers, no skills, no
// env manifest, nowhere for record_plan to write — so accepting a bad
// path produces a daemon that starts cleanly and knows nothing. That
// is the exact failure #945 exists to prevent.
func TestResolveAgentsDirRejectsBadPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cases := []struct {
		name      string
		flagValue string
		wantIn    string
	}{
		{"missing directory", filepath.Join(dir, "nope"), "nope"},
		{"a file, not a directory", file, "not a directory"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A valid derivation is available and must NOT be used as
			// a fallback: silently falling back is the same silent
			// failure with an extra step.
			_, _, err := resolveAgentsDir(tc.flagValue, "/etc/core-agent/config.json", "/etc/core-agent")
			if err == nil {
				t.Fatalf("resolveAgentsDir(%q) = nil error, want a refusal", tc.flagValue)
			}
			if !strings.Contains(err.Error(), "--agents-dir") {
				t.Errorf("error %q does not name the flag the operator has to fix", err)
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error %q missing %q", err, tc.wantIn)
			}
		})
	}
}

// TestAgentsDirSourceString: the startup summary reads these, and a
// wrong one sends an operator debugging a bad agentsDir to the wrong
// place entirely.
func TestAgentsDirSourceString(t *testing.T) {
	t.Parallel()
	cases := map[agentsDirSource]string{
		agentsDirExplicit:  "via --agents-dir",
		agentsDirFromCfg:   "derived from filepath.Dir(-c)",
		agentsDirDiscovery: "via .agents/ discovery",
		agentsDirNone:      "",
	}
	for src, want := range cases {
		if got := src.String(); got != want {
			t.Errorf("agentsDirSource(%d).String() = %q, want %q", src, got, want)
		}
	}
}

// TestSplitTreeWarning covers the combination that is confusing rather
// than wrong: --agents-dir with no -c, while discovery landed on a
// config.json in a different tree. Settings then come from one place
// and content from another, and the symptom looks like content that
// failed to load rather than content that was never looked for.
func TestSplitTreeWarning(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		source     agentsDirSource
		cfgPath    string
		agentsDir  string
		discovered string
		wantWarn   bool
	}{
		{
			name:       "explicit flag, no -c, config discovered elsewhere",
			source:     agentsDirExplicit,
			agentsDir:  "/opt/recipe/.agents",
			discovered: "/home/op/proj/.agents",
			wantWarn:   true,
		},
		{
			// -c present means the operator named both halves on
			// purpose. That is the whole point of the flag, so
			// warning about it would be noise on every recipe boot.
			name:       "explicit flag with -c is deliberate",
			source:     agentsDirExplicit,
			cfgPath:    "/etc/core-agent/config.json",
			agentsDir:  "/opt/recipe/.agents",
			discovered: "/etc/core-agent",
			wantWarn:   false,
		},
		{
			name:       "explicit flag pointing at the discovered tree",
			source:     agentsDirExplicit,
			agentsDir:  "/home/op/proj/.agents",
			discovered: "/home/op/proj/.agents",
			wantWarn:   false,
		},
		{
			name:      "explicit flag, nothing was discovered",
			source:    agentsDirExplicit,
			agentsDir: "/opt/recipe/.agents",
			wantWarn:  false,
		},
		{
			name:       "no flag at all",
			source:     agentsDirDiscovery,
			agentsDir:  "/home/op/proj/.agents",
			discovered: "/home/op/proj/.agents",
			wantWarn:   false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := splitTreeWarning(tc.source, tc.cfgPath, tc.agentsDir, tc.discovered)
			if (got != "") != tc.wantWarn {
				t.Fatalf("splitTreeWarning = %q, wantWarn=%v", got, tc.wantWarn)
			}
			if !tc.wantWarn {
				return
			}
			// The warning has to name both trees and the fix; "your
			// config and content differ" would leave the operator no
			// better off than the silence it replaces.
			for _, want := range []string{tc.agentsDir, tc.discovered, "-c "} {
				if !strings.Contains(got, want) {
					t.Errorf("warning %q missing %q", got, want)
				}
			}
		})
	}
}

// TestSplitTreeWarningIgnoresPathSpelling: "." and the absolute cwd
// are the same directory. Warning about a difference in spelling would
// train operators to ignore the line.
func TestSplitTreeWarningIgnoresPathSpelling(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "agents")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := splitTreeWarning(agentsDirExplicit, "", link, real); got != "" {
		t.Errorf("a symlink to the same directory warned: %q", got)
	}
	if got := splitTreeWarning(agentsDirExplicit, "", filepath.Join(real, "."), real); got != "" {
		t.Errorf("the same directory spelled two ways warned: %q", got)
	}
}
