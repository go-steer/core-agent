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

// This file compares what minversion.go says a recipe's config needs
// against what the recipe's deploy artifacts actually pin (#680).

package recipecheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// daemonImages are the image names that carry a core-agent daemon, so a
// pin on one of them is a pin on the runtime this recipe's config talks
// to. core-agent-tui is deliberately absent: it is an operator's client,
// not the thing that reads config.json.
var daemonImages = []string{
	"ghcr.io/go-steer/core-agent",
	"ghcr.io/go-steer/core-agent-slim",
}

// kustomizationNames are the filenames kustomize accepts, in its own
// precedence order.
var kustomizationNames = []string{"kustomization.yaml", "kustomization.yml", "Kustomization"}

// provenanceMarker lets a digest pin state the release it was resolved
// from, since a digest carries no version and kustomize makes `digest:`
// and `newTag:` mutually exclusive.
//
// Without it the gate would forbid digest pinning outright for any
// recipe with a version-gated config — and digest pinning is the
// stronger practice, so a gate that punishes it is a gate someone
// deletes. Writing the version down is the smallest thing that keeps
// both properties.
//
//	# core-agent-version: 2.9.0-dev.1
//	- name: ghcr.io/go-steer/core-agent
//	  digest: sha256:...
//
// The match is POSITIONAL, not file-scoped. A file-scoped match reads a
// marker out of a commented-out documentation example — and every one of
// these kustomizations carries a commented-out digest example, complete
// with a marker, showing operators the pattern. A gate that credited a
// live pin with a version lifted from the manual would be asserting
// something nobody wrote. So the marker has to be attached to the pin:
// the head or line comment of the images entry for YAML, the contiguous
// comment block immediately above the line for Dockerfiles and shell.
// Comment syntax then does the scoping for free — `# - name:` is not a
// node and has no comments of its own.
var provenanceMarker = regexp.MustCompile(`(?m)^\s*#*\s*core-agent-version:\s*(\S+)\s*$`)

// Pin is a core-agent image reference one deploy artifact resolves to.
type Pin struct {
	// Source is the file that carries the pin, relative to the artifact
	// being judged. It can differ from the overlay when the overlay
	// composes another one (example-otel composes example) or inherits
	// the image straight off a base's Deployment (gke-deploy does).
	Source string
	// Image is the matched entry from daemonImages.
	Image string
	// Tag is the tag, empty for a digest pin.
	Tag string
	// Digest is the digest, empty for a tag pin.
	Digest string
	// DigestVersion is the release a digest pin declared itself resolved
	// from, via the core-agent-version comment. Empty otherwise.
	DigestVersion string
}

// VersionFinding is one deploy artifact whose daemon pin cannot be shown
// to satisfy the recipe's own config.
type VersionFinding struct {
	// Overlay is the deploy artifact at fault, relative to the examples
	// dir: the directory an operator would `kubectl apply -k`, the
	// Dockerfile whose FROM names the daemon, or the script that deploys
	// it.
	Overlay string
	// Pin is the reference as written, or "" when there is none.
	Pin string
	// Reason says what is wrong and what to do about it.
	Reason string
}

func (f VersionFinding) String() string {
	if f.Pin == "" {
		return fmt.Sprintf("%s: no core-agent image pin: %s", f.Overlay, f.Reason)
	}
	return fmt.Sprintf("%s: pins %q: %s", f.Overlay, f.Pin, f.Reason)
}

