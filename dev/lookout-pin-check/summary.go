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

// The run report, in markdown, for $GITHUB_STEP_SUMMARY.
//
// This exists because of what the job used to say on a clean week:
// nothing. Every non-frozen pin current meant `drift=false`, no pull
// request, and a frozen recipe sitting further behind upstream every
// Tuesday with its skew visible only in the stderr of a green scheduled
// run — which is to say, nowhere (#791). A summary is written on EVERY
// run, drift or not, and it always lists the freezes.
//
// It is a report, not a gate. Nothing here decides anything: the drift
// verdict is --check's, and the review verdict is --check-freezes's.

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-steer/core-agent/v2/internal/imagepin"
)

// appendSummary writes the markdown report to path.
//
// APPEND, not truncate, because the path this is pointed at in CI is
// $GITHUB_STEP_SUMMARY — a file several steps of the same job may write
// to, where a truncating writer silently eats whatever ran before it.
func appendSummary(path string, t *imagepin.Tracked, upstream upstreamState, r scanResult,
	now time.Time) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // operator-supplied path
	if err != nil {
		return err
	}
	if _, err := f.WriteString(summaryMarkdown(t, upstream, r, now)); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// summaryMarkdown renders the report.
func summaryMarkdown(t *imagepin.Tracked, upstream upstreamState, r scanResult,
	now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## lookout pin check\n\n")
	fmt.Fprintf(&b, "Upstream [`%s`](https://github.com/%s) latest release with a pullable image: **`%s`**.\n\n",
		t.UpstreamRepo, t.UpstreamRepo, upstream.Target.Tag)
	fmt.Fprintf(&b, "Scanned **%d** declaration(s): %d stale, %d current, %d frozen.\n\n",
		len(r.all), len(r.stale), len(r.current), len(r.frozen))

	switch {
	case len(r.stale) > 0:
		fmt.Fprintf(&b, "### Drift\n\n")
		fmt.Fprintf(&b, "%d pin(s) are behind and will be rewritten to `%s`:\n\n",
			len(r.stale), upstream.Target.Tag)
		fmt.Fprintf(&b, "| Site | Kind | Pinned |\n| --- | --- | --- |\n")
		for _, s := range r.stale {
			fmt.Fprintf(&b, "| `%s:%d` | %s | `%s` |\n", s.Path, s.Line, s.Kind, s.Tag)
		}
		b.WriteString("\n")
	default:
		fmt.Fprintf(&b, "No drift: every pin that tracks upstream is at `%s`.\n\n",
			upstream.Target.Tag)
	}

	groups := r.freezeGroups()
	if len(groups) == 0 {
		// Said out loud rather than omitted. "No frozen pins" and "the
		// freeze section was left out this week" look identical on a page
		// nobody reads closely, and only one of them is good news.
		b.WriteString("### Frozen pins\n\nNone: no recipe is opted out of the weekly bump.\n\n")
		return b.String()
	}

	b.WriteString("### Frozen pins\n\n")
	b.WriteString("These do not track upstream. Nothing bumps them; the review date is when " +
		"someone should ask whether the freeze still holds.\n\n")
	b.WriteString("| Recipe | Pinned | Behind | Review by | Status | Reason |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, g := range groups {
		status := "ok"
		if g.overdue(now) {
			status = "**OVERDUE**"
		}
		fmt.Fprintf(&b, "| `%s` (%d site(s)) | %s | %s | %s | %s | %s |\n",
			g.group, g.sites, codeList(g.tags), behind(upstream, g.tags),
			g.review.Format(reviewDateLayout), status, escapePipes(g.reason))
	}
	b.WriteString("\n")

	if overdue := r.overdueFreezes(now); len(overdue) > 0 {
		fmt.Fprintf(&b, "> **%d freeze(s) are past their review date.** A freeze is a decision "+
			"with a shelf life, not a permanent exemption. Either move the `review:` date on in "+
			"the `pin-frozen:` marker, or drop the recipe from `Tracked.Frozen` in "+
			"`internal/imagepin/lookout.go` and let it track upstream again.\n\n", len(overdue))
	}
	return b.String()
}

// behind counts how many upstream releases sit above a frozen pin —
// the number that says whether "frozen" has quietly become
// "unmaintained".
//
// Reported rather than acted on. There is no threshold here and
// deliberately so: nobody can yet say whether five releases behind is
// fine or alarming for a case study, and a gate built on a number
// guessed today is a gate someone disables next quarter. The count is
// the evidence that would let that threshold be chosen later.
func behind(upstream upstreamState, tags []string) string {
	oldest, ok := oldestTag(tags)
	if !ok {
		return "?"
	}
	var n int
	for _, rel := range upstream.Releases {
		v, parsed := parseTag(rel.Tag)
		if parsed && oldest.less(v) {
			n++
		}
	}
	if n == 0 {
		return "current"
	}
	return strconv.Itoa(n) + " release(s)"
}

// oldestTag is the furthest-behind tag in a freeze group, which is the
// one the distance should be measured from.
func oldestTag(tags []string) (version, bool) {
	var out version
	var found bool
	for _, tag := range tags {
		v, ok := parseTag(tag)
		if !ok {
			continue
		}
		if !found || v.less(out) {
			out, found = v, true
		}
	}
	return out, found
}

func codeList(tags []string) string {
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		out = append(out, "`"+tag+"`")
	}
	return strings.Join(out, ", ")
}

// escapePipes keeps a reason containing a `|` from splitting the table
// cell it lives in. A freeze reason is free prose written in a YAML
// comment, so it can contain anything.
func escapePipes(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}
