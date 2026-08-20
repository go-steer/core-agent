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

// The visibility and expiry halves of #791: a frozen pin used to be
// invisible on a clean week and permanent by default.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/internal/imagepin"
)

func date(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// frozenTree is one frozen recipe at v1.0.0 and one live recipe at the
// tag the caller is about to call current.
func frozenTree(t *testing.T, marker, liveTag string) (*imagepin.Tracked, string) {
	t.Helper()
	tr := frozen(testTracked("recipes"), "recipes/frozen")
	root := writeTree(t, map[string]string{
		"recipes/frozen/kustomization.yaml": "images:\n" +
			"  " + marker + "\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
		"recipes/live/kustomization.yaml": "images:\n" +
			"  - name: example.test/widget\n    newTag: \"" + liveTag + "\"\n",
	})
	return tr, root
}

func upstreamAt(target string, tags ...string) upstreamState {
	var releases []Release
	for _, tag := range tags {
		releases = append(releases, rel(tag))
	}
	return upstreamState{Releases: releases, Target: rel(target)}
}

// sites is a shorthand for the little scanResult literals below.
func sites(s ...imagepin.Site) []imagepin.Site { return s }

func imagepinSite(path, group, tag string) imagepin.Site {
	return imagepin.Site{Path: path, Group: group, Tag: tag, Kind: imagepin.KindKustomizeTag}
}

func frozenSite(path, group, tag string, review time.Time) imagepin.Site {
	s := imagepinSite(path, group, tag)
	s.Frozen, s.FrozenReason, s.FrozenReview = true, "#704 — case study", review
	return s
}

// TestSummary_NamesTheFrozenPinsOnACleanRun is the defect #791 reports.
// With every live pin current the job answered drift=false, opened no
// pull request and said nothing at all, so a frozen recipe could fall
// further behind upstream every Tuesday with nobody told. The summary is
// written on EVERY run, drift or not, and always carries the freezes.
func TestSummary_NamesTheFrozenPinsOnACleanRun(t *testing.T) {
	t.Parallel()
	tr, root := frozenTree(t, "# pin-frozen: #704 — case study (review: 2027-02-01)", "v2.0.0")
	r := scanFor(t, tr, root, "v2.0.0")
	if v := verdict(r); v != "drift=false" {
		t.Fatalf("verdict = %q, want drift=false — this test is about the SILENT week", v)
	}

	got := summaryMarkdown(tr, upstreamAt("v2.0.0", "v2.0.0", "v1.5.0", "v1.0.0"), r,
		date(2026, time.August, 20))
	for _, want := range []string{
		"Frozen pins",
		"recipes/frozen",
		"`v1.0.0`",
		"2027-02-01",
		"#704 — case study",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("clean-run summary does not mention %q:\n%s", want, got)
		}
	}
}

// TestSummary_CountsHowFarBehindAFrozenPinIs. "Frozen" quietly becoming
// "unmaintained" is the thing a reader needs to judge, and the number is
// the evidence a distance threshold could later be chosen from. It is
// reported, never acted on — there is no threshold in the tool.
func TestSummary_CountsHowFarBehindAFrozenPinIs(t *testing.T) {
	t.Parallel()
	tr, root := frozenTree(t, "# pin-frozen: #704 — case study (review: 2027-02-01)", "v2.0.0")
	r := scanFor(t, tr, root, "v2.0.0")

	got := summaryMarkdown(tr, upstreamAt("v2.0.0", "v2.0.0", "v1.5.0", "v1.1.0", "v1.0.0"), r,
		date(2026, time.August, 20))
	if !strings.Contains(got, "3 release(s)") {
		t.Errorf("summary does not say the frozen pin is 3 releases behind:\n%s", got)
	}
}

