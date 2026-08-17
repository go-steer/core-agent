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

// Package imagepin discovers where this repo pins a container image.
//
// It answers the mechanical half of two different questions that are
// deliberately kept apart:
//
//   - "Can this daemon run this config?" — examples/internal/recipecheck
//     (#680), an offline in-tree oracle that runs in test-unit.
//   - "Is this pin still current upstream?" — dev/lookout-pin-check
//     (#787), a weekly job that has to call another repo.
//
// Both need the same fact — where is this image declared, and what tag
// does the declaration carry — so that fact is computed once, here.
// Everything version-judging stays with the caller.
//
// # Two entry points, on purpose
//
// [Resolve] answers "what does an operator running `kubectl apply -k`
// against this overlay actually deploy". It walks the kustomize
// composition graph in kustomize's own precedence order and returns the
// ONE effective pin. That is the right shape for a capability check: a
// base's `images:` transformer applies to the overlay too, so an
// overlay that inherits a pin instead of restating it is judged on the
// pin it really gets.
//
// [Sites] answers "where is this tag written down". It is a flat
// filesystem walk that returns EVERY declaration with a byte range, so
// a caller can rewrite them. Resolution would be wrong here: the sites
// an inherited pin hides are exactly the ones a bump must not miss.
//
// The two share the per-file extractors, the image-name predicate, and
// the positional comment-marker matching — which is the part that took
// the work and the part that must not be written twice.
//
// # Families are a parameter
//
// Every scan path is scoped by a [Family], not by a hardcoded image
// name. recipecheck passes its own daemon family; dev/lookout-pin-check
// passes [Lookout]. Adding a third image family is a declaration, not a
// fork.
//
// The daemon family is declared in recipecheck, where its one consumer
// lives; [Lookout] is declared here because it has two consumers, in
// internal trees that cannot import each other.
package imagepin

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Family is one container image — plus any alternate names that pin the
// same thing — whose declarations are read as a unit.
//
// Build one with [NewFamily]; the zero value matches nothing.
type Family struct {
	names []string
	// refRe matches `<name>[:@]<ref>` for any name in the family, with
	// the tag or digest captured. Precompiled because both entry points
	// run it over every file in a tree.
	refRe *regexp.Regexp
	// provenance matches the comment that records which release a
	// DIGEST pin was resolved from, since a digest carries no version
	// and kustomize makes `digest:` and `newTag:` mutually exclusive.
	// Optional: a family with no version-ordering consumer needs none.
	provenance *regexp.Regexp
}

// NewFamily builds a family from one or more image names.
//
// Names are matched exactly and on the PRE-rename spelling, which is
// what kustomize keys its transformer on: a `newName:` remap to a
// private mirror still pins the same image.
func NewFamily(provenance *regexp.Regexp, names ...string) *Family {
	// Longest first: Go's alternation is leftmost-first, and while RE2
	// will still find the overall match, ordering keeps the captured
	// name unambiguous for prefix-shaped pairs like
	// core-agent / core-agent-slim.
	sorted := append([]string{}, names...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	alts := make([]string, len(sorted))
	for i, n := range sorted {
		alts[i] = regexp.QuoteMeta(n)
	}
	// The reference alternation stops at `}` so the common
	// `"${IMAGE_REF:-ghcr.io/go-steer/core-agent:2.8.0}"` shell default
	// reads as the tag and not as the tag plus the closing brace.
	return &Family{
		names: sorted,
		refRe: regexp.MustCompile(`(?:` + strings.Join(alts, "|") + `)` +
			`[:@](?:\$\{[A-Za-z_][A-Za-z0-9_]*\}|\$[A-Za-z_][A-Za-z0-9_]*|[A-Za-z0-9][A-Za-z0-9._-]*)`),
		provenance: provenance,
	}
}

// Names returns the image names this family matches, longest first.
func (f *Family) Names() []string { return append([]string{}, f.names...) }

// Matches reports whether an image name belongs to this family.
func (f *Family) Matches(name string) bool {
	for _, want := range f.names {
		if name == want {
			return true
		}
	}
	return false
}

// provenanceOf reads the family's provenance marker out of the comments
// attached to one node, if the family declares one.
func (f *Family) provenanceIn(comment string) (string, bool) {
	if f.provenance == nil {
		return "", false
	}
	if m := f.provenance.FindStringSubmatch(comment); m != nil {
		return m[1], true
	}
	return "", false
}

// kustomizationNames are the filenames kustomize accepts, in its own
// precedence order.
var kustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// Pin is one image reference a deploy artifact resolves to.
type Pin struct {
	// Source is the file that carries the pin, relative to the artifact
	// being judged. It can differ from the overlay when the overlay
	// composes another one (example-otel composes example) or inherits
	// the image straight off a base's Deployment (gke-deploy does).
	Source string
	// Image is the matched family name.
	Image string
	// Tag is the tag, empty for a digest pin.
	Tag string
	// Digest is the digest, empty for a tag pin.
	Digest string
	// DigestVersion is the release a digest pin declared itself resolved
	// from, via the family's provenance comment. Empty otherwise.
	DigestVersion string
}

// KustomizationPath returns the kustomization file in dir, if any.
//
// dir is a directory inside the checkout being inspected — a recipe's
// deploy/overlays/<name> or a path declared in a [Tracked]. Nothing here
// is reachable from a request, and the caller wants exactly the file it
// names, so there is no traversal to defend against: the whole job is to
// read the repository it was pointed at.
func KustomizationPath(dir string) (string, bool) {
	for _, name := range kustomizationNames {
		p := filepath.Join(dir, name)
		// #nosec G703 -- dir is a checkout path from the caller, see above.
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, true
		}
	}
	return "", false
}

