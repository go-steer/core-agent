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

// The review-date half of the freeze contract (#791): a freeze is a
// decision with a shelf life, not a permanent exemption.

package imagepin

import (
	"strings"
	"testing"
	"time"
)

func freezeTree(marker string) map[string]string {
	return map[string]string{
		"recipes/frozen/kustomization.yaml": "images:\n" +
			"  " + marker + "\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
	}
}

// TestSites_AFreezeWithNoReviewDateIsRejected is the whole point of
// #791. A marker with a perfectly good reason and no date freezes the
// pin FOREVER, silently: it is exempt from the bump, it reports no
// drift, and nothing ever asks whether the exemption still holds. That
// is #787's failure — a pin nobody was told about — differing only in
// that somebody meant it once.
func TestSites_AFreezeWithNoReviewDateIsRejected(t *testing.T) {
	t.Parallel()
	tr := testTracked("recipes")
	tr.Frozen = []string{"recipes/frozen"}
	root := writeTree(t, freezeTree("# pin-frozen: #704 — portability case study"))

	_, err := tr.Sites(root)
	if err == nil {
		t.Fatal("Sites() accepted a freeze with no review date, so the freeze is permanent " +
			"and nothing will ever ask about it again")
	}
	if !strings.Contains(err.Error(), "review") {
		t.Errorf("error does not mention the review date, so the reader cannot act on it: %v", err)
	}
}

// TestSites_AnImpossibleReviewDateIsRejected: the date goes through
// time.Parse rather than a shape check, so a month or day out of range
// is caught here rather than silently becoming some other day.
func TestSites_AnImpossibleReviewDateIsRejected(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"2027-13-01", "2027-02-30", "2027-00-10"} {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			tr := testTracked("recipes")
			tr.Frozen = []string{"recipes/frozen"}
			root := writeTree(t, freezeTree("# pin-frozen: #704 — case study (review: "+bad+")"))
			if _, err := tr.Sites(root); err == nil {
				t.Fatalf("Sites() accepted review date %q", bad)
			}
		})
	}
}

// TestSites_AMalformedMarkerIsNotReportedAsAMissingOne. The two failures
// need different fixes and the reader has to be able to tell them apart:
// "no marker was found" sends someone hunting for a comment that is
// sitting right in front of them.
func TestSites_AMalformedMarkerIsNotReportedAsAMissingOne(t *testing.T) {
	t.Parallel()
	tr := testTracked("recipes")
	tr.Frozen = []string{"recipes/frozen"}
	root := writeTree(t, freezeTree("# pin-frozen: #704 — portability case study"))

	_, err := tr.Sites(root)
	if err == nil {
		t.Fatal("Sites() accepted a dateless freeze")
	}
	if strings.Contains(err.Error(), "no usable pin-frozen marker was found") {
		t.Errorf("reported a present-but-dateless marker as an absent one: %v", err)
	}
	// And it names the pin the marker is attached to — file and line, the
	// same coordinates every other message in this package uses — rather
	// than only the recipe.
	if !strings.Contains(err.Error(), "recipes/frozen/kustomization.yaml:4") {
		t.Errorf("error does not point at the frozen pin: %v", err)
	}
}

// TestSites_TheReviewDateIsParsedAndPropagatedAcrossTheRecipe. The
// freeze itself already propagates from the marked pin to every pin in
// the recipe; the date has to travel with it, or a README pin in a
// frozen recipe would read as a freeze with no expiry.
func TestSites_TheReviewDateIsParsedAndPropagatedAcrossTheRecipe(t *testing.T) {
	t.Parallel()
	tr := testTracked("recipes")
	tr.Frozen = []string{"recipes/frozen"}
	tr.Docs = []string{"recipes/frozen/README.md"}
	root := writeTree(t, map[string]string{
		"recipes/frozen/kustomization.yaml": "images:\n" +
			"  # pin-frozen: #704 — portability case study (review: 2027-02-01)\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
		"recipes/frozen/README.md": "This recipe pins `v1.0.0`.\n",
	})

	want := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	sites := mustSites(t, tr, root)
	if len(sites) != 2 {
		t.Fatalf("found %d sites, want 2 (the manifest and the README)", len(sites))
	}
	for _, s := range sites {
		if !s.Frozen {
			t.Fatalf("%s: not frozen", s.Path)
		}
		if !s.FrozenReview.Equal(want) {
			t.Errorf("%s: FrozenReview = %v, want %v", s.Path, s.FrozenReview, want)
		}
	}
}

