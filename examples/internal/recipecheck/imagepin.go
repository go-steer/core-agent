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
//
// Finding the pins is not this file's job — internal/imagepin does
// that, scoped to whatever image family it is handed. This file
// supplies the daemon family and all of the judgment. The split exists
// because a second consumer asks a different question of the same
// declaration sites (dev/lookout-pin-check, #787: "is this pin still
// current upstream?"), and it must not drag a network call into
// test-unit to do it.

package recipecheck

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/go-steer/core-agent/v2/internal/imagepin"
	"github.com/go-steer/core-agent/v2/pkg/config"
)

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
// The match is POSITIONAL, not file-scoped; internal/imagepin's
// commentOnEntry / CommentMarkerAbove do the scoping. See the comment
// there for why (every one of these kustomizations carries a
// commented-out digest example, complete with a marker).
var provenanceMarker = regexp.MustCompile(`(?m)^\s*#*\s*core-agent-version:\s*(\S+)\s*$`)

// daemonFamily are the image names that carry a core-agent daemon, so a
// pin on one of them is a pin on the runtime this recipe's config talks
// to. core-agent-tui is deliberately absent: it is an operator's client,
// not the thing that reads config.json.
var daemonFamily = imagepin.NewFamily(provenanceMarker,
	"ghcr.io/go-steer/core-agent",
	"ghcr.io/go-steer/core-agent-slim",
)

// Pin is one image reference a deploy artifact resolves to.
type Pin = imagepin.Pin

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
// the bug is identical in each: a kustomize overlay's `images:`
// transformer, a container `image:` in a manifest the overlay composes,
// a recipe Dockerfile's `FROM`, and a literal daemon reference in a
// deploy script. internal/imagepin documents each and its limits.
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
	data, err := os.ReadFile(path) //nolint:gosec // discovered recipe config path
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
	overlays, err := imagepin.OverlayDirs(filepath.Join(root, "deploy", "overlays"))
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
		pin, err := imagepin.Resolve(dir, daemonFamily)
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
	files, err := imagepin.PinBearingFiles(root)
	if err != nil {
		return nil, fmt.Errorf("recipecheck: %s: %w", r.Name, err)
	}
	var findings []VersionFinding
	for _, f := range files {
		body, readErr := os.ReadFile(f.Path) //nolint:gosec // discovered recipe path
		if readErr != nil {
			return nil, fmt.Errorf("recipecheck: %s: %s: %w", r.Name, rel(f.Path), readErr)
		}
		for _, pin := range f.Extract(body, daemonFamily) {
			for _, finding := range judgePin(&pin, req, released) {
				finding.Overlay = rel(f.Path)
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
