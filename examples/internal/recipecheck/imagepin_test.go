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

package recipecheck_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/examples/internal/recipecheck"
)

// releasedForTest is a fixed release set, so the synthetic cases below
// keep testing the rules and not the state of CHANGELOG.md.
func releasedForTest(t *testing.T) []recipecheck.Version {
	t.Helper()
	var out []recipecheck.Version
	for _, s := range []string{"2.7.0", "2.8.0", "2.9.0-dev.1"} {
		v, err := recipecheck.ParseVersion(s)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", s, err)
		}
		out = append(out, v)
	}
	return out
}

// gatedConfig is a minimal recipe config that needs a v2.9 daemon: an
// alert target, which is what registers the `alert` tool.
const gatedConfig = `{
  "version": 1,
  "model": {"name": "gemini-3.5-flash", "provider": "vertex"},
  "alerts": {"targets": [{"name": "oncall", "url_env": "HOOK", "template": "generic"}]}
}`

// ungatedConfig uses nothing version-gated.
const ungatedConfig = `{
  "version": 1,
  "model": {"name": "gemini-3.5-flash", "provider": "vertex"}
}`

// writeRecipe lays out a recipe with the shape all three deployable
// recipes share — config root, deploy/base, deploy/overlays/<name> —
// and returns the examples dir and the Recipe to check.
func writeRecipe(t *testing.T, cfgJSON, overlayKustomization string) (string, recipecheck.Recipe) {
	t.Helper()
	examples := t.TempDir()
	cfgDir := filepath.Join(examples, "recipe", "deploy", "base", "config")
	overlay := filepath.Join(examples, "recipe", "deploy", "overlays", "example")
	for _, d := range []string{cfgDir, overlay} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cfgDir, "config.json"), cfgJSON)
	// The marker Discover looks for, and the file the base ConfigMap
	// would carry.
	write(filepath.Join(cfgDir, "AGENTS.md"), "# test recipe\n")
	write(filepath.Join(examples, "recipe", "deploy", "base", "kustomization.yaml"),
		"resources:\n  - 50-deployment.yaml\n")
	write(filepath.Join(examples, "recipe", "deploy", "base", "50-deployment.yaml"),
		"apiVersion: apps/v1\nkind: Deployment\n")
	write(filepath.Join(overlay, "kustomization.yaml"), overlayKustomization)
	return examples, recipecheck.Recipe{Name: "recipe", Dir: cfgDir}
}

func overlayPinning(ref string) string {
	if strings.HasPrefix(ref, "sha256:") {
		return "resources:\n  - ../../base\nimages:\n  - name: ghcr.io/go-steer/core-agent\n    digest: " + ref + "\n"
	}
	return "resources:\n  - ../../base\nimages:\n  - name: ghcr.io/go-steer/core-agent\n    newTag: \"" + ref + "\"\n"
}

