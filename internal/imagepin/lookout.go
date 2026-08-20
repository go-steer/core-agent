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

// The lookout watcher image: the one image family this repo tracks
// against an upstream release feed (#787).
//
// This declaration lives in a module-wide internal package rather than
// beside either of its consumers because it has two, in different
// internal trees that cannot import each other:
// dev/lookout-pin-check (the weekly job) and
// examples/gke-troubleshoot-agent (the offline in-tree test). One list
// of documents and one pair of prose matchers, read by both.

package imagepin

import "regexp"

// lookoutImage is the watcher image go-steer/k8s-lookout publishes.
const lookoutImage = "ghcr.io/go-steer/lookout"

// semverTagPattern is the shape of a lookout release tag. Lookout keeps
// the leading `v`, unlike this repo's own images — which is what stops
// this family's matchers from ever firing on a core-agent pin sitting
// two lines away in the same `images:` block.
const semverTagPattern = `v[0-9]+\.[0-9]+\.[0-9]+`

// Lookout is the tracked declaration for the watcher image.
//
// Scope note: the core-agent daemon images are DELIBERATELY not tracked
// here. #680's recipecheck gate already reads those same declaration
// sites to answer a different question ("can this daemon run this
// config?"), and a weekly upstream-bump job would mix a mechanical
// third-party bump with a question about our own release cadence. The
// walker is parameterised by [Family] so adding that second family is a
// declaration plus a resolver, not a rewrite — see the package doc.
var Lookout = &Tracked{
	Family:       NewFamily(nil, lookoutImage),
	UpstreamRepo: "go-steer/k8s-lookout",

	// Everything the repo ships as a deployable recipe, plus the one
	// deploy artifact that lives outside them. Walking the whole
	// examples directory rather than naming recipes keeps a recipe added
	// next year from being invisible; the e2e harness has to be named
	// because it is CI's file, not a recipe's, and its WATCHER_IMAGE
	// default is what the kind run actually pulls.
	Roots: []string{
		"examples",
		"dev/tools/e2e-recipe-gke-troubleshoot-agent",
	},

	// The documents that quote the pin to a reader. A stale one is not
	// merely an out-of-date sentence: DEMO.md's preflight SHELLS OUT
	// (`crane digest ghcr.io/go-steer/lookout:<tag>`), so a bump that
	// misses it hands an operator a command that verifies a tag the
	// recipe no longer deploys — and the site pages are what a copier
	// reads before they ever open a manifest.
	//
	// examples/kube-platform-agent's README is absent on purpose: that
	// recipe is frozen (#704) and states its tag in forms these
	// matchers do not reach anyway (`lookout:v0.18.0` without the
	// registry, `deploy/12 @ v0.18.0`).
	Docs: []string{
		"examples/gke-troubleshoot-agent/README.md",
		"examples/gke-troubleshoot-agent/DEMO.md",
		"README.md",
		"docs/site/src/content/docs/reference/troubleshooting-agent.md",
		"docs/site/src/content/docs/examples/index.md",
	},

	Literals: []Literal{{
		Path: "examples/gke-troubleshoot-agent/recipe_test.go",
		Re:   regexp.MustCompile(`wantWatcherTag\s*=\s*"(` + semverTagPattern + `)"`),
		Why: "wantWatcherTag, the constant TestWatcherImagePinIsConsistent and " +
			"TestWatcherTagInDocsIsCurrent hold every other site to",
	}},

	// The two shapes a document names the pin in, unchanged from the
	// pair PR #788 landed: a full image reference, and the prose
	// "pins/pinned <tag>" (backticked or not).
	//
	// What they DON'T match is the whole point. "pinning back below
	// `v0.17.0`", "`v0.11.0` is where the watcher started sending
	// Content-Type", "v0.14.0 remains the capability floor" — every one
	// of those is history that must survive a bump, and a rewrite that
	// clobbered them would corrupt the rationale silently. The
	// exclusion is structural, not a list: neither matcher can reach a
	// bare version mention, and "pinning" is not "pins" or "pinned".
	ImageRefRe: regexp.MustCompile(regexp.QuoteMeta(lookoutImage) + `:(` + semverTagPattern + `)`),
	ProseRe:    regexp.MustCompile("pin(?:s|ned) `?(" + semverTagPattern + ")`?"),
	TagRe:      regexp.MustCompile(`^` + semverTagPattern + `$`),

	// examples/kube-platform-agent does not track upstream. It is a
	// portability case study (#704) whose v0.18.0 pin is the version its
	// vendored content and its RBAC were written against; bumping it
	// would change what the study demonstrates.
	//
	// Naming it here is the exemption. The marker in the manifest is the
	// explanation AND the expiry, and the two are checked against each
	// other — see [Tracked.Frozen].
	Frozen: []string{"examples/kube-platform-agent"},

	// The frozen opt-out. It must own its comment line and carry a real
	// reason, and it must be attached to the pin — but "attached" means
	// two different things, and the split is by SITE KIND, not by file:
	//
	//   - For a kustomize `newTag:` (the only kind read off the YAML
	//     node tree), the marker may be the head or the line comment of
	//     the `images:` entry or of either of its keys. Position within
	//     the entry does not matter; belonging to the entry does.
	//
	//   - For EVERY OTHER KIND — an image reference, a prose mention, a
	//     declared literal — there is no node tree, so the marker must
	//     be in the contiguous block of `#` comment lines immediately
	//     above the reference. A blank line ends that block, and a
	//     trailing comment on the reference's own line is NOT read.
	//
	// That is a per-site rule, not a per-file one: a raw `image:` ref
	// inside a `patches:` block of a kustomization.yaml is an image
	// reference, so it takes the comment-block rule even though the
	// file it sits in also has newTag entries that do not.
	//
	//	# pin-frozen: #704 — portability case study (review: 2027-02-01)
	//	- name: ghcr.io/go-steer/lookout
	//	  newTag: "v0.18.0"
	//
	// The `review:` clause is required, not decoration — see
	// [reviewClause]. A freeze is a decision with a shelf life; the date
	// is when someone should ask whether it still holds, and the weekly
	// job reports the lapse rather than bumping anything.
	//
	// The pattern requires the `#` at the start of the (trimmed) comment
	// line, so a marker indented inside a larger comment block — which
	// is how a worked example naturally gets written — is not one. That
	// plus the placeholder rule plus [Tracked.Frozen] are three
	// independent reasons the paragraph above this comment cannot freeze
	// anything, and all three are wanted: the first two make an accident
	// inert, the third makes it loud.
	FrozenMarker: regexp.MustCompile(`(?m)^#[ \t]?pin-frozen:[ \t]*(\S.*?)[ \t]*$`),
}
