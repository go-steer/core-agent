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

// The pull-request body.
//
// A bump PR that says only "v0.11.0 → v0.21.0" is a PR that gets
// rubber-stamped, and rubber-stamping is how the pin went ten releases
// stale in the first place. So the body carries the intervening release
// notes IN FULL and points at the three things that have actually
// broken this recipe before.

package main

import (
	"fmt"
	"strings"

	"github.com/go-steer/core-agent/v2/internal/imagepin"
)

// maxBodyBytes keeps the assembled body inside GitHub's 65,536-byte
// limit on a pull-request body, with room for the workflow's own
// additions. A body over the limit is rejected outright, which would
// turn a long changelog delta into no pull request at all.
const maxBodyBytes = 58000

// riskWords mark release-note lines worth reading twice. Each earned
// its place from something that has actually gone wrong with this
// dependency, not from a generic list of scary words.
var riskWords = []string{
	"break", "rename", "remove", "removi", "drop", "delet",
	"rbac", "clusterrole", "role", "permission", "requirement",
	"flag", "default", "deprecat", "migrat", "incompatib",
}

// pullRequestBody assembles the review material for a bump.
func pullRequestBody(t *imagepin.Tracked, upstream upstreamState, r scanResult) string {
	var b strings.Builder
	from := strings.Join(r.currentTags(), ", ")
	if from == "" {
		from = "(nothing)"
	}
	fmt.Fprintf(&b, "Bumps the pinned %s watcher image from %s to `%s`.\n\n",
		t.Family.Names()[0], "`"+strings.ReplaceAll(from, ", ", "`, `")+"`", upstream.Target.Tag)
	fmt.Fprintf(&b, "Upstream: https://github.com/%s\n\n", t.UpstreamRepo)

	writeSiteSections(&b, r)
	delta := releaseDelta(upstream, r)
	writeChecklist(&b, upstream, delta)
	writeReleaseNotes(&b, t, delta, maxBodyBytes-b.Len()-adversarialLen)
	b.WriteString(adversarialSection(upstream.Target.Tag, r))
	return b.String()
}

// writeSiteSections lists what moved and what deliberately did not.
func writeSiteSections(b *strings.Builder, r scanResult) {
	b.WriteString("## What changed\n\n")
	for _, s := range r.stale {
		fmt.Fprintf(b, "- `%s:%d` — %s, was `%s`\n", s.Path, s.Line, s.Kind, s.Tag)
	}
	if len(r.frozen) > 0 {
		b.WriteString("\n## Left alone (frozen)\n\n")
		b.WriteString("These belong to a recipe named in `Tracked.Frozen` " +
			"(`internal/imagepin/lookout.go`) that also carries a `pin-frozen:` marker on the " +
			"pin itself, so they are not drift and were not touched. Each marker names the date " +
			"the freeze should be revisited; the weekly job reports a lapse, it never bumps a " +
			"frozen pin:\n\n")
		for _, s := range r.frozen {
			fmt.Fprintf(b, "- `%s:%d` — still `%s` (%s)\n", s.Path, s.Line, s.Tag, s.FrozenReason)
		}
	}
	b.WriteString("\n")
}

// releaseDelta is every release from just after the oldest tag the tree
// currently carries, through the target, newest first.
//
// It keys on the OLDEST current tag rather than the newest because a
// tree that disagrees with itself — which is the state this tool exists
// to catch — has a reader somewhere running the older one.
func releaseDelta(upstream upstreamState, r scanResult) []Release {
	tags := r.currentTags()
	floor, haveFloor := version{}, false
	if len(tags) > 0 {
		floor, haveFloor = parseTag(tags[0])
	}
	target, _ := parseTag(upstream.Target.Tag)
	var out []Release
	for _, rel := range upstream.Releases {
		v, ok := parseTag(rel.Tag)
		if !ok || target.less(v) {
			continue
		}
		if haveFloor && !floor.less(v) {
			continue
		}
		out = append(out, rel)
	}
	return out
}

// writeChecklist names the three failure modes this dependency has
// actually produced, then points at the release-note lines that mention
// anything like them.
func writeChecklist(b *strings.Builder, upstream upstreamState, delta []Release) {
	b.WriteString("## Review checklist\n\n")
	b.WriteString("The job verified that the rewrite is complete and self-consistent. " +
		"It cannot verify that the new release is compatible with this recipe. " +
		"Before merging, read the notes below for:\n\n")
	b.WriteString("- [ ] **Breaking renames.** The watcher has renamed its binary, its " +
		"container args and its config keys across releases. The manifests in " +
		"`examples/gke-troubleshoot-agent/deploy/` name all three.\n")
	b.WriteString("- [ ] **RBAC requirement changes.** The watcher declares what it needs via " +
		"`LoadClusterListRequirements()`; the recipe's ClusterRole is asserted against that list " +
		"by `TestWatcherRBACMatchesEnrichLists`. A new enrichment list means a new rule.\n")
	b.WriteString("- [ ] **Flag defaults.** A default that flips upstream changes what this " +
		"recipe deploys without any diff here saying so.\n")
	b.WriteString("- [ ] **The kind e2e passed on this pull request.** It is what actually pulls " +
		"the new image and runs the recipe against it.\n")
	for _, s := range upstream.Skipped {
		fmt.Fprintf(b, "- [ ] **`%s` was skipped**: it is released upstream but its image was "+
			"not pullable when this ran. If that is still true, upstream has a broken release.\n",
			s.Tag)
	}
	if lines := riskLines(delta); len(lines) > 0 {
		b.WriteString("\nLines from the notes that mention a rename, a permission, a flag or a " +
			"removal:\n\n")
		for _, l := range lines {
			fmt.Fprintf(b, "- %s\n", l)
		}
	}
	b.WriteString("\n")
}