// TestSummary_MarksAnOverdueFreezeAndSaysWhatToDo. A status column
// nobody can act on is decoration.
func TestSummary_MarksAnOverdueFreezeAndSaysWhatToDo(t *testing.T) {
	t.Parallel()
	tr, root := frozenTree(t, "# pin-frozen: #704 — case study (review: 2026-01-01)", "v2.0.0")
	r := scanFor(t, tr, root, "v2.0.0")
	up := upstreamAt("v2.0.0", "v2.0.0", "v1.0.0")

	overdue := summaryMarkdown(tr, up, r, date(2026, time.August, 20))
	if !strings.Contains(overdue, "OVERDUE") {
		t.Errorf("a freeze eight months past its review date is not marked overdue:\n%s", overdue)
	}
	if !strings.Contains(overdue, "Tracked.Frozen") {
		t.Errorf("summary does not name the two ways out of an overdue freeze:\n%s", overdue)
	}

	// And it does not cry wolf: the same tree before the date says ok.
	fine := summaryMarkdown(tr, up, r, date(2025, time.December, 31))
	if strings.Contains(fine, "OVERDUE") {
		t.Errorf("a freeze inside its review window was marked overdue:\n%s", fine)
	}
}

// TestSummary_SaysSoWhenNothingIsFrozen. "No frozen pins" and "the
// freeze section was left out this week" look identical on a page nobody
// reads closely, and only one of them is good news.
func TestSummary_SaysSoWhenNothingIsFrozen(t *testing.T) {
	t.Parallel()
	tr := testTracked("recipes")
	root := writeTree(t, map[string]string{
		"recipes/live/kustomization.yaml": "images:\n" +
			"  - name: example.test/widget\n    newTag: \"v2.0.0\"\n",
	})
	got := summaryMarkdown(tr, upstreamAt("v2.0.0", "v2.0.0"),
		scanFor(t, tr, root, "v2.0.0"), date(2026, time.August, 20))
	if !strings.Contains(got, "Frozen pins") || !strings.Contains(got, "None") {
		t.Errorf("summary omits the freeze section entirely rather than saying it is empty:\n%s", got)
	}
}

// TestSummary_ListsTheDriftItIsAboutToRewrite, so the summary is a
// report of the run rather than a freeze-only appendix.
func TestSummary_ListsTheDriftItIsAboutToRewrite(t *testing.T) {
	t.Parallel()
	tr, root := frozenTree(t, "# pin-frozen: #704 — case study (review: 2027-02-01)", "v1.5.0")
	r := scanFor(t, tr, root, "v2.0.0")

	got := summaryMarkdown(tr, upstreamAt("v2.0.0", "v2.0.0", "v1.5.0", "v1.0.0"), r,
		date(2026, time.August, 20))
	if !strings.Contains(got, "### Drift") {
		t.Errorf("summary has no drift section on a run that found drift:\n%s", got)
	}
	if !strings.Contains(got, "recipes/live/kustomization.yaml") {
		t.Errorf("summary does not name the stale site:\n%s", got)
	}
	// The frozen pin is at v1.0.0 too, and must not be listed as drift.
	if strings.Contains(got, "| `recipes/frozen/kustomization.yaml:4` |") {
		t.Errorf("summary lists a frozen pin as drift:\n%s", got)
	}
}

