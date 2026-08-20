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

// Keeps this repo's pin of the upstream lookout watcher image current.
//
// The gke-troubleshoot-agent recipe deploys ghcr.io/go-steer/lookout
// alongside the daemon, and the tag is written down in eleven places:
// a base Deployment, two kustomize overlays, the e2e harness, a Go test
// constant, and five documents. Nothing detected when that pin went
// stale. It sat at v0.11.0 for ten upstream releases, straight through
// a breaking rename, and what finally caught it was a person reading a
// manifest. This tool is the thing that should have caught it (#787).
//
// Run weekly by .github/workflows/lookout-pin-check.yml, which opens a
// pull request carrying the rewrite plus upstream's release notes.
//
// Usage:
//
//	# Ask "is the pin stale?" without writing anything:
//	go run ./dev/lookout-pin-check --check
//
//	# Rewrite every non-frozen site to the current upstream release:
//	go run ./dev/lookout-pin-check
//
//	# Both of the above against a captured release list, no network:
//	go run ./dev/lookout-pin-check --check --releases=/tmp/releases.json
//
//	# Rewrite, and leave a pull-request body on disk:
//	go run ./dev/lookout-pin-check --pr-body=/tmp/body.md
//
//	# Ask once, act on that same answer (what the workflow does):
//	go run ./dev/lookout-pin-check --check --resolved=/tmp/upstream.json
//	go run ./dev/lookout-pin-check --releases=/tmp/upstream.json
//
//	# Has a frozen pin outlived its review date?
//	go run ./dev/lookout-pin-check --check-freezes --releases=/tmp/upstream.json
//
//	# Leave a markdown report where a human will see it (any mode):
//	go run ./dev/lookout-pin-check --check --summary="$GITHUB_STEP_SUMMARY"
//
// # The verdicts report on stdout, not through the exit code
//
// --check prints exactly one line on stdout, `drift=true` or
// `drift=false`, and exits 0 either way. --check-freezes is the same
// shape for the second question: `freeze-review=ok` or
// `freeze-review=overdue`, one line, exit 0. Everything a human reads
// goes to stderr, or to --summary. A non-zero exit ALWAYS means the tool
// itself failed — unreachable API, unreadable tree, a declaration that
// no longer matches anything.
//
// The exit code cannot carry the verdict because the caller runs this
// through `go run`, and `go run` collapses every non-zero child status
// to 1. An exit-code convention would make "GitHub timed out"
// indistinguishable from "upstream shipped v0.22.0", and the weekly job
// would open a pull request every time the network hiccuped. Same
// reasoning, same shape, as dev/regen-builtin-pricing.
//
// # Two questions, two gates
//
// There is a second check reading these same declaration sites:
// examples/internal/recipecheck (#680) asks "can this daemon run this
// recipe's config?". They are not merged and must not be. That one is
// offline, in-tree, and runs in test-unit on every pull request; this
// one has to call another repo, so it runs weekly and out of band. The
// shared half — where is this image declared, and what tag does the
// declaration carry — lives in internal/imagepin, parameterised by
// image family, so there is exactly one pin walker in the repo.
//
// # A freeze has a shelf life
//
// A recipe can opt out of the bump: it is named in Tracked.Frozen and
// each of its pins carries a `pin-frozen: <why> (review: YYYY-MM-DD)`
// marker. Both halves are required, and so is the date.
//
// The date exists because "frozen" used to mean "forever, silently"
// (#791). On a week where every live pin was current, this job exited 0
// with drift=false and said nothing at all, while a frozen recipe fell
// another release behind — the shape of #787's original failure, a pin
// nobody was told about, differing only in that somebody meant it once.
// Two things close that: --summary reports the freezes on EVERY run,
// including clean ones, and --check-freezes goes overdue when the date
// passes.
//
// A lapsed review never bumps anything and never opens a pull request.
// It cannot: whether a case study should start tracking upstream again
// is a question about what the study is for, which no rewrite answers.
// The workflow runs the freeze check LAST, after the bump PR, so an
// overdue review can never block the mechanism that keeps the other
// pins current.
//
// # Scope: the watcher image only
//
// This tool tracks ghcr.io/go-steer/lookout and nothing else. The
// core-agent daemon images are deliberately out of scope: they are
// OURS, their tags follow this repo's own release cadence rather than
// an upstream feed, and recipecheck already holds them to the released
// set. Mixing a mechanical third-party bump into that would make one
// pull request answer two unrelated questions. The walker is
// parameterised by imagepin.Family precisely so that decision stays a
// decision — adding a second tracked family is a declaration next to
// imagepin.Lookout plus a resolver, not a rewrite of any walking code.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/go-steer/core-agent/v2/internal/imagepin"
)