// maxRiskLines bounds the highlight list. Past a couple of dozen it
// stops being a highlight and becomes the changelog again.
const maxRiskLines = 24

// riskLines pulls the release-note lines that mention a risk word.
func riskLines(delta []Release) []string {
	var out []string
	for _, rel := range delta {
		for _, raw := range strings.Split(rel.Body, "\n") {
			// Strip the note's own bullet marker; these lines are
			// re-bulleted below and a doubled dash reads as a typo.
			line := strings.TrimLeft(strings.TrimSpace(raw), "-*• ")
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			lower := strings.ToLower(line)
			hit := false
			for _, w := range riskWords {
				if strings.Contains(lower, w) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			if len(line) > 300 {
				line = line[:300] + "…"
			}
			out = append(out, fmt.Sprintf("`%s` — %s", rel.Tag, line))
			if len(out) >= maxRiskLines {
				return append(out, "_(more matches; read the full notes below)_")
			}
		}
	}
	return out
}

// writeReleaseNotes reproduces the intervening notes, newest first,
// within the byte budget it is given.
func writeReleaseNotes(b *strings.Builder, t *imagepin.Tracked, delta []Release, budget int) {
	b.WriteString("## Upstream release notes\n\n")
	if len(delta) == 0 {
		b.WriteString("_Upstream published no release notes between the pinned tag and this " +
			"one._\n\n")
		return
	}
	for _, rel := range delta {
		var section strings.Builder
		title := rel.Name
		if title == "" || title == rel.Tag {
			title = rel.Tag
		} else {
			title = rel.Tag + " — " + title
		}
		fmt.Fprintf(&section, "### %s\n\n", title)
		if rel.URL != "" {
			fmt.Fprintf(&section, "%s\n\n", rel.URL)
		}
		body := strings.TrimSpace(rel.Body)
		if body == "" {
			body = "_(no notes)_"
		}
		section.WriteString(body)
		section.WriteString("\n\n")
		if section.Len() > budget {
			fmt.Fprintf(b, "_Truncated: the remaining notes did not fit in a pull-request "+
				"body. Read them at https://github.com/%s/releases._\n\n", t.UpstreamRepo)
			return
		}
		budget -= section.Len()
		b.WriteString(section.String())
	}
}

// adversarialLen is a conservative reservation for the section below,
// so the notes never crowd it out. review-gate.yml requires it, and a
// body that dropped it would fail a required check.
const adversarialLen = 1800

// adversarialSection satisfies review-gate.yml, honestly: it says what
// the job checked and, more usefully, what it structurally cannot.
//
// Every claim here is written at REWRITE time, which is before the
// workflow's own verification steps run. So it states what the tool
// itself established and names the later steps as steps, rather than
// reporting results it has not seen. A generated body that overclaims
// is worse than a terse one: it is the reviewer's only summary, and the
// point of this PR is that the reviewer does the reading.
func adversarialSection(newTag string, r scanResult) string {
	return fmt.Sprintf(`## Adversarial review

Opened by the weekly `+"`lookout-pin-check`"+` job, so the adversarial pass is the
reviewer's rather than the author's. What the job established before opening this:

- %d of the %d non-frozen declarations needed a rewrite (%d already read `+"`%s`"+`). The
  tree was then re-scanned from scratch, and all %d read back as `+"`%s`"+` — counted as
  well as read, so a rewrite that made a site undiscoverable fails too. A rewrite
  that misses a site is reverted rather than pushed.
- The %d frozen declarations were re-scanned and are byte-for-byte where they were.
  Each is frozen by name in `+"`internal/imagepin/lookout.go`"+` AND by a `+"`pin-frozen:`"+`
  marker on the pin; the two disagreeing is an error, not an exemption.
- `+"`dev/ci/presubmits/lint-go`"+` and `+"`dev/ci/presubmits/test-unit`"+` run against the
  rewritten tree in the job step after this body was written, and the job fails
  without opening a PR if either goes red. Their status on this PR is the check
  list below the diff, not this paragraph.

What none of that asserts, and what review is for: that this release is compatible
with the recipe. The checklist above is the specific form that question takes here.
`, len(r.stale), len(r.stale)+len(r.current), len(r.current), newTag,
		len(r.stale)+len(r.current), newTag, len(r.frozen))
}
