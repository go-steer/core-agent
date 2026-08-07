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

package compose

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// TestReproduceAgent_ThreadsContentRoots is the regression gate proving the
// multi-session factory honors SessionFactoryDeps.ContentRoots — the primary
// hub/attach deployment path for the external-content-root feature. Without
// the threading in attachProviderOpts, a session's /memory and /skills views
// would silently omit the operator-declared external tree even though the
// daemon-wide startup load includes it. Asserting through the adapter's
// AttachMemory / AttachSkills (the real provider closures) means this fails if
// either WithContentRoots call in multi_session.go is dropped.
func TestReproduceAgent_ThreadsContentRoots(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// An external tree that is NOT the project root: its own AGENTS.md and a
	// skill under skills/. This stands in for an unmodified external checkout
	// declared via content_roots / --agents-content-dir.
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "AGENTS.md"), []byte("EXTERNAL_PLATFORM_PERSONA\n"), 0o644); err != nil {
		t.Fatalf("write external AGENTS.md: %v", err)
	}
	skillDir := filepath.Join(external, "skills", "gke-reliability")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir external skill: %v", err)
	}
	skillBody := "---\nname: gke-reliability\ndescription: external reliability skill\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillBody), 0o644); err != nil {
		t.Fatalf("write external SKILL.md: %v", err)
	}

	// A project root with nothing of its own — so anything that surfaces came
	// from the external content root, not the project scope.
	project := t.TempDir()

	deps := SessionFactoryDeps{
		DaemonCtx:    ctx,
		Model:        stubLLM{},
		Template:     permissions.New(permissions.Options{}),
		ProjectRoot:  project,
		AgentsDir:    project,
		ContentRoots: []string{external},
	}

	ag, cancelAg, err := ReproduceAgent(deps, auth.Anonymous, "sid-content", "created")
	if err != nil {
		t.Fatalf("ReproduceAgent: %v", err)
	}
	t.Cleanup(cancelAg)

	// Memory: the external AGENTS.md surfaces as a "content"-scoped source.
	var sawExternalMemory bool
	for _, m := range ag.AttachMemory() {
		if m.Scope == "content" && strings.HasPrefix(m.Path, external) {
			sawExternalMemory = true
		}
	}
	if !sawExternalMemory {
		t.Errorf("AttachMemory did not include the external content root; content_roots not threaded into the memory provider: %+v", ag.AttachMemory())
	}

	// Skills: the external skill is discovered through the skills provider.
	var sawExternalSkill bool
	for _, s := range ag.AttachSkills() {
		if s.Name == "gke-reliability" {
			sawExternalSkill = true
		}
	}
	if !sawExternalSkill {
		t.Errorf("AttachSkills did not include the external skill; content_roots not threaded into the skills provider: %+v", ag.AttachSkills())
	}
}