// CheckDeployPins reports every way r's deploy artifacts disagree with
// r's config about which daemon release they are for.
//
// # The three rules
//
//  1. A pin that names a version must name a version that EXISTS. This
//     is the rule kube-platform-agent broke by pinning "2.9.0", a tag
//     this repo has never cut. An ordering check alone waves that
//     through and the Pod fails ImagePullBackOff.
//  2. A pin must be ORDERABLE. A floating tag ("main", "main-<sha>",
//     "latest") is not a version, cannot be compared, and moves under
//     the operator between one `kubectl apply` and the next; a bare
//     digest carries no version either, so it has to say which release
//     it came from.
//  3. When the config uses a version-gated feature, the pin must be at
//     least the release that introduced it.
//
// Rules 1 and 2 are unconditional. An earlier revision gated them on the
// recipe having a version-gated config, on the theory that a recipe
// asserting nothing has nothing for the pin to contradict. That was
// wrong twice over. Empirically it left four of the six discovered
// recipes with an empty floor and therefore no check at all — reverting
// cloud-run-deploy's FROM to the floating `:main` this issue exists to
// fix produced a clean run. And structurally the premise does not hold:
// an unorderable pin is not "unconstrained", it is a pin whose contents
// this check cannot see, which is the same blindness under a different
// name. Rule 3 is inherently conditional; it is a no-op when the floor
// is empty, which needs no special case.
//
// # The artifacts
//
// Four deploy shapes are covered, because the repo ships all four and
// the bug is identical in each:
//
//   - a kustomize overlay's `images:` transformer;
//   - a container `image:` in a manifest the overlay composes, which is
//     how gke-deploy pins (no transformer entry at all — the earlier
//     revision of this check called that recipe "unpinned" and could not
//     see the pin that was actually deploying);
//   - a recipe Dockerfile's `FROM`, including the `ARG`-substituted form
//     the cloud-run README documents;
//   - a literal daemon reference in a deploy script.
//
// cloud-run-deploy is the reason for the last two: it has no manifests
// at all — it bakes its .agents/ bundle onto the daemon image — and its
// FROM was pinned to the floating `:main` tag under a comment promising
// to sweep back to a semver "once v2.4.0 cuts".
//
// What is NOT covered, and why: an image set by a kustomize `patches:`
// entry or by a generator, and a shell reference assembled from
// variables the script computes. Both need an evaluator, not a parser.
// A literal `ghcr.io/go-steer/core-agent:${TAG}` IS reported — that is
// the honest answer ("this deploys something, and the file does not say
// what") rather than a skip.
func CheckDeployPins(examplesDir string, r Recipe, released []Version) ([]VersionFinding, error) {
	req, err := requirementFor(examplesDir, r)
	if err != nil {
		return nil, err
	}
	// The roots are resolved as absolute paths, so display paths have to
	// be rebased off an absolute examples dir too — otherwise every
	// finding names an absolute path from the machine that ran the test.
	absExamples, err := filepath.Abs(examplesDir)
	if err != nil {
		return nil, fmt.Errorf("recipecheck: %s: %w", r.Name, err)
	}
	rel := func(p string) string {
		out, relErr := filepath.Rel(absExamples, p)
		if relErr != nil {
			return p
		}
		return filepath.ToSlash(out)
	}

	findings, err := checkOverlayPins(examplesDir, r, req, released, rel)
	if err != nil {
		return nil, err
	}
	files, err := checkFilePins(examplesDir, r, req, released, rel)
	if err != nil {
		return nil, err
	}
	return append(findings, files...), nil
}