// TestCheckDeployPins is the hermetic half of the #680 gate. The real
// tree is checked by TestOverlayPinsSatisfyRecipeConfig in
// allrecipes_test.go, but a tree whose pins are correct cannot prove
// the check has teeth — these cases can, and they keep proving it after
// the pins are fixed.
func TestCheckDeployPins(t *testing.T) {
	t.Parallel()
	released := releasedForTest(t)
	for _, tc := range []struct {
		name    string
		cfg     string
		overlay string
		want    string // substring of the expected finding; "" means clean
	}{
		{
			name:    "a pin older than the config's gated features",
			cfg:     gatedConfig,
			overlay: overlayPinning("2.8.0"),
			want:    "requires ≥ 2.9.0-dev.1",
		},
		{
			name:    "a pin naming a release that was never cut",
			cfg:     gatedConfig,
			overlay: overlayPinning("2.9.0"),
			want:    "is not a version this repo has released",
		},
		{
			name:    "a floating tag cannot be ordered",
			cfg:     gatedConfig,
			overlay: overlayPinning("main-1a2b3c4"),
			want:    "a floating tag carries no version",
		},
		{
			name:    "latest cannot be ordered either",
			cfg:     gatedConfig,
			overlay: overlayPinning("latest"),
			want:    "a floating tag carries no version",
		},
		{
			name:    "a bare digest cannot be ordered",
			cfg:     gatedConfig,
			overlay: overlayPinning("sha256:" + strings.Repeat("0", 64)),
			want:    "a digest carries no version",
		},
		{
			name: "a digest that records the release it came from is fine",
			cfg:  gatedConfig,
			overlay: "resources:\n  - ../../base\nimages:\n" +
				"  # core-agent-version: 2.9.0-dev.1\n" +
				"  - name: ghcr.io/go-steer/core-agent\n    digest: sha256:" + strings.Repeat("0", 64) + "\n",
		},
		{
			name: "a digest recording a release that is too old is still caught",
			cfg:  gatedConfig,
			overlay: "resources:\n  - ../../base\nimages:\n" +
				"  # core-agent-version: 2.8.0\n" +
				"  - name: ghcr.io/go-steer/core-agent\n    digest: sha256:" + strings.Repeat("0", 64) + "\n",
			want: "requires ≥ 2.9.0-dev.1",
		},
		{
			name: "a marker in a commented-out example does not vouch for a live digest",
			cfg:  gatedConfig,
			overlay: "resources:\n  - ../../base\nimages:\n" +
				"  - name: ghcr.io/go-steer/core-agent\n    digest: sha256:" + strings.Repeat("0", 64) + "\n" +
				"\n  # (B) example — digest pinning, for the docs:\n" +
				"  # core-agent-version: 2.9.0-dev.1\n" +
				"  # - name: ghcr.io/go-steer/core-agent\n" +
				"  #   digest: sha256:" + strings.Repeat("1", 64) + "\n",
			want: "a digest carries no version",
		},
		{
			name:    "no pin at all leaves the base's image unconstrained",
			cfg:     gatedConfig,
			overlay: "resources:\n  - ../../base\n",
			want:    "nothing in this overlay's composition names a core-agent daemon image",
		},
		{
			name:    "an exact match on the floor passes",
			cfg:     gatedConfig,
			overlay: overlayPinning("2.9.0-dev.1"),
		},
		{
			name:    "a config with nothing gated still may not float",
			cfg:     ungatedConfig,
			overlay: overlayPinning("main-1a2b3c4"),
			want:    "a floating tag carries no version",
		},
		{
			name:    "...nor name a release that does not exist",
			cfg:     ungatedConfig,
			overlay: overlayPinning("2.9.0"),
			want:    "is not a version this repo has released",
		},
		{
			name:    "an ungated config on a released pin is clean",
			cfg:     ungatedConfig,
			overlay: overlayPinning("2.8.0"),
		},
		{
			name:    "the slim daemon variant is a daemon pin too",
			cfg:     gatedConfig,
			overlay: "resources:\n  - ../../base\nimages:\n  - name: ghcr.io/go-steer/core-agent-slim\n    newTag: \"2.8.0\"\n",
			want:    "requires ≥ 2.9.0-dev.1",
		},
		{
			name:    "a pin on some other image is not a daemon pin",
			cfg:     gatedConfig,
			overlay: "resources:\n  - ../../base\nimages:\n  - name: ghcr.io/go-steer/lookout\n    newTag: \"v0.18.0\"\n",
			want:    "nothing in this overlay's composition names a core-agent daemon image",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			examples, r := writeRecipe(t, tc.cfg, tc.overlay)
			findings, err := recipecheck.CheckDeployPins(examples, r, released)
			if err != nil {
				t.Fatalf("CheckDeployPins: %v", err)
			}
			joined := joinFindings(findings)
			switch {
			case tc.want == "" && len(findings) > 0:
				t.Fatalf("want clean, got:\n%s", joined)
			case tc.want != "" && !strings.Contains(joined, tc.want):
				t.Fatalf("want a finding containing %q, got:\n%s", tc.want, joined)
			}
		})
	}
}