func main() {
	root := flag.String("root", ".", "repository root to scan and rewrite")
	check := flag.Bool("check", false,
		"write nothing; print drift=true/drift=false on stdout depending on whether any "+
			"non-frozen pin differs from upstream's current release")
	releasesPath := flag.String("releases", "",
		"path to a captured releases JSON snapshot to use instead of calling GitHub; "+
			"accepts a raw `gh api repos/OWNER/REPO/releases` array")
	resolvedPath := flag.String("resolved", "",
		"path to write the upstream resolution as a snapshot --releases can replay, so a "+
			"later run reasons about the same release this one did")
	prBody := flag.String("pr-body", "",
		"path to write a pull-request body describing the bump (rewrite mode only)")
	checkFreezes := flag.Bool("check-freezes", false,
		"write nothing; print freeze-review=ok/freeze-review=overdue on stdout depending on "+
			"whether any frozen pin has outlived the review date its marker committed to")
	summaryPath := flag.String("summary", "",
		"path to APPEND a markdown report to, in any mode — the frozen pins and their review "+
			"dates included, whether or not anything drifted (point it at $GITHUB_STEP_SUMMARY)")
	flag.Parse()

	if *check && *prBody != "" {
		die("--check writes nothing, so --pr-body has nothing to describe")
	}
	// One question per invocation. Both modes own stdout entirely, and a
	// caller reading two verdicts off one stream could not tell which
	// line answered which question.
	if *check && *checkFreezes {
		die("--check and --check-freezes each answer on stdout; run one at a time")
	}
	if *checkFreezes && *prBody != "" {
		die("--check-freezes writes nothing, so --pr-body has nothing to describe")
	}
	absRoot, err := filepath.Abs(*root)
	if err != nil {
		die("resolve --root: %v", err)
	}

	tracked := imagepin.Lookout
	sites, err := tracked.Sites(absRoot)
	if err != nil {
		die("scan %s: %v", absRoot, err)
	}
	// Finding nothing is a broken tool, not a clean tree. Every one of
	// the declarations below is load-bearing, so "no sites" means the
	// scan roots or the matchers stopped matching reality — and a gate
	// that answers drift=false when it has gone blind is worse than no
	// gate at all.
	if len(sites) == 0 {
		die("found no %s declaration anywhere under %s — the scan roots or the matchers in "+
			"internal/imagepin/lookout.go have gone stale; refusing to report a verdict",
			tracked.Family.Names()[0], absRoot)
	}

	resolver, err := resolverFor(tracked, *releasesPath)
	if err != nil {
		die("%v", err)
	}
	upstream, err := resolveUpstream(context.Background(), resolver)
	if err != nil {
		die("resolve upstream %s: %v", tracked.UpstreamRepo, err)
	}
	if *resolvedPath != "" {
		if err := writeSnapshot(*resolvedPath, upstream); err != nil {
			die("write --resolved %s: %v", *resolvedPath, err)
		}
	}

	now := time.Now().UTC()
	result := classify(sites, upstream.Target.Tag)
	report(tracked, upstream, result, now)

	// Written before either verdict returns, and in rewrite mode too, so
	// the frozen pins are visible on EVERY run. That is the point of the
	// file: a tree whose live pins are all current used to say nothing at
	// all, and a freeze nobody is reminded of is a freeze nobody revisits
	// (#791).
	if *summaryPath != "" {
		if err := appendSummary(*summaryPath, tracked, upstream, result, now); err != nil {
			die("write --summary %s: %v", *summaryPath, err)
		}
	}

	if *checkFreezes {
		fmt.Println(freezeVerdict(result, now))
		return
	}
	if *check {
		fmt.Println(verdict(result))
		return
	}
	if len(result.stale) == 0 {
		return
	}
	if err := rewrite(absRoot, tracked, result, upstream.Target.Tag); err != nil {
		die("%v", err)
	}
	if *prBody != "" {
		body := pullRequestBody(tracked, upstream, result)
		if err := os.WriteFile(*prBody, []byte(body), 0o600); err != nil {
			die("write --pr-body %s: %v", *prBody, err)
		}
	}
	// The tag rides on stdout so the workflow can title the pull
	// request without parsing prose. --check never prints it: that
	// mode's stdout is the verdict and nothing else.
	fmt.Printf("tag=%s\n", upstream.Target.Tag)
}

// resolverFor picks the live resolver or the offline replay.
func resolverFor(t *imagepin.Tracked, releasesPath string) (Resolver, error) {
	if releasesPath == "" {
		return newGitHubResolver(t.UpstreamRepo, t.Family.Names()[0], os.Getenv("GITHUB_TOKEN")), nil
	}
	snap, err := loadSnapshot(releasesPath)
	if err != nil {
		return nil, fmt.Errorf("load --releases: %w", err)
	}
	return newStubResolver(snap), nil
}

