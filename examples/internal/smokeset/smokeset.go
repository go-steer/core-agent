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

// Package smokeset records which of the example programs under
// examples/ can be run unattended, and — for every one that cannot —
// the reason it is excluded.
//
// The problem it exists for (#852): `go build ./...` proves the
// examples compile and nothing proved they still *do what they say*,
// which is the only reason an example exists. examples/parallel-spawn
// had been exiting 1 for some time — every one of its spawns refused
// with "ad-hoc subagents are disabled" after ad-hoc spawns became
// opt-in — while its README went on advertising "Exits 0". It was
// found by hand during unrelated work (#476), not by a check.
//
// The set has to be a list rather than a glob, because the examples
// split three ways: runnable unattended, deliberately not runnable
// (needs a provider key, binds a listener, wants an argument), and
// covered by its own tests. A list alone would rot the same way, so
// the companion test requires every examples/*/main.go to be
// accounted for here — a new example is either on the runnable list or
// excluded with a reason, and there is no third state in which it is
// silently uncovered.
//
// # What the gate does and does not prove
//
// It proves a program runs to completion and exits 0 with no
// credentials and no arguments. That is strictly weaker than "the
// example demonstrates what its README claims": only parallel-spawn
// and compose-multi-session assert on their own results today, and the
// other seven exit non-zero only when an operation returns an error.
// A stronger gate would need each example to check its own output,
// which is worth doing per example and is not what this package
// claims to have done.
//
// The other thing it cannot catch is a Skip whose reason has stopped
// being true — an example that grows a default prompt, or drops its
// credential requirement, stays excluded until somebody re-reads the
// row. Proving that needs running the thing, which is the argument
// this package exists to have; the reason is written down so the
// re-reading is possible at all.
package smokeset

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Disposition says what CI does with an example program.
type Disposition string

const (
	// Run means the smoke gate builds and runs it with no arguments
	// and no credentials, and requires exit 0.
	Run Disposition = "run"

	// Skip means the gate deliberately leaves it alone. Every Skip
	// entry carries a Reason, because an unexplained exclusion is
	// indistinguishable from an oversight — which is the failure this
	// package exists to prevent, one level up.
	Skip Disposition = "skip"
)

// DefaultTimeout bounds a single example's run. The whole runnable set
// finishes in about four seconds on a warm build cache; the bound is
// generous because what it is really guarding against is an example
// that starts blocking forever, and a gate that flakes on a slow
// runner is a gate people disable.
const DefaultTimeout = 90 * time.Second

// Entry is one example program's disposition.
type Entry struct {
	// Name is the directory under examples/.
	Name string

	// Disposition is Run or Skip.
	Disposition Disposition

	// Reason is required for Skip and must be empty for Run: a
	// runnable example needs no justification, and prose sitting on a
	// Run entry would read as a caveat nobody has to honour.
	Reason string

	// Timeout overrides DefaultTimeout for this example. Zero means
	// DefaultTimeout.
	Timeout time.Duration
}

// Entries is the recorded disposition of every example program in the
// tree, sorted by name. Adding an examples/<name>/main.go without
// adding a line here fails TestEveryExampleProgramIsAccountedFor.
var Entries = []Entry{
	{Name: "attach-daemon", Disposition: Skip,
		Reason: "binds an HTTP listener and serves until interrupted; it has no terminating path to exit 0 from"},
	{Name: "autonomous", Disposition: Run},
	{Name: "autonomous-handle", Disposition: Run},
	{Name: "autonomous-resume", Disposition: Run},
	{Name: "background-monitor", Disposition: Run},
	{Name: "basic", Disposition: Skip,
		Reason: "demonstrates the default provider path, so it needs a real provider key by design"},
	{Name: "bouncer-preflight", Disposition: Skip,
		Reason: "needs GOOGLE_API_KEY, and its logic is already covered by its own go tests in the same directory"},
	{Name: "compose-multi-session", Disposition: Run},
	{Name: "parallel-spawn", Disposition: Run},
	{Name: "replay", Disposition: Run},
	{Name: "scheduled-monitor", Disposition: Run},
	{Name: "streaming", Disposition: Skip,
		Reason: "takes the prompt as an argument and exits 2 with a usage message without one; it also needs a provider key"},
	{Name: "with-subagent", Disposition: Run},
	{Name: "with-tools", Disposition: Skip,
		Reason: "demonstrates the default provider path against real tools, so it needs a real provider key by design"},
}

// Runnable returns the Run entries, in Entries order.
func Runnable() []Entry {
	var out []Entry
	for _, e := range Entries {
		if e.Disposition == Run {
			out = append(out, e)
		}
	}
	return out
}

// Bound returns the timeout to apply to e.
func (e Entry) Bound() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return DefaultTimeout
}

// Discover lists the example programs in examplesDir: every immediate
// subdirectory holding a main.go.
//
// main.go is the rule rather than "any Go file" or "any directory"
// because it is what makes a directory a runnable program. Several
// directories under examples/ are recipes or docs — config, manifests
// and a README with no Go in them at all — and they are covered by
// examples/internal/recipecheck and by the recipe e2e workflows, not
// by this gate. A rule that swept them in would need a permanent row
// of "not a Go program" exclusions saying nothing.
func Discover(examplesDir string) ([]string, error) {
	ents, err := os.ReadDir(examplesDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", examplesDir, err)
	}
	var names []string
	for _, ent := range ents {
		if !ent.IsDir() || ent.Name() == "internal" {
			continue
		}
		if _, err := os.Stat(filepath.Join(examplesDir, ent.Name(), "main.go")); err != nil {
			continue
		}
		names = append(names, ent.Name())
	}
	sort.Strings(names)
	return names, nil
}
