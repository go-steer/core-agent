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

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/agentenv"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/instruction"
	"github.com/go-steer/core-agent/v2/pkg/skills"
)

// writeEnvBundle lays out a minimal bundle: <root>/.agents/AGENTS.md
// plus one skill under <root>/.agents/skills/, both referencing env
// vars the way a deployed recipe does. Returns the project root and the
// agents dir, in the shape run() resolves them.
func writeEnvBundle(t *testing.T, agentsMD, skillMD string) (projectRoot, agentsDir string) {
	t.Helper()
	projectRoot = t.TempDir()
	agentsDir = filepath.Join(projectRoot, ".agents")
	skillDir := filepath.Join(agentsDir, "skills", "triage")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "AGENTS.md"), []byte(agentsMD), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillMD), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectRoot, agentsDir
}

// loadBundle runs the boot-time loaders exactly as run() wires them:
// one interpolator from newEnvInterpolator, threaded into the
// instruction load and the skill load.
func loadBundle(t *testing.T, projectRoot, agentsDir string, r *agentenv.Resolver) (string, *agentenv.ResidualRefs) {
	t.Helper()
	interp, residual := newEnvInterpolator(r)
	loaded, err := instruction.Load(projectRoot, "", instruction.WithInterpolator(interp))
	if err != nil {
		t.Fatalf("instruction.Load: %v", err)
	}
	if _, err := skills.LoadAll(context.Background(), agentsDir, "", nil, skills.WithInterpolator(interp)); err != nil {
		t.Fatalf("skills.LoadAll: %v", err)
	}
	return loaded.Instruction, residual
}

// TestEnvResidual_NoManifestIsReported is the #712 regression. With no
// .agents/env.yaml, LoadManifest returns nil, NewResolver returns a nil
// *Resolver, InterpolateFunc returns nil, and every loader honours that
// as "no interpolation" — so the persona reaches the model still saying
// ${env:GOOGLE_CLOUD_PROJECT}. That degradation is unchanged and
// deliberate; what must not happen any more is it happening silently.
//
// Pre-fix this fails at the final check: the loaders got a nil
// interpolator, nothing scanned the loaded bodies, and Warning() is
// empty.
func TestEnvResidual_NoManifestIsReported(t *testing.T) {
	t.Parallel()
	projectRoot, agentsDir := writeEnvBundle(t,
		"You operate in project ${env:GOOGLE_CLOUD_PROJECT}.\n",
		"---\nname: triage\ndescription: Triage incidents\n---\n\nAddress clusters in ${env:GKE_LOCATION}.\n")

	manifest, err := agentenv.LoadManifest(agentsDir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if manifest != nil {
		t.Fatalf("fixture writes no manifest, got %+v", manifest)
	}
	resolver := agentenv.NewResolver(manifest, os.LookupEnv)

	prompt, residual := loadBundle(t, projectRoot, agentsDir, resolver)

	// The defect itself, pinned so a future change to interpolation
	// semantics has to come past this test deliberately.
	if !strings.Contains(prompt, "${env:GOOGLE_CLOUD_PROJECT}") {
		t.Fatalf("expected the unsubstituted placeholder in the loaded persona; got:\n%s", prompt)
	}

	w := residual.Warning(agentsDir)
	if w == "" {
		t.Fatal("no boot warning for a bundle whose persona still contains ${env:...} and has no manifest")
	}
	for _, want := range []string{"${env:GOOGLE_CLOUD_PROJECT}", "${env:GKE_LOCATION}", "interpolation is OFF", agentsDir} {
		if !strings.Contains(w, want) {
			t.Errorf("warning missing %q; got: %s", want, w)
		}
	}
}

// TestEnvResidual_ManifestPresentStaysQuiet is the other half of the
// contract: a bundle that declares its vars and has them set must
// interpolate exactly as before and say nothing. Without this, "warn on
// ${env:" would be a boot line on every healthy deployment.
func TestEnvResidual_ManifestPresentStaysQuiet(t *testing.T) {
	projectRoot, agentsDir := writeEnvBundle(t,
		"You operate in project ${env:RESIDUAL_TEST_PROJECT}.\n",
		"---\nname: triage\ndescription: Triage incidents\n---\n\nAddress clusters in ${env:RESIDUAL_TEST_LOCATION}.\n")
	writeManifest(t, agentsDir, "RESIDUAL_TEST_PROJECT", "RESIDUAL_TEST_LOCATION")
	t.Setenv("RESIDUAL_TEST_PROJECT", "acme-prod")
	t.Setenv("RESIDUAL_TEST_LOCATION", "us-central1")

	manifest, err := agentenv.LoadManifest(agentsDir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	resolver := agentenv.NewResolver(manifest, os.LookupEnv)
	if errs := resolver.Errors(); len(errs) > 0 {
		t.Fatalf("resolver errors: %v", errs)
	}

	prompt, residual := loadBundle(t, projectRoot, agentsDir, resolver)
	if !strings.Contains(prompt, "acme-prod") {
		t.Errorf("interpolation regressed; persona:\n%s", prompt)
	}
	if w := residual.Warning(agentsDir); w != "" {
		t.Errorf("healthy bundle produced a warning: %s", w)
	}
}

// TestEnvResidual_RootedSubagent covers the shape the issue's live
// evidence came from: a declarative subagent with its own content root
// loads its OWN AGENTS.md through the same interpolator, and pre-fix
// that path was every bit as silent as the parent's.
func TestEnvResidual_RootedSubagent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"),
		[]byte("Investigate clusters in ${env:GKE_CLUSTER}.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var noManifest *agentenv.Resolver
	interp, residual := newEnvInterpolator(noManifest)
	got, err := rootedSubagentInstruction(config.SubagentSpec{Name: "cluster", Root: root}, root, interp)
	if err != nil {
		t.Fatalf("rootedSubagentInstruction: %v", err)
	}
	if !strings.Contains(got, "${env:GKE_CLUSTER}") {
		t.Fatalf("expected the unsubstituted placeholder in the subagent persona; got:\n%s", got)
	}
	w := residual.Warning("")
	if !strings.Contains(w, "${env:GKE_CLUSTER}") {
		t.Fatalf("rooted subagent content not reported; warning: %q", w)
	}
}

func writeManifest(t *testing.T, agentsDir string, names ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("version: 1\nenv:\n")
	for _, n := range names {
		b.WriteString("  - name: " + n + "\n    required: true\n")
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "env.yaml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