// scanResult is the tree's declarations, sorted into what the caller
// has to do about each.
type scanResult struct {
	all     []imagepin.Site
	stale   []imagepin.Site // non-frozen and not at the target tag
	current []imagepin.Site // non-frozen and already at it
	frozen  []imagepin.Site // opted out, never rewritten, never drift
}

// classify sorts every discovered site against the target tag.
//
// Drift is per-site, with no requirement that the tree agree with
// itself first. That is on purpose: the skew this tool exists to catch
// was a tree that DID disagree with itself, and a rule of the form
// "refuse unless every site already matches" would have declined to
// answer in exactly the case that mattered. Whole-tree agreement is a
// different property, already gated offline and per-pull-request by the
// recipe's own TestWatcherImagePinIsConsistent.
func classify(sites []imagepin.Site, targetTag string) scanResult {
	var out scanResult
	out.all = sites
	for _, s := range sites {
		switch {
		case s.Frozen:
			out.frozen = append(out.frozen, s)
		case s.Tag == targetTag:
			out.current = append(out.current, s)
		default:
			out.stale = append(out.stale, s)
		}
	}
	return out
}

// verdict is the whole of --check's stdout contract: one line, either
// `drift=true` or `drift=false`, and nothing else ever.
//
// A frozen site is never drift, no matter how far behind it is. That is
// design, not an oversight: examples/kube-platform-agent pins v0.18.0
// on purpose (#704), and a check that reported it forever would be a
// check people learn to ignore.
func verdict(r scanResult) string {
	if len(r.stale) > 0 {
		return "drift=true"
	}
	return "drift=false"
}

// freezeVerdict is --check-freezes's whole stdout contract: one line,
// either `freeze-review=ok` or `freeze-review=overdue`.
//
// It is a SECOND question, deliberately not folded into `drift`. A
// frozen pin must never be rewritten, so answering drift=true for one
// would send the weekly job into a rewrite that produces an empty diff
// and a pull request about nothing. And the two want different
// responses: drift opens a PR, a lapsed review asks a human whether the
// freeze still holds — a question no rewrite can answer.
//
// Like --check, it reports on stdout and exits 0. A non-zero exit still
// only ever means the tool itself failed.
func freezeVerdict(r scanResult, now time.Time) string {
	if len(r.overdueFreezes(now)) > 0 {
		return "freeze-review=overdue"
	}
	return "freeze-review=ok"
}

// overdueFreezes are the freeze groups whose review date has passed,
// sorted by group. One entry per recipe, not per site: the review is a
// decision about the recipe, and naming the same lapse three times is
// how a report becomes something people skim past.
func (r scanResult) overdueFreezes(now time.Time) []freezeGroup {
	var out []freezeGroup
	for _, g := range r.freezeGroups() {
		if g.overdue(now) {
			out = append(out, g)
		}
	}
	return out
}

// freezeGroup is one frozen recipe, folded from its sites.
type freezeGroup struct {
	group  string
	reason string
	review time.Time
	tags   []string
	sites  int
}

func (g freezeGroup) overdue(now time.Time) bool {
	return imagepin.ReviewLapsed(g.review, now)
}

// freezeGroups folds the frozen sites into one row per recipe, sorted by
// group, with the distinct tags each pins.
func (r scanResult) freezeGroups() []freezeGroup {
	byGroup := map[string]*freezeGroup{}
	var order []string
	for _, s := range r.frozen {
		g, ok := byGroup[s.Group]
		if !ok {
			g = &freezeGroup{group: s.Group, reason: s.FrozenReason, review: s.FrozenReview}
			byGroup[s.Group] = g
			order = append(order, s.Group)
		}
		g.sites++
		if !slices.Contains(g.tags, s.Tag) {
			g.tags = append(g.tags, s.Tag)
		}
	}
	sort.Strings(order)
	out := make([]freezeGroup, 0, len(order))
	for _, name := range order {
		out = append(out, *byGroup[name])
	}
	return out
}

// currentTags are the distinct tags the non-frozen sites carry, oldest
// first. More than one means the tree is internally inconsistent, which
// is worth saying out loud.
func (r scanResult) currentTags() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]imagepin.Site{}, r.stale...), r.current...) {
		if !seen[s.Tag] {
			seen[s.Tag] = true
			out = append(out, s.Tag)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, aok := parseTag(out[i])
		b, bok := parseTag(out[j])
		if aok && bok {
			return a.less(b)
		}
		return out[i] < out[j]
	})
	return out
}

