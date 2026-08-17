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

// Site enumeration: every place in the tree that WRITES DOWN a tag for
// a tracked image family, with the byte range needed to rewrite it.
//
// This is the enumeration half described in the package doc. It walks
// the filesystem rather than the kustomize composition graph, because a
// bump has to reach the declarations an inherited pin hides, not the
// one an operator ends up with.

package imagepin

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SiteKind names the shape a tag was written in. It exists for
// reporting: a reviewer reading "prose" behaves differently from one
// reading "kustomize newTag".
type SiteKind string

const (
	// KindKustomizeTag is a `newTag:` under an `images:` entry.
	KindKustomizeTag SiteKind = "kustomize newTag"
	// KindImageRef is a literal `<image>:<tag>` anywhere in a file — a
	// container `image:`, a Dockerfile FROM, a shell default, or a
	// documented `crane digest` command.
	KindImageRef SiteKind = "image reference"
	// KindProse is an English statement of the pin ("the recipe pins
	// v0.21.0") that names no image.
	KindProse SiteKind = "prose"
	// KindLiteral is a bare tag located by a declared regexp, for the
	// sites no image-adjacent rule can reach — a Go test constant, say.
	KindLiteral SiteKind = "declared literal"
)

// Site is one written-down tag, located precisely enough to rewrite.
type Site struct {
	// Path is repo-relative and slash-separated.
	Path string
	// Line is 1-based, for messages.
	Line int
	// Tag is the tag as written.
	Tag string
	// Kind is the shape it was written in.
	Kind SiteKind
	// Text is the surrounding match, trimmed, for the report.
	Text string
	// Group is the unit freezing applies to: the recipe directory this
	// site belongs to, or the file's own path when it sits outside any
	// scanned root.
	Group string
	// Frozen is set on every site in a group where a pin declared
	// itself frozen. See [Tracked.FrozenMarker].
	Frozen bool
	// FrozenReason is the text the marker carried.
	FrozenReason string

	// start/end bound the TAG ITSELF within the file, so a rewrite
	// touches nothing else on the line.
	start, end int
}

func (s Site) String() string {
	return fmt.Sprintf("%s:%d: %s %s (%s)", s.Path, s.Line, s.Kind, s.Tag, s.Text)
}

// Literal declares a bare-tag site that no image-adjacent rule can
// find, because the file states the image name and the tag separately.
type Literal struct {
	// Path is repo-relative.
	Path string
	// Re must capture the tag in group 1.
	Re *regexp.Regexp
	// Why records what the site is, so a reader of the declaration does
	// not have to open the file to find out.
	Why string
}

