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

// Every test here is offline. The two calls that reach out live behind
// [Resolver], and [stubResolver] is what these use — see the note at
// the top of upstream.go for why that is load-bearing rather than
// merely convenient.

package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/internal/imagepin"
)

func rel(tag string, opts ...func(*Release)) Release {
	r := Release{
		Tag:  tag,
		Name: tag,
		Body: "notes for " + tag,
		URL:  "https://example.test/releases/" + tag,
	}
	for _, o := range opts {
		o(&r)
	}
	return r
}

func draft(r *Release)      { r.Draft = true }
func prerelease(r *Release) { r.Prerelease = true }

// testTracked mirrors [imagepin.Lookout]'s shape on a fake image, so
// these tests do not move every time the real recipe does.
func testTracked(roots ...string) *imagepin.Tracked {
	const image = "example.test/widget"
	const semver = `v[0-9]+\.[0-9]+\.[0-9]+`
	return &imagepin.Tracked{
		Family:       imagepin.NewFamily(nil, image),
		UpstreamRepo: "example/widget",
		Roots:        roots,
		ImageRefRe:   regexp.MustCompile(regexp.QuoteMeta(image) + `:(` + semver + `)`),
		ProseRe:      regexp.MustCompile("pin(?:s|ned) `?(" + semver + ")`?"),
		TagRe:        regexp.MustCompile(`^` + semver + `$`),
		FrozenMarker: regexp.MustCompile(`(?m)^#[ \t]?pin-frozen:[ \t]*(\S.*?)[ \t]*$`),
	}
}