// TestAppendSummary_AppendsRatherThanTruncates. The path this is aimed
// at in CI is $GITHUB_STEP_SUMMARY, which several steps of one job may
// write to; a truncating writer would silently eat whatever ran before.
func TestAppendSummary_AppendsRatherThanTruncates(t *testing.T) {
	t.Parallel()
	tr, root := frozenTree(t, "# pin-frozen: #704 — case study (review: 2027-02-01)", "v2.0.0")
	r := scanFor(t, tr, root, "v2.0.0")

	path := filepath.Join(t.TempDir(), "summary.md")
	if err := os.WriteFile(path, []byte("## an earlier step\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := appendSummary(path, tr, upstreamAt("v2.0.0", "v2.0.0"), r,
			date(2026, time.August, 20)); err != nil {
			t.Fatalf("appendSummary: %v", err)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "## an earlier step") {
		t.Errorf("appendSummary clobbered what was already in the file:\n%s", body)
	}
	if n := strings.Count(string(body), "## lookout pin check"); n != 2 {
		t.Errorf("wrote the report %d time(s) over two calls, want 2:\n%s", n, body)
	}
}

// TestSummary_EscapesAPipeInAFreezeReason: the reason is free prose in a
// YAML comment, and an unescaped `|` splits the table cell it lands in.
func TestSummary_EscapesAPipeInAFreezeReason(t *testing.T) {
	t.Parallel()
	tr, root := frozenTree(t,
		"# pin-frozen: #704 — case study | not a live deploy (review: 2027-02-01)", "v2.0.0")
	got := summaryMarkdown(tr, upstreamAt("v2.0.0", "v2.0.0"),
		scanFor(t, tr, root, "v2.0.0"), date(2026, time.August, 20))
	if !strings.Contains(got, `case study \| not a live deploy`) {
		t.Errorf("an unescaped pipe in a freeze reason breaks the table row:\n%s", got)
	}
}

// TestFreezeVerdict_IsExactlyOneOfTwoStrings. The workflow parses this
// and fails the job on the second value; a third possible output is a
// scheduled run that goes red on nonsense.
func TestFreezeVerdict_IsExactlyOneOfTwoStrings(t *testing.T) {
	t.Parallel()
	now := date(2026, time.August, 20)
	live := imagepinSite("recipes/live/x.yaml", "recipes/live", "v2.0.0")
	fine := frozenSite("recipes/frozen/x.yaml", "recipes/frozen", "v1.0.0", date(2027, time.February, 1))
	lapsed := frozenSite("recipes/old/x.yaml", "recipes/old", "v1.0.0", date(2026, time.January, 1))

	for _, tc := range []struct {
		name string
		in   scanResult
		want string
	}{
		{"empty", scanResult{}, "freeze-review=ok"},
		{"no freezes at all", scanResult{current: sites(live)}, "freeze-review=ok"},
		{"a freeze inside its window", scanResult{frozen: sites(fine)}, "freeze-review=ok"},
		{"a lapsed freeze", scanResult{frozen: sites(lapsed)}, "freeze-review=overdue"},
		{"one of each", scanResult{frozen: sites(fine, lapsed)}, "freeze-review=overdue"},
	} {
		if got := freezeVerdict(tc.in, now); got != tc.want {
			t.Errorf("%s: freezeVerdict = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestFreezeVerdict_IsSeparateFromDrift. Folding the two together would
// break the workflow in a specific way: drift=true drives a REWRITE, and
// a frozen pin must never be rewritten, so the bump step would produce
// an empty diff and a pull request about nothing.
func TestFreezeVerdict_IsSeparateFromDrift(t *testing.T) {
	t.Parallel()
	tr, root := frozenTree(t, "# pin-frozen: #704 — case study (review: 2026-01-01)", "v2.0.0")
	r := scanFor(t, tr, root, "v2.0.0")
	now := date(2026, time.August, 20)

	if got := verdict(r); got != "drift=false" {
		t.Errorf("an overdue freeze changed the drift verdict to %q — the weekly job would "+
			"rewrite nothing and open an empty pull request", got)
	}
	if got := freezeVerdict(r, now); got != "freeze-review=overdue" {
		t.Errorf("freezeVerdict = %q, want freeze-review=overdue", got)
	}
}

// TestFreezeGroups_FoldOnePerRecipe. The review is a decision about the
// recipe, so naming the same lapse once per pin is how a report becomes
// something people skim past.
func TestFreezeGroups_FoldOnePerRecipe(t *testing.T) {
	t.Parallel()
	tr := frozen(testTracked("recipes"), "recipes/frozen")
	root := writeTree(t, map[string]string{
		"recipes/frozen/kustomization.yaml": "images:\n" +
			"  # pin-frozen: #704 — case study (review: 2027-02-01)\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
		"recipes/frozen/deploy.yaml": "image: example.test/widget:v1.0.0\n",
	})
	r := scanFor(t, tr, root, "v2.0.0")
	if len(r.frozen) != 2 {
		t.Fatalf("frozen sites = %d, want 2", len(r.frozen))
	}
	groups := r.freezeGroups()
	if len(groups) != 1 {
		t.Fatalf("freezeGroups = %d, want 1 (both pins are the same recipe)", len(groups))
	}
	if groups[0].sites != 2 {
		t.Errorf("group.sites = %d, want 2", groups[0].sites)
	}
	if got := groups[0].review.Format(reviewDateLayout); got != "2027-02-01" {
		t.Errorf("group.review = %s, want 2027-02-01", got)
	}
}