// Tracked declares an image family this repo keeps current against
// upstream, and everywhere a tag for it is written down.
//
// Adding a second tracked family is this declaration plus a resolver —
// not a change to any of the walking code above.
type Tracked struct {
	// Family is the image-name predicate every scan path is scoped by.
	Family *Family
	// UpstreamRepo is the "owner/name" the resolver asks for releases.
	UpstreamRepo string
	// Roots are repo-relative paths walked for declarations. A directory
	// is walked whole and a site's freeze group is its immediate
	// subdirectory of that root — the recipe directory; a single file is
	// scanned as itself and is its own freeze group.
	//
	// Naming one file is not a special case bolted on: a deploy artifact
	// can live outside the tree it deploys, and the e2e harness that
	// pins the watcher image sits in dev/tools precisely because it is
	// CI's, not the recipe's.
	Roots []string
	// Docs are repo-relative documents that state the pin to a reader.
	// They are read for image references AND for [Tracked.ProseRe].
	//
	// This list is DECLARED rather than discovered because the prose
	// matchers are deliberately narrow, and a discovered list would
	// have to be "every markdown file", which sweeps in the changelog
	// and every historical version mention in it.
	//
	// Non-vacuity — "each of these still states the pin at least once"
	// — is NOT enforced here. That is an offline, in-tree property, and
	// it is already gated in test-unit by the recipe's own
	// TestWatcherTagInDocsIsCurrent, which reads this same list.
	Docs []string
	// Literals are the declared bare-tag sites.
	Literals []Literal
	// ImageRefRe matches a full image reference with the tag captured
	// in group 1. It is the narrow, assertion-facing sibling of the
	// family's internal discovery regexp: discovery deliberately also
	// matches `:latest` and `:${VAR}` refs so it can SEE them and skip
	// them on purpose, while this one only ever matches a real tag.
	// Exported so the recipe's own offline test asserts against the
	// same matcher the rewriter uses.
	ImageRefRe *regexp.Regexp
	// ProseRe matches an English statement of the pin, capturing the
	// tag in group 1. Applied ONLY to Docs.
	ProseRe *regexp.Regexp
	// TagRe is the shape a rewritable tag has. A reference whose tag
	// fails it — `:latest`, `:${WATCHER_IMAGE##*:}`, a digest — is not
	// a site: there is nothing there to bump.
	TagRe *regexp.Regexp
	// Frozen names the freeze groups — recipe directories, exactly as
	// [freezeGroup] renders them — that deliberately do not track
	// upstream.
	//
	// This list is the SAFETY and [Tracked.FrozenMarker] is the
	// EXPLANATION, and both are required. A marker alone used to be
	// enough, and that was a hole of the same class this whole package
	// exists to close: the natural place to document how freezing works
	// is next to a pin, an adjacent comment is indistinguishable from an
	// attached one, and a tree that exempts itself by accident reports
	// drift=false forever while going stale. So the two disagreeing is
	// an ERROR in both directions — a marker outside a declared group,
	// and a declared group with no marker — never a silent exemption.
	Frozen []string
	// FrozenMarker matches the opt-out comment, capturing the reason in
	// group 1. It must match a whole comment line. A marker with no
	// reason, or one whose reason is an angle-bracketed placeholder,
	// does not count: an unexplained freeze is how a gate rots, and a
	// `<why>` placeholder is a worked example rather than a decision.
	FrozenMarker *regexp.Regexp
}

// placeholderReason matches an angle-bracketed fill-in-the-blank.
//
// A document explaining HOW to freeze writes `pin-frozen: <reason>`. A
// real freeze never needs angle brackets, so rejecting them keeps the
// instructions for using this mechanism from being an instance of it.
var placeholderReason = regexp.MustCompile(`<[^<>]*>`)

// frozenIn adapts FrozenMarker to the marker-matching callback shape.
func (t *Tracked) frozenIn(comment string) (string, bool) {
	if t.FrozenMarker == nil {
		return "", false
	}
	m := t.FrozenMarker.FindStringSubmatch(comment)
	if m == nil {
		return "", false
	}
	reason := strings.TrimSpace(m[1])
	if reason == "" || placeholderReason.MatchString(reason) {
		return "", false
	}
	return reason, true
}

// Sites returns every declaration of t's tag under repoRoot, sorted by
// path then position.
//
// A declared Doc or Literal path that does not exist is skipped
// silently: the tool must still answer on an older tree (that is how
// the gate was demonstrated against the pre-#788 checkout), and the
// in-tree test is what fails when a live document stops stating its
// pin.
func (t *Tracked) Sites(repoRoot string) ([]Site, error) {
	files, err := t.scanTargets(repoRoot)
	if err != nil {
		return nil, err
	}
	var out []Site
	for _, f := range files {
		body, readErr := os.ReadFile(f.abs) //nolint:gosec // repo-relative walk
		if readErr != nil {
			return nil, fmt.Errorf("imagepin: %s: %w", f.rel, readErr)
		}
		found, sErr := t.sitesInFile(f, body)
		if sErr != nil {
			return nil, sErr
		}
		out = append(out, found...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].start < out[j].start
	})
	return t.resolveFreezes(out)
}

// scanTarget is one file to read, with the freeze group it belongs to
// and whether the prose matcher applies.
type scanTarget struct {
	abs, rel, group string
	doc             bool
	literals        []Literal
}

