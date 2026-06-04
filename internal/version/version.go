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

// Package version centralizes build-identity reporting for both
// cmd/core-agent and cmd/core-agent-tui. The package vars are
// overridable at release time via -ldflags; plain `go build` falls
// back to the VCS metadata Go embeds when -buildvcs=true (the
// default since Go 1.18) so dev builds still report a real SHA.
//
// Release process (see docs/release-process.md):
//
//	go build -ldflags "\
//	  -X github.com/go-steer/core-agent/internal/version.Version=v2.2.0 \
//	  -X github.com/go-steer/core-agent/internal/version.Commit=$(git rev-parse HEAD) \
//	  -X github.com/go-steer/core-agent/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
//	" ./cmd/core-agent ./cmd/core-agent-tui
//
// After cutting a tag, bump Version below to the next minor + "-dev"
// (e.g. v2.1.0 release → main becomes v2.2.0-dev) so post-release
// dev builds report their next-target version.
package version

import (
	"fmt"
	"runtime/debug"
)

// Build-time metadata. Defaults assume an in-development build off
// main; release-time -ldflags injection overrides them with the real
// tag, commit, and build date.
var (
	// Version is the semver tag for released builds, or vX.Y.Z-dev
	// for in-development builds. Bump this manually on main right
	// after cutting a release so post-release builds report the
	// next target version.
	Version = "v2.3.0"

	// Commit is the git SHA the binary was built from. Defaults to
	// "none" so the debug.BuildInfo fallback can detect that nothing
	// was injected; release builds get the full SHA via -ldflags.
	Commit = "none"

	// Date is the build timestamp in ISO 8601. Same default-sentinel
	// pattern as Commit.
	Date = "unknown"
)

// String renders the build identity for a --version flag. prog is
// the binary name (e.g. "core-agent", "core-agent-tui") so the
// format starts with what the operator typed.
//
// Format:
//
//	<prog> <semver> (commit <8-char-sha>[, modified], built <date>)
//
// The leading two tokens are always (prog, version) so scripts can
// grep without parsing the parenthesized suffix.
func String(prog string) string {
	v, c, d, dirty := resolveBuildInfo(Version, Commit, Date)
	return formatVersion(prog, v, c, d, dirty)
}

// resolveBuildInfo returns the version/commit/date/dirty tuple to
// report. ldflags-injected values are authoritative when present;
// when the defaults are still in place we fall back to the VCS
// metadata Go embeds via -buildvcs=true so a plain `go build` at
// least surfaces the SHA + commit time + dirty marker.
func resolveBuildInfo(ldVersion, ldCommit, ldDate string) (v, c, d string, dirty bool) {
	v, c, d = ldVersion, ldCommit, ldDate
	// Only consult ReadBuildInfo when nothing was injected — the
	// release-time ldflags win when set.
	if c != "none" {
		return v, c, d, false
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return v, c, d, false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if s.Value != "" {
				c = s.Value
			}
		case "vcs.time":
			if s.Value != "" {
				d = s.Value
			}
		case "vcs.modified":
			if s.Value == "true" {
				dirty = true
			}
		}
	}
	return v, c, d, dirty
}

// formatVersion is the deterministic string-building half, split out
// so tests can exercise format choices without juggling build-info
// state.
func formatVersion(prog, v, c, d string, dirty bool) string {
	short := c
	if len(short) > 8 {
		short = short[:8]
	}
	suffix := ""
	if dirty {
		suffix = ", modified"
	}
	return fmt.Sprintf("%s %s (commit %s%s, built %s)", prog, v, short, suffix, d)
}