// requirementFor is the floor every way of running this recipe puts
// under the daemon image.
//
// It is the union over EVERY config the recipe ships, not just the
// config.json that Discover keyed on. kube-platform-agent is why: its
// daemon Deployment runs `-c /opt/kube-platform-agent/.agents/
// config.hub.json`, a second config with a strictly larger gated surface
// than the config.json beside it, and judging the pin against the file
// the manifest does not pass is judging the wrong thing.
//
// Reading the `-c` argument out of the manifests instead would be more
// precise in principle and guesswork in practice: the argument is a
// container path (/opt/...), mapping it back to a repo path means
// reversing a ConfigMap generator or an image build, and every such
// mapping is a place for this check to quietly resolve to nothing. The
// union needs none of that. It is also the answer that is safe for the
// operator, who may run whichever config the recipe ships: the floor
// becomes the floor of the strictest way to run it.
func requirementFor(examplesDir string, r Recipe) (Requirement, error) {
	root := recipeRoot(examplesDir, r.Dir)
	if root == "" {
		root = r.Dir
	}
	paths, err := recipeConfigFiles(root)
	if err != nil {
		return Requirement{}, fmt.Errorf("recipecheck: %s: %w", r.Name, err)
	}
	var req Requirement
	seen := map[string]bool{}
	for _, path := range paths {
		cfg, loadErr := loadConfigFile(path)
		if loadErr != nil {
			return Requirement{}, fmt.Errorf("recipecheck: %s: %w", r.Name, loadErr)
		}
		one, reqErr := RequiredVersion(cfg)
		if reqErr != nil {
			return Requirement{}, fmt.Errorf("recipecheck: %s: %s: %w", r.Name, filepath.Base(path), reqErr)
		}
		for _, reason := range one.Reasons {
			if seen[reason.Path] {
				continue
			}
			seen[reason.Path] = true
			req.Reasons = append(req.Reasons, reason)
			if req.Min.Compare(reason.Min) < 0 {
				req.Min = reason.Min
			}
		}
	}
	return req, nil
}