// scanTargets is the file set: everything textual under Roots, plus the
// declared Docs and Literal paths.
func (t *Tracked) scanTargets(repoRoot string) ([]scanTarget, error) {
	byRel := map[string]*scanTarget{}
	// add returns nil for anything that is not a readable file, which
	// covers a declared path that has been renamed or deleted. That is
	// deliberately not an error here: a stale Docs entry is caught by the
	// recipe's own tests (which assert each document states the tag),
	// where the failure names the document, rather than by this walker,
	// where it would only say "missing".
	add := func(rel string) *scanTarget {
		rel = filepath.ToSlash(rel)
		if got, ok := byRel[rel]; ok {
			return got
		}
		abs := filepath.Join(repoRoot, filepath.FromSlash(rel))
		// #nosec G703 -- rel comes from the Tracked declaration or from
		// walking repoRoot; both are checkout paths, not request input.
		if info, err := os.Stat(abs); err != nil || info.IsDir() {
			return nil
		}
		st := &scanTarget{abs: abs, rel: rel, group: rel}
		byRel[rel] = st
		return st
	}

	for _, root := range t.Roots {
		absRoot := filepath.Join(repoRoot, filepath.FromSlash(root))
		info, statErr := os.Stat(absRoot)
		if statErr != nil {
			continue
		}
		if !info.IsDir() {
			add(root)
			continue
		}
		err := filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				return relErr
			}
			st := add(filepath.ToSlash(rel))
			if st == nil {
				return nil
			}
			st.group = freezeGroup(root, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("imagepin: walk %s: %w", root, err)
		}
	}
	for _, doc := range t.Docs {
		if st := add(doc); st != nil {
			st.doc = true
		}
	}
	for _, lit := range t.Literals {
		if st := add(lit.Path); st != nil {
			st.literals = append(st.literals, lit)
		}
	}

	out := make([]scanTarget, 0, len(byRel))
	for _, st := range byRel {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out, nil
}

// freezeGroup is the immediate subdirectory of a scanned root — the
// RECIPE DIRECTORY a site belongs to, never the scan root itself.
//
// Freezing is a property of a recipe, not of a line: a recipe whose
// Deployment is frozen at an old release while its README tracks latest
// is incoherent, and its README carries no comment syntax to hold a
// marker of its own. The blast radius of one marker is therefore one
// recipe — and it is bounded a second time by [Tracked.Frozen], which
// has to name that recipe before the marker means anything at all.
func freezeGroup(root, rel string) string {
	rest := strings.TrimPrefix(rel, normalizeGroup(root)+"/")
	if i := strings.Index(rest, "/"); i >= 0 {
		return normalizeGroup(root) + "/" + rest[:i]
	}
	return normalizeGroup(rel)
}

// normalizeGroup puts a path in the one spelling group identity is
// compared in.
//
// Both sides go through it. [Tracked.Frozen] is written by hand, so it
// arrives with whatever a person typed — a trailing slash, a leading
// `./`, a doubled separator — while the walker's side is machine-built
// and already clean. Comparing the two raw is how an operator ends up
// reading `"examples/kube-platform-agent" is not in Tracked.Frozen`
// while looking straight at that string in the source, with the
// dead-declaration error that would explain it never printing because
// the stray check returns first.
func normalizeGroup(p string) string {
	p = filepath.ToSlash(p)
	if p == "" {
		return ""
	}
	return strings.TrimSuffix(path.Clean(p), "/")
}

