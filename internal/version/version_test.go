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

package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

// TestFormatVersion pins the wire format of --version output.
func TestFormatVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		prog, v, c, d string
		dirty         bool
		want          string
	}{
		{
			name: "release",
			prog: "core-agent",
			v:    "v2.1.0", c: "a1b2c3d4e5f6", d: "2026-05-31T12:00:00Z",
			want: "core-agent v2.1.0 (commit a1b2c3d4, built 2026-05-31T12:00:00Z)",
		},
		{
			name: "dev-dirty",
			prog: "core-agent-tui",
			v:    "v2.2.0-dev", c: "deadbeefcafe", d: "2026-06-01T08:00:00Z", dirty: true,
			want: "core-agent-tui v2.2.0-dev (commit deadbeef, modified, built 2026-06-01T08:00:00Z)",
		},
		{
			name: "short-sha-untouched",
			prog: "core-agent",
			v:    "v2.1.0", c: "abc", d: "2026-05-31T12:00:00Z",
			want: "core-agent v2.1.0 (commit abc, built 2026-05-31T12:00:00Z)",
		},
		{
			name: "no-vcs-info",
			prog: "core-agent",
			v:    "v2.2.0-dev", c: "none", d: "unknown",
			want: "core-agent v2.2.0-dev (commit none, built unknown)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := formatVersion(tc.prog, tc.v, tc.c, tc.d, tc.dirty)
			if got != tc.want {
				t.Errorf("formatVersion =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

// TestResolveBuildInfo_LdflagsWin asserts that injected values are
// authoritative — the debug.BuildInfo fallback only fires when the
// defaults are still in place.
func TestResolveBuildInfo_LdflagsWin(t *testing.T) {
	t.Parallel()
	v, c, d, dirty := resolveBuildInfo("v2.1.0", "abcd1234", "2026-05-31T00:00:00Z")
	if v != "v2.1.0" || c != "abcd1234" || d != "2026-05-31T00:00:00Z" || dirty {
		t.Errorf("resolveBuildInfo with ldflags = (%q, %q, %q, %v), want injected values unchanged",
			v, c, d, dirty)
	}
}

// TestResolveBuildInfo_FallbackUsesVCS proves the fallback path
// reaches the embedded VCS metadata. Skips when debug.BuildInfo
// isn't populated (e.g. binary built without -buildvcs=true).
func TestResolveBuildInfo_FallbackUsesVCS(t *testing.T) {
	t.Parallel()
	info, ok := debug.ReadBuildInfo()
	if !ok {
		t.Skip("debug.ReadBuildInfo unavailable")
	}
	var haveRevision bool
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			haveRevision = true
			break
		}
	}
	if !haveRevision {
		t.Skip("test binary has no vcs.revision (built outside a git checkout)")
	}
	// With the default "none" commit, the fallback must populate
	// commit from vcs.revision.
	_, c, _, _ := resolveBuildInfo("dev", "none", "unknown")
	if c == "none" {
		t.Errorf("resolveBuildInfo fallback: commit stayed %q despite vcs.revision being present", c)
	}
}

// TestString_LeadingTokens guarantees the format starts with
// "<prog> <version>" so scripts can grep / cut the first two tokens
// without parsing the parenthesized suffix.
func TestString_LeadingTokens(t *testing.T) {
	t.Parallel()
	out := String("core-agent")
	fields := strings.Fields(out)
	if len(fields) < 2 {
		t.Fatalf("String() = %q, want at least two whitespace-separated tokens", out)
	}
	if fields[0] != "core-agent" {
		t.Errorf("first token = %q, want %q", fields[0], "core-agent")
	}
	if !strings.HasPrefix(fields[1], "v") && fields[1] != "dev" {
		t.Errorf("second token = %q, want a version starting with v… or \"dev\"", fields[1])
	}
}
