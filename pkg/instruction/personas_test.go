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

package instruction

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- the library itself ---------------------------------------------

func TestEveryRegisteredPersonaLoads(t *testing.T) {
	t.Parallel()
	names := BuiltinNames()
	if len(names) == 0 {
		t.Fatal("the persona library is empty")
	}
	for _, name := range names {
		body, err := Builtin(name)
		if err != nil {
			t.Errorf("Builtin(%q): %v", name, err)
			continue
		}
		if strings.TrimSpace(body) == "" {
			t.Errorf("builtin persona %q is empty", name)
		}
		if BuiltinSummary(name) == "" {
			t.Errorf("builtin persona %q has no summary", name)
		}
	}
}

// TestEveryPersonaFileIsRegistered is the other direction: a .md file
// added under personas/ but not to builtinPersonas is embedded into
// every binary and reachable by nobody.
func TestEveryPersonaFileIsRegistered(t *testing.T) {
	t.Parallel()
	entries, err := personaFS.ReadDir("personas")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".md")
		if _, ok := builtinPersonas[name]; !ok {
			t.Errorf("personas/%s is embedded but not in builtinPersonas — no @include can reach it", e.Name())
		}
	}
}

// TestBuiltinsContainNoInterpolation pins the decision in loadBuiltin's
// doc comment. Builtins are NOT run through the ${env:VAR}
// interpolator, and an undeclared name resolves to the empty string
// rather than erroring — so a `${env:FOO}` that crept into shipped
// prose would either sit there literally (confusing) or, if someone
// later decided to interpolate builtins, silently blank a line. The
// cheap fix is for the text never to contain one.
func TestBuiltinsContainNoInterpolation(t *testing.T) {
	t.Parallel()
	for _, name := range BuiltinNames() {
		body, err := Builtin(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, "${") {
			t.Errorf("builtin persona %q contains an interpolation reference; builtins are shipped text and are never interpolated", name)
		}
	}
}

// TestBuiltin_UnknownNameNamesTheAlternatives: the only way to reach
// this error is a hand-typed @include, so the message has to be enough
// to fix the typo from.
func TestBuiltin_UnknownNameNamesTheAlternatives(t *testing.T) {
	t.Parallel()
	_, err := Builtin("site")
	if err == nil {
		t.Fatal("expected an error for an unknown persona")
	}
	for _, name := range BuiltinNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name the available persona %q", err, name)
		}
	}
}

// --- resolution through the loader ----------------------------------