// resolveFreezes reconciles the markers found in the tree against the
// declared frozen set, then propagates each surviving freeze across its
// recipe.
//
// Disagreement between the two is an error, in both directions. There
// is deliberately no way for this function to return a site that is
// exempt from the bump without someone having written the recipe down
// in Go AND written the reason next to the pin.
func (t *Tracked) resolveFreezes(sites []Site) ([]Site, error) {
	declared := map[string]bool{}
	for _, g := range t.Frozen {
		declared[normalizeGroup(g)] = true
	}
	reason, stray := partitionMarkers(sites, declared)
	if len(stray) > 0 {
		return nil, fmt.Errorf("imagepin: pin-frozen marker on a pin whose recipe is not "+
			"declared frozen:\n  %s\nA marker EXPLAINS a freeze; it does not grant one. Add the "+
			"recipe to Tracked.Frozen if the exemption is intended, or delete the marker — a "+
			"comment must never be able to exempt the pin below it on its own",
			strings.Join(stray, "\n  "))
	}
	if dead := undeclaredReasons(sites, declared, reason); len(dead) > 0 {
		return nil, fmt.Errorf("imagepin: declared frozen in Tracked.Frozen, but no usable "+
			"pin-frozen marker was found:\n  %s\nA frozen pin has to say why where an operator "+
			"reading the manifest will see it. Attach a marker to the pin itself (for a kustomize "+
			"newTag, the head or line comment of the images entry; for every other kind, the "+
			"contiguous # block immediately above the reference), give it a real reason rather "+
			"than a bracketed placeholder, or drop the recipe from Tracked.Frozen",
			strings.Join(dead, "\n  "))
	}
	for i := range sites {
		if why := reason[sites[i].Group]; why != "" {
			sites[i].Frozen = true
			sites[i].FrozenReason = why
		}
	}
	return sites, nil
}

// partitionMarkers splits the markers found into the reason each
// declared group gets, and the ones sitting where nothing declared them.
func partitionMarkers(sites []Site, declared map[string]bool) (map[string]string, []string) {
	reason := map[string]string{}
	var stray []string
	for _, s := range sites {
		if !s.Frozen {
			continue
		}
		if !declared[s.Group] {
			stray = append(stray, fmt.Sprintf("%s:%d — %q is not in Tracked.Frozen (reason "+
				"given: %q)", s.Path, s.Line, s.Group, s.FrozenReason))
			continue
		}
		if reason[s.Group] == "" {
			reason[s.Group] = s.FrozenReason
		}
	}
	return reason, stray
}

// undeclaredReasons names every declared-frozen group that no marker
// explained, distinguishing "the pins are there and say nothing" from
// "there are no pins here at all", because the fixes differ.
func undeclaredReasons(sites []Site, declared map[string]bool, reason map[string]string) []string {
	found := map[string]int{}
	for _, s := range sites {
		found[s.Group]++
	}
	var out []string
	for _, g := range sortedKeys(declared) {
		if reason[g] != "" {
			continue
		}
		if n := found[g]; n > 0 {
			out = append(out, fmt.Sprintf("%s — %d declaration(s), none carrying a marker", g, n))
			continue
		}
		out = append(out, fmt.Sprintf("%s — no declaration of this image found under it at all; "+
			"the recipe was renamed, deleted, or never pinned it", g))
	}
	return out
}

// sitesInFile applies every rule that fits one file, de-duplicating by
// byte offset so a kustomization's `newTag:` is not also reported by a
// literal-reference sweep.
func (t *Tracked) sitesInFile(f scanTarget, body []byte) ([]Site, error) {
	if !scannable(f, body) {
		return nil, nil
	}
	lines := lineStarts(body)
	seen := map[int]bool{}
	var out []Site
	keep := func(s Site) {
		if seen[s.start] {
			return
		}
		seen[s.start] = true
		s.Path, s.Group = f.rel, f.group
		s.Line = lineOf(lines, s.start)
		out = append(out, s)
	}

	// Go source is scanned for its declared literals and for NOTHING
	// else, not even a plain image reference. See [scannable] for why
	// the boundary is drawn here and what it costs.
	if isGoSource(f.rel) {
		for _, lit := range f.literals {
			for _, s := range t.literalSites(body, lit) {
				keep(s)
			}
		}
		return out, nil
	}

	if isKustomization(f.rel) {
		found, err := t.kustomizeSites(f.rel, body, lines)
		if err != nil {
			return nil, err
		}
		for _, s := range found {
			keep(s)
		}
	}
	for _, s := range t.refSites(body) {
		keep(s)
	}
	if f.doc && t.ProseRe != nil {
		for _, s := range t.proseSites(body) {
			keep(s)
		}
	}
	for _, lit := range f.literals {
		for _, s := range t.literalSites(body, lit) {
			keep(s)
		}
	}
	return out, nil
}

