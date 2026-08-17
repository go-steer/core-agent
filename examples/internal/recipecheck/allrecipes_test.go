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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/examples/internal/recipecheck"
)

// examplesDir is the examples/ tree, relative to this package.
const examplesDir = "../.."

// changelogPath is the repo's CHANGELOG.md — the offline oracle for
// which versions have actually been released. See ReleasedVersions.
const changelogPath = examplesDir + "/../CHANGELOG.md"

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
	// #644 bug in a second recipe, and it was a design call rather than a
	// mechanical fix. #674 decided it: **accept the gap and disclose it**,
	// over the two alternatives it weighed.
	//
	//   - Enabling `bash` fixes nothing — the brain image is distroless,
	//     so a shell finds no kubectl/gcloud to run, and it would give up
	//     propose-only-by-construction (#617/#621) to convert a fact CI
	//     can see offline into a runtime "command not found".
	//   - A translation overlay (map each kubectl/gcloud step onto a
	//     read-only `gke` MCP call) covers a minority of the steps: 40 of
	//     the 99 CLI findings are mutations, executions or interactive
	//     channels, which a read-only endpoint cannot serve BY
	//     CONSTRUCTION — no claim about its tool list required. Which of
	//     the remaining reads it serves is not decidable offline, since an
	//     MCP tool list needs a live dial. So the best case is a table
	//     covering well under half the steps, unverifiable in CI, over
	//     content the recipe has stopped maintaining (#704).
	//
	// Two arguments deliberately NOT made, because this tree contradicts
	// them: that an overlay is structurally too weak to override a skill
	// (the CI-enforced #703 fix is itself an overlay-precedence claim —
	// recipe_test.go requires "this overlay wins" in cluster/AGENTS.md);
	// and that an overlay is worthless because it removes no findings here
	// (that judges runtime behavior by a static gate — #622's own #703 fix
	// removed none either).
	//
	// The disclosure lives in the recipe README under "What does not
	// execute", which is the resolution's deliverable — not this comment.
	//
	// Waived, not ignored: Check still produces every finding, the test
	// logs how many were waived, and WaiveMinFindings puts a floor under
	// each tree so a waiver cannot quietly become a blindfold. #766 is why:
	// when the six cluster skills moved under a subagent root they fell out
	// of the walk entirely and NO glob claimed them — the stale entry was
	// `skills/`, pointing at the emptied .agents/skills/, and only this
	// WaiveReason's prose still described both trees. The floors are
	// floors, not ratchets: they sit under the 120 and 68 findings the
	// trees produce today, so a vendoring bump can move the count without
	// touching this file, but a tree going dark cannot. The counts
	// themselves are pinned next to the prose that quotes them, by
	// kube-platform-agent's TestPublishedFindingCountsMatchTheDocs.
	"kube-platform-agent/.agents": {
		WaiveFileGlobs: []string{"../upstream/skills/", "../cluster/skills/"},
		WaiveMinFindings: map[string]int{
			"../upstream/skills/": 90,
			"../cluster/skills/":  50,
		},
		WaiveReason: "../upstream/skills/ (18 skills, 120 findings, loaded by the parent via content_roots) " +
			"and ../cluster/skills/ (6 skills, 68 findings, loaded by the `cluster` subagent from its own " +
			"root) are byte-for-byte snapshots of gke-labs/kube-agents content; editing them would defeat " +
			"the recipe's no-drift guarantee. #674 resolved this as accept-and-disclose: enabling `bash` " +
			"buys nothing (distroless image, no kubectl/gcloud for a shell to find), and an AGENTS.d " +
			"translation overlay reaches a minority of the steps — 40 of the 99 CLI findings are mutations " +
			"or executions a read-only endpoint cannot serve by construction, and whether it serves the " +
			"remaining reads is not decidable without a live dial. The gap is stated in the recipe README " +
			"under \"What does not execute\".",
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

// TestOverlayPinsSatisfyRecipeConfig is the deploy-time counterpart
// (#680). TestEverySkillNamedToolIsReachable asks whether the content
// is executable against the config; this asks whether the config is
// executable against the image the manifests actually ship.
//
// Both recipes were wrong when this landed, in two different ways a
// single-rule check would have missed: gke-troubleshoot-agent pinned
// 2.8.0 under a config rebuilt on v2.9's `alerts` and
// `tools.wait_and_verify` (#644 one layer down — pkg/config ignores
// unknown keys, so that daemon boots clean and registers neither tool),
// and kube-platform-agent pinned "2.9.0", a version this repo has never
// cut, so the Pod could not even pull.
//
// Discovery-driven for the same reason as the test above: a recipe that
// gains manifests, or moves them, is covered without anyone editing
// this file.
func TestOverlayPinsSatisfyRecipeConfig(t *testing.T) {
	released, err := recipecheck.ReleasedVersions(changelogPath)
	if err != nil {
		t.Fatalf("ReleasedVersions(%s): %v", changelogPath, err)
	}
	recipes, err := recipecheck.Discover(examplesDir)
	if err != nil {
		t.Fatalf("Discover(%s): %v", examplesDir, err)
	}
	for _, r := range recipes {
		t.Run(r.Name, func(t *testing.T) {
			findings, err := recipecheck.CheckDeployPins(examplesDir, r, released)
			if err != nil {
				t.Fatalf("CheckDeployPins: %v", err)
			}
			for _, f := range findings {
				t.Errorf("%s", f)
			}
		})
	}
}

// TestOverlayPinsSurviveTheGAFold is the regression for the way this
// check would otherwise have broken itself at the next GA.
//
// dev/release/cut-ga-tag.sh folds every pre-release section since the
// last GA into the new GA entry and deletes those sections. So the
// moment v2.9.0 is cut, `## [2.9.0-dev.1]` stops existing — and every
// recipe pinned to 2.9.0-dev.1 (all four overlays today) would start
// failing TestOverlayPinsSatisfyRecipeConfig with "not a version this
// repo has released". On the release commit. In the required `test`
// job. On main. That is not a hypothetical shape: the current CHANGELOG
// has zero dev sections for 2.8.0, 2.7.0 or any earlier GA, because they
// were all folded exactly this way.
//
// Rather than assert against a hand-written fixture that could drift
// from what the script does, this runs the script's own fold — the
// python3 heredoc, lifted out and fed the real CHANGELOG — and then runs
// the whole gate against the result.
func TestOverlayPinsSurviveTheGAFold(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		// cut-ga-tag.sh needs python3 too, so a machine without it cannot
		// cut a release anyway. TestFoldTrailerMatchesReleaseScript still
		// runs and still catches the trailer wording drifting.
		t.Skipf("python3 not available: %v", err)
	}
	fold := foldScript(t)

	folded := filepath.Join(t.TempDir(), "CHANGELOG.md")
	original, err := os.ReadFile(changelogPath)
	if err != nil {
		t.Fatalf("read %s: %v", changelogPath, err)
	}
	if err := os.WriteFile(folded, original, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(python, "-", folded, "v2.9.0", "2026-09-01")
	cmd.Stdin = strings.NewReader(fold)
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		t.Fatalf("running cut-ga-tag.sh's fold: %v\n%s", runErr, out)
	}

	body, err := os.ReadFile(folded)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "## [2.9.0-dev.1]") {
		t.Fatal("the fold left the pre-release section in place; this test is no longer " +
			"exercising the case it was written for")
	}

	released, err := recipecheck.ReleasedVersions(folded)
	if err != nil {
		t.Fatalf("ReleasedVersions on a folded changelog: %v", err)
	}
	recipes, err := recipecheck.Discover(examplesDir)
	if err != nil {
		t.Fatalf("Discover(%s): %v", examplesDir, err)
	}
	for _, r := range recipes {
		findings, err := recipecheck.CheckDeployPins(examplesDir, r, released)
		if err != nil {
			t.Fatalf("%s: CheckDeployPins: %v", r.Name, err)
		}
		for _, f := range findings {
			t.Errorf("after the v2.9.0 GA fold, %s", f)
		}
	}
}

// foldScript lifts the python3 heredoc out of cut-ga-tag.sh. The
// surrounding bash is preflight — a pricing-drift check and git tag
// sequencing — that has nothing to do with the changelog and everything
// to do with the network and the git history.
func foldScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(examplesDir, "..", "dev", "release", "cut-ga-tag.sh")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	const open = "python3 - \"$CHANGELOG\" \"$TAG\" \"$TODAY\" <<'PY'\n"
	_, rest, ok := strings.Cut(string(body), open)
	if !ok {
		t.Fatalf("%s no longer invokes its fold as %q, so this test cannot run it. "+
			"Re-point it at however the fold is invoked now — the property under test "+
			"(a folded changelog must still satisfy the pin gate) has not changed.", path, open)
	}
	script, _, ok := strings.Cut(rest, "\nPY\n")
	if !ok {
		t.Fatalf("%s: unterminated python heredoc", path)
	}
	return script
}
