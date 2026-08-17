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

package recipecheck_test

import (
	"testing"

	"github.com/go-steer/core-agent/v2/examples/internal/recipecheck"
)

// examplesDir is the examples/ tree, relative to this package.
const examplesDir = "../.."

// policies overrides the default Policy for specific config roots, keyed
// by the Recipe.Name that Discover produces. A recipe absent from this
// map is checked with zero waivers — which is the state every recipe
// should be able to reach.
var policies = map[string]recipecheck.Policy{
	// kube-platform-agent loads two skill trees, both deliberately
	// unmodified snapshots of gke-labs/kube-agents content:
	//
	//   - ../upstream/skills/ — the 18 kube-agents skills the parent
	//     loads via content_roots.
	//   - ../cluster/skills/  — the six GKE domain-diagnostic skills the
	//     `cluster` subagent loads from its own content root
	//     (subagents[0].root, #621). #617 had vendored these into the
	//     parent's .agents/skills/; that directory no longer exists, and
	//     between #621 and #766 this tree was scanned by nothing at all.
	//
	// "No copied tree to drift" is the recipe's whole thesis, so
	// rewriting them into propose-only playbooks would defeat it.
	//
	// They fail the check for real: they are Hermes content, written to
	// be run with a shell, and this recipe disables `bash`. That is the
	// #644 bug in a second recipe, and it is a design call rather than a
	// mechanical fix — filed as #674 (accept / enable bash / ship a
	// translation overlay).
	//
	// Waived, not ignored: Check still produces every finding, the test
	// logs how many were waived, and WaiveMinFindings puts a floor under
	// each tree so a waiver cannot quietly become a blindfold. That is the
	// #766 failure mode itself — ../cluster/skills/ was covered by a glob
	// while nothing walked it — and a floor is what makes it loud. The
	// floors are floors, not ratchets: they sit well under the 120 and 68
	// findings the trees produce today, so a vendoring bump can move the
	// count without touching this file, but a tree going dark cannot.
	"kube-platform-agent/.agents": {
		WaiveFileGlobs: []string{"../upstream/skills/", "../cluster/skills/"},
		WaiveMinFindings: map[string]int{
			"../upstream/skills/": 90,
			"../cluster/skills/":  50,
		},
		WaiveReason: "../upstream/skills/ (18 skills, 120 findings, loaded by the parent via content_roots) " +
			"and ../cluster/skills/ (6 skills, 68 findings, loaded by the `cluster` subagent from its own " +
			"root) are byte-for-byte snapshots of gke-labs/kube-agents content; editing them would defeat " +
			"the recipe's no-drift guarantee. Resolution tracked in #674.",
	},
}

// TestEverySkillNamedToolIsReachable is the executability gate the
// gke-troubleshoot recipe needed and did not have (#645). It discovers
// every config root under examples/ and fails on any tool or CLI the
// skill content names that the recipe's own config cannot produce.
//
// Discovery rather than a hardcoded list is deliberate: a new recipe is
// covered the day it lands, and a recipe that moves its skills (as
// kube-platform-agent does in #621) does not silently stop being checked.
func TestEverySkillNamedToolIsReachable(t *testing.T) {
	recipes, err := recipecheck.Discover(examplesDir)
	if err != nil {
		t.Fatalf("Discover(%s): %v", examplesDir, err)
	}
	if len(recipes) == 0 {
		t.Fatalf("Discover(%s) found no config roots; the walk is broken, not the tree", examplesDir)
	}
	t.Logf("checking %d config roots", len(recipes))

	for _, r := range recipes {
		t.Run(r.Name, func(t *testing.T) {
			findings, err := recipecheck.Check(r, policies[r.Name])
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if n := len(findings) - len(recipecheck.Unwaived(findings)); n > 0 {
				t.Logf("%d finding(s) waived: %s", n, policies[r.Name].WaiveReason)
			}
			for _, f := range recipecheck.Unwaived(findings) {
				t.Errorf("%s", f)
			}
		})
	}
}
