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

package imagepin

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// testTracked is a stand-in for [Lookout] with the same matcher shapes,
// so these tests exercise the real rules without pinning themselves to
// the real repo layout.
func testTracked(roots ...string) *Tracked {
	const image = "example.test/widget"
	const semver = `v[0-9]+\.[0-9]+\.[0-9]+`
	return &Tracked{
		Family:       NewFamily(nil, image),
		UpstreamRepo: "example/widget",
		Roots:        roots,
		ImageRefRe:   regexp.MustCompile(regexp.QuoteMeta(image) + `:(` + semver + `)`),
		ProseRe:      regexp.MustCompile("pin(?:s|ned) `?(" + semver + ")`?"),
		TagRe:        regexp.MustCompile(`^` + semver + `$`),
		FrozenMarker: regexp.MustCompile(`(?m)^#[ \t]?pin-frozen:[ \t]*(\S.*?)[ \t]*$`),
	}
}

// writeTree materializes a path→contents map under a temp dir.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// siteKeys renders sites as "path:kind:tag[:frozen]" for comparison.
func siteKeys(sites []Site) []string {
	out := make([]string, 0, len(sites))
	for _, s := range sites {
		key := s.Path + ":" + string(s.Kind) + ":" + s.Tag
		if s.Frozen {
			key += ":frozen"
		}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func mustSites(t *testing.T, tr *Tracked, root string) []Site {
	t.Helper()
	sites, err := tr.Sites(root)
	if err != nil {
		t.Fatalf("Sites: %v", err)
	}
	return sites
}

// TestSites_FindsEveryDeclarationShape covers the five shapes this repo
// actually writes a tag in. Each one has been the site of a missed bump
// somewhere; the extensionless shell harness is the one that hides best.
func TestSites_FindsEveryDeclarationShape(t *testing.T) {
	tr := testTracked("recipes", "harness/run-e2e")
	tr.Docs = []string{"recipes/demo/README.md"}
	tr.Literals = []Literal{{
		Path: "recipes/demo/recipe_test.go",
		Re:   regexp.MustCompile(`wantTag\s*=\s*"(v[0-9]+\.[0-9]+\.[0-9]+)"`),
		Why:  "the in-tree oracle",
	}}
	root := writeTree(t, map[string]string{
		"recipes/demo/deploy/base/deployment.yaml": "spec:\n  containers:\n" +
			"    - image: example.test/widget:v1.0.0\n",
		"recipes/demo/deploy/overlays/example/kustomization.yaml": "images:\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
		"recipes/demo/README.md":      "The recipe pins `v1.0.0`.\n\nRun `crane digest example.test/widget:v1.0.0`.\n",
		"recipes/demo/recipe_test.go": "const wantTag = \"v1.0.0\"\n",
		"harness/run-e2e":             "#!/usr/bin/env bash\nIMAGE=\"${IMAGE:-example.test/widget:v1.0.0}\"\n",
	})

	got := siteKeys(mustSites(t, tr, root))
	want := []string{
		"harness/run-e2e:image reference:v1.0.0",
		"recipes/demo/README.md:image reference:v1.0.0",
		"recipes/demo/README.md:prose:v1.0.0",
		"recipes/demo/deploy/base/deployment.yaml:image reference:v1.0.0",
		"recipes/demo/deploy/overlays/example/kustomization.yaml:kustomize newTag:v1.0.0",
		"recipes/demo/recipe_test.go:declared literal:v1.0.0",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Sites() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestSites_UnrewritableTagsAreNotSites: a floating tag, a variable
// substitution and a digest are references, but there is nothing in
// them to bump. Reporting them would make every run drift forever.
func TestSites_UnrewritableTagsAreNotSites(t *testing.T) {
	tr := testTracked("recipes")
	root := writeTree(t, map[string]string{
		"recipes/demo/deploy.yaml": "image: example.test/widget:latest\n" +
			"image: example.test/widget:${WIDGET_TAG}\n" +
			"image: example.test/widget@sha256:abc123\n" +
			"image: example.test/widget:v1.0.0\n",
	})
	if got, want := siteKeys(mustSites(t, tr, root)),
		[]string{"recipes/demo/deploy.yaml:image reference:v1.0.0"}; !equal(got, want) {
		t.Errorf("Sites() = %v, want %v", got, want)
	}
}

// TestSites_HistoricalMentionsAreNotSites is the property that makes an
// automated rewrite safe to run on prose at all. A document that
// explains why a floor exists names old versions on purpose, and a
// rewrite that clobbered them would corrupt the rationale while looking
// like a clean bump.
func TestSites_HistoricalMentionsAreNotSites(t *testing.T) {
	tr := testTracked("recipes")
	tr.Docs = []string{"recipes/demo/README.md"}
	root := writeTree(t, map[string]string{
		"recipes/demo/README.md": "The recipe pins `v1.2.0`.\n" +
			"`v0.9.0` is where the handshake changed, so pinning back below `v0.9.0` breaks it.\n" +
			"v1.0.0 remains the capability floor.\n",
	})
	if got, want := siteKeys(mustSites(t, tr, root)),
		[]string{"recipes/demo/README.md:prose:v1.2.0"}; !equal(got, want) {
		t.Errorf("Sites() = %v, want %v", got, want)
	}
}

// TestSites_FrozenMarkerMustBeAttachedToTheLivePin is #680's finding F6
// in its second setting: a marker lifted out of a commented-out example
// is a marker nobody wrote about the live pin. If a file-scoped match
// were good enough, every kustomization that documents the freeze
// syntax would freeze itself.
func TestSites_FrozenMarkerMustBeAttachedToTheLivePin(t *testing.T) {
	tr := testTracked("recipes")
	tr.Frozen = []string{"recipes/attached"}
	root := writeTree(t, map[string]string{
		// The marker here sits in a documentation block, detached from
		// any node, and freezes nothing.
		"recipes/loose/deploy/kustomization.yaml": "# To opt out of automated bumps, put this on the entry:\n" +
			"#\n" +
			"#   # pin-frozen: #123 — worked example, not a real freeze\n" +
			"#\n" +
			"images:\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
		// Here it is a head comment on the entry itself.
		"recipes/attached/deploy/kustomization.yaml": "images:\n" +
			"  # pin-frozen: #456 — deliberately not tracking latest\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
	})

	got := siteKeys(mustSites(t, tr, root))
	want := []string{
		"recipes/attached/deploy/kustomization.yaml:kustomize newTag:v1.0.0:frozen",
		"recipes/loose/deploy/kustomization.yaml:kustomize newTag:v1.0.0",
	}
	if !equal(got, want) {
		t.Errorf("Sites() = %v, want %v", got, want)
	}
}

// TestSites_FrozenMarkerOnAnImageReference is the non-YAML half: a plain
// manifest or a script has no node tree, so adjacency of the comment
// block does the scoping. The reference sits mid-line, which is exactly
// where the naive "split at the offset and walk up" reads the live line
// as its own first neighbour and finds nothing.
func TestSites_FrozenMarkerOnAnImageReference(t *testing.T) {
	tr := testTracked("recipes")
	tr.Frozen = []string{"recipes/demo"}
	root := writeTree(t, map[string]string{
		"recipes/demo/deploy.yaml": "spec:\n" +
			"  # pin-frozen: #789 — case study, stays put\n" +
			"  image: example.test/widget:v1.0.0\n" +
			"\n" +
			"  # an unrelated comment, then a blank line, then a live pin\n" +
			"\n" +
			"  image: example.test/widget:v1.1.0\n",
	})
	got := siteKeys(mustSites(t, tr, root))
	// The freeze propagates across the recipe, so both are frozen — but
	// it must be the ATTACHED one that caused it. The loose-marker test
	// above is what proves the marker is not merely present.
	want := []string{
		"recipes/demo/deploy.yaml:image reference:v1.0.0:frozen",
		"recipes/demo/deploy.yaml:image reference:v1.1.0:frozen",
	}
	if !equal(got, want) {
		t.Errorf("Sites() = %v, want %v", got, want)
	}
}

// TestSites_FreezeIsPerRecipeNotPerFile: a recipe whose Deployment is
// frozen at an old release but whose README tracks latest is incoherent,
// and a README carries no comment syntax to hold a marker of its own.
func TestSites_FreezeIsPerRecipeNotPerFile(t *testing.T) {
	tr := testTracked("recipes")
	tr.Frozen = []string{"recipes/frozen"}
	tr.Docs = []string{"recipes/frozen/README.md", "recipes/live/README.md"}
	root := writeTree(t, map[string]string{
		"recipes/frozen/deploy/kustomization.yaml": "images:\n" +
			"  # pin-frozen: #704 — portability case study\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
		"recipes/frozen/README.md": "This recipe pins `v1.0.0`.\n",
		"recipes/live/deploy/kustomization.yaml": "images:\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
		"recipes/live/README.md": "This recipe pins `v1.0.0`.\n",
	})

	for _, s := range mustSites(t, tr, root) {
		wantFrozen := strings.HasPrefix(s.Path, "recipes/frozen/")
		if s.Frozen != wantFrozen {
			t.Errorf("%s: Frozen = %v, want %v", s.Path, s.Frozen, wantFrozen)
		}
		if wantFrozen && s.FrozenReason != "#704 — portability case study" {
			t.Errorf("%s: FrozenReason = %q", s.Path, s.FrozenReason)
		}
	}
}

// TestSites_ReasonlessMarkerDoesNotFreeze: an unexplained freeze is how
// a gate rots — nobody can tell later whether it is still wanted.
func TestSites_ReasonlessMarkerDoesNotFreeze(t *testing.T) {
	tr := testTracked("recipes")
	root := writeTree(t, map[string]string{
		"recipes/demo/deploy/kustomization.yaml": "images:\n" +
			"  # pin-frozen:\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
	})
	for _, s := range mustSites(t, tr, root) {
		if s.Frozen {
			t.Errorf("%s froze on a reasonless marker", s.Path)
		}
	}
}

// docBlockAboveALivePin is the shape that broke the marker-only design:
// somebody documents how freezing works, in a comment, immediately above
// the very pin the documentation is about. Nothing here is malicious and
// nothing here is unusual — it is the most natural place to put it.
const docBlockAboveALivePin = "images:\n" +
	"  # This recipe tracks upstream. To stop it, attach a marker to the\n" +
	"  # entry below, like this:\n" +
	"  #\n" +
	"  #   # pin-frozen: #123 — why this recipe should not track upstream\n" +
	"  #\n" +
	"  # The weekly job then leaves it alone.\n" +
	"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n"

// TestSites_AMarkerOutsideTheDeclaredSetIsAnError is the hole finding 1
// closed, in its own words: a marker adjacent to a live pin used to
// exempt it, silently, with the reason printed back as whatever the
// comment happened to say. Now the marker only ever EXPLAINS a freeze
// that Tracked.Frozen granted, so an accident is loud.
func TestSites_AMarkerOutsideTheDeclaredSetIsAnError(t *testing.T) {
	tr := testTracked("recipes") // nothing declared frozen
	root := writeTree(t, map[string]string{
		"recipes/demo/kustomization.yaml": "images:\n" +
			"  # pin-frozen: #123 — a real-looking reason, on a recipe nobody froze\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
	})
	_, err := tr.Sites(root)
	if err == nil {
		t.Fatal("Sites() accepted a marker on a recipe that is not declared frozen")
	}
	for _, want := range []string{"not declared frozen", "recipes/demo", "Tracked.Frozen"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestSites_ADeclaredFreezeWithoutAMarkerIsAnError is the other
// direction. Locality is the point of the marker: an operator reading
// the manifest must be able to see why the pin does not move without
// opening a Go file.
func TestSites_ADeclaredFreezeWithoutAMarkerIsAnError(t *testing.T) {
	tr := testTracked("recipes")
	tr.Frozen = []string{"recipes/demo"}
	root := writeTree(t, map[string]string{
		"recipes/demo/kustomization.yaml": "images:\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
	})
	_, err := tr.Sites(root)
	if err == nil {
		t.Fatal("Sites() accepted a declared freeze that nothing in the tree explains")
	}
	if !strings.Contains(err.Error(), "none carrying a marker") {
		t.Errorf("error = %v, want it to say the declarations carry no marker", err)
	}
}

// TestSites_ADeadFreezeDeclarationIsAnError: a recipe renamed out from
// under the declaration leaves a frozen entry that exempts nothing and
// hides nothing, which is the state a reader most easily mistakes for
// working.
func TestSites_ADeadFreezeDeclarationIsAnError(t *testing.T) {
	tr := testTracked("recipes")
	tr.Frozen = []string{"recipes/renamed-away"}
	root := writeTree(t, map[string]string{
		"recipes/demo/kustomization.yaml": "images:\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
	})
	if _, err := tr.Sites(root); err == nil {
		t.Fatal("Sites() accepted a frozen declaration matching nothing in the tree")
	} else if !strings.Contains(err.Error(), "no declaration of this image found under it") {
		t.Errorf("error = %v, want it to say the declaration is dead", err)
	}
}

// TestSites_FreezeDeclarationSpellingIsNormalised. Tracked.Frozen is
// hand-written and the walker's group names are machine-built, so the
// two can differ by a trailing slash or a `./` and mean the same
// recipe. Comparing them raw produced the worst possible message: the
// operator is told "examples/kube-platform-agent" is not in
// Tracked.Frozen while looking straight at that string in the source,
// and the dead-declaration error that would explain it never prints,
// because the stray check returns first.
func TestSites_FreezeDeclarationSpellingIsNormalised(t *testing.T) {
	for _, spelling := range []string{
		"recipes/frozen", "recipes/frozen/", "./recipes/frozen", "recipes//frozen",
	} {
		t.Run(spelling, func(t *testing.T) {
			tr := testTracked("recipes")
			tr.Frozen = []string{spelling}
			root := writeTree(t, map[string]string{
				"recipes/frozen/kustomization.yaml": "images:\n" +
					"  # pin-frozen: #704 — portability case study\n" +
					"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
			})
			sites := mustSites(t, tr, root)
			if len(sites) != 1 {
				t.Fatalf("found %d sites, want 1: %v", len(sites), siteKeys(sites))
			}
			if !sites[0].Frozen {
				t.Errorf("%q did not freeze the recipe it names", spelling)
			}
		})
	}
}

// TestSites_DocumentingTheMarkerDoesNotArmIt is the adversary's case run
// end to end. Three independent rules have to hold for this to stay
// inert, so the test asserts the OUTCOME rather than any one of them:
// the pin is live, and the walk did not error either (an error here
// would be its own kind of unusable — a repo that cannot document its
// own tooling).
func TestSites_DocumentingTheMarkerDoesNotArmIt(t *testing.T) {
	tr := testTracked("recipes")
	root := writeTree(t, map[string]string{
		"recipes/demo/kustomization.yaml": docBlockAboveALivePin,
	})
	got := siteKeys(mustSites(t, tr, root))
	want := []string{"recipes/demo/kustomization.yaml:kustomize newTag:v1.0.0"}
	if !equal(got, want) {
		t.Errorf("Sites() = %v, want %v (the documentation froze the pin it describes)", got, want)
	}
}

// TestSites_APlaceholderReasonIsNotAReason: `<why>` is the universal
// shape of "fill this in", and a mechanism whose own instructions arm it
// is a mechanism that fires by accident.
func TestSites_APlaceholderReasonIsNotAReason(t *testing.T) {
	tr := testTracked("recipes")
	tr.Frozen = []string{"recipes/demo"}
	root := writeTree(t, map[string]string{
		"recipes/demo/kustomization.yaml": "images:\n" +
			"  # pin-frozen: <why this recipe should not track upstream>\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
	})
	// Declared frozen, so a placeholder that counted would freeze the
	// pin silently; one that does not count trips the missing-marker
	// error instead. Either way it must not read as a live freeze.
	if _, err := tr.Sites(root); err == nil {
		t.Fatal("a bracketed placeholder was accepted as a freeze reason")
	}
}

// TestSites_AMarkerMustOwnItsCommentLine: indentation inside a comment
// block is what separates a worked example from a decision, and it is
// the only signal available — both look like `#` lines by the time the
// scanner sees them.
func TestSites_AMarkerMustOwnItsCommentLine(t *testing.T) {
	tr := testTracked("recipes")
	root := writeTree(t, map[string]string{
		"recipes/demo/deploy.yaml": "spec:\n" +
			"  # here is how you would freeze this:\n" +
			"  #   # pin-frozen: #123 — a perfectly good reason, indented\n" +
			"  image: example.test/widget:v1.0.0\n",
	})
	got := siteKeys(mustSites(t, tr, root))
	if want := []string{"recipes/demo/deploy.yaml:image reference:v1.0.0"}; !equal(got, want) {
		t.Errorf("Sites() = %v, want %v", got, want)
	}
}

// TestSites_ABlankLineEndsTheCommentBlock: "immediately above" has to
// mean immediately, or a detached block claims the next pin down the
// file — which is how a marker written about one thing ends up
// exempting another.
func TestSites_ABlankLineEndsTheCommentBlock(t *testing.T) {
	tr := testTracked("recipes")
	root := writeTree(t, map[string]string{
		"recipes/demo/deploy.yaml": "# pin-frozen: #123 — about something that is no longer here\n" +
			"\n" +
			"image: example.test/widget:v1.0.0\n",
	})
	// No error: the marker was never seen, so there is nothing to
	// reconcile against the (empty) declared set. The pin is live.
	got := siteKeys(mustSites(t, tr, root))
	if want := []string{"recipes/demo/deploy.yaml:image reference:v1.0.0"}; !equal(got, want) {
		t.Errorf("Sites() = %v, want %v", got, want)
	}
}

// TestSites_GoSourceIsScannedForItsDeclaredLiteralsOnly. A test fixture
// states a tag twice — once in the fixture, once in the assertion — and
// the rewriter can only see the first. Bumping a fixture out from under
// its assertion leaves a green test that checks nothing, which is
// strictly worse than not reaching the file at all.
//
// So the rule is narrower than "declared or not". A declared .go file
// is scanned for ITS LITERAL and nothing else: declaring one site does
// not re-open free-text reference scanning over the rest of the file,
// which would put every fixture and every historical tag in it back in
// range of the rewriter. Both halves are asserted here.
func TestSites_GoSourceIsScannedForItsDeclaredLiteralsOnly(t *testing.T) {
	tr := testTracked("recipes")
	tr.Literals = []Literal{{
		Path: "recipes/demo/recipe_test.go",
		Re:   regexp.MustCompile(`wantTag\s*=\s*"(v[0-9]+\.[0-9]+\.[0-9]+)"`),
		Why:  "the in-tree oracle",
	}}
	root := writeTree(t, map[string]string{
		// Declared — but only the literal is in range. The pre-rename
		// fixture below it is deliberately old and must stay v0.9.0.
		"recipes/demo/recipe_test.go": "const wantTag = \"v1.0.0\"\n" +
			"// fixture: \"image: example.test/widget:v0.9.0\" historical pre-rename case\n",
		// Undeclared: its fixture and its assertion state the same tag in
		// two shapes, and exactly one of them is machine-visible.
		"recipes/other/scanner_test.go": "const fixture = \"- image: example.test/widget:v1.0.0\"\n" +
			"// assertion: strings.Contains(out, \"v1.0.0\")\n",
	})
	got := siteKeys(mustSites(t, tr, root))
	want := []string{"recipes/demo/recipe_test.go:declared literal:v1.0.0"}
	if !equal(got, want) {
		t.Errorf("Sites() = %v, want %v", got, want)
	}
}

// TestSites_QuotedAndUnquotedScalars: the byte range must be the tag,
// not the quote. Splicing over the quote produces invalid YAML that the
// verification pass would catch only by luck.
func TestSites_QuotedAndUnquotedScalars(t *testing.T) {
	tr := testTracked("recipes")
	root := writeTree(t, map[string]string{
		"recipes/quoted/kustomization.yaml": "images:\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
		"recipes/bare/kustomization.yaml": "images:\n" +
			"  - name: example.test/widget\n    newTag: v1.0.0\n",
		"recipes/single/kustomization.yaml": "images:\n" +
			"  - name: example.test/widget\n    newTag: 'v1.0.0'\n",
	})
	sites := mustSites(t, tr, root)
	if len(sites) != 3 {
		t.Fatalf("found %d sites, want 3: %v", len(sites), siteKeys(sites))
	}
	plan, err := PlanRewrite(root, sites, "v2.0.0")
	if err != nil {
		t.Fatalf("PlanRewrite: %v", err)
	}
	if err := plan.Apply(root); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, want := range []struct{ path, contains string }{
		{"recipes/quoted/kustomization.yaml", `newTag: "v2.0.0"`},
		{"recipes/bare/kustomization.yaml", `newTag: v2.0.0`},
		{"recipes/single/kustomization.yaml", `newTag: 'v2.0.0'`},
	} {
		body := read(t, root, want.path)
		if !strings.Contains(body, want.contains) {
			t.Errorf("%s = %q, want it to contain %q", want.path, body, want.contains)
		}
	}
}

// TestRewritePlan_ApplyThenRevertRestoresTheTree is what makes "refuse
// to write anything if a single site fails" achievable after the fact:
// verification happens on the finished tree, so the undo has to be
// real.
func TestRewritePlan_ApplyThenRevertRestoresTheTree(t *testing.T) {
	tr := testTracked("recipes")
	root := writeTree(t, map[string]string{
		"recipes/demo/kustomization.yaml": "images:\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
		"recipes/demo/deploy.yaml": "image: example.test/widget:v1.0.0\n",
	})
	before := map[string]string{
		"recipes/demo/kustomization.yaml": read(t, root, "recipes/demo/kustomization.yaml"),
		"recipes/demo/deploy.yaml":        read(t, root, "recipes/demo/deploy.yaml"),
	}
	plan, err := PlanRewrite(root, mustSites(t, tr, root), "v2.0.0")
	if err != nil {
		t.Fatalf("PlanRewrite: %v", err)
	}
	if err := plan.Apply(root); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for path, was := range before {
		if now := read(t, root, path); now == was {
			t.Fatalf("%s was not rewritten", path)
		}
	}
	if err := plan.Revert(root); err != nil {
		t.Fatalf("Revert: %v", err)
	}
	for path, was := range before {
		if now := read(t, root, path); now != was {
			t.Errorf("%s after Revert = %q, want %q", path, now, was)
		}
	}
}

// TestPlanRewrite_RefusesWhenASiteMoved: planning re-reads and re-checks
// every byte range, so a tree edited between discovery and rewrite is
// refused whole rather than half-written.
func TestPlanRewrite_RefusesWhenASiteMoved(t *testing.T) {
	tr := testTracked("recipes")
	root := writeTree(t, map[string]string{
		"recipes/a/deploy.yaml": "image: example.test/widget:v1.0.0\n",
		"recipes/b/deploy.yaml": "image: example.test/widget:v1.0.0\n",
	})
	sites := mustSites(t, tr, root)

	// Something else edits one file after discovery.
	if err := os.WriteFile(filepath.Join(root, "recipes", "b", "deploy.yaml"),
		[]byte("# a new line on top\nimage: example.test/widget:v1.0.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := read(t, root, "recipes/a/deploy.yaml")

	if _, err := PlanRewrite(root, sites, "v2.0.0"); err == nil {
		t.Fatal("PlanRewrite accepted a moved site")
	} else if !strings.Contains(err.Error(), "nothing was written") {
		t.Errorf("error = %v, want it to say nothing was written", err)
	}
	if now := read(t, root, "recipes/a/deploy.yaml"); now != before {
		t.Errorf("the other file was written anyway: %q", now)
	}
}

// TestSites_MissingDeclaredPathsAreSkipped: the tool has to be able to
// answer on an older tree — which is how the gate was demonstrated
// against the checkout that predates the sites it now tracks.
func TestSites_MissingDeclaredPathsAreSkipped(t *testing.T) {
	tr := testTracked("recipes", "harness/absent-file")
	tr.Docs = []string{"recipes/demo/README.md", "docs/not-written-yet.md"}
	tr.Literals = []Literal{{
		Path: "recipes/demo/absent_test.go",
		Re:   regexp.MustCompile(`wantTag = "(v[0-9]+\.[0-9]+\.[0-9]+)"`),
		Why:  "a site added in a later release",
	}}
	root := writeTree(t, map[string]string{
		"recipes/demo/README.md": "This recipe pins `v1.0.0`.\n",
	})
	if got, want := siteKeys(mustSites(t, tr, root)),
		[]string{"recipes/demo/README.md:prose:v1.0.0"}; !equal(got, want) {
		t.Errorf("Sites() = %v, want %v", got, want)
	}
}

// TestSites_BinaryAndUnknownFilesAreNotScanned keeps the walk off
// artifacts where a byte sequence that looks like a reference is not
// one.
func TestSites_BinaryAndUnknownFilesAreNotScanned(t *testing.T) {
	tr := testTracked("recipes")
	root := writeTree(t, map[string]string{
		"recipes/demo/logo.png":    "\x89PNG\r\n\x1a\n example.test/widget:v1.0.0\n",
		"recipes/demo/deploy.yaml": "image: example.test/widget:v1.0.0\n",
	})
	if got, want := siteKeys(mustSites(t, tr, root)),
		[]string{"recipes/demo/deploy.yaml:image reference:v1.0.0"}; !equal(got, want) {
		t.Errorf("Sites() = %v, want %v", got, want)
	}
}

func read(t *testing.T, root, rel string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
