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
	"strings"
	"sync"
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
	values   map[string]string   // name → resolved value (post-default); read-only after NewResolver
	sens     map[string]struct{} // names flagged sensitive: true; read-only after NewResolver
	errs     []error             // required-var-missing errors; read-only after NewResolver

	// mu guards seenRefs and cfgRefs, the only fields mutated after
	// construction. The same resolver's InterpolateFunc is shared across
	// sessions (pkg/compose captures it into SessionFactoryDeps.EnvInterp),
	// so concurrent POST /sessions -> ReproduceAgent -> Interpolate can
	// write seenRefs from multiple goroutines at once. Without this lock
	// that's a data race (guaranteed -race failure, panic under load).
	mu       sync.Mutex
	seenRefs map[string]struct{} // names encountered during interpolation
	cfgRefs  map[string]struct{} // names a *_env config field refers to
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
		cfgRefs:  make(map[string]struct{}),
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
		r.mu.Lock()
		if r.seenRefs != nil {
			r.seenRefs[name] = struct{}{}
		}
		r.mu.Unlock()
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

// NoteConfigRefs records env-var names the CONFIG refers to by name —
// the `*_env` fields (alerts.targets[].url_env, attach.token_env,
// auth.bearer_env, …) whose value is a var name rather than a secret.
//
// These never reach Interpolate: config is parsed by pkg/config and
// resolved late by whichever component owns the field, so it never flows
// through this resolver. Without this call ReportDrift sees only
// ${env:NAME} references in bundle text and reports a config-only var as
// unreferenced — which is how the kube-platform-native deployment came
// to warn that nothing referenced PLATFORM_AGENT_ALERT_WEBHOOK while the
// alert target was reading it by name.
//
// Recorded separately from seenRefs rather than folded in, so the
// undeclared-reference warning can name the right convention: telling an
// operator to add `${env:FOO}` to their manifest when FOO is a
// bearer_env would send them looking for a bundle reference that does
// not exist.
//
// Nil-safe and idempotent. Callers pass (*config.Config).EnvRefs().
func (r *Resolver) NoteConfigRefs(names []string) {
	if r == nil || len(names) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cfgRefs == nil {
		r.cfgRefs = make(map[string]struct{}, len(names))
	}
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			r.cfgRefs[n] = struct{}{}
		}
	}
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
// A name counts as referenced if EITHER convention reaches it: a
// ${env:NAME} in bundle text (recorded by Interpolate) or a `*_env`
// config field naming it (recorded by NoteConfigRefs). The undeclared
// warning names whichever convention actually made the reference, so the
// operator is pointed at the file they'd have to edit.
//
// Both are advisory (per the #322 issue: warn, not error). The daemon
// keeps running; the recipe author sees the warnings and cleans up on
// their next iteration.
//
// Callers must invoke ReportDrift AFTER all bundle files have flowed
// through Interpolate at least once AND after NoteConfigRefs; earlier
// invocation reports every declaration as unreferenced.
func (r *Resolver) ReportDrift() []string {
	if r == nil {
		return nil
	}
	var warnings []string

	declared := make(map[string]struct{}, len(r.manifest.Env))
	for _, e := range r.manifest.Env {
		declared[e.Name] = struct{}{}
	}

	// Snapshot both reference sets under the lock so a concurrent
	// Interpolate can't mutate a map while we range over it (see the
	// Resolver.mu note).
	seen, cfg := r.snapshotRefs()

	// Undeclared references: referenced but not in the manifest. Ambient
	// system env vars (HOME, PATH, etc.) that the bundle happens to
	// reference count as undeclared — arguably the right behavior, since
	// the recipe author should be explicit about what environmental
	// context the bundle assumes.
	//
	// Reported per convention. A name reached both ways is reported once,
	// as a bundle reference: that is the one the operator can see by
	// grepping the bundle, and the config field will be right next to it
	// in their head anyway.
	undeclared := make([]string, 0)
	undeclaredCfg := make([]string, 0)
	for name := range seen {
		if _, ok := declared[name]; !ok {
			undeclared = append(undeclared, name)
		}
	}
	for name := range cfg {
		if _, ok := declared[name]; ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		undeclaredCfg = append(undeclaredCfg, name)
	}
	sort.Strings(undeclared)
	sort.Strings(undeclaredCfg)
	for _, name := range undeclared {
		warnings = append(warnings, fmt.Sprintf("agentenv: ${env:%s} is referenced but not declared in the manifest", name))
	}
	for _, name := range undeclaredCfg {
		warnings = append(warnings, fmt.Sprintf("agentenv: config names env var %q (a *_env field) but it is not declared in the manifest", name))
	}

	// Unreferenced declarations: in the manifest but reached by neither
	// convention. Common during recipe evolution — a var got renamed but
	// the old entry stayed behind, or a bundle used to reference it and
	// no longer does.
	unref := make([]string, 0)
	for name := range declared {
		if _, ok := seen[name]; ok {
			continue
		}
		if _, ok := cfg[name]; ok {
			continue
		}
		unref = append(unref, name)
	}
	sort.Strings(unref)
	for _, name := range unref {
		warnings = append(warnings, fmt.Sprintf("agentenv: manifest declares %q but nothing in the bundle references it", name))
	}

	return warnings
}

// snapshotRefs returns copies of both reference sets taken under the
// lock, so callers can range over them without racing a concurrent
// Interpolate or NoteConfigRefs.
func (r *Resolver) snapshotRefs() (seen, cfg map[string]struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen = make(map[string]struct{}, len(r.seenRefs))
	for name := range r.seenRefs {
		seen[name] = struct{}{}
	}
	cfg = make(map[string]struct{}, len(r.cfgRefs))
	for name := range r.cfgRefs {
		cfg[name] = struct{}{}
	}
	return seen, cfg
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
