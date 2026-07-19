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
	"os"
	"sort"
)

// Resolver interpolates ${env:NAME} in strings using an env-var lookup,
// with awareness of which names are declared in the manifest.
//
// Construction is a two-phase process:
//
//  1. NewResolver(manifest, lookup) — records the manifest, resolves
//     each declared name against lookup, records errors for missing
//     required vars. This is when required-var validation fires.
//
//  2. Interpolation call sites (pkg/instruction, pkg/skills, pkg/mcp)
//     use Interpolate(s) as they load bundle files. Interpolate
//     records every unique NAME it sees to enable the "undeclared
//     reference" drift warning at ReportDrift time.
//
// A nil Resolver is safe — Interpolate is a no-op, Errors returns nil,
// IsSensitive returns false. Loaders should tolerate the nil case
// (bundle without a manifest) rather than requiring a stub.
type Resolver struct {
	manifest *Manifest
	values   map[string]string   // name → resolved value (post-default)
	sens     map[string]struct{} // names flagged sensitive: true
	errs     []error             // required-var-missing errors
	seenRefs map[string]struct{} // names encountered during interpolation
}

// NewResolver builds a Resolver from a parsed manifest and an env-var
// lookup function (usually os.LookupEnv). Passing nil manifest returns
// nil — matches the "no manifest, no interpolation" backwards-compat
// path expected by pkg/config.LoadOrDefault callers.
//
// lookup must return (value, true) if the var is set (even to empty
// string) and ("", false) if unset. os.LookupEnv has this shape
// directly; tests can pass a map-backed closure.
func NewResolver(manifest *Manifest, lookup func(name string) (string, bool)) *Resolver {
	if manifest == nil {
		return nil
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	r := &Resolver{
		manifest: manifest,
		values:   make(map[string]string, len(manifest.Env)),
		sens:     make(map[string]struct{}),
		seenRefs: make(map[string]struct{}),
	}
	for _, e := range manifest.Env {
		val, ok := lookup(e.Name)
		switch {
		case ok:
			r.values[e.Name] = val
		case e.Required:
			// Fail-loud: this is the whole point of the manifest.
			// Surfacing all missing-required errors (not just the first)
			// lets the operator fix them in one round-trip instead of
			// restart → fail → fix → restart → fail.
			r.errs = append(r.errs, fmt.Errorf("agentenv: required env var %q is not set (%s)", e.Name, describeUsage(e)))
			// Still register a value so subsequent interpolation
			// doesn't fall back to the ambient os.Getenv path and
			// silently substitute something unrelated.
			r.values[e.Name] = ""
		default:
			// Optional + unset → default (empty string if no default).
			r.values[e.Name] = e.Default
		}
		if e.Sensitive {
			r.sens[e.Name] = struct{}{}
		}
	}
	return r
}

// Errors returns fatal validation problems from Resolver construction
// (currently: missing required env vars). Empty slice → boot may
// proceed; non-empty → boot should log each and exit.
func (r *Resolver) Errors() []error {
	if r == nil {
		return nil
	}
	out := make([]error, len(r.errs))
	copy(out, r.errs)
	return out
}

// Interpolate substitutes ${env:NAME} in s using the resolved manifest
// values. Undeclared NAMEs fall back to the ambient os.Getenv path so a
// bundle can still reference standard system env vars (HOME, PATH,
// etc.) without declaring them — those show up as "undeclared
// reference" warnings via ReportDrift but don't break interpolation.
//
// Every unique NAME seen is recorded so ReportDrift can compute the
// undeclared-reference set.
func (r *Resolver) Interpolate(s string) string {
	if r == nil {
		return s
	}
	return interpolate(s, func(name string) string {
		if r.seenRefs != nil {
			r.seenRefs[name] = struct{}{}
		}
		if v, ok := r.values[name]; ok {
			return v
		}
		return os.Getenv(name)
	})
}

// InterpolateFunc returns a bare closure suitable for passing to
// loaders that don't want to import agentenv directly (pkg/instruction,
// pkg/skills). Nil-safe: nil Resolver returns nil, which loaders
// interpret as "no interpolation."
func (r *Resolver) InterpolateFunc() func(string) string {
	if r == nil {
		return nil
	}
	return r.Interpolate
}

// IsSensitive reports whether the named var is marked sensitive in the
// manifest. Used by log-sanitization paths that already redact certain
// values (mcp.json headers, /stats surfaces) to also redact env-var
// values marked in the manifest. Nil-safe: nil Resolver → false.
func (r *Resolver) IsSensitive(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.sens[name]
	return ok
}

// SensitiveValues returns the set of resolved values that should be
// redacted in logs, sorted for stable output. Empty when no sensitive
// entries are declared or nothing has been resolved yet.
//
// Callers that need to grep-and-redact a downstream string (like a full
// log line) can walk this list; callers that already know which VAR
// they're logging should prefer IsSensitive.
func (r *Resolver) SensitiveValues() []string {
	if r == nil || len(r.sens) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.sens))
	for name := range r.sens {
		if v := r.values[name]; v != "" {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// ReportDrift returns non-fatal warnings the daemon should log at boot:
//
//   - "undeclared reference: BAR referenced in bundle but not in
//     manifest" — the recipe author probably meant to add it.
//   - "unreferenced declaration: FOO declared in manifest but not
//     referenced anywhere" — leftover from a refactor.
//
// Both are advisory (per the #322 issue: warn, not error). The daemon
// keeps running; the recipe author sees the warnings and cleans up on
// their next iteration.
//
// Callers must invoke ReportDrift AFTER all bundle files have flowed
// through Interpolate at least once; earlier invocation reports every
// declaration as unreferenced.
func (r *Resolver) ReportDrift() []string {
	if r == nil {
		return nil
	}
	var warnings []string

	declared := make(map[string]struct{}, len(r.manifest.Env))
	for _, e := range r.manifest.Env {
		declared[e.Name] = struct{}{}
	}

	// Undeclared references: seen during interpolation but not in the
	// manifest. Ambient system env vars (HOME, PATH, etc.) that the
	// bundle happens to reference count as undeclared — arguably the
	// right behavior, since the recipe author should be explicit about
	// what environmental context the bundle assumes.
	undeclared := make([]string, 0)
	for name := range r.seenRefs {
		if _, ok := declared[name]; !ok {
			undeclared = append(undeclared, name)
		}
	}
	sort.Strings(undeclared)
	for _, name := range undeclared {
		warnings = append(warnings, fmt.Sprintf("agentenv: ${env:%s} is referenced but not declared in the manifest", name))
	}

	// Unreferenced declarations: in the manifest but never seen. Common
	// during recipe evolution — a var got renamed but the old entry
	// stayed behind, or a bundle used to reference it and no longer
	// does.
	unref := make([]string, 0)
	for name := range declared {
		if _, ok := r.seenRefs[name]; !ok {
			unref = append(unref, name)
		}
	}
	sort.Strings(unref)
	for _, name := range unref {
		warnings = append(warnings, fmt.Sprintf("agentenv: manifest declares %q but nothing in the bundle references it", name))
	}

	return warnings
}

// describeUsage builds a short hint string for the "required var
// missing" error — describes what the var is for, so the operator sees
// context without hunting through the manifest file. Falls back to
// used_by hints if there's no description.
func describeUsage(e Entry) string {
	if e.Description != "" {
		return e.Description
	}
	if len(e.UsedBy) > 0 {
		return "used by: " + joinComma(e.UsedBy)
	}
	return "no description in manifest"
}

func joinComma(xs []string) string {
	out := ""
	for i, x := range xs {
		if i > 0 {
			out += ", "
		}
		out += x
	}
	return out
}