// TestSites_TheReviewClauseIsAcceptedWithoutParentheses. The date is the
// requirement; the punctuation around it is not, and a rule that
// rejected a reason ending "…, review: 2027-02-01" would be a rule
// people fight rather than follow.
func TestSites_TheReviewClauseIsAcceptedWithoutParentheses(t *testing.T) {
	t.Parallel()
	for _, marker := range []string{
		"# pin-frozen: #704 — case study (review: 2027-02-01)",
		"# pin-frozen: #704 — case study, review: 2027-02-01",
		"# pin-frozen: #704 — case study [Review: 2027-02-01]",
		"# pin-frozen: review:2027-02-01 — case study",
	} {
		t.Run(marker, func(t *testing.T) {
			t.Parallel()
			tr := testTracked("recipes")
			tr.Frozen = []string{"recipes/frozen"}
			root := writeTree(t, freezeTree(marker))
			sites := mustSites(t, tr, root)
			if len(sites) == 0 {
				t.Fatal("no sites")
			}
			want := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
			if !sites[0].FrozenReview.Equal(want) {
				t.Errorf("FrozenReview = %v, want %v", sites[0].FrozenReview, want)
			}
		})
	}
}

// TestSites_APlaceholderIsStillInertEvenWithADate: the worked example
// in a document that explains HOW to freeze must not be an instance of
// one, and adding a date to the example must not change that.
func TestSites_APlaceholderIsStillInertEvenWithADate(t *testing.T) {
	t.Parallel()
	tr := testTracked("recipes")
	root := writeTree(t, freezeTree("# pin-frozen: <why> (review: <date>)"))
	for _, s := range mustSites(t, tr, root) {
		if s.Frozen {
			t.Errorf("%s froze on a placeholder marker", s.Path)
		}
	}
}

// TestReviewLapsed pins the boundary. A review date is a day somebody
// wrote in a comment, not an instant: "2027-02-01" means the whole of
// that day, so the freeze goes overdue on the 2nd. Comparing instants
// would call it overdue from one second past midnight in a timezone
// nobody chose.
func TestReviewLapsed(t *testing.T) {
	t.Parallel()
	review := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		review time.Time
		now    time.Time
		want   bool
	}{
		{"well before", review, time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC), false},
		{"the day before", review, time.Date(2027, 1, 31, 23, 59, 59, 0, time.UTC), false},
		{"first moment of the review day", review,
			time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC), false},
		{"last moment of the review day", review,
			time.Date(2027, 2, 1, 23, 59, 59, 0, time.UTC), false},
		{"the day after", review, time.Date(2027, 2, 2, 0, 0, 1, 0, time.UTC), true},
		{"long after", review, time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC), true},
		// A zero date cannot occur on a frozen site — Sites() rejects one
		// — and defaulting the unset case to "overdue" would turn a future
		// caller's oversight into a red scheduled job every Tuesday.
		{"unset never lapses", time.Time{}, time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ReviewLapsed(tc.review, tc.now); got != tc.want {
				t.Errorf("ReviewLapsed(%v, %v) = %v, want %v",
					tc.review.Format(reviewDateLayout), tc.now, got, tc.want)
			}
		})
	}
}

// TestFreezeOverdue_OnlyAppliesToAFrozenSite: a live pin has no review
// date and must never be reported as an overdue freeze.
func TestFreezeOverdue_OnlyAppliesToAFrozenSite(t *testing.T) {
	t.Parallel()
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	if (Site{Frozen: false, FrozenReview: past}).FreezeOverdue(now) {
		t.Error("a non-frozen site reported an overdue freeze")
	}
	if !(Site{Frozen: true, FrozenReview: past}).FreezeOverdue(now) {
		t.Error("a frozen site past its review date did not report overdue")
	}
}