// OverlayDirs returns the immediate subdirectories of overlaysDir that
// hold a kustomization, in stable order.
//
// "An overlay is a directory under deploy/overlays" is this repo's own
// convention across every deployable recipe, and it maps exactly onto
// "the thing an operator runs kubectl apply -k against". The
// alternatives are worse: "any kustomization" pulls in the base and the
// components, which are pinned by their consumers; and "any
// kustomization nothing else references" wrongly drops
// gke-troubleshoot-agent's example overlay, which is both directly
// applicable AND composed by example-otel.
func OverlayDirs(overlaysDir string) ([]string, error) {
	entries, err := os.ReadDir(overlaysDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(overlaysDir, e.Name())
		if _, ok := KustomizationPath(dir); ok {
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out, nil
}

// kustomization is the slice of the kustomize schema this package reads.
type kustomization struct {
	Resources  []string `yaml:"resources"`
	Components []string `yaml:"components"`
}

// Resolve finds the image from fam that an overlay ends up deploying.
//
// Two passes over the same composition graph, in kustomize's own
// precedence order:
//
//  1. The `images:` transformer, depth-first in declaration order. A
//     base's transformer applies to the overlay too, so an overlay that
//     composes a pinned base or a pinned sibling IS pinned —
//     gke-troubleshoot-agent's example-otel composes example. Both
//     restate the pin today; resolving through composition is what stops
//     a caller from becoming a false positive the first time someone
//     stops restating it.
//  2. Failing that, the container `image:` written into a manifest the
//     composition pulls in. This is not a fallback for tidiness: it is
//     how gke-deploy pins, with a bare `image: ghcr.io/...:2.8.0` on the
//     base Deployment and every `images:` example in the overlay
//     commented out. A caller that stopped after pass 1 reported that
//     recipe as unpinned while a perfectly good pin sat one file away.
//
// Returns nil when the composition names no image from fam anywhere.
// Pin.Source comes back relative to dir.
//
// What is NOT covered, and why: an image set by a kustomize `patches:`
// entry or by a generator. That needs an evaluator, not a parser. A
// literal `<image>:${TAG}` IS returned — that is the honest answer
// ("this deploys something, and the file does not say what") rather
// than a skip.
func Resolve(dir string, fam *Family) (*Pin, error) {
	if pin, err := transformerPin(dir, fam); pin != nil || err != nil {
		return pin, err
	}
	return manifestPin(dir, fam)
}

// transformerPin is pass 1: the nearest `images:` entry for the family
// in the composition.
func transformerPin(dir string, fam *Family) (*Pin, error) {
	return walkComposition(dir, map[string]bool{}, func(kustPath string, root *yaml.Node) (*Pin, error) {
		pin := pinFromImages(root, fam)
		if pin != nil {
			pin.Source = filepath.Base(kustPath)
		}
		return pin, nil
	})
}

// manifestPin is pass 2: the first container `image:` naming the family
// in any manifest file the composition lists as a resource.
func manifestPin(dir string, fam *Family) (*Pin, error) {
	return walkComposition(dir, map[string]bool{}, func(kustPath string, root *yaml.Node) (*Pin, error) {
		var k kustomization
		if err := root.Decode(&k); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(kustPath), err)
		}
		base := filepath.Dir(kustPath)
		for _, ref := range k.Resources {
			path := filepath.Join(base, ref)
			info, statErr := os.Stat(path)
			if statErr != nil || info.IsDir() {
				continue // a directory (walkComposition recurses into it) or a remote URL
			}
			body, readErr := os.ReadFile(path) //nolint:gosec // repo-relative path from a kustomization
			if readErr != nil {
				return nil, readErr
			}
			if pin := pinFromManifest(body, fam); pin != nil {
				pin.Source = ref
				return pin, nil
			}
		}
		return nil, nil
	})
}

