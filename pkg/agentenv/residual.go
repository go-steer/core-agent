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

package agentenv

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// residualMarker is the cheap substring gate used before the regex.
// Bodies without it — the overwhelming majority — skip scanning
// entirely.
const residualMarker = "${env:"

// residualRe matches a ${env:...} opener through its closing brace on
// the same line, whether or not the name inside is well-formed. It is
// deliberately looser than interpRe: interpRe defines what the
// interpolator will substitute, and this one defines what an operator
// reading their AGENTS.md would recognise as a placeholder. The gap
// between the two is a real failure mode of its own — `${env:my-var}`
// has a hyphen, never matches interpRe, and so survives interpolation
// untouched even when a manifest is active.
var residualRe = regexp.MustCompile(`\$\{env:[^}\n]*\}`)

// maxReportedPlaceholders bounds the boot line. A bundle with hundreds
// of surviving placeholders is already diagnosed by the first dozen,
// and a log line that wraps forty times is one an operator scrolls past.
const maxReportedPlaceholders = 12

// ResidualRefs records ${env:...} placeholders that are still literally
// present in loaded bundle content AFTER the interpolation pass ran —
// or, in the case this exists for, after it silently did not.
//
// The problem it solves (#712): interpolation is gated on an optional
// manifest. No .agents/env.yaml means LoadManifest returns nil, which
// means NewResolver returns a nil *Resolver, which means
// InterpolateFunc returns nil, which every loader documents as "no
// interpolation" and honours by passing bodies through untouched. Each
// link is correct in isolation and the composition is a no-op that
// announces nothing, so ${env:GOOGLE_CLOUD_PROJECT} reaches the model
// as literal persona text and reads to it like a value.
//
// Scope is the manifest-gated loaders — instruction files and skills,
// for the parent and for rooted subagents. mcp.json is NOT affected and
// is not tracked: pkg/mcp interpolates Env / Headers through
// InterpolateEnv, which reads the ambient process env with no manifest
// involved, so nothing there can survive for want of one.
//
// Detection is deliberately placed on the loaded *content* rather than
// on the manifest's absence. Content is the thing that actually goes
// wrong: a bundle with no manifest and no placeholders is fine and must
// stay quiet, and a bundle with a manifest can still ship a placeholder
// the interpolator will never match (see residualRe) — which no
// manifest-shaped check could catch, and which ReportDrift is
// structurally blind to, since it reports names Interpolate SAW and a
// non-matching placeholder is never seen.
//
// A ResidualRefs is safe for concurrent use and safe to use as the zero
// value. Nil-safe throughout, matching Resolver's conventions.
type ResidualRefs struct {
	mu       sync.Mutex
	found    map[string]struct{}
	resolver *Resolver // the one Track wrapped; nil = no manifest
}

// Track returns the interpolator to hand every content loader: r's
// substitution (none, when r is nil) followed by a scan of the result
// for placeholders that survived it.
//
// The returned function is non-nil even when r is nil, and that is the
// whole point rather than an implementation detail. A nil *Resolver
// yields a nil InterpolateFunc, loaders skip a nil interpolator
// entirely, and so the one path that most needs observing is the one
// that used to be unobservable. Cost is one substring check per file
// body plus, only for bodies that pass it, one regex scan. It does mean
// skill .md bodies now take a string round-trip through pkg/skills'
// sanitizing filesystem where a nil interpolator used to short-circuit;
// against the file read that precedes it, that is noise.
//
// Track also records r, so Warning can describe the right cause without
// the caller having to hand it back a second time and get it right.
// Call it once per resolver, before the loaders run.
//
// Nil receiver returns r.InterpolateFunc() unchanged, so a caller that
// wants no tracking need not special-case the wiring.
func (t *ResidualRefs) Track(r *Resolver) func(string) string {
	next := r.InterpolateFunc()
	if t == nil {
		return next
	}
	t.mu.Lock()
	t.resolver = r
	t.mu.Unlock()
	return func(s string) string {
		if next != nil {
			s = next(s)
		}
		t.note(s)
		return s
	}
}

// note records every placeholder still present in s.
func (t *ResidualRefs) note(s string) {
	if !strings.Contains(s, residualMarker) {
		return
	}
	matches := residualRe.FindAllString(s, -1)
	if len(matches) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.found == nil {
		t.found = make(map[string]struct{}, len(matches))
	}
	for _, m := range matches {
		t.found[m] = struct{}{}
	}
}

// Placeholders returns the unique surviving placeholders, verbatim and
// sorted, so callers can produce stable output. Empty when nothing
// survived — which is the healthy case, including for the many bundles
// that use no ${env:} references at all.
func (t *ResidualRefs) Placeholders() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.found) == 0 {
		return nil
	}
	out := make([]string, 0, len(t.found))
	for p := range t.found {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Warning formats the boot diagnostic for whatever survived, or "" when
// nothing did. The caller emits it through the same startup-line
// channel as the instruction/skill/subagent lines.
//
// Whether a manifest was active changes both the diagnosis and the
// remedy, so the two cases get different text rather than one line
// hedging between them; Track recorded which one applies. agentsDir
// names the directory the manifest would have been read from, and is ""
// when no .agents/ was discovered at all — a third thing worth saying
// out loud, since "add env.yaml" is not actionable advice when there is
// nowhere to put it.
//
// Warning-not-error is a deliberate choice: ${env:...} appears
// legitimately as literal text in content that *documents* the feature.
// Two of the three skill bundles this repo ships under SKILLS/ quote it
// inside example mcp.json blocks, and SKILLS/README.md tells operators
// to copy those bundles straight into .agents/skills/ — so refusing to
// boot on a surviving placeholder would turn a documentation string
// into an outage.
func (t *ResidualRefs) Warning(agentsDir string) string {
	found := t.Placeholders()
	if len(found) == 0 {
		return ""
	}
	t.mu.Lock()
	resolver := t.resolver
	t.mu.Unlock()
	shown := found
	suffix := ""
	if len(shown) > maxReportedPlaceholders {
		shown = shown[:maxReportedPlaceholders]
		suffix = fmt.Sprintf(" (+%d more)", len(found)-maxReportedPlaceholders)
	}

	var cause string
	switch {
	case resolver != nil:
		cause = "the manifest is active and interpolation ran, so these are not valid ${env:NAME} " +
			"references (letters, digits, underscore, no leading digit) and nothing substituted them"
	case agentsDir == "":
		cause = "no .agents/ directory was discovered, so there is no " + ManifestFileYAML +
			" to read and interpolation is OFF"
	default:
		cause = fmt.Sprintf("no %s or %s in %s, so interpolation is OFF",
			ManifestFileYAML, ManifestFileJSON, agentsDir)
	}

	return fmt.Sprintf(
		"agentenv: %d ${env:...} placeholder(s) survived loading and will reach the model as literal text: %s%s — %s",
		len(found), strings.Join(shown, ", "), suffix, cause)
}