// TestCheckDeployPinsResolvesThroughComposition covers the shape
// gke-troubleshoot-agent's example-otel overlay has: an overlay that
// composes another overlay instead of restating its pin. kustomize
// applies the composed overlay's images transformer, so the pin is real
// and the check must find it rather than report "no pin".
func TestCheckDeployPinsResolvesThroughComposition(t *testing.T) {
	t.Parallel()
	examples, r := writeRecipe(t, gatedConfig, overlayPinning("2.8.0"))
	otel := filepath.Join(examples, "recipe", "deploy", "overlays", "example-otel")
	if err := os.MkdirAll(otel, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otel, "kustomization.yaml"),
		[]byte("resources:\n  - ../example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := recipecheck.CheckDeployPins(examples, r, releasedForTest(t))
	if err != nil {
		t.Fatalf("CheckDeployPins: %v", err)
	}
	joined := joinFindings(findings)
	if !strings.Contains(joined, "overlays/example-otel") {
		t.Errorf("the composing overlay was not judged at all:\n%s", joined)
	}
	if strings.Contains(joined, "no `images:` entry for the daemon") {
		t.Errorf("the composed overlay's pin was not resolved through `resources`:\n%s", joined)
	}
	if !strings.Contains(joined, "overlays/example/kustomization.yaml") {
		t.Errorf("the finding does not point at the file that actually carries the pin:\n%s", joined)
	}
}

// TestCheckDeployPinsReadsTheBaseManifest covers the shape gke-deploy
// has: no `images:` transformer entry anywhere (every example in the
// overlay is commented out), the daemon pinned by a plain
// `image: ghcr.io/...` on the base Deployment. An earlier revision of
// this check stopped after the transformer and reported that recipe as
// unpinned while a perfectly good pin sat one file away.
func TestCheckDeployPinsReadsTheBaseManifest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		image string
		want  string
	}{
		{
			name:  "a released tag at the floor on the base Deployment satisfies the check",
			image: "          image: ghcr.io/go-steer/core-agent:2.9.0-dev.1\n",
		},
		{
			name:  "a too-old tag on the base Deployment is caught",
			image: "          image: ghcr.io/go-steer/core-agent:2.7.0\n",
			want:  "requires ≥ 2.9.0-dev.1",
		},
		{
			name:  "a bare digest on the base Deployment cannot be ordered",
			image: "          image: ghcr.io/go-steer/core-agent@sha256:" + strings.Repeat("0", 64) + "\n",
			want:  "a digest carries no version",
		},
		{
			name: "a digest with the provenance comment above it is ordered",
			image: "          # core-agent-version: 2.9.0-dev.1\n" +
				"          image: ghcr.io/go-steer/core-agent@sha256:" + strings.Repeat("0", 64) + "\n",
		},
		{
			name: "a commented-out image line is not a pin",
			image: "          # image: ghcr.io/go-steer/core-agent:2.8.0\n" +
				"          image: ghcr.io/go-steer/core-agent:2.9.0-dev.1\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			examples, r := writeRecipe(t, gatedConfig, "resources:\n  - ../../base\n")
			deployment := filepath.Join(examples, "recipe", "deploy", "base", "50-deployment.yaml")
			body := "apiVersion: apps/v1\nkind: Deployment\nspec:\n  template:\n    spec:\n      containers:\n" +
				"        - name: core-agent\n" + tc.image
			if err := os.WriteFile(deployment, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			findings, err := recipecheck.CheckDeployPins(examples, r, releasedForTest(t))
			if err != nil {
				t.Fatalf("CheckDeployPins: %v", err)
			}
			joined := joinFindings(findings)
			switch {
			case tc.want == "" && len(findings) > 0:
				t.Fatalf("want clean, got:\n%s", joined)
			case tc.want != "" && !strings.Contains(joined, tc.want):
				t.Fatalf("want a finding containing %q, got:\n%s", tc.want, joined)
			}
		})
	}
}