// walkComposition applies visit to every kustomization reachable from
// dir, depth-first in declaration order, and returns the first pin any
// of them yields. visit sets Pin.Source relative to the kustomization's
// own directory; the walk rewrites it to be relative to dir as it
// unwinds, so a finding names a path a reader can follow from the
// overlay they were told to apply.
func walkComposition(dir string, seen map[string]bool, visit func(string, *yaml.Node) (*Pin, error)) (*Pin, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if seen[abs] {
		return nil, nil // a cycle; kustomize would reject it, we just stop
	}
	seen[abs] = true

	path, ok := KustomizationPath(dir)
	if !ok {
		return nil, nil
	}
	body, err := os.ReadFile(path) //nolint:gosec // discovered kustomization path
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if pin, visitErr := visit(path, &root); pin != nil || visitErr != nil {
		return pin, visitErr
	}
	var k kustomization
	if err := root.Decode(&k); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	for _, ref := range append(append([]string{}, k.Resources...), k.Components...) {
		child := filepath.Join(dir, ref)
		if info, statErr := os.Stat(child); statErr != nil || !info.IsDir() {
			continue // a plain manifest file, or a remote URL
		}
		pin, err := walkComposition(child, seen, visit)
		if err != nil {
			return nil, err
		}
		if pin != nil {
			pin.Source = filepath.ToSlash(filepath.Join(ref, pin.Source))
			return pin, nil
		}
	}
	return nil, nil
}

// pinFromImages reads the `images:` transformer straight off the YAML
// node tree rather than a decoded struct, because the provenance marker
// is a comment and only the node tree knows which entry a comment is
// attached to.
func pinFromImages(root *yaml.Node, fam *Family) *Pin {
	for _, entry := range imagesEntries(root) {
		name := scalarValue(mappingValue(entry, "name"))
		if !fam.Matches(name) {
			continue
		}
		pin := &Pin{
			Image:  name,
			Tag:    scalarValue(mappingValue(entry, "newTag")),
			Digest: scalarValue(mappingValue(entry, "digest")),
		}
		if pin.Digest != "" {
			pin.DigestVersion = commentOnEntry(entry, fam.provenanceIn)
		}
		return pin
	}
	return nil
}

// imagesEntries returns the `images:` sequence items of a kustomization
// node tree, or nil when there is no such block.
func imagesEntries(root *yaml.Node) []*yaml.Node {
	seq := mappingValue(documentBody(root), "images")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	return seq.Content
}

// commentOnEntry looks for a marker in the comments attached to one
// images entry: above the entry, at the end of any of its lines, or
// above/beside any of its keys.
//
// The match is POSITIONAL, not file-scoped. A file-scoped match reads a
// marker out of a commented-out documentation example — and several of
// this repo's kustomizations carry a commented-out digest example,
// complete with a marker, showing operators the pattern. A check that
// credited a live pin with a marker lifted from the manual would be
// asserting something nobody wrote. Comment syntax then does the
// scoping for free: `# - name:` is not a node and has no comments of
// its own. (#680's adversarial review, finding F6.)
func commentOnEntry(entry *yaml.Node, match func(string) (string, bool)) string {
	nodes := append([]*yaml.Node{entry}, entry.Content...)
	for _, n := range nodes {
		for _, comment := range []string{n.HeadComment, n.LineComment, n.FootComment} {
			if v, ok := match(comment); ok {
				return v
			}
		}
	}
	return ""
}

