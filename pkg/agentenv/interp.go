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

// Package agentenv parses the optional .agents/env.yaml (or .env.json)
// manifest and produces a Resolver that agent-bundle loaders can use to
// interpolate ${env:VAR} references in AGENTS.md, skill files, and
// mcp.json values.
//
// The mechanism has two halves:
//
//  1. A machine-readable manifest declaring which env vars the bundle
//     expects — required vs optional, defaults, sensitive markers, and
//     free-text descriptions. The daemon validates required vars at
//     boot; missing required vars are fail-loud errors, not silent
//     empty-string interpolation.
//
//  2. ${env:VAR} interpolation applied at instruction-file load time.
//     Syntax matches what pkg/mcp/config.go already accepts in mcp.json
//     header values, so operators only learn one substitution
//     convention across the whole .agents/ bundle.
//
// Bundles without a manifest keep working unchanged — no manifest, no
// interpolation, no validation. Zero regression path for existing
// deployments.
package agentenv

import (
	"os"
	"regexp"
	"sort"
)

// interpRe matches ${env:NAME} where NAME is a POSIX-shell-style
// identifier (letters, digits, underscore; leading char not a digit).
// Same shape mcp.json has accepted since day one — see the pre-agentenv
// implementation in pkg/mcp/config.go (kept as a delegating alias for
// backwards compat).
var interpRe = regexp.MustCompile(`\$\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// interpolate substitutes ${env:NAME} occurrences in s using lookup.
// Unknown / unset names resolve to the empty string (shell semantics),
// which matches operator expectations from the pre-agentenv era; the
// Resolver layer is where "unknown = warn" and "required = error"
// enforcement lives, not here.
//
// Exported callers should generally go through Resolver.Interpolate so
// they pick up sensitive-value tracking. This bare function exists for
// pkg/mcp's legacy free-function path (no manifest, direct os.Getenv).
func interpolate(s string, lookup func(string) string) string {
	if !interpRe.MatchString(s) {
		return s
	}
	return interpRe.ReplaceAllStringFunc(s, func(match string) string {
		sub := interpRe.FindStringSubmatch(match)
		if len(sub) < 2 {
			return ""
		}
		return lookup(sub[1])
	})
}

// InterpolateEnv is the legacy free-function used by pkg/mcp. It looks
// up each ${env:NAME} directly via os.Getenv with no manifest awareness
// — no required-var checks, no sensitive tracking, no drift warnings.
// Kept exported so pkg/mcp callers didn't churn when interpolation
// moved into this package; new call sites should go through a Resolver
// instead (see NewResolver).
func InterpolateEnv(s string) string {
	return interpolate(s, os.Getenv)
}

// InterpolateMap returns a copy of m with each value run through
// InterpolateEnv. Nil / empty maps pass through as nil (matches the
// pre-agentenv pkg/mcp semantics for absent headers / env blocks).
func InterpolateMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = InterpolateEnv(v)
	}
	return out
}

// FindReferences returns the unique set of NAME values referenced in s
// via the ${env:NAME} syntax. Order is deterministic (sorted) so callers
// can produce stable log lines and diff output.
//
// Used by the Resolver during construction to (a) compute the set of
// names referenced anywhere in the bundle for the "undeclared reference"
// drift warning, and (b) enable "unreferenced declaration" detection on
// the manifest side.
func FindReferences(s string) []string {
	matches := interpRe.FindAllStringSubmatch(s, -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		seen[m[1]] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
