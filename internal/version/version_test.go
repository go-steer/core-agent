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
			// `go install module@tag`: module version known, no
			// commit/date — the sentinels are omitted, not printed.
			name: "no-vcs-info",
			prog: "core-agent",
			v:    "v2.8.0-dev.5", c: "none", d: "unknown",
			want: "core-agent v2.8.0-dev.5",
		},
		{
			name: "commit-without-date",
			prog: "core-agent",
			v:    "v2.2.0-dev", c: "deadbeefcafe", d: "unknown",
			want: "core-agent v2.2.0-dev (commit deadbeef)",
		},
		{
			name: "date-without-commit",
			prog: "core-agent",
			v:    "v2.2.0-dev", c: "none", d: "2026-06-01T08:00:00Z",
			want: "core-agent v2.2.0-dev (built 2026-06-01T08:00:00Z)",
		},
		{
			// A broken release pipeline injecting empty -X values
			// must not yield "(commit , built )".
			name: "empty-injections-omitted",
			prog: "core-agent",
			v:    "v2.2.0-dev", c: "", d: "",
			want: "core-agent v2.2.0-dev",
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

// TestResolveFromBuildInfo covers the two BuildInfo fallback sources
// and their precedence: vcs.* settings (git-checkout builds) win over
// Main.Version (module-cache builds), and "(devel)" never leaks into
// the reported version.
func TestResolveFromBuildInfo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		info      *debug.BuildInfo
		wantV     string
		wantC     string
		wantD     string
		wantDirty bool
	}{
		{
			// `go install module@tag`: no vcs.*, exact module version.
			name:  "module-exact-tag",
			info:  &debug.BuildInfo{Main: debug.Module{Version: "v2.8.0-dev.5"}},
			wantV: "v2.8.0-dev.5", wantC: "none", wantD: "unknown",
		},
		{
			// `go install module@main`: pseudo-version suffix yields
			// commit SHA + commit time.
			name:  "module-pseudo-version",
			info:  &debug.BuildInfo{Main: debug.Module{Version: "v2.8.0-dev.5.0.20260802143512-abcdef123456"}},
			wantV: "v2.8.0-dev.5.0.20260802143512-abcdef123456",
			wantC: "abcdef123456", wantD: "2026-08-02T14:35:12Z",
		},
		{
			// Plain `go build` in a checkout: vcs.* populated,
			// Main.Version is "(devel)" and must not override the
			// -dev fallback version.
			name: "vcs-checkout",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "deadbeefcafe0123"},
					{Key: "vcs.time", Value: "2026-06-01T08:00:00Z"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			wantV: "v2.8.0-dev", wantC: "deadbeefcafe0123",
			wantD: "2026-06-01T08:00:00Z", wantDirty: true,
		},
		{
			// vcs.revision present → module version is not consulted.
			name: "vcs-wins-over-module",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v2.8.0-dev.5"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "deadbeefcafe0123"},
				},
			},
			wantV: "v2.8.0-dev", wantC: "deadbeefcafe0123", wantD: "unknown",
		},
		{
			// Nothing usable anywhere: defaults pass through.
			name:  "empty-buildinfo",
			info:  &debug.BuildInfo{},
			wantV: "v2.8.0-dev", wantC: "none", wantD: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, c, d, dirty := resolveFromBuildInfo("v2.8.0-dev", "none", "unknown", tc.info)
			if v != tc.wantV || c != tc.wantC || d != tc.wantD || dirty != tc.wantDirty {
				t.Errorf("resolveFromBuildInfo = (%q, %q, %q, %v), want (%q, %q, %q, %v)",
					v, c, d, dirty, tc.wantV, tc.wantC, tc.wantD, tc.wantDirty)
			}
		})
	}
}

// TestParsePseudoVersion pins the suffix grammar: 14-digit UTC commit
// time + 12-hex short SHA, anchored at the end of the version.
func TestParsePseudoVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		mv       string
		wantSHA  string
		wantWhen string
		wantOK   bool
	}{
		{
			name:    "pre-release-base",
			mv:      "v2.8.0-dev.5.0.20260802143512-abcdef123456",
			wantSHA: "abcdef123456", wantWhen: "2026-08-02T14:35:12Z", wantOK: true,
		},
		{
			name:    "release-base",
			mv:      "v2.7.1-0.20260715090102-0123456789ab",
			wantSHA: "0123456789ab", wantWhen: "2026-07-15T09:01:02Z", wantOK: true,
		},
		{
			// Form (1) in x/mod's grammar: no tagged base version for
			// this major, so no "0." segment before the timestamp.
			name:    "no-base-tag",
			mv:      "v3.0.0-20260802143512-abcdef123456",
			wantSHA: "abcdef123456", wantWhen: "2026-08-02T14:35:12Z", wantOK: true,
		},
		{
			name:    "incompatible-suffix",
			mv:      "v2.7.1-0.20260715090102-0123456789ab+incompatible",
			wantSHA: "0123456789ab", wantWhen: "2026-07-15T09:01:02Z", wantOK: true,
		},
		{name: "exact-tag", mv: "v2.8.0-dev.5"},
		{name: "exact-release-tag", mv: "v2.7.0"},
		{name: "devel", mv: "(devel)"},
		{
			// Right shape, impossible calendar date — must not parse.
			name: "invalid-timestamp",
			mv:   "v2.8.0-0.20261399999999-abcdef123456",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sha, when, ok := parsePseudoVersion(tc.mv)
			if sha != tc.wantSHA || when != tc.wantWhen || ok != tc.wantOK {
				t.Errorf("parsePseudoVersion(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.mv, sha, when, ok, tc.wantSHA, tc.wantWhen, tc.wantOK)
			}
		})
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