// TestCheckDeployPinsUnionsEveryConfigTheRecipeShips is the
// kube-platform-agent shape: the recipe ships config.json AND
// config.hub.json, and the daemon Deployment runs
// `-c .../config.hub.json`. Judging the pin against the config.json that
// Discover happened to key on judges the wrong file — the hub config has
// the larger gated surface, and it is the one that deploys.
func TestCheckDeployPinsUnionsEveryConfigTheRecipeShips(t *testing.T) {
	t.Parallel()
	examples, r := writeRecipe(t, ungatedConfig, overlayPinning("2.8.0"))
	hub := filepath.Join(examples, "recipe", "deploy", "base", "config", "config.hub.json")
	if err := os.WriteFile(hub, []byte(gatedConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	findings, err := recipecheck.CheckDeployPins(examples, r, releasedForTest(t))
	if err != nil {
		t.Fatalf("CheckDeployPins: %v", err)
	}
	joined := joinFindings(findings)
	if !strings.Contains(joined, "requires ≥ 2.9.0-dev.1") {
		t.Fatalf("the variant config's floor was not applied to the pin:\n%s", joined)
	}
}

// TestCheckDeployPinsWithoutManifests: most examples are local-run
// recipes with no deploy tree. There is no pin to be wrong, so there is
// nothing to say.
func TestCheckDeployPinsWithoutManifests(t *testing.T) {
	t.Parallel()
	examples, r := writeLocalRecipe(t, gatedConfig, nil)
	findings, err := recipecheck.CheckDeployPins(examples, r, releasedForTest(t))
	if err != nil {
		t.Fatalf("CheckDeployPins: %v", err)
	}
	if len(findings) > 0 {
		t.Fatalf("want clean, got:\n%s", joinFindings(findings))
	}
}

// writeLocalRecipe lays out a manifest-less recipe — a config root under
// the recipe dir, plus whatever extra files the case needs, keyed by
// path relative to the recipe root.
func writeLocalRecipe(t *testing.T, cfgJSON string, extra map[string]string) (string, recipecheck.Recipe) {
	t.Helper()
	examples := t.TempDir()
	dir := filepath.Join(examples, "local", ".agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"config.json": cfgJSON, "AGENTS.md": "# local\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for rel, body := range extra {
		p := filepath.Join(examples, "local", rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return examples, recipecheck.Recipe{Name: "local", Dir: dir}
}

// TestCheckDeployPinsInDockerfile covers the shape cloud-run-deploy has:
// no manifests at all, the daemon pinned by a Dockerfile `FROM` that
// bakes the recipe's .agents/ bundle onto the image. A check that only
// read kustomizations called that recipe clean while its FROM sat on
// the rolling `:main` tag for five releases.
func TestCheckDeployPinsInDockerfile(t *testing.T) {
	t.Parallel()
	released := releasedForTest(t)
	for _, tc := range []struct {
		name  string
		cfg   string
		files map[string]string
		want  string // substring of the expected finding; "" means clean
	}{
		{
			name:  "a FROM older than the config's gated features",
			cfg:   gatedConfig,
			files: map[string]string{"Dockerfile": "FROM ghcr.io/go-steer/core-agent:2.8.0\nCOPY .agents/ /etc/core-agent/.agents/\n"},
			want:  "requires ≥ 2.9.0-dev.1",
		},
		{
			name:  "a FROM on the rolling main tag",
			cfg:   gatedConfig,
			files: map[string]string{"Dockerfile": "FROM ghcr.io/go-steer/core-agent:main\n"},
			want:  "a floating tag carries no version",
		},
		{
			name:  "an implicit :latest is read as :latest, not as no pin",
			cfg:   gatedConfig,
			files: map[string]string{"Dockerfile": "FROM ghcr.io/go-steer/core-agent\n"},
			want:  "a floating tag carries no version",
		},
		{
			name:  "a FROM naming a release that was never cut",
			cfg:   gatedConfig,
			files: map[string]string{"Dockerfile": "FROM ghcr.io/go-steer/core-agent:2.9.0\n"},
			want:  "is not a version this repo has released",
		},
		{
			name:  "--platform and an AS stage name do not hide the ref",
			cfg:   gatedConfig,
			files: map[string]string{"Dockerfile": "FROM --platform=$BUILDPLATFORM ghcr.io/go-steer/core-agent:2.8.0 AS base\n"},
			want:  "requires ≥ 2.9.0-dev.1",
		},
		{
			name:  "a digest FROM with provenance is fine",
			cfg:   gatedConfig,
			files: map[string]string{"Dockerfile": "# core-agent-version: 2.9.0-dev.1\nFROM ghcr.io/go-steer/core-agent@sha256:" + strings.Repeat("0", 64) + "\n"},
		},
		{
			name:  "a digest FROM without provenance cannot be ordered",
			cfg:   gatedConfig,
			files: map[string]string{"Dockerfile": "FROM ghcr.io/go-steer/core-agent@sha256:" + strings.Repeat("0", 64) + "\n"},
			want:  "a digest carries no version",
		},
		{
			name:  "a FROM at the floor passes",
			cfg:   gatedConfig,
			files: map[string]string{"Dockerfile": "FROM ghcr.io/go-steer/core-agent:2.9.0-dev.1\n"},
		},
		{
			name:  "a content image Dockerfile is not a daemon pin",
			cfg:   gatedConfig,
			files: map[string]string{"build/content.Dockerfile": "FROM scratch\nCOPY . /recipe\n"},
		},
		{
			name:  "a Dockerfile nested under the recipe is still read",
			cfg:   gatedConfig,
			files: map[string]string{"build/Dockerfile.runtime": "FROM ghcr.io/go-steer/core-agent:2.8.0\n"},
			want:  "requires ≥ 2.9.0-dev.1",
		},
		{
			name:  "an ungated config may not sit on a floating FROM either",
			cfg:   ungatedConfig,
			files: map[string]string{"Dockerfile": "FROM ghcr.io/go-steer/core-agent:main\n"},
			want:  "a floating tag carries no version",
		},
		{
			name:  "...nor on a release that does not exist",
			cfg:   ungatedConfig,
			files: map[string]string{"Dockerfile": "FROM ghcr.io/go-steer/core-agent:2.9.0\n"},
			want:  "is not a version this repo has released",
		},
		{
			name: "an ARG default is substituted into the FROM",
			cfg:  ungatedConfig,
			files: map[string]string{"Dockerfile": "ARG CORE_AGENT_VERSION=2.8.0\n" +
				"FROM ghcr.io/go-steer/core-agent-slim:${CORE_AGENT_VERSION}\n"},
		},
		{
			name: "an ARG default that is too old is caught through the substitution",
			cfg:  gatedConfig,
			files: map[string]string{"Dockerfile": "ARG CORE_AGENT_VERSION=2.8.0\n" +
				"FROM ghcr.io/go-steer/core-agent-slim:${CORE_AGENT_VERSION}\n"},
			want: "requires ≥ 2.9.0-dev.1",
		},
		{
			name:  "a FROM interpolating an ARG with no default stays unorderable",
			cfg:   ungatedConfig,
			files: map[string]string{"Dockerfile": "FROM ghcr.io/go-steer/core-agent:${CORE_AGENT_VERSION}\n"},
			want:  "a floating tag carries no version",
		},
		{
			name: "a deploy script's default image reference is a pin",
			cfg:  gatedConfig,
			files: map[string]string{"scripts/deploy.sh": "#!/usr/bin/env bash\n" +
				"IMAGE_REF=\"${IMAGE_REF:-ghcr.io/go-steer/core-agent:2.8.0}\"\n"},
			want: "requires ≥ 2.9.0-dev.1",
		},
		{
			name: "a script reference assembled from a variable is reported, not skipped",
			cfg:  ungatedConfig,
			files: map[string]string{"scripts/deploy.sh": "#!/usr/bin/env bash\n" +
				"IMAGE_REF=\"ghcr.io/go-steer/core-agent:${IMAGE_TAG}\"\n"},
			want: "a floating tag carries no version",
		},
		{
			name: "a daemon reference inside a script comment is not a pin",
			cfg:  ungatedConfig,
			files: map[string]string{"scripts/deploy.sh": "#!/usr/bin/env bash\n" +
				"# e.g. IMAGE_REF=ghcr.io/go-steer/core-agent:main\n" +
				"IMAGE_REF=\"${IMAGE_REF:-ghcr.io/go-steer/core-agent:2.8.0}\"\n"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			examples, r := writeLocalRecipe(t, tc.cfg, tc.files)
			findings, err := recipecheck.CheckDeployPins(examples, r, released)
			if err != nil {
				t.Fatalf("CheckDeployPins: %v", err)
			}
			joined := joinFindings(findings)
			switch {
			case tc.want == "" && len(findings) > 0:
				t.Fatalf("want clean, got:\n%s", joined)
			case tc.want != "" && !strings.Contains(joined, tc.want):
				t.Fatalf("want a finding containing %q, got:\n%s", tc.want, joined)
			}
		})
	}
}

func joinFindings(fs []recipecheck.VersionFinding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString("  " + f.String() + "\n")
	}
	return b.String()
}
