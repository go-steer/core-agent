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

// The first-party persona library (#656).
//
// WHY THIS IS IN THE BINARY AND NOT IN A TEMPLATE. Until now every
// recipe in this repo wrote its own persona from scratch, and the ones
// that were written by importing another project's system prompt
// behaved worst: docs/persona-library.md has the ledger. The failures
// were not exotic. Agents announced "all tasks complete, exiting
// session" because the imported prose described a task worker.
// Agents read their own process environment to work out what they
// were allowed to do. Agents answered a general question with an
// incident report because the persona only knew one output shape.
// None of that is domain knowledge — it is what a core-agent agent
// is, and it should not have to be rediscovered per deployment.
//
// THE SHAPE: identity -> equipment -> conduct. A persona has three
// layers and only two of them are portable.
//
//   - identity: what kind of process you are, and what honesty means
//     here. Portable. This is builtin:core.
//   - conduct: how work of a given kind is done — changing software,
//     operating someone else's system. Portable across deployments in
//     the same line of work. This is builtin:coder and builtin:sre.
//   - equipment: which tools this deployment registered, which cluster,
//     which project, which approval mode. NOT portable, and
//     deliberately not shipped here. It belongs in the recipe's own
//     AGENTS.md, next to the config that makes it true.
//
// So a recipe's AGENTS.md becomes short and specific: one @include
// line for the conduct, and then the part only that deployment knows.
// See docs/persona-library.md for the authoring guide.
//
// WHAT IS NOT HERE. Rules that restate a constraint the runtime
// already enforces (#865). Telling a model in prose that it may not
// call a tool it was never registered is not a safety property, it is
// prompt weight, and it teaches the model that the prose is where the
// rules live. Each rule below is either something no mechanism can
// enforce (say only what's true) or something the mechanism enforces
// bluntly and the model should understand rather than collide with
// (loop detection, approval gates).

package instruction

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

// personaFS holds the builtin persona bodies. Embedded rather than
// read from disk so `go install`-ing the binary is enough: a builtin
// that depended on a file next to the executable would be a builtin
// that half the deployments silently do not have.
//
//go:embed personas/*.md
var personaFS embed.FS

// builtinPrefix marks an @include target as naming a builtin persona
// rather than a path: `@include builtin:sre`.
//
// A prefix rather than a new directive because everything downstream of
// the directive already works — Load, LoadForSession and Expand all
// route through processIncludes, so a declarative subagent's inline
// `instructions` gets builtins for free, provenance lands in Sources
// next to the files, and the dedup that stops AGENTS.md and an
// @include double-loading the same file stops two personas
// double-loading builtin:core.
//
// "builtin:" cannot collide with a real relative path target: a path
// containing a colon before any separator is rejected by
// validateIncludePath as a scheme or a drive letter.
const builtinPrefix = "builtin:"

// builtinScope is the Source.Scope recorded for a builtin. Operators
// see it in /memory, where it is the answer to "where did this
// paragraph come from" for text that has no path on disk.
const builtinScope = "builtin"

// builtinPersonas maps each builtin name to its one-line summary. The
// summary is what an operator sees when they misspell a name, and it
// is the thing docs/persona-library.md must stay consistent with —
// TestPersonaLibraryIsDocumented asserts that.
//
// The map is also the registry: a file under personas/ that nobody
// added here is not reachable, and TestEveryPersonaFileIsRegistered
// fails rather than letting it rot unused.
var builtinPersonas = map[string]string{
	"core":  "identity and honesty for any core-agent agent; included by the others",
	"coder": "conduct for changing software in a codebase you did not write",
	"sre":   "conduct for operating a live system you did not build",
}

// BuiltinNames returns the available builtin persona names, sorted.
func BuiltinNames() []string {
	names := make([]string, 0, len(builtinPersonas))
	for n := range builtinPersonas {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// BuiltinSummary returns the one-line description of a builtin persona,
// or "" if the name is not one.
func BuiltinSummary(name string) string { return builtinPersonas[name] }

// Builtin returns the raw body of a builtin persona.
//
// The error names every available persona, because the only way to get
// here is a hand-written @include line and the operator who typo'd
// "builtin:sre" as "builtin:site" needs the list, not a bare
// not-found.
func Builtin(name string) (string, error) {
	if _, ok := builtinPersonas[name]; !ok {
		return "", fmt.Errorf("no builtin persona %q (available: %s)",
			name, strings.Join(BuiltinNames(), ", "))
	}
	b, err := personaFS.ReadFile("personas/" + name + ".md")
	if err != nil {
		// Registered in builtinPersonas but missing from the embed —
		// a build-time mistake, caught by TestEveryRegisteredPersonaLoads
		// before it can ship.
		return "", fmt.Errorf("builtin persona %q is registered but not embedded: %w", name, err)
	}
	return string(b), nil
}

// builtinBody is the seam loadBuiltin reads persona text through.
//
// It exists for one test and is worth the indirection because of which
// test. The shipped personas contain no file include — that is the
// property — so nothing in the tree can make loadBuiltin's refusal path
// execute, and a guard whose only coverage is a direct call to
// processIncludes proves the guard works without proving anybody sets
// it. Swapping this in a test is how "loadBuiltin passes inBuiltin=true"
// becomes a fact rather than a reading of the source.
var builtinBody = Builtin

// loadBuiltin resolves one `@include builtin:NAME`, mirroring loadFile
// for content that has no path.
//
// Three things are deliberately different from loadFile:
//
//   - No interpolation. ${env:VAR} against an undeclared name resolves
//     to the empty string (agentenv falls back to os.Getenv), so
//     running the interpolator over prose nobody can edit could blank
//     a line for a reason no operator could see. Builtins are shipped
//     text; they have no deployment-specific values in them, and
//     TestBuiltinsContainNoInterpolation keeps it that way.
//   - No size cap and no UTF-8 check. Both guard against what an
//     operator might point us at. These bytes are in the binary.
//   - No filesystem access at all, which is strictly stronger than the
//     scope-root containment a file include gets.
//
// The visited key is the "builtin:NAME" string, which cannot collide
// with a canonical path (always absolute).
func loadBuiltin(name string, depth int, visited map[string]bool, sources *[]Source) (string, error) {
	if depth > maxIncludeDepth {
		return "", fmt.Errorf("instruction: include depth exceeded (max %d) at builtin:%s", maxIncludeDepth, name)
	}
	key := builtinPrefix + name
	if visited[key] {
		// First-encounter-wins, same as a file. This is what makes
		// `@include builtin:coder` and `@include builtin:sre` in one
		// AGENTS.md emit builtin:core exactly once.
		return "", nil
	}
	visited[key] = true

	body, err := builtinBody(name)
	if err != nil {
		return "", fmt.Errorf("instruction: @include %s: %w", key, err)
	}
	*sources = append(*sources, Source{Scope: builtinScope, Path: key, Bytes: len(body)})

	// inBuiltin=true: a builtin has no directory, so a relative include
	// inside one has nothing to resolve against. Refused loudly rather
	// than resolved against the including file's directory, which would
	// make a shipped persona's meaning depend on who included it.
	return processIncludes(body, builtinScope, "", true, "", depth+1, visited, sources, nil)
}