// frozen declares the recipes a fixture means to exempt. A marker alone
// no longer exempts anything, so any fixture that plants one has to say
// so here too — which is the property [imagepin.Tracked.Frozen]
// exists for, exercised incidentally by every test that uses it.
func frozen(tr *imagepin.Tracked, recipes ...string) *imagepin.Tracked {
	tr.Frozen = recipes
	return tr
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for path, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func scanFor(t *testing.T, tr *imagepin.Tracked, root, targetTag string) scanResult {
	t.Helper()
	sites, err := tr.Sites(root)
	if err != nil {
		t.Fatalf("Sites: %v", err)
	}
	return classify(sites, targetTag)
}

// TestResolveUpstream_SkipsAReleaseWhoseImageIsNotPublishedYet is the
// documented answer to "what if the newest release has no image".
//
// A GitHub release and its container image are two events. Bumping to a
// tag that cannot be pulled opens a pull request whose kind e2e fails
// on ImagePullBackOff, and next week's run opens another one.
func TestResolveUpstream_SkipsAReleaseWhoseImageIsNotPublishedYet(t *testing.T) {
	r := newStubResolver(snapshot{
		Releases:    []Release{rel("v1.2.0"), rel("v1.1.0"), rel("v1.0.0")},
		Unpublished: []string{"v1.2.0"},
	})
	got, err := resolveUpstream(context.Background(), r)
	if err != nil {
		t.Fatalf("resolveUpstream: %v", err)
	}
	if got.Target.Tag != "v1.1.0" {
		t.Errorf("Target = %s, want v1.1.0", got.Target.Tag)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].Tag != "v1.2.0" {
		t.Errorf("Skipped = %v, want [v1.2.0]", got.Skipped)
	}
}

// TestResolveUpstream_FailsWhenNothingIsPullable: "could not determine"
// must be a tool failure, never a silent drift=false.
func TestResolveUpstream_FailsWhenNothingIsPullable(t *testing.T) {
	r := newStubResolver(snapshot{
		Releases:    []Release{rel("v1.1.0"), rel("v1.0.0")},
		Unpublished: []string{"v1.1.0", "v1.0.0"},
	})
	if _, err := resolveUpstream(context.Background(), r); err == nil {
		t.Fatal("resolveUpstream accepted a repository with no pullable image")
	}
}

// TestPublishedReleases_DropsDraftsAndPrereleasesAndOrdersBySemver.
// Ordering by version rather than by publication date matters: a
// backported patch cut after a newer minor would otherwise present
// itself as latest and walk the pin backwards.
func TestPublishedReleases_DropsDraftsAndPrereleasesAndOrdersBySemver(t *testing.T) {
	got := publishedReleases([]Release{
		rel("v1.0.1"), // backport, published last
		rel("v2.0.0-rc.1", prerelease),
		rel("v3.0.0", draft),
		rel("v1.9.0"),
		rel("nightly"),
		rel("v1.10.0"),
	})
	var tags []string
	for _, r := range got {
		tags = append(tags, r.Tag)
	}
	want := []string{"v1.10.0", "v1.9.0", "v1.0.1"}
	if strings.Join(tags, ",") != strings.Join(want, ",") {
		t.Errorf("publishedReleases = %v, want %v", tags, want)
	}
}

// TestVerdict_FrozenOnlyDriftIsNotDrift is the design call the frozen
// marker exists for: examples/kube-platform-agent is deliberately ten
// releases behind (#704), and a check that reported that forever is a
// check people learn to ignore.
func TestVerdict_FrozenOnlyDriftIsNotDrift(t *testing.T) {
	tr := frozen(testTracked("recipes"), "recipes/frozen")
	root := writeTree(t, map[string]string{
		"recipes/frozen/kustomization.yaml": "images:\n" +
			"  # pin-frozen: #704 — portability case study\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
		"recipes/live/kustomization.yaml": "images:\n" +
			"  - name: example.test/widget\n    newTag: \"v2.0.0\"\n",
	})
	got := scanFor(t, tr, root, "v2.0.0")
	if len(got.frozen) != 1 {
		t.Fatalf("frozen = %d, want 1", len(got.frozen))
	}
	if v := verdict(got); v != "drift=false" {
		t.Errorf("verdict = %q, want drift=false (the only lagging pin is frozen)", v)
	}

	// And the live one going stale is still drift.
	if v := verdict(scanFor(t, tr, root, "v3.0.0")); v != "drift=true" {
		t.Errorf("verdict = %q, want drift=true", v)
	}
}

// TestVerdict_IsExactlyOneOfTwoStrings. The workflow parses this; a
// third possible value is a workflow that opens pull requests on
// nonsense.
func TestVerdict_IsExactlyOneOfTwoStrings(t *testing.T) {
	site := imagepin.Site{Path: "x", Tag: "v1.0.0"}
	for _, tc := range []struct {
		name string
		in   scanResult
		want string
	}{
		{"clean", scanResult{current: []imagepin.Site{site}}, "drift=false"},
		{"empty", scanResult{}, "drift=false"},
		{"stale", scanResult{stale: []imagepin.Site{site}}, "drift=true"},
	} {
		if got := verdict(tc.in); got != tc.want {
			t.Errorf("%s: verdict = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRewrite_VerifiesItsOwnWorkByRediscovery: the plan and the check
// share no code path beyond the scanner, so a rewrite that leaves the
// tree in a state discovery cannot read is caught here rather than
// pushed. And when it is caught, nothing is left written.
func TestRewrite_VerifiesItsOwnWorkByRediscovery(t *testing.T) {
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

	t.Run("a clean rewrite is accepted", func(t *testing.T) {
		got := scanFor(t, tr, root, "v2.0.0")
		if err := rewrite(root, tr, got, "v2.0.0"); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
		for path := range before {
			if !strings.Contains(read(t, root, path), "v2.0.0") {
				t.Errorf("%s was not rewritten", path)
			}
		}
	})

	t.Run("a rewrite that hides the sites is refused and reverted", func(t *testing.T) {
		after := map[string]string{
			"recipes/demo/kustomization.yaml": read(t, root, "recipes/demo/kustomization.yaml"),
			"recipes/demo/deploy.yaml":        read(t, root, "recipes/demo/deploy.yaml"),
		}
		// A tag the matchers cannot see afterwards. Re-discovery then
		// finds NOTHING, which reads as "every site is at the new tag"
		// unless the count is checked too.
		got := scanFor(t, tr, root, "not-a-semver")
		err := rewrite(root, tr, got, "not-a-semver")
		if err == nil {
			t.Fatal("rewrite accepted a tree its own scanner can no longer read")
		}
		if !strings.Contains(err.Error(), "nothing was left written") {
			t.Errorf("error = %v, want it to say nothing was left written", err)
		}
		for path, was := range after {
			if now := read(t, root, path); now != was {
				t.Errorf("%s = %q after the refused rewrite, want %q", path, now, was)
			}
		}
	})
}

// TestReleaseDelta_StartsAtTheOLDESTTagTheTreeCarries. A tree that
// disagrees with itself — the exact state this tool exists to catch —
// has a reader somewhere running the older pin, and the notes they need
// are the ones from there forward.
func TestReleaseDelta_StartsAtTheOldestTagTheTreeCarries(t *testing.T) {
	upstream := upstreamState{
		Releases: []Release{rel("v1.3.0"), rel("v1.2.0"), rel("v1.1.0"), rel("v1.0.0")},
		Target:   rel("v1.3.0"),
	}
	r := scanResult{
		stale: []imagepin.Site{{Tag: "v1.0.0"}, {Tag: "v1.2.0"}},
		// A frozen site is far behind on purpose and must not drag the
		// delta back with it.
		frozen: []imagepin.Site{{Tag: "v0.1.0"}},
	}
	var tags []string
	for _, d := range releaseDelta(upstream, r) {
		tags = append(tags, d.Tag)
	}
	want := []string{"v1.3.0", "v1.2.0", "v1.1.0"}
	if strings.Join(tags, ",") != strings.Join(want, ",") {
		t.Errorf("releaseDelta = %v, want %v", tags, want)
	}
}

// TestPullRequestBody_CarriesTheReviewMaterial. A bump body that says
// only "v0.11.0 → v0.21.0" is a body that gets rubber-stamped, and
// rubber-stamping is how the pin went ten releases stale.
func TestPullRequestBody_CarriesTheReviewMaterial(t *testing.T) {
	tr := testTracked("recipes")
	upstream := upstreamState{
		Releases: []Release{
			func() Release {
				r := rel("v1.1.0")
				r.Body = "- BREAKING: renamed the `--watch` flag\n- tidy-up"
				return r
			}(),
			rel("v1.0.0"),
		},
		Target:  rel("v1.1.0"),
		Skipped: []Release{rel("v1.2.0")},
	}
	r := scanResult{
		stale: []imagepin.Site{{
			Path: "recipes/demo/kustomization.yaml", Line: 3,
			Kind: imagepin.KindKustomizeTag, Tag: "v1.0.0",
		}},
		frozen: []imagepin.Site{{
			Path: "recipes/frozen/kustomization.yaml", Line: 4,
			Kind: imagepin.KindKustomizeTag, Tag: "v0.1.0", Frozen: true,
			FrozenReason: "#704 — portability case study",
		}},
	}
	body := pullRequestBody(tr, upstream, r)

	for _, want := range []string{
		"recipes/demo/kustomization.yaml:3",    // what moved
		"recipes/frozen/kustomization.yaml:4",  // what did not
		"#704 — portability case study",        // and why not
		"LoadClusterListRequirements()",        // the RBAC checklist item
		"Breaking renames",                     // the rename checklist item
		"Flag defaults",                        // the flag checklist item
		"BREAKING: renamed the `--watch` flag", // the notes themselves
		"`v1.2.0` was skipped",                 // the unpullable release
		"## Adversarial review",                // review-gate.yml's requirement
		"https://github.com/example/widget",    // where to read more
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pull-request body is missing %q:\n%s", want, body)
		}
	}
	if len(body) > maxBodyBytes {
		t.Errorf("body is %d bytes, over the %d budget", len(body), maxBodyBytes)
	}
}

// TestPullRequestBody_StaysUnderGitHubsLimit. A body over the limit is
// rejected outright, which would turn a long changelog delta into no
// pull request at all.
func TestPullRequestBody_StaysUnderGitHubsLimit(t *testing.T) {
	huge := strings.Repeat("upstream wrote a great deal about this release.\n", 4000)
	var releases []Release
	for _, tag := range []string{"v1.4.0", "v1.3.0", "v1.2.0", "v1.1.0"} {
		r := rel(tag)
		r.Body = huge
		releases = append(releases, r)
	}
	body := pullRequestBody(testTracked(), upstreamState{
		Releases: releases,
		Target:   releases[0],
	}, scanResult{stale: []imagepin.Site{{Path: "x", Tag: "v1.0.0"}}})

	if len(body) > maxBodyBytes {
		t.Errorf("body is %d bytes, over the %d budget", len(body), maxBodyBytes)
	}
	for _, want := range []string{"Truncated", "## Adversarial review"} {
		if !strings.Contains(body, want) {
			t.Errorf("truncated body is missing %q", want)
		}
	}
}

// TestParseTag rejects everything that is not a plain three-part
// semver, because a tag this cannot order is one the tool declines to
// reason about rather than guesses at.
func TestParseTag(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"v1.2.3", true},
		{"v0.21.0", true},
		{"1.2.3", false},
		{"v1.2", false},
		{"v1.2.3-rc.1", false},
		{"latest", false},
		{"v1.2.x", false},
	} {
		if _, ok := parseTag(tc.in); ok != tc.ok {
			t.Errorf("parseTag(%q) ok = %v, want %v", tc.in, ok, tc.ok)
		}
	}
	a, _ := parseTag("v0.9.0")
	b, _ := parseTag("v0.10.0")
	if !a.less(b) {
		t.Error("v0.9.0 should order before v0.10.0 (numeric, not lexical)")
	}
}

// TestLoadSnapshot_AcceptsARawReleasesArray so a captured
// `gh api repos/OWNER/REPO/releases` response can be replayed with no
// hand-editing.
func TestLoadSnapshot_AcceptsARawReleasesArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "releases.json")
	raw := `[{"tag_name":"v1.1.0","draft":false,"prerelease":false},
	         {"tag_name":"v1.0.0","draft":false,"prerelease":false}]`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadSnapshot(path)
	if err != nil {
		t.Fatalf("loadSnapshot: %v", err)
	}
	if len(got.Releases) != 2 || got.Releases[0].Tag != "v1.1.0" {
		t.Errorf("loadSnapshot = %+v, want the two releases", got.Releases)
	}
}