// scannable keeps the walk to text this repo could plausibly pin in.
// The shebang arm is load-bearing: the e2e harness that pins the
// watcher image lives at dev/tools/e2e-recipe-gke-troubleshoot-agent,
// with no extension at all.
//
// GO SOURCE IS A STATED BOUNDARY, not an oversight, and the rule is
// narrower than "declared or not". A .go file is scanned only if a
// [Literal] names it, and even then ONLY that literal's regexp is run
// over it — [Tracked.sitesInFile] returns before the image-reference
// and prose sweeps. So a lookout pin embedded in Go source anywhere
// outside a declared literal is NOT DISCOVERED, by design, including a
// perfectly ordinary `image: <ref>` in an embedded manifest.
//
// The reason is that Go source is not a deployment artifact, and a
// tag in it is usually half of a pair. A test states a tag twice: once
// in the fixture it feeds the code under test, once in the assertion
// it checks the answer against. A rewriter can see the first — a
// fixture containing a pod spec is a pod spec — and cannot see the
// second, which is usually a bare string. Bumping one and not the
// other leaves a green test asserting nothing, which is worse than a
// missed site and is exactly the silent-pass class this package
// exists to close. The same file will also hold deliberately historical
// tags (a pre-rename fixture, a floor mentioned in a comment) that a
// free-text sweep cannot tell apart from a live pin.
//
// The cost is real and is accepted: pin a deployable image from Go
// source and this walker will not find it. Declare a [Literal] for it,
// with a regexp narrow enough to name the one site meant, or move the
// pin into a manifest where it belongs.
func scannable(f scanTarget, body []byte) bool {
	if isGoSource(f.rel) {
		return len(f.literals) > 0
	}
	ext := strings.ToLower(filepath.Ext(f.rel))
	switch ext {
	case ".yaml", ".yml", ".md", ".json", ".sh", ".txt":
		return true
	}
	if classify(filepath.Base(f.rel)) != fileOther {
		return true
	}
	return strings.HasPrefix(string(body), "#!")
}

func isGoSource(rel string) bool {
	return strings.EqualFold(filepath.Ext(rel), ".go")
}

func isKustomization(rel string) bool {
	base := filepath.Base(rel)
	for _, name := range kustomizationNames {
		if base == name {
			return true
		}
	}
	return false
}

// kustomizeSites reads `newTag:` off the node tree, because the frozen
// marker is a comment and only the node tree knows which entry a
// comment is attached to.
func (t *Tracked) kustomizeSites(rel string, body []byte, lines []int) ([]Site, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("imagepin: %s: %w", rel, err)
	}
	var out []Site
	for _, entry := range imagesEntries(&root) {
		if !t.Family.Matches(scalarValue(mappingValue(entry, "name"))) {
			continue
		}
		tagNode := mappingValue(entry, "newTag")
		if tagNode == nil || tagNode.Kind != yaml.ScalarNode || !t.TagRe.MatchString(tagNode.Value) {
			continue
		}
		start, ok := scalarOffset(body, lines, tagNode)
		if !ok {
			return nil, fmt.Errorf("imagepin: %s: cannot locate newTag %q at line %d",
				rel, tagNode.Value, tagNode.Line)
		}
		why, frozen := t.frozenIn(entryComments(entry))
		out = append(out, Site{
			Kind: KindKustomizeTag, Tag: tagNode.Value, Text: "newTag: " + tagNode.Value,
			Frozen: frozen, FrozenReason: why,
			start: start, end: start + len(tagNode.Value),
		})
	}
	return out, nil
}