func documentBody(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.DocumentNode && len(n.Content) == 1 {
		return n.Content[0]
	}
	return n
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	n = documentBody(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func scalarValue(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}

// manifestImage matches a container `image:` field. Anchoring on
// `image:` at the start of a line is what keeps commented-out examples
// out: `# image: ...` does not match.
var manifestImage = regexp.MustCompile(`(?m)^\s*image:\s*["']?(\S+?)["']?\s*$`)

// pinFromManifest reads the first `image:` from the family out of a
// plain Kubernetes manifest.
func pinFromManifest(body []byte, fam *Family) *Pin {
	for _, loc := range manifestImage.FindAllSubmatchIndex(body, -1) {
		image, tag, digest := SplitImageRef(string(body[loc[2]:loc[3]]))
		if !fam.Matches(image) {
			continue
		}
		pin := &Pin{Image: image, Tag: tag, Digest: digest}
		if digest != "" {
			pin.DigestVersion = CommentMarkerAbove(body, loc[0], fam.provenanceIn)
		}
		return pin
	}
	return nil
}

// PinBearingFile is one non-manifest artifact plus the extractor that
// knows its syntax.
type PinBearingFile struct {
	Path    string
	Extract func([]byte, *Family) []Pin
}

// PinBearingFiles finds every Dockerfile and shell script under root,
// in stable order.
//
// The whole tree is walked rather than just the root: a recipe may keep
// build files under deploy/ (kube-platform-agent does), and a vendored
// tree that builds an image is exactly as deployable as one at the top.
// A walk error on one subtree is returned rather than skipped — the
// alternative is a check that quietly stops looking.
func PinBearingFiles(root string) ([]PinBearingFile, error) {
	var out []PinBearingFile
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch classify(d.Name()) {
		case fileDockerfile:
			out = append(out, PinBearingFile{Path: path, Extract: PinsFromDockerfile})
		case fileShell:
			out = append(out, PinBearingFile{Path: path, Extract: PinsFromShell})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

type fileClass int

const (
	fileOther fileClass = iota
	fileDockerfile
	fileShell
)

func classify(name string) fileClass {
	switch {
	case name == "Dockerfile" || strings.HasSuffix(name, ".Dockerfile") ||
		strings.HasPrefix(name, "Dockerfile."):
		return fileDockerfile
	case strings.HasSuffix(name, ".sh"):
		return fileShell
	}
	return fileOther
}

// dockerFrom captures the image reference of a FROM instruction,
// tolerating `--platform=` flags and a trailing `AS <stage>`.
var dockerFrom = regexp.MustCompile(`(?im)^\s*FROM\s+(?:--\S+\s+)*(\S+)`)

// dockerArg captures a build argument's default value.
var dockerArg = regexp.MustCompile(`(?im)^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)=["']?([^"'\s]*)["']?`)

// dockerVar matches ${NAME} / $NAME inside an image reference.
var dockerVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// PinsFromDockerfile returns one Pin per FROM naming the family. A
// multi-stage build can legitimately have several.
//
// ARG defaults declared above a FROM are substituted into it, because
// that is a shape this repo documents:
//
//	ARG CORE_AGENT_VERSION=2.8.0
//	FROM ghcr.io/go-steer/core-agent-slim:${CORE_AGENT_VERSION}
//
// Reading `${CORE_AGENT_VERSION}` as a tag would advise the author to
// "pin a released semver instead" when that is exactly what they did.
// A `--build-arg` at build time can override the default and this
// cannot see it, which is the same limit `docker build` puts on anyone
// reading the file: the default is what the artifact says it deploys,
// and a substitution that resolves to nothing stays unresolved and is
// reported.
func PinsFromDockerfile(body []byte, fam *Family) []Pin {
	args := map[string]string{}
	for _, m := range dockerArg.FindAllSubmatch(body, -1) {
		args[string(m[1])] = string(m[2])
	}
	var out []Pin
	for _, loc := range dockerFrom.FindAllSubmatchIndex(body, -1) {
		ref := expandVars(string(body[loc[2]:loc[3]]), args)
		image, tag, digest := SplitImageRef(ref)
		if !fam.Matches(image) {
			continue
		}
		pin := Pin{Image: image, Tag: tag, Digest: digest}
		if digest != "" {
			pin.DigestVersion = CommentMarkerAbove(body, loc[0], fam.provenanceIn)
		}
		out = append(out, pin)
	}
	return out
}

// PinsFromShell returns one Pin per literal family reference in a
// script. cloud-run-deploy/scripts/deploy-from-prebuilt-image.sh is the
// artifact this exists for: it defaults IMAGE_REF to a full daemon
// reference, and that default is what most people who run the script
// deploy.
//
// Only lines that are not comments are read. A reference whose tag is a
// shell variable is kept rather than skipped — the script does deploy
// something, and "the file does not say what" is a finding, not a pass.
func PinsFromShell(body []byte, fam *Family) []Pin {
	var out []Pin
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, ref := range fam.refRe.FindAllString(line, -1) {
			image, tag, digest := SplitImageRef(ref)
			if !fam.Matches(image) {
				continue
			}
			out = append(out, Pin{Image: image, Tag: tag, Digest: digest})
		}
	}
	return out
}

// expandVars substitutes known ARG defaults. An unknown name is left
// alone so the reference stays visibly unresolved.
func expandVars(ref string, vars map[string]string) string {
	return dockerVar.ReplaceAllStringFunc(ref, func(match string) string {
		m := dockerVar.FindStringSubmatch(match)
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if val, ok := vars[name]; ok {
			return val
		}
		return match
	})
}

// CommentMarkerAbove reads a marker out of the contiguous block of `#`
// comment lines immediately above the byte at off. Adjacency is what
// scopes it: a commented-out example elsewhere in the file is separated
// from the live line by the live line's own neighbours.
//
// "Contiguous" is literal — a blank line ends the block. A comment with
// whitespace between it and the line below is a comment about something
// else, and reading through the gap would let a detached block claim a
// pin nobody wrote it about.
//
// That blank-line stop is NEW as of #787, and it is a real semantics
// change for the two pre-existing callers — [Family.pinFromManifest]
// and [Family.PinsFromDockerfile], both resolving DigestVersion
// provenance for #680's gate. Before, a `# core-agent-version:` comment
// separated from its digest pin by a blank line still resolved; now it
// does not, and the pin reads as having no declared version. Nothing in
// the tree relied on that (no family image is digest-pinned with a
// detached provenance comment, and a differential run of CheckDeployPins
// across the change produced byte-identical findings), but a provenance
// comment that stops working after someone inserts a blank line is a
// surprise worth having written down.
//
// The line off lands in is dropped first, so a caller may point at the
// reference itself rather than at the margin. Every caller in this
// package passes a line-start offset, where that makes no difference;
// it is here for the mid-line callers in the site scanner.
func CommentMarkerAbove(body []byte, off int, match func(string) (string, bool)) string {
	lines := strings.Split(string(body[:off]), "\n")
	lines = lines[:len(lines)-1]
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || !strings.HasPrefix(line, "#") {
			return ""
		}
		if v, ok := match(line); ok {
			return v
		}
	}
	return ""
}

// SplitImageRef splits `name[:tag][@digest]` the way a registry client
// does: the digest wins if both are present, and a colon inside a
// registry's host:port is not a tag separator.
func SplitImageRef(ref string) (image, tag, digest string) {
	image = ref
	if at := strings.Index(image, "@"); at >= 0 {
		digest = image[at+1:]
		image = image[:at]
	}
	if colon := strings.LastIndex(image, ":"); colon >= 0 && !strings.Contains(image[colon+1:], "/") {
		tag = image[colon+1:]
		image = image[:colon]
	}
	// A reference with neither tag nor digest is `:latest` by Docker's
	// rule, and :latest is unorderable — say so rather than reading it
	// as "no pin".
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return image, tag, digest
}