// TestSnapshotRoundTrip_ReplaysTheSameResolution. The weekly job asks
// upstream once and hands the answer to the rewrite, so that both steps
// reason about one release rather than two calls minutes apart. That
// only holds if the written file replays identically — and in
// particular if it carries the SKIPPED tags, since dropping them would
// make the replay pick an unpullable release the verdict rejected.
func TestSnapshotRoundTrip_ReplaysTheSameResolution(t *testing.T) {
	live := newStubResolver(snapshot{
		Releases: []Release{
			rel("v1.3.0"), rel("v1.2.0"), rel("v1.1.0"),
			rel("v2.0.0-rc.1", prerelease), rel("v9.9.9", draft),
		},
		Unpublished: []string{"v1.3.0"},
	})
	first, err := resolveUpstream(context.Background(), live)
	if err != nil {
		t.Fatalf("resolveUpstream: %v", err)
	}

	path := filepath.Join(t.TempDir(), "upstream.json")
	if err := writeSnapshot(path, first); err != nil {
		t.Fatalf("writeSnapshot: %v", err)
	}
	replayed, err := loadSnapshot(path)
	if err != nil {
		t.Fatalf("loadSnapshot: %v", err)
	}
	second, err := resolveUpstream(context.Background(), newStubResolver(replayed))
	if err != nil {
		t.Fatalf("resolveUpstream (replay): %v", err)
	}

	if second.Target.Tag != first.Target.Tag {
		t.Errorf("replayed target = %s, want %s", second.Target.Tag, first.Target.Tag)
	}
	if second.Target.Tag != "v1.2.0" {
		t.Errorf("target = %s, want v1.2.0 (v1.3.0 has no image)", second.Target.Tag)
	}
	if len(second.Skipped) != len(first.Skipped) {
		t.Errorf("replayed skipped = %v, want %v", second.Skipped, first.Skipped)
	}
	// The notes have to survive too: they are the body of the PR.
	if len(second.Releases) != len(first.Releases) {
		t.Fatalf("replayed %d releases, want %d", len(second.Releases), len(first.Releases))
	}
	for i := range first.Releases {
		if second.Releases[i].Tag != first.Releases[i].Tag ||
			second.Releases[i].Body != first.Releases[i].Body {
			t.Errorf("release %d = %+v, want %+v", i, second.Releases[i], first.Releases[i])
		}
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