// report writes the human-readable half to stderr.
func report(t *imagepin.Tracked, upstream upstreamState, r scanResult, now time.Time) {
	say("upstream %s: latest published release with a pullable image is %s",
		t.UpstreamRepo, upstream.Target.Tag)
	for _, s := range upstream.Skipped {
		say("  skipped %s: released upstream, but %s:%s is not pullable yet",
			s.Tag, t.Family.Names()[0], s.Tag)
	}
	say("scanned %d declaration(s): %d stale, %d current, %d frozen",
		len(r.all), len(r.stale), len(r.current), len(r.frozen))
	if tags := r.currentTags(); len(tags) > 1 {
		say("  NOTE: the non-frozen sites do not agree with each other (%s)",
			strings.Join(tags, ", "))
	}
	for _, s := range r.stale {
		say("  stale   %s:%d  %s %s → %s", s.Path, s.Line, s.Kind, s.Tag, upstream.Target.Tag)
	}
	for _, s := range r.frozen {
		say("  frozen  %s:%d  %s %s (%s)", s.Path, s.Line, s.Kind, s.Tag, s.FrozenReason)
	}
	for _, g := range r.overdueFreezes(now) {
		say("  OVERDUE %s is frozen at %s and its review date (%s) has passed — decide whether "+
			"the freeze still holds, then either move the date on or drop it from Tracked.Frozen",
			g.group, strings.Join(g.tags, ", "), g.review.Format(reviewDateLayout))
	}
}

// reviewDateLayout is the spelling a review date is written and reported
// in. Same layout imagepin parses; stated here so the report does not
// reach into that package for a format string.
const reviewDateLayout = "2006-01-02"

// rewrite applies the bump and then re-reads the tree to confirm it
// took, restoring every touched file if it did not.
//
// Verifying by RE-DISCOVERY rather than by trusting the splice is the
// whole point: the plan and the check share no code path beyond the
// scanner, so a rewrite that silently missed a site — a matcher that
// finds a tag it cannot locate a byte range for, say — is caught here
// rather than shipped in a pull request that looks complete.
func rewrite(root string, t *imagepin.Tracked, r scanResult, newTag string) error {
	plan, err := imagepin.PlanRewrite(root, r.stale, newTag)
	if err != nil {
		return err
	}
	if err := plan.Apply(root); err != nil {
		return fmt.Errorf("apply rewrite: %w", err)
	}
	if verr := verify(root, t, newTag, r); verr != nil {
		if rerr := plan.Revert(root); rerr != nil {
			return fmt.Errorf("%w\nAND the revert failed too, so the tree is now half-written: "+
				"%v\n`git checkout --` the paths above", verr, rerr)
		}
		return fmt.Errorf("%w\nthe tree was restored; nothing was left written", verr)
	}
	say("rewrote %d site(s) across %d file(s) to %s:",
		plan.Sites(), len(plan.Paths()), newTag)
	for _, p := range plan.Paths() {
		say("  %s", p)
	}
	return nil
}

// verify re-runs discovery over the rewritten tree.
func verify(root string, t *imagepin.Tracked, newTag string, before scanResult) error {
	after, err := t.Sites(root)
	if err != nil {
		return fmt.Errorf("re-scan after rewrite: %w", err)
	}
	var bad []string
	var live int
	for _, s := range after {
		if s.Frozen {
			continue
		}
		live++
		if s.Tag != newTag {
			bad = append(bad, fmt.Sprintf("%s:%d still reads %s", s.Path, s.Line, s.Tag))
		}
	}
	// Counting matters as much as reading. "Every site says the new tag"
	// is satisfied vacuously by a rewrite that made the sites
	// undiscoverable — a spliced-over quote, a tag shape the matchers
	// cannot see — and that is precisely the failure a reviewer would
	// not spot in the diff.
	if want := len(before.stale) + len(before.current); live != want {
		bad = append(bad, fmt.Sprintf(
			"%d non-frozen declaration(s) after the rewrite, %d before — the rewrite made "+
				"some of them unreadable", live, want))
	}
	if got, want := frozenKey(after), frozenKey(before.all); got != want {
		bad = append(bad, fmt.Sprintf("the frozen pins moved: %s → %s", want, got))
	}
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("the rewrite did not take cleanly:\n  %s", strings.Join(bad, "\n  "))
}

// frozenKey is a stable fingerprint of the frozen sites, so verify can
// assert they were left exactly as they were.
func frozenKey(sites []imagepin.Site) string {
	var out []string
	for _, s := range sites {
		if s.Frozen {
			out = append(out, fmt.Sprintf("%s:%d=%s", s.Path, s.Line, s.Tag))
		}
	}
	sort.Strings(out)
	return "[" + strings.Join(out, " ") + "]"
}

func say(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "lookout-pin-check: "+format+"\n", args...)
}

// die reports a tool failure. Any non-zero exit means "could not
// determine" — never "the pin is stale", which --check reports on
// stdout instead. See the --check note at the top of this file.
func die(format string, args ...any) {
	say(format, args...)
	os.Exit(1)
}
