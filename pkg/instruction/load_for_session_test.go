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

package instruction_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/pkg/instruction"
)

func writeMemoryFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s/%s: %v", dir, name, err)
	}
}

func TestLoadForSession_NoOverlayWhenUsersDirEmpty(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	projectAgents := filepath.Join(root, ".agents")
	writeMemoryFile(t, projectAgents, "AGENTS.md", "project rules")

	withOverlay, err := instruction.LoadForSession(root, "", "alice@example.com", "")
	if err != nil {
		t.Fatalf("LoadForSession: %v", err)
	}
	withoutOverlay, err := instruction.Load(root, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if withOverlay.Instruction != withoutOverlay.Instruction {
		t.Errorf("empty usersDir must behave identically to Load\n got:  %q\n want: %q", withOverlay.Instruction, withoutOverlay.Instruction)
	}
}

func TestLoadForSession_NoOverlayWhenCallerEmpty(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	usersDir := t.TempDir()
	projectAgents := filepath.Join(root, ".agents")
	writeMemoryFile(t, projectAgents, "AGENTS.md", "project rules")

	// Overlay dir exists for a caller, but we pass callerIdentity=""
	// → overlay must not load.
	overlayAgents := filepath.Join(usersDir, "alice@example.com", ".agents")
	writeMemoryFile(t, overlayAgents, "AGENTS.md", "alice-only overlay")

	loaded, err := instruction.LoadForSession(root, "", "", usersDir)
	if err != nil {
		t.Fatalf("LoadForSession: %v", err)
	}
	if strings.Contains(loaded.Instruction, "alice-only overlay") {
		t.Errorf("empty callerIdentity should NOT load any overlay; got:\n%s", loaded.Instruction)
	}
}

func TestLoadForSession_OverlayAppendedAfterProject(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	usersDir := t.TempDir()
	writeMemoryFile(t, filepath.Join(root, ".agents"), "AGENTS.md", "project body")
	writeMemoryFile(t, filepath.Join(usersDir, "alice@example.com", ".agents"), "AGENTS.md", "alice overlay body")

	loaded, err := instruction.LoadForSession(root, "", "alice@example.com", usersDir)
	if err != nil {
		t.Fatalf("LoadForSession: %v", err)
	}
	if !strings.Contains(loaded.Instruction, "project body") {
		t.Errorf("project content missing from output:\n%s", loaded.Instruction)
	}
	if !strings.Contains(loaded.Instruction, "alice overlay body") {
		t.Errorf("alice overlay missing from output:\n%s", loaded.Instruction)
	}
	// Ordering: project should appear before the overlay (overlay
	// appended after project's loadScopeWithFallback call).
	projectIdx := strings.Index(loaded.Instruction, "project body")
	overlayIdx := strings.Index(loaded.Instruction, "alice overlay body")
	if projectIdx > overlayIdx {
		t.Errorf("overlay should be appended AFTER project content; got project at %d, overlay at %d", projectIdx, overlayIdx)
	}
}

func TestLoadForSession_OverlayAppearsInSources(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	usersDir := t.TempDir()
	writeMemoryFile(t, filepath.Join(root, ".agents"), "AGENTS.md", "project body")
	writeMemoryFile(t, filepath.Join(usersDir, "alice@example.com", ".agents"), "AGENTS.md", "alice overlay body")

	loaded, err := instruction.LoadForSession(root, "", "alice@example.com", usersDir)
	if err != nil {
		t.Fatalf("LoadForSession: %v", err)
	}
	var sawCallerScope bool
	for _, s := range loaded.Sources {
		if s.Scope == "caller" {
			sawCallerScope = true
			break
		}
	}
	if !sawCallerScope {
		t.Errorf("expected at least one Source with Scope=\"caller\"; got %+v", loaded.Sources)
	}
}

func TestLoadForSession_MissingOverlayDirSilentlySkipped(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	usersDir := t.TempDir() // empty — alice's overlay dir doesn't exist
	writeMemoryFile(t, filepath.Join(root, ".agents"), "AGENTS.md", "project body")

	loaded, err := instruction.LoadForSession(root, "", "alice@example.com", usersDir)
	if err != nil {
		t.Fatalf("missing overlay dir should NOT error; got %v", err)
	}
	if !strings.Contains(loaded.Instruction, "project body") {
		t.Errorf("project content missing despite missing overlay")
	}
}

func TestLoadForSession_RejectsPathTraversalIdentity(t *testing.T) {
	t.Parallel()
	cases := []string{
		"..",
		"../etc",
		"alice/../bob",
		`alice\windows`,
		"alice/passwd",
	}
	for _, id := range cases {
		t.Run(id, func(t *testing.T) {
			_, err := instruction.LoadForSession("", "", id, "/tmp")
			if err == nil {
				t.Fatalf("expected error for path-traversal identity %q", id)
			}
			if !errors.Is(err, instruction.ErrInvalidCallerIdentity) {
				t.Errorf("expected ErrInvalidCallerIdentity, got %v", err)
			}
		})
	}
}

func TestLoadForSession_AcceptsEmailAndServiceAccountIdentities(t *testing.T) {
	t.Parallel()
	// Legitimate identity shapes (email, sa-marker) must pass through.
	// These tests don't need a populated overlay — we just verify the
	// validation doesn't fail-fast on the identity itself.
	for _, id := range []string{"alice@example.com", "sa:slack-bot", "alice", "bob_smith", "user-1"} {
		t.Run(id, func(t *testing.T) {
			_, err := instruction.LoadForSession("", "", id, t.TempDir())
			if err != nil {
				t.Errorf("identity %q should be accepted, got error: %v", id, err)
			}
		})
	}
}