// recipeConfigFiles finds every daemon config a recipe ships, anywhere
// under it: `config.json` and the `config.<variant>.json` spelling this
// repo uses for a second way to run the same recipe.
func recipeConfigFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if name == config.ConfigFileName ||
			(strings.HasPrefix(name, "config.") && strings.HasSuffix(name, ".json")) {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// loadConfigFile is config.Load for a file that is not named
// config.json. It reproduces Load's contract — decode over the defaults,
// then validate — because the gated paths are read off the decoded
// struct and a half-populated one would answer wrong.
func loadConfigFile(path string) (*config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	cfg := config.DefaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return cfg, nil
}

// checkOverlayPins judges every kustomize overlay under the recipe's
// deploy/overlays.
func checkOverlayPins(examplesDir string, r Recipe, req Requirement, released []Version, rel func(string) string) ([]VersionFinding, error) {
	root := deployRoot(examplesDir, r.Dir)
	if root == "" {
		return nil, nil
	}
	overlays, err := overlayDirs(filepath.Join(root, "deploy", "overlays"))
	if err != nil {
		return nil, fmt.Errorf("recipecheck: %s: %w", r.Name, err)
	}
	if len(overlays) == 0 {
		if req.Empty() {
			return nil, nil
		}
		return []VersionFinding{{
			Overlay: rel(filepath.Join(root, "deploy")),
			Reason: fmt.Sprintf("this recipe has a deploy tree and a config that requires ≥ %s, "+
				"but deploy/overlays holds no kustomization to pin it in", req.Min),
		}}, nil
	}

	var findings []VersionFinding
	for _, dir := range overlays {
		pin, err := resolvePin(dir)
		if err != nil {
			return nil, fmt.Errorf("recipecheck: %s: %s: %w", r.Name, rel(dir), err)
		}
		for _, f := range judgePin(pin, req, released) {
			f.Overlay = rel(dir)
			if pin != nil {
				f.Reason += fmt.Sprintf(" (pinned in %s)", rel(filepath.Join(dir, pin.Source)))
			}
			findings = append(findings, f)
		}
	}
	return findings, nil
}

// checkFilePins judges every Dockerfile FROM and every literal daemon
// reference in a shell script under the recipe.
//
// Unlike the overlay path there is no "no pin" case to report: a file
// that never names a daemon image is not a daemon deploy artifact, it is
// just a file (kube-platform-agent's content.Dockerfile builds a
// FROM-scratch content image and must not be dragged into this).
func checkFilePins(examplesDir string, r Recipe, req Requirement, released []Version, rel func(string) string) ([]VersionFinding, error) {
	root := recipeRoot(examplesDir, r.Dir)
	if root == "" {
		return nil, nil
	}
	files, err := pinBearingFiles(root)
	if err != nil {
		return nil, fmt.Errorf("recipecheck: %s: %w", r.Name, err)
	}
	var findings []VersionFinding
	for _, f := range files {
		body, readErr := os.ReadFile(f.path)
		if readErr != nil {
			return nil, fmt.Errorf("recipecheck: %s: %s: %w", r.Name, rel(f.path), readErr)
		}
		for _, pin := range f.extract(body) {
			for _, finding := range judgePin(&pin, req, released) {
				finding.Overlay = rel(f.path)
				findings = append(findings, finding)
			}
		}
	}
	return findings, nil
}

// judgePin applies the three rules to one resolved pin. The Overlay
// field is filled in by the caller, which knows the paths.
func judgePin(pin *Pin, req Requirement, released []Version) []VersionFinding {
	if pin == nil {
		return []VersionFinding{{Reason: noPinReason(req)}}
	}
	ref, v, ok := pinVersion(pin)
	if !ok {
		return []VersionFinding{{Pin: ref, Reason: unorderableReason(pin, req)}}
	}
	var out []VersionFinding
	if !isReleased(v, released) {
		out = append(out, VersionFinding{Pin: ref, Reason: fmt.Sprintf(
			"%s is not a version this repo has released — CHANGELOG.md has no section for it, so "+
				"the image was never published and the pull fails (ImagePullBackOff on a Pod, a "+
				"failed FROM on a build)", v)})
	}
	if unmet := req.Unmet(v); len(unmet) > 0 {
		out = append(out, VersionFinding{Pin: ref, Reason: fmt.Sprintf(
			"but the recipe's config requires ≥ %s. A %s daemon does not fail on this config — "+
				"pkg/config ignores unknown keys — it boots clean and silently drops:%s",
			req.Min, v, Bullets(unmet))})
	}
	return out
}

func noPinReason(req Requirement) string {
	reason := "nothing in this overlay's composition names a core-agent daemon image — not an " +
		"`images:` transformer entry, not a container `image:` in any manifest it pulls in. " +
		"Either it does not deploy the daemon (in which case it does not belong under " +
		"deploy/overlays), or the image arrives by a route this check cannot read, such as a " +
		"`patches:` entry or a generator, which means nobody reviewing the tree can read it either" +
		"\n\tAdd an `images:` entry pinning a released semver"
	if !req.Empty() {
		reason += fmt.Sprintf("\n\tThe recipe's config requires ≥ %s:%s", req.Min, Bullets(req.Reasons))
	}
	return reason
}

// pinVersion resolves a pin to a comparable version, and reports false
// when it cannot be ordered at all.
func pinVersion(pin *Pin) (ref string, v Version, ok bool) {
	if pin.Digest != "" {
		ref = pin.Digest
		if pin.DigestVersion == "" {
			return ref, Version{}, false
		}
		v, err := ParseVersion(pin.DigestVersion)
		return ref, v, err == nil
	}
	v, err := ParseVersion(pin.Tag)
	return pin.Tag, v, err == nil
}

func unorderableReason(pin *Pin, req Requirement) string {
	var need string
	if !req.Empty() {
		need = fmt.Sprintf("\n\tThe recipe's config requires ≥ %s:%s", req.Min, Bullets(req.Reasons))
	}
	if pin.Digest != "" {
		return "a digest carries no version, so this check cannot tell which release deploys" +
			"\n\tKeep the digest and add a `# core-agent-version: <release>` comment on the entry " +
			"itself recording the release it was resolved from (`crane digest` prints the digest; " +
			"you know the tag you asked for)" + need
	}
	return "a floating tag carries no version and moves under the operator between one apply and " +
		"the next, so this check cannot tell which release deploys" +
		"\n\tPin a released semver instead" + need
}

// deployRoot walks up from a config root to the recipe directory that
// owns a deploy/ tree, stopping at examplesDir. Returns "" when the
// recipe ships no manifests.
//
// The walk is needed because a config root is not a recipe root and the
// two sit at different depths per recipe: gke-troubleshoot-agent's
// config lives at deploy/base/config, kube-platform-agent's at .agents.
func deployRoot(examplesDir, configRoot string) string {
	absExamples, err := filepath.Abs(examplesDir)
	if err != nil {
		return ""
	}
	dir, err := filepath.Abs(configRoot)
	if err != nil {
		return ""
	}
	for {
		if info, statErr := os.Stat(filepath.Join(dir, "deploy")); statErr == nil && info.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if dir == absExamples || parent == dir {
			return ""
		}
		dir = parent
	}
}

// overlayDirs returns the immediate subdirectories of deploy/overlays
// that hold a kustomization, in stable order.
//
// "An overlay is a directory under deploy/overlays" is this repo's own
// convention across all three deployable recipes, and it maps exactly
// onto "the thing an operator runs kubectl apply -k against". The
// alternatives are worse: "any kustomization" pulls in the base and the
// components, which are pinned by their consumers; and "any
// kustomization nothing else references" wrongly drops
// gke-troubleshoot-agent's example overlay, which is both directly
// applicable AND composed by example-otel.
func overlayDirs(overlaysDir string) ([]string, error) {
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
		if _, ok := kustomizationPath(dir); ok {
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out, nil
}

func kustomizationPath(dir string) (string, bool) {
	for _, name := range kustomizationNames {
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p, true
		}
	}
	return "", false
}

// kustomization is the slice of the kustomize schema this check reads.
type kustomization struct {
	Resources  []string `yaml:"resources"`
	Components []string `yaml:"components"`
}

// resolvePin finds the daemon image an overlay ends up deploying.
//
// Two passes over the same composition graph, in kustomize's own
// precedence order:
//
//  1. The `images:` transformer, depth-first in declaration order. A
//     base's transformer applies to the overlay too, so an overlay that
//     composes a pinned base or a pinned sibling IS pinned —
//     gke-troubleshoot-agent's example-otel composes example. Both
//     restate the pin today; resolving through composition is what stops
//     this check from becoming a false positive the first time someone
//     stops restating it.
//  2. Failing that, the container `image:` written into a manifest the
//     composition pulls in. This is not a fallback for tidiness: it is
//     how gke-deploy pins, with a bare `image: ghcr.io/...:2.8.0` on the
//     base Deployment and every `images:` example in the overlay
//     commented out. A check that stopped after pass 1 reported that
//     recipe as unpinned while a perfectly good pin sat one file away.
//
// Returns nil when the composition names no daemon image anywhere. Pin
// Source comes back relative to dir.
func resolvePin(dir string) (*Pin, error) {
	if pin, err := transformerPin(dir); pin != nil || err != nil {
		return pin, err
	}
	return manifestPin(dir)
}

// transformerPin is pass 1: the nearest `images:` entry for a daemon
// image in the composition.
func transformerPin(dir string) (*Pin, error) {
	return walkComposition(dir, map[string]bool{}, func(kustPath string, root *yaml.Node) (*Pin, error) {
		pin := pinFromImages(root)
		if pin != nil {
			pin.Source = filepath.Base(kustPath)
		}
		return pin, nil
	})
}

// manifestPin is pass 2: the first container `image:` naming a daemon in
// any manifest file the composition lists as a resource.
func manifestPin(dir string) (*Pin, error) {
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
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil, readErr
			}
			if pin := pinFromManifest(body); pin != nil {
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

	path, ok := kustomizationPath(dir)
	if !ok {
		return nil, nil
	}
	body, err := os.ReadFile(path)
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
// attached to. See provenanceMarker.
func pinFromImages(root *yaml.Node) *Pin {
	seq := mappingValue(documentBody(root), "images")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	for _, entry := range seq.Content {
		name := scalarValue(mappingValue(entry, "name"))
		if !isDaemonImage(name) {
			continue
		}
		pin := &Pin{
			Image:  name,
			Tag:    scalarValue(mappingValue(entry, "newTag")),
			Digest: scalarValue(mappingValue(entry, "digest")),
		}
		if pin.Digest != "" {
			pin.DigestVersion = provenanceOf(entry)
		}
		return pin
	}
	return nil
}

// provenanceOf looks for the marker in the comments attached to one
// images entry: above the entry, at the end of any of its lines, or
// above any of its keys.
func provenanceOf(entry *yaml.Node) string {
	var nodes []*yaml.Node
	nodes = append(nodes, entry)
	nodes = append(nodes, entry.Content...)
	for _, n := range nodes {
		for _, comment := range []string{n.HeadComment, n.LineComment} {
			if m := provenanceMarker.FindStringSubmatch(comment); m != nil {
				return m[1]
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

// manifestImage matches a container `image:` naming a daemon. Anchoring
// on `image:` at the start of a line is what keeps commented-out
// examples out: `# image: ...` does not match.
var manifestImage = regexp.MustCompile(`(?m)^\s*image:\s*["']?(\S+?)["']?\s*$`)

// pinFromManifest reads the first daemon `image:` out of a plain
// Kubernetes manifest.
func pinFromManifest(body []byte) *Pin {
	for _, loc := range manifestImage.FindAllSubmatchIndex(body, -1) {
		image, tag, digest := splitImageRef(string(body[loc[2]:loc[3]]))
		if !isDaemonImage(image) {
			continue
		}
		pin := &Pin{Image: image, Tag: tag, Digest: digest}
		if digest != "" {
			pin.DigestVersion = commentProvenanceAbove(body, loc[0])
		}
		return pin
	}
	return nil
}

// isDaemonImage matches on the pre-rename name, which is what kustomize
// keys its transformer on: a `newName:` remap to a private mirror still
// pins the same daemon.
func isDaemonImage(name string) bool {
	for _, want := range daemonImages {
		if name == want {
			return true
		}
	}
	return false
}

// recipeRoot is the immediate child of examplesDir that contains the
// config root — the directory a reader would call "the recipe".
//
// deployRoot cannot serve here: it keys on owning a deploy/ tree, and
// one recipe this exists for (cloud-run-deploy) has none.
func recipeRoot(examplesDir, configRoot string) string {
	absExamples, err := filepath.Abs(examplesDir)
	if err != nil {
		return ""
	}
	dir, err := filepath.Abs(configRoot)
	if err != nil {
		return ""
	}
	for {
		parent := filepath.Dir(dir)
		if parent == absExamples {
			return dir
		}
		if parent == dir {
			return "" // walked past examplesDir without finding it
		}
		dir = parent
	}
}

// pinBearingFile is one non-manifest deploy artifact plus the extractor
// that knows its syntax.
type pinBearingFile struct {
	path    string
	extract func([]byte) []Pin
}

// pinBearingFiles finds every Dockerfile and shell script under a
// recipe, in stable order.
//
// The whole tree is walked rather than just the root: a recipe may keep
// build files under deploy/ (kube-platform-agent does), and a vendored
// tree that builds a daemon image is exactly as deployable as one at the
// root. A walk error on one subtree is returned rather than skipped —
// the alternative is a check that quietly stops looking.
func pinBearingFiles(root string) ([]pinBearingFile, error) {
	var out []pinBearingFile
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		switch {
		case name == "Dockerfile" || strings.HasSuffix(name, ".Dockerfile") ||
			strings.HasPrefix(name, "Dockerfile."):
			out = append(out, pinBearingFile{path: path, extract: pinsFromDockerfile})
		case strings.HasSuffix(name, ".sh"):
			out = append(out, pinBearingFile{path: path, extract: pinsFromShell})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// dockerFrom captures the image reference of a FROM instruction,
// tolerating `--platform=` flags and a trailing `AS <stage>`.
var dockerFrom = regexp.MustCompile(`(?im)^\s*FROM\s+(?:--\S+\s+)*(\S+)`)

// dockerArg captures a build argument's default value.
var dockerArg = regexp.MustCompile(`(?im)^\s*ARG\s+([A-Za-z_][A-Za-z0-9_]*)=["']?([^"'\s]*)["']?`)

// dockerVar matches ${NAME} / $NAME inside an image reference.
var dockerVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// pinsFromDockerfile returns one Pin per FROM naming a daemon image. A
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
// A `--build-arg` at build time can override the default and this cannot
// see it, which is the same limit `docker build` puts on anyone reading
// the file: the default is what the artifact says it deploys, and a
// substitution that resolves to nothing stays unorderable and is
// reported.
func pinsFromDockerfile(body []byte) []Pin {
	args := map[string]string{}
	for _, m := range dockerArg.FindAllSubmatch(body, -1) {
		args[string(m[1])] = string(m[2])
	}
	var out []Pin
	for _, loc := range dockerFrom.FindAllSubmatchIndex(body, -1) {
		ref := expandVars(string(body[loc[2]:loc[3]]), args)
		image, tag, digest := splitImageRef(ref)
		if !isDaemonImage(image) {
			continue
		}
		pin := Pin{Image: image, Tag: tag, Digest: digest}
		if digest != "" {
			pin.DigestVersion = commentProvenanceAbove(body, loc[0])
		}
		out = append(out, pin)
	}
	return out
}

// shellDaemonRef matches a literal daemon reference in a script. The tag
// alternation stops at `}` so the common
// `"${IMAGE_REF:-ghcr.io/go-steer/core-agent:2.8.0}"` default reads as
// the tag and not as the tag plus the closing brace.
var shellDaemonRef = regexp.MustCompile(
	`ghcr\.io/go-steer/core-agent(?:-slim)?[:@](?:\$\{[A-Za-z_][A-Za-z0-9_]*\}|\$[A-Za-z_][A-Za-z0-9_]*|[A-Za-z0-9][A-Za-z0-9._-]*)`)

// pinsFromShell returns one Pin per literal daemon reference in a
// script. cloud-run-deploy/scripts/deploy-from-prebuilt-image.sh is the
// artifact this exists for: it defaults IMAGE_REF to a full daemon
// reference, and that default is what most people who run the script
// deploy.
//
// Only lines that are not comments are read. A reference whose tag is a
// shell variable is kept as an unorderable pin rather than skipped — the
// script does deploy something, and "the file does not say what" is a
// finding, not a pass.
func pinsFromShell(body []byte) []Pin {
	var out []Pin
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, ref := range shellDaemonRef.FindAllString(line, -1) {
			image, tag, digest := splitImageRef(ref)
			if !isDaemonImage(image) {
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

// commentProvenanceAbove reads the marker out of the contiguous block of
// `#` comment lines immediately above the byte at off. Adjacency is what
// scopes it: a commented-out example elsewhere in the file is separated
// from the live line by the live line's own neighbours.
func commentProvenanceAbove(body []byte, off int) string {
	lines := strings.Split(string(body[:off]), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			return ""
		}
		if m := provenanceMarker.FindStringSubmatch(line); m != nil {
			return m[1]
		}
	}
	return ""
}

// splitImageRef splits `name[:tag][@digest]` the way a registry client
// does: the digest wins if both are present, and a colon inside a
// registry's host:port is not a tag separator.
func splitImageRef(ref string) (image, tag, digest string) {
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
	// rule, and :latest is unorderable — say so rather than reading it as
	// "no pin".
	if tag == "" && digest == "" {
		tag = "latest"
	}
	return image, tag, digest
}
