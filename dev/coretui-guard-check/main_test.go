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

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot is two levels up from dev/coretui-guard-check, which is
// where `go test` runs.
const repoRoot = "../.."

// fakeUpstream is a synthetic core-tui roster. The unit tests below
// drive the comparison with this rather than the real module so that
// each failure mode can be provoked deliberately — the whole point of
// a gate is that someone has watched it fire.
func fakeUpstream(names ...string) upstream {
	up := upstream{Version: "v9.9.9-test", Interfaces: map[string]string{}}
	for _, n := range names {
		up.Interfaces[n] = "capabilities.go:1"
	}
	return up
}

// writeGuards writes a synthetic guard file into a temp root and parses
// it through the same code path the real one takes. It also points the
// package-level adapter table at it, so scanStray and the remedy text
// agree with the fixture.
func writeGuards(t *testing.T, body string) (string, *guardFile) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "host"), 0o750); err != nil {
		t.Fatal(err)
	}
	src := "package host\n\nimport coretui \"" + coretuiPkg + "\"\n\n" + body
	if err := os.WriteFile(filepath.Join(root, "host", "guards.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	a := adapter{Label: "test host", Guards: "host/guards.go", Receiver: "*Fake"}
	saved := adapters
	adapters = []adapter{a}
	t.Cleanup(func() { adapters = saved })

	gf, err := parseGuardFile(root, a)
	if err != nil {
		t.Fatalf("parseGuardFile: %v", err)
	}
	return root, gf
}

// wantFindings asserts how many findings fired and that between them
// they say the things the author of the failing change needs to read.
func wantFindings(t *testing.T, got []string, n int, substrings ...string) {
	t.Helper()
	all := strings.Join(got, "\n---\n")
	if len(got) != n {
		t.Fatalf("want exactly %d finding(s), got %d:\n%s", n, len(got), all)
	}
	for _, s := range substrings {
		if !strings.Contains(all, s) {
			t.Errorf("findings do not mention %q:\n%s", s, all)
		}
	}
}

// --- the failure modes this gate claims to catch ----------------------

// A core-tui bump adds an interface. It lands in neither list, nothing
// implements it, and before #812 nothing said so. This is the case the
// guards themselves cannot catch.
func TestNewUpstreamInterfaceIsUnaccountedFor(t *testing.T) {
	_, gf := writeGuards(t, `var _ coretui.Agent = (*Fake)(nil)`)
	got := check(fakeUpstream("Agent", "Teleporter"), []*guardFile{gf}, nil)
	wantFindings(t, got, 1,
		"coretui.Teleporter is neither guarded nor declined",
		"//coretui:declined Teleporter",
		"_ coretui.Teleporter = (*Fake)(nil)")
}

// A guard line deleted in a refactor. Nothing else asserts the list's
// membership, so this is indistinguishable from the case above — which
// is exactly why one check covers both.
func TestDeletedGuardIsUnaccountedFor(t *testing.T) {
	_, gf := writeGuards(t, "var (\n\t_ coretui.Agent = (*Fake)(nil)\n)")
	got := check(fakeUpstream("Agent", "Reloader"), []*guardFile{gf}, nil)
	wantFindings(t, got, 1, "coretui.Reloader is neither guarded nor declined")
}

// A decline for an interface upstream no longer exports. The rationale
// still reads as a live decision about a live capability.
func TestStaleDeclineForRemovedInterface(t *testing.T) {
	_, gf := writeGuards(t, "var _ coretui.Agent = (*Fake)(nil)\n\n"+
		"//coretui:declined Ghost\n//   - coretui.Ghost: went away upstream.\n")
	got := check(fakeUpstream("Agent"), []*guardFile{gf}, nil)
	wantFindings(t, got, 1, "//coretui:declined Ghost names an interface core-tui v9.9.9-test does not export")
}

// #811's shape: a capability becomes implementable, someone implements
// it and adds the guard, and the not-implemented note is left behind.
// The compiler settles the argument; the prose is the stale half.
func TestGuardedAndDeclinedContradict(t *testing.T) {
	_, gf := writeGuards(t, "var _ coretui.Agent = (*Fake)(nil)\n\n"+
		"//coretui:declined Agent\n//   - coretui.Agent: not implemented here.\n")
	got := check(fakeUpstream("Agent"), []*guardFile{gf}, nil)
	wantFindings(t, got, 1, "coretui.Agent is both guarded", "and declined")

	// The two cited positions must be the guard's and the directive's,
	// not the guard's twice — the whole value of the message is being
	// pointed at both halves of the contradiction.
	cited := regexp.MustCompile(`\(host/guards\.go:(\d+)\)`).FindAllStringSubmatch(got[0], -1)
	if len(cited) != 2 || cited[0][1] == cited[1][1] {
		t.Fatalf("want two distinct source positions, got %v in:\n%s", cited, got[0])
	}
}

// The directive without the reason. Permitting this would turn the
// decline list back into a machine-readable list nobody has to justify,
// which is the thing #810 wrote the prose for in the first place.
func TestDeclineWithoutProseIsRejected(t *testing.T) {
	_, gf := writeGuards(t, "var _ coretui.Agent = (*Fake)(nil)\n\n//coretui:declined Reloader\n")
	got := check(fakeUpstream("Agent", "Reloader"), []*guardFile{gf}, nil)
	// Two findings, and both are wanted: the malformed directive, plus
	// Reloader still being unaccounted for — a decline the gate cannot
	// read is not a decline.
	wantFindings(t, got, 2,
		"//coretui:declined Reloader is not followed by prose naming coretui.Reloader",
		"coretui.Reloader is neither guarded nor declined")
}

// The directive and the bullet drifting apart — a rename applied to one
// and not the other.
func TestDeclineProseMustNameTheSameInterface(t *testing.T) {
	_, gf := writeGuards(t, "var _ coretui.Agent = (*Fake)(nil)\n\n"+
		"//coretui:declined Reloader\n//   - coretui.SomethingElse: wrong bullet.\n")
	got := check(fakeUpstream("Agent", "Reloader"), []*guardFile{gf}, nil)
	wantFindings(t, got, 2, "is not followed by prose naming coretui.Reloader")
}

// Stacked directives sharing one bullet: three declines, one reason,
// still bound.
func TestStackedDeclinesShareOneBullet(t *testing.T) {
	_, gf := writeGuards(t, "var _ coretui.Agent = (*Fake)(nil)\n\n"+
		"//coretui:declined Asker\n//coretui:declined Elicitor\n"+
		"//   - coretui.Asker / coretui.Elicitor: core-tui ships both concretely.\n")
	if got := check(fakeUpstream("Agent", "Asker", "Elicitor"), []*guardFile{gf}, nil); len(got) != 0 {
		t.Fatalf("want no findings, got:\n%s", strings.Join(got, "\n---\n"))
	}
}

// A guard added next to its method instead of in the guard file. It
// compiles and it catches signature drift, so nothing else objects —
// but this checker only reads the registered files, so leaving it there
// would make the interface report as unaccounted for with no clue why.
func TestStrayGuardOutsideTheRegisteredFiles(t *testing.T) {
	root, gf := writeGuards(t, `var _ coretui.Agent = (*Fake)(nil)`)
	other := filepath.Join(root, "host", "adapter.go")
	src := "package host\n\nimport coretui \"" + coretuiPkg + "\"\n\n" +
		"var _ coretui.Reloader = (*Fake)(nil)\n"
	if err := os.WriteFile(other, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	strays, err := scanStray(root)
	if err != nil {
		t.Fatal(err)
	}
	// The roster deliberately does not include Reloader, so the stray is
	// the only finding — this is about the location of the guard, not
	// about the interface being unaccounted for.
	got := check(fakeUpstream("Agent"), []*guardFile{gf}, strays)
	wantFindings(t, got, 1,
		"host/adapter.go:5: guard for coretui.Reloader lives outside the registered guard files",
		"register it in the `adapters`")
}

// A decline directive parked in some other file is invisible to the
// gate, so it is an error there too.
func TestStrayDeclineDirective(t *testing.T) {
	root, _ := writeGuards(t, `var _ coretui.Agent = (*Fake)(nil)`)
	other := filepath.Join(root, "host", "notes.go")
	src := "package host\n\n//coretui:declined Reloader\n//   - coretui.Reloader: nope.\nvar x = 1\n"
	if err := os.WriteFile(other, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	strays, err := scanStray(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(strays) != 1 || strays[0].what != "//coretui:declined Reloader" {
		t.Fatalf("want the stray directive, got %+v", strays)
	}
}

// --- build tags -------------------------------------------------------

// cmd/core-agent's guard file is behind `//go:build !no_tui`. The
// checker reads it as source rather than through the build system
// precisely so that the answer does not depend on the tags the caller
// happens to have set — under `-tags no_tui` a package-loading
// implementation would see zero guards and report the entire roster as
// unaccounted for.
func TestGuardsAreReadThroughTheBuildTag(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "host"), 0o750); err != nil {
		t.Fatal(err)
	}
	src := "//go:build !no_tui\n\npackage host\n\nimport coretui \"" + coretuiPkg + "\"\n\n" +
		"var _ coretui.Agent = (*Fake)(nil)\n"
	if err := os.WriteFile(filepath.Join(root, "host", "guards.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	a := adapter{Label: "tagged host", Guards: "host/guards.go", Receiver: "*Fake"}
	gf, err := parseGuardFile(root, a)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gf.Guarded["Agent"]; !ok {
		t.Fatalf("guard behind //go:build !no_tui was not seen: %+v", gf.Guarded)
	}
	if gf.Constraint != "!no_tui" {
		t.Fatalf("constraint = %q, want %q", gf.Constraint, "!no_tui")
	}
}

// Widen or drop the guard file's tag and the guards silently leave the
// build the adapter is still in.
func TestBuildConstraintMustMatchTheAdapter(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "host"), 0o750); err != nil {
		t.Fatal(err)
	}
	write := func(name, head string) {
		src := head + "package host\n\nimport coretui \"" + coretuiPkg + "\"\n\n" +
			"var _ coretui.Agent = (*Fake)(nil)\n"
		if err := os.WriteFile(filepath.Join(root, "host", name), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("guards.go", "")
	write("adapter.go", "//go:build !no_tui\n\n")

	a := adapter{Label: "tagged host", Guards: "host/guards.go", TagPeer: "host/adapter.go", Receiver: "*Fake"}
	gf, err := parseGuardFile(root, a)
	if err != nil {
		t.Fatal(err)
	}
	got, err := checkConstraints(root, []*guardFile{gf})
	if err != nil {
		t.Fatal(err)
	}
	wantFindings(t, got, 1, "build constraint does not match", "(no build constraint)", "//go:build !no_tui")
}

// --- upstream enumeration --------------------------------------------

// An exported alias to an interface in the same package is a capability
// like any other. Missing it would be a silent hole in the roster.
func TestExportedInterfaceAliasesAreCounted(t *testing.T) {
	dir := t.TempDir()
	src := "package tui\n\ntype Agent interface{ Run() }\n\ntype Alias = Agent\n\n" +
		"type notExported interface{ x() }\n\ntype Struct struct{}\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := exportedInterfaces(dir, []string{"a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["Agent"] == "" || got["Alias"] == "" {
		t.Fatalf("want Agent + Alias, got %v", got)
	}
}

// An exported alias to an UNEXPORTED interface is still an exported
// capability. Resolving only exported targets would leave a hole.
func TestExportedAliasToUnexportedInterfaceIsCounted(t *testing.T) {
	dir := t.TempDir()
	src := "package tui\n\ntype secret interface{ Run() }\n\ntype Public = secret\n"
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := exportedInterfaces(dir, []string{"a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["Public"] == "" {
		t.Fatalf("want Public only, got %v", got)
	}
}

// --- the prose binding follows the file's own import alias ------------

// The guard half resolves the import alias rather than assuming
// "coretui". The prose half has to agree, or a host that imported the
// package unaliased would have every one of its declines rejected.
func TestProseBindingUsesTheFilesImportAlias(t *testing.T) {
	parse := func(t *testing.T, body string) *guardFile {
		t.Helper()
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "host"), 0o750); err != nil {
			t.Fatal(err)
		}
		// Imported WITHOUT an alias, so the local name is `tui`.
		src := "package host\n\nimport \"" + coretuiPkg + "\"\n\n" + body
		if err := os.WriteFile(filepath.Join(root, "host", "guards.go"), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		a := adapter{Label: "unaliased host", Guards: "host/guards.go", Receiver: "*Fake"}
		gf, err := parseGuardFile(root, a)
		if err != nil {
			t.Fatalf("parseGuardFile: %v", err)
		}
		if gf.Alias != "tui" {
			t.Fatalf("alias = %q, want %q", gf.Alias, "tui")
		}
		return gf
	}

	t.Run("prose in the file's own alias binds", func(t *testing.T) {
		gf := parse(t, "var _ tui.Agent = (*Fake)(nil)\n\n"+
			"//coretui:declined Reloader\n//   - tui.Reloader: not here.\n")
		if got := check(fakeUpstream("Agent", "Reloader"), []*guardFile{gf}, nil); len(got) != 0 {
			t.Fatalf("want no findings, got:\n%s", strings.Join(got, "\n---\n"))
		}
	})

	t.Run("prose in some other package's spelling does not", func(t *testing.T) {
		gf := parse(t, "var _ tui.Agent = (*Fake)(nil)\n\n"+
			"//coretui:declined Reloader\n//   - coretui.Reloader: not here.\n")
		got := check(fakeUpstream("Agent", "Reloader"), []*guardFile{gf}, nil)
		wantFindings(t, got, 2, "is not followed by prose naming tui.Reloader")
	})
}

// A CRLF checkout leaves \r on the end of every comment. The directive
// regexp is anchored, so without trimming it none of them would match
// and every decline would read as missing.
func TestDirectiveSurvivesCarriageReturns(t *testing.T) {
	_, gf := writeGuards(t, "var _ coretui.Agent = (*Fake)(nil)\r\n\r\n"+
		"//coretui:declined Reloader\r\n//   - coretui.Reloader: not here.\r\n")
	if got := check(fakeUpstream("Agent", "Reloader"), []*guardFile{gf}, nil); len(got) != 0 {
		t.Fatalf("want no findings, got:\n%s", strings.Join(got, "\n---\n"))
	}
}

// --- the real tree ----------------------------------------------------

// The gate, against the repo as it stands. This duplicates what
// dev/ci/presubmits/verify-coretui-guards runs, deliberately: a bump
// that leaves a capability unaccounted for should fail `go test` too,
// not only the dedicated job.
func TestRepositoryIsExhaustiveAgainstThePinnedCoreTUI(t *testing.T) {
	up, err := resolveUpstream(repoRoot)
	if err != nil {
		t.Fatalf("resolving the pinned core-tui: %v", err)
	}
	var files []*guardFile
	for _, a := range adapters {
		gf, err := parseGuardFile(repoRoot, a)
		if err != nil {
			t.Fatalf("%s: %v", a.Guards, err)
		}
		files = append(files, gf)
	}
	strays, err := scanStray(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	tags, err := checkConstraints(repoRoot, files)
	if err != nil {
		t.Fatal(err)
	}
	if got := append(check(up, files, strays), tags...); len(got) != 0 {
		t.Fatalf("core-tui %s: %d finding(s)\n\n%s", up.Version, len(got), strings.Join(got, "\n\n"))
	}
}

// The version under test is the one go.mod pins, and it is discovered
// rather than declared — a gate with a hardcoded version is the bug it
// exists to prevent. Read go.mod independently and compare.
func TestResolvedVersionComesFromGoMod(t *testing.T) {
	up, err := resolveUpstream(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	mod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	want := "github.com/go-steer/core-tui " + up.Version
	if !strings.Contains(string(mod), want) {
		t.Fatalf("resolved %q but go.mod has no %q line", up.Version, want)
	}
}

// `-tags no_tui` must not change the answer. It is a plausible way for
// someone to run this (the slim image build uses it) and it is exactly
// the configuration in which cmd/core-agent's guard file drops out of
// the build.
func TestNoTUITagDoesNotChangeTheAnswer(t *testing.T) {
	t.Setenv("GOFLAGS", "-tags=no_tui")
	up, err := resolveUpstream(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(up.Interfaces) == 0 {
		t.Fatal("no interfaces resolved under -tags no_tui")
	}
	gf, err := parseGuardFile(repoRoot, adapters[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := check(up, []*guardFile{gf}, nil); len(got) != 0 {
		t.Fatalf("findings under -tags no_tui:\n%s", strings.Join(got, "\n\n"))
	}
}