func TestLoad_BuiltinInclude(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeFile(t, filepath.Join(project, ".agents"), "AGENTS.md",
		"# Cluster bot\n\n@include builtin:sre\n\nThe cluster is prod-us-east.\n")

	loaded, err := Load(project, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The persona's own text, and the text of the persona IT includes.
	for _, want := range []string{
		"Operating a system you did not build", // sre
		"Say only what's true",                 // core, pulled in by sre
		"The cluster is prod-us-east.",         // the recipe's own words
	} {
		if !strings.Contains(loaded.Instruction, want) {
			t.Errorf("assembled instruction is missing %q", want)
		}
	}
	// The @include line itself is consumed, not echoed.
	if strings.Contains(loaded.Instruction, "@include builtin:sre") {
		t.Error("the @include directive survived into the prompt")
	}

	var got []string
	for _, s := range loaded.Sources {
		if s.Scope == builtinScope {
			got = append(got, s.Path)
			if s.Bytes == 0 {
				t.Errorf("source %q recorded zero bytes", s.Path)
			}
		}
	}
	if strings.Join(got, ",") != "builtin:sre,builtin:core" {
		t.Errorf("builtin provenance = %v, want [builtin:sre builtin:core]", got)
	}
}

// TestLoad_BuiltinsDedupeAcrossPersonas is the property that makes
// composition safe. Both shipped conduct personas open with
// `@include builtin:core`, so a recipe that wants both — or a recipe
// that spells core out itself and then includes one — must not get the
// identity section twice. Duplicated instruction text is not merely
// wasteful: contradicting yourself by repetition is how a system
// prompt stops being believed.
func TestLoad_BuiltinsDedupeAcrossPersonas(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeFile(t, filepath.Join(project, ".agents"), "AGENTS.md",
		"@include builtin:core\n@include builtin:coder\n@include builtin:sre\n")

	loaded, err := Load(project, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const identityMarker = "## Who you are"
	if n := strings.Count(loaded.Instruction, identityMarker); n != 1 {
		t.Errorf("builtin:core appears %d times in the prompt, want 1", n)
	}
	var cores int
	for _, s := range loaded.Sources {
		if s.Path == "builtin:core" {
			cores++
		}
	}
	if cores != 1 {
		t.Errorf("builtin:core recorded %d times in Sources, want 1", cores)
	}
	// And the two conduct personas both made it in.
	for _, want := range []string{"Working in someone else's codebase", "Diagnosis before treatment"} {
		if !strings.Contains(loaded.Instruction, want) {
			t.Errorf("assembled instruction is missing %q", want)
		}
	}
}

// TestLoad_UnknownBuiltinIsFatal: a typo'd persona name must stop the
// load, for the same reason a missing @include file does. Silently
// dropping it ships an agent whose identity section is simply absent,
// which is the failure mode hardest to notice from the outside.
func TestLoad_UnknownBuiltinIsFatal(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeFile(t, filepath.Join(project, ".agents"), "AGENTS.md", "@include builtin:site\n")

	_, err := Load(project, t.TempDir())
	if err == nil {
		t.Fatal("a typo'd builtin name loaded successfully")
	}
	if !strings.Contains(err.Error(), "builtin:site") || !strings.Contains(err.Error(), "sre") {
		t.Errorf("error %q should name both the typo and the alternatives", err)
	}
}

// TestExpand_BuiltinInclude covers the declarative-subagent door. A
// subagent declared in YAML has inline `instructions` and no file of
// its own, and it is the surface most likely to want a one-line
// persona.
func TestExpand_BuiltinInclude(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	out, sources, err := Expand("@include builtin:coder\n\nOnly touch pkg/api.\n", root, root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Nothing is done until you have run it") {
		t.Error("the coder persona did not expand")
	}
	if !strings.Contains(out, "Only touch pkg/api.") {
		t.Error("the subagent's own instructions were lost")
	}
	if len(sources) != 2 || sources[0].Path != "builtin:coder" || sources[1].Path != "builtin:core" {
		t.Errorf("sources = %+v, want builtin:coder then builtin:core", sources)
	}
}

// TestBuiltinCannotIncludeAFile pins the containment rule. A builtin
// body lives in the binary and has no directory, so there is no
// defensible base for a relative path: resolving against whichever
// recipe happened to include it would make one shipped sentence mean
// different things in different deployments, and resolving against the
// process working directory would be worse. Refused, loudly.
//
// Driven through processIncludes directly because no shipped persona
// contains such a line — and TestBuiltinsIncludeOnlyBuiltins below is
// what keeps it that way.
func TestBuiltinCannotIncludeAFile(t *testing.T) {
	t.Parallel()
	var sources []Source
	_, err := processIncludes("@include ../../etc/motd\n", builtinScope, "", true, "",
		1, map[string]bool{}, &sources, nil)
	if err == nil {
		t.Fatal("a builtin resolved a relative file include")
	}
	if !strings.Contains(err.Error(), "builtin") {
		t.Errorf("error %q does not explain that the include came from a builtin", err)
	}
}

// TestLoadBuiltinIsTheOneThatSetsTheFlag closes the gap the test above
// leaves open.
//
// TestBuiltinCannotIncludeAFile proves the guard refuses when
// inBuiltin is true. It cannot prove that anything ever passes true —
// flip the argument in loadBuiltin to false and that test still passes,
// because it never calls loadBuiltin. Since no shipped persona contains
// a file include (by design, and asserted below), the only way to reach
// the refusal through the real entry point is to substitute the body,
// which is what builtinBody exists for.
//
// Not parallel: it swaps a package var.
func TestLoadBuiltinIsTheOneThatSetsTheFlag(t *testing.T) {
	orig := builtinBody
	t.Cleanup(func() { builtinBody = orig })
	builtinBody = func(string) (string, error) {
		return "# core\n\n@include ../shared/house-style.md\n", nil
	}

	var sources []Source
	_, err := loadBuiltin("core", 1, map[string]bool{}, &sources)
	if err == nil {
		t.Fatal("loadBuiltin resolved a relative file include from a builtin body — it is not passing inBuiltin=true")
	}
	if !strings.Contains(err.Error(), "builtin") {
		t.Errorf("error %q does not explain that the include came from a builtin", err)
	}
}

// TestBuiltinsIncludeOnlyBuiltins checks the shipped text against the
// rule above, so that adding `@include ../shared.md` to a persona file
// fails here rather than at some operator's first load.
func TestBuiltinsIncludeOnlyBuiltins(t *testing.T) {
	t.Parallel()
	for _, name := range BuiltinNames() {
		body, err := Builtin(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(body, "\n") {
			rel, ok := parseIncludeLine(line)
			if !ok {
				continue
			}
			if !strings.HasPrefix(rel, builtinPrefix) {
				t.Errorf("personas/%s.md:%d includes %q; a builtin may only include other builtins", name, i+1, rel)
			}
		}
	}
}

// TestBuiltinIncludeInAFenceStaysLiteral: the authoring docs show the
// directive in a code block, and those docs get read by agents.
func TestBuiltinIncludeInAFenceStaysLiteral(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	writeFile(t, filepath.Join(project, ".agents"), "AGENTS.md",
		"Write this:\n\n```\n@include builtin:sre\n```\n")

	loaded, err := Load(project, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loaded.Instruction, "@include builtin:sre") {
		t.Error("the fenced example was expanded instead of left alone")
	}
	if strings.Contains(loaded.Instruction, "Diagnosis before treatment") {
		t.Error("a fenced directive pulled in the persona")
	}
}

// TestPersonaLibraryIsDocumented: the library is only useful if a
// recipe author can find the names, and the names live in two places.
func TestPersonaLibraryIsDocumented(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "persona-library.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range BuiltinNames() {
		if !strings.Contains(string(doc), builtinPrefix+name) {
			t.Errorf("docs/persona-library.md never mentions %s%s", builtinPrefix, name)
		}
	}
}
