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
// `go install module@version` builds carry no VCS metadata, so those
// fall back to the module version Go records instead: exact tags
// report the tag, and branch installs report the pseudo-version,
// whose suffix encodes the commit time and short SHA.
//
// Release process (see docs/release-process.md):
//
//	go build -ldflags "\
//	  -X github.com/go-steer/core-agent/v2/internal/version.Version=v2.2.0 \
//	  -X github.com/go-steer/core-agent/v2/internal/version.Commit=$(git rev-parse HEAD) \
//	  -X github.com/go-steer/core-agent/v2/internal/version.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
//	" ./cmd/core-agent ./cmd/core-agent-tui
//
// After cutting a tag, bump Version below to the next minor + "-dev"
// (e.g. v2.1.0 release → main becomes v2.2.0-dev) so post-release
// dev builds report their next-target version.
package version

import (
	"fmt"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Build-time metadata. Defaults assume an in-development build off
// main; release-time -ldflags injection overrides them with the real
// tag, commit, and build date.
var (
	// Version is the semver tag for released builds, or vX.Y.Z-dev
	// for in-development builds. Bump this manually on main right
	// after cutting a release so post-release builds report the
	// next target version. Enforced by
	// dev/ci/presubmits/verify-version-fallback so drift caught in
	// v2.4→v2.5→v2.6 (skipped bumps produced stale --version output
	// on go-installed binaries during the v2.7.0-dev.N demo drive)
	// can't happen again silently.
	Version = "v2.8.0"

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
// Fields still holding their "none"/"unknown" sentinels are omitted
// rather than printed; when neither commit nor date is known the
// parenthesized suffix is dropped entirely (the module version alone
// identifies the build for `go install module@tag` binaries).
//
// The leading two tokens are always (prog, version) so scripts can
// grep without parsing the parenthesized suffix.
func String(prog string) string {
	v, c, d, dirty := resolveBuildInfo(Version, Commit, Date)
	return formatVersion(prog, v, c, d, dirty)
}

// Effective returns just the version token String() reports (module
// version for `go install module@version` builds, the ldflags or
// in-repo value otherwise). For callers that advertise a version
// string (e.g. the A2A agent card) and want the same identity
// --version prints, without the commit/date suffix.
//
// Cached: the value is immutable after link time, debug.ReadBuildInfo
// re-parses on every call (go1.26), and one caller serves the
// unauthenticated agent-card route per request.
func Effective() string {
	return effective()
}

var effective = sync.OnceValue(func() string {
	v, _, _, _ := resolveBuildInfo(Version, Commit, Date)
	return v
})

// resolveBuildInfo returns the version/commit/date/dirty tuple to
// report. ldflags-injected values are authoritative when present;
// when the defaults are still in place we fall back to whatever
// debug.BuildInfo carries for this build flavor.
func resolveBuildInfo(ldVersion, ldCommit, ldDate string) (v, c, d string, dirty bool) {
	// Only consult ReadBuildInfo when nothing was injected — the
	// release-time ldflags win when set. An empty Commit counts as
	// not-injected too, so a release pipeline whose $(git rev-parse)
	// expands empty can't silently produce "commit ".
	if ldCommit != "none" && ldCommit != "" {
		return ldVersion, ldCommit, ldDate, false
	}
	ldCommit = "none"
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ldVersion, ldCommit, ldDate, false
	}
	return resolveFromBuildInfo(ldVersion, ldCommit, ldDate, info)
}

// resolveFromBuildInfo is the deterministic half of resolveBuildInfo,
// split out so tests can hand it a constructed BuildInfo. Two
// fallback sources, in order:
//
//  1. vcs.* settings — present when built from a git checkout with
//     -buildvcs=true (plain `go build`/`go install ./...`). Surfaces
//     the SHA + commit time + dirty marker.
//  2. Main.Version — present for `go install module@version` builds,
//     which compile from the module cache and carry no vcs.*
//     settings. Exact tags report the tag alone; pseudo-versions
//     (branch installs) additionally yield the commit SHA and commit
//     time encoded in their suffix.
func resolveFromBuildInfo(ldVersion, ldCommit, ldDate string, info *debug.BuildInfo) (v, c, d string, dirty bool) {
	v, c, d = ldVersion, ldCommit, ldDate
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
	if c != "none" {
		return v, c, d, dirty
	}
	// No vcs.* settings → module-cache build. Note Go 1.24+ also
	// stamps Main.Version (e.g. "v2.8.0-dev.5+dirty") for checkout
	// builds, but only alongside vcs.* settings, so the early return
	// above intentionally keeps vcs metadata (and the ldflags -dev
	// version) authoritative for those.
	if mv := info.Main.Version; mv != "" && mv != "(devel)" {
		v = mv
		if sha, when, ok := parsePseudoVersion(mv); ok {
			c, d = sha, when
		}
	}
	return v, c, d, dirty
}

// pseudoVersionSuffix matches the "<UTC commit time>-<12-hex short
// SHA>" tail of Go pseudo-versions, mirroring the grammar in
// golang.org/x/mod/module (pseudo.go). Three prefixes carry it:
//
//	vX.0.0-<time>-<sha>            no base tag for this major
//	vX.Y.Z-pre.0.<time>-<sha>      base tag is a pre-release
//	vX.Y.(Z+1)-0.<time>-<sha>      base tag is a release
//
// plus an optional "+incompatible"/"+dirty"-style build-metadata
// tail. End-anchoring keeps base-version dots and hyphens from
// confusing it.
var pseudoVersionSuffix = regexp.MustCompile(
	`(?:^v[0-9]+\.0\.0-|[.-]0\.)([0-9]{14})-([0-9a-f]{12})(?:\+[0-9A-Za-z.-]+)?$`)

// parsePseudoVersion extracts the short commit SHA and RFC 3339
// commit time from a Go module pseudo-version. ok is false for
// exact tags and anything else that doesn't carry a well-formed
// pseudo-version timestamp+SHA tail.
func parsePseudoVersion(mv string) (sha, when string, ok bool) {
	m := pseudoVersionSuffix.FindStringSubmatch(mv)
	if m == nil {
		return "", "", false
	}
	t, err := time.Parse("20060102150405", m[1])
	if err != nil {
		return "", "", false
	}
	return m[2], t.UTC().Format(time.RFC3339), true
}

// formatVersion is the deterministic string-building half, split out
// so tests can exercise format choices without juggling build-info
// state. Fields still holding their sentinels ("none"/"unknown") are
// omitted; with neither known the output is just "<prog> <version>".
func formatVersion(prog, v, c, d string, dirty bool) string {
	var parts []string
	if c != "none" && c != "" {
		short := c
		if len(short) > 8 {
			short = short[:8]
		}
		commit := "commit " + short
		if dirty {
			commit += ", modified"
		}
		parts = append(parts, commit)
	}
	if d != "unknown" && d != "" {
		parts = append(parts, "built "+d)
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%s %s", prog, v)
	}
	return fmt.Sprintf("%s %s (%s)", prog, v, strings.Join(parts, ", "))
}