// entryComments joins every comment positionally attached to one images
// entry, so the frozen marker can sit above the entry, beside its
// `newTag:`, or above either key.
func entryComments(entry *yaml.Node) string {
	var b strings.Builder
	for _, n := range append([]*yaml.Node{entry}, entry.Content...) {
		for _, c := range []string{n.HeadComment, n.LineComment, n.FootComment} {
			if c != "" {
				b.WriteString(c)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// refSites finds every literal `<image>:<tag>` whose tag is rewritable.
func (t *Tracked) refSites(body []byte) []Site {
	var out []Site
	for _, loc := range t.Family.refRe.FindAllIndex(body, -1) {
		ref := string(body[loc[0]:loc[1]])
		image, tag, _ := SplitImageRef(ref)
		if !t.Family.Matches(image) || !t.TagRe.MatchString(tag) {
			continue
		}
		start := loc[1] - len(tag)
		why, frozen := t.frozenIn(CommentMarkerAboveText(body, loc[0]))
		out = append(out, Site{
			Kind: KindImageRef, Tag: tag, Text: ref,
			Frozen: frozen, FrozenReason: why,
			start: start, end: loc[1],
		})
	}
	return out
}

// CommentMarkerAboveText returns the contiguous block of `#` comment
// lines immediately above the byte at off, as one string.
//
// "Contiguous" is literal: a blank line ENDS the block. A comment
// separated from a pin by whitespace is a comment about something else,
// and reading through the gap is what lets a visually detached
// documentation block attach itself to the next pin down the file.
//
// off may point mid-line — a reference sits after `image: `, not at the
// margin — so the partial line it lands in is dropped before the walk
// upward. That is a capability this package's own reference scanner
// needs, not a fix for a defect: [CommentMarkerAbove]'s pre-existing
// callers all pass a line-start offset, where the two behave alike.
func CommentMarkerAboveText(body []byte, off int) string {
	lines := strings.Split(string(body[:off]), "\n")
	lines = lines[:len(lines)-1]
	var block []string
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || !strings.HasPrefix(line, "#") {
			break
		}
		block = append(block, line)
	}
	return strings.Join(block, "\n")
}

// proseSites finds English statements of the pin.
func (t *Tracked) proseSites(body []byte) []Site {
	var out []Site
	for _, loc := range t.ProseRe.FindAllSubmatchIndex(body, -1) {
		tag := string(body[loc[2]:loc[3]])
		if !t.TagRe.MatchString(tag) {
			continue
		}
		out = append(out, Site{
			Kind: KindProse, Tag: tag, Text: string(body[loc[0]:loc[1]]),
			start: loc[2], end: loc[3],
		})
	}
	return out
}

// literalSites finds the declared bare-tag sites.
func (t *Tracked) literalSites(body []byte, lit Literal) []Site {
	var out []Site
	for _, loc := range lit.Re.FindAllSubmatchIndex(body, -1) {
		tag := string(body[loc[2]:loc[3]])
		if !t.TagRe.MatchString(tag) {
			continue
		}
		out = append(out, Site{
			Kind: KindLiteral, Tag: tag, Text: lit.Why,
			start: loc[2], end: loc[3],
		})
	}
	return out
}

// patchedFile is one file's before and after, plus the mode to write
// it back with.
type patchedFile struct {
	old, patched []byte
	mode         os.FileMode
}

// RewritePlan is a validated set of edits that has not been applied.
//
// Planning and applying are separate because a PARTIAL rewrite is worse
// than none: a tree where the Deployment moved and the README did not
// is a tree that lies to its reader, and the in-tree consistency test
// would then fail on a change nobody can explain. Every site is located
// and checked before the first byte is written, and [RewritePlan.Revert]
// puts the originals back if verification of the finished tree fails.
type RewritePlan struct {
	newTag string
	files  map[string]patchedFile
	sites  int
}

// NewTag is the tag every planned site will be set to.
func (p *RewritePlan) NewTag() string { return p.newTag }

// Sites is how many individual tags the plan rewrites.
func (p *RewritePlan) Sites() int { return p.sites }

// Paths are the repo-relative files the plan touches, sorted.
func (p *RewritePlan) Paths() []string {
	out := make([]string, 0, len(p.files))
	for path := range p.files {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// PlanRewrite computes the edit that sets every site in the slice to
// newTag, without touching the disk.
//
// Callers must have filtered out frozen sites first; the plan does not
// second-guess the selection. Splices run back to front within a file
// so earlier offsets stay valid.
func PlanRewrite(repoRoot string, sites []Site, newTag string) (*RewritePlan, error) {
	byFile := map[string][]Site{}
	for _, s := range sites {
		byFile[s.Path] = append(byFile[s.Path], s)
	}
	plan := &RewritePlan{newTag: newTag, files: map[string]patchedFile{}, sites: len(sites)}
	for _, path := range sortedKeys(byFile) {
		abs := filepath.Join(repoRoot, filepath.FromSlash(path))
		info, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("imagepin: %s: %w", path, err)
		}
		original, err := os.ReadFile(abs) //nolint:gosec // repo-relative path from discovery
		if err != nil {
			return nil, fmt.Errorf("imagepin: %s: %w", path, err)
		}
		body := append([]byte{}, original...)
		in := byFile[path]
		sort.Slice(in, func(i, j int) bool { return in[i].start > in[j].start })
		for _, s := range in {
			if s.end > len(body) || string(body[s.start:s.end]) != s.Tag {
				return nil, fmt.Errorf("imagepin: %s:%d: site moved under us (expected %q at "+
					"%d-%d) — nothing was written", path, s.Line, s.Tag, s.start, s.end)
			}
			body = append(body[:s.start], append([]byte(newTag), body[s.end:]...)...)
		}
		plan.files[path] = patchedFile{old: original, patched: body, mode: info.Mode().Perm()}
	}
	return plan, nil
}

// Apply writes the plan.
func (p *RewritePlan) Apply(repoRoot string) error {
	return p.write(repoRoot, func(f patchedFile) []byte { return f.patched })
}

// Revert restores the contents every planned file had when the plan was
// computed. It is the undo for a rewrite whose result failed
// verification.
func (p *RewritePlan) Revert(repoRoot string) error {
	return p.write(repoRoot, func(f patchedFile) []byte { return f.old })
}

func (p *RewritePlan) write(repoRoot string, pick func(patchedFile) []byte) error {
	for _, path := range p.Paths() {
		f := p.files[path]
		abs := filepath.Join(repoRoot, filepath.FromSlash(path))
		if err := os.WriteFile(abs, pick(f), f.mode); err != nil {
			return fmt.Errorf("imagepin: %s: %w", path, err)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// lineStarts records the byte offset each line begins at.
func lineStarts(body []byte) []int {
	starts := []int{0}
	for i, b := range body {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

// lineOf maps a byte offset to a 1-based line number.
func lineOf(starts []int, off int) int {
	n := sort.SearchInts(starts, off+1)
	if n < 1 {
		return 1
	}
	return n
}

// scalarOffset converts a YAML scalar node's (line, column) into the
// byte offset of its VALUE — skipping the quote a quoted scalar starts
// at, which is why the column alone will not do.
func scalarOffset(body []byte, starts []int, n *yaml.Node) (int, bool) {
	if n.Line < 1 || n.Line > len(starts) {
		return 0, false
	}
	lineStart := starts[n.Line-1]
	lineEnd := len(body)
	if n.Line < len(starts) {
		lineEnd = starts[n.Line] - 1
	}
	if lineStart > lineEnd {
		return 0, false
	}
	from := lineStart + n.Column - 1
	if from < lineStart || from > lineEnd {
		from = lineStart
	}
	idx := strings.Index(string(body[from:lineEnd]), n.Value)
	if idx < 0 {
		return 0, false
	}
	return from + idx, true
}
