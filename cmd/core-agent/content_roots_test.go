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
	"reflect"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/instruction"
	"github.com/go-steer/core-agent/v2/pkg/skills"
)

// TestResolveContentRoots covers the merge + resolve rules the run() wiring
// depends on: config-first CLI-after order, relative-against-base resolution,
// absolute passthrough, empty-entry dropping, and the nil-when-empty no-op.
func TestResolveContentRoots(t *testing.T) {
	t.Parallel()
	base := filepath.FromSlash("/agents/dir")

	cases := []struct {
		name     string
		cfgRoots []string
		cliDirs  []string
		want     []string
	}{
		{"nil both", nil, nil, nil},
		{"empty both", []string{}, []string{}, nil},
		{
			"config only, relative resolved against base",
			[]string{"../ext/platform"}, nil,
			[]string{filepath.Join(base, "../ext/platform")},
		},
		{
			"absolute passes through unchanged",
			[]string{filepath.FromSlash("/opt/agents")}, nil,
			[]string{filepath.FromSlash("/opt/agents")},
		},
		{
			"config first, then CLI, order preserved",
			[]string{"a"}, []string{"b", "c"},
			[]string{filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "c")},
		},
		{
			"empty/whitespace entries dropped",
			[]string{"", "  ", "keep"}, []string{"\t"},
			[]string{filepath.Join(base, "keep")},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := resolveContentRoots(c.cfgRoots, c.cliDirs, base)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("resolveContentRoots(%v, %v, %q) = %v, want %v", c.cfgRoots, c.cliDirs, base, got, c.want)
			}
		})
	}
}

// TestContentRoots_EndToEndLoaders is the ε.b end-to-end check: a config
// content_root plus a --agents-content-dir flag value, resolved exactly as
// run() resolves them, actually cause the instruction and skills loaders to
// pick up an UNMODIFIED external tree that lives outside the project root —
// the whole point of the feature. It exercises the real seam
// (resolveContentRoots → instruction.LoadForSession / skills.LoadAll with
// WithContentRoots), not the loader options in isolation.
func TestContentRoots_EndToEndLoaders(t *testing.T) {
	t.Parallel()

	// Two external trees, neither under the project root: one stands in for
	// the config's content_roots entry, one for a --agents-content-dir flag.
	cfgTree := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfgTree, "AGENTS.md"), []byte("EXTERNAL_PLATFORM_PERSONA\n"), 0o644); err != nil {
		t.Fatalf("write external AGENTS.md: %v", err)
	}
	writeSkill(t, cfgTree, "gke-reliability")

	flagTree := t.TempDir()
	writeSkill(t, flagTree, "gke-storage")

	// A project with its own overlay — the external persona must land ahead
	// of it (scopes concatenate; content_roots insert before the project).
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "AGENTS.md"), []byte("PROJECT_OVERLAY\n"), 0o644); err != nil {
		t.Fatalf("write project AGENTS.md: %v", err)
	}

	// Resolve the way run() does: config roots first, flag dirs after, base =
	// the agents dir (here the project root itself).
	roots := resolveContentRoots([]string{cfgTree}, []string{flagTree}, project)

	// Instruction: external persona present AND ahead of the project overlay.
	loaded, err := instruction.LoadForSession(project, "", "", "",
		instruction.WithContentRoots(roots))
	if err != nil {
		t.Fatalf("LoadForSession: %v", err)
	}
	ext := strings.Index(loaded.Instruction, "EXTERNAL_PLATFORM_PERSONA")
	proj := strings.Index(loaded.Instruction, "PROJECT_OVERLAY")
	if ext < 0 {
		t.Fatalf("external persona not loaded via content root:\n%s", loaded.Instruction)
	}
	if proj < 0 {
		t.Fatalf("project overlay missing:\n%s", loaded.Instruction)
	}
	if ext > proj {
		t.Errorf("external content should precede project: ext=%d proj=%d\n%s", ext, proj, loaded.Instruction)
	}

	// Skills: both external skills discovered alongside the (absent) project
	// skills — proving config-root AND flag-dir skills both compose in.
	sk, err := skills.LoadAll(context.Background(), project, "", nil,
		skills.WithContentRoots(roots))
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	names := map[string]bool{}
	for _, info := range sk.Infos {
		names[info.Name] = true
	}
	if !names["gke-reliability"] {
		t.Errorf("config-root skill gke-reliability not discovered: %+v", sk.Infos)
	}
	if !names["gke-storage"] {
		t.Errorf("flag-dir skill gke-storage not discovered: %+v", sk.Infos)
	}
}
