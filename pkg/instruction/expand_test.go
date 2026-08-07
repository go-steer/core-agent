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

package instruction

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestExpand_PlainBody: a body with no @include is returned verbatim,
// with no Sources (the inline text is not a file).
func TestExpand_PlainBody(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const body = "You are a read-only cluster investigator.\nNever mutate state.\n"
	got, srcs, err := Expand(body, root, root)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got != body {
		t.Errorf("plain body should pass through unchanged:\n got %q\nwant %q", got, body)
	}
	if len(srcs) != 0 {
		t.Errorf("plain body should produce no Sources, got %v", srcs)
	}
}

// TestExpand_Include: an @include line is replaced by the referenced
// file's content, resolved relative to baseDir and confined to scopeRoot.
// The included file shows up as a Source; the inline body does not.
func TestExpand_Include(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "upstream", "cluster"), "SOUL.md", "CLUSTER PERSONA\n")

	got, srcs, err := Expand("@include upstream/cluster/SOUL.md\n", root, root)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !strings.Contains(got, "CLUSTER PERSONA") {
		t.Errorf("expanded body should contain the included persona, got %q", got)
	}
	if len(srcs) != 1 {
		t.Fatalf("expected exactly 1 Source (the included file), got %d: %v", len(srcs), srcs)
	}
	if srcs[0].Scope != "subagent" {
		t.Errorf("included Source.Scope = %q, want %q", srcs[0].Scope, "subagent")
	}
}

// TestExpand_RejectEscape: a subagent @include must be confined to
// scopeRoot exactly like the parent's — ../ out of the project fails.
func TestExpand_RejectEscape(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	writeFile(t, root, ".keep", "") // ensure root exists
	writeFile(t, parent, "escape.md", "secret\n")

	_, _, err := Expand("@include ../escape.md\n", root, root)
	if err == nil {
		t.Fatal("expected error for @include escaping scopeRoot")
	}
	if !strings.Contains(err.Error(), "escape") && !strings.Contains(err.Error(), "scope") {
		t.Errorf("error should mention scope escape: %v", err)
	}
}

// TestExpand_MissingTarget: a typo'd @include target fails loudly (same
// contract as Load) rather than silently yielding an empty persona.
func TestExpand_MissingTarget(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	_, _, err := Expand("@include upstream/cluster/SOUL.md\n", root, root)
	if err == nil {
		t.Fatal("expected error for missing @include target")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found': %v", err)
	}
}

// TestExpand_Interpolator: the WithInterpolator transform runs on the
// inline body before include resolution, so a ${env:VAR} that expands to
// an @include line is itself resolved.
func TestExpand_Interpolator(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "persona.md", "INTERPOLATED PERSONA\n")

	interp := func(s string) string {
		return strings.ReplaceAll(s, "${PERSONA}", "@include persona.md")
	}
	got, srcs, err := Expand("${PERSONA}\n", root, root, WithInterpolator(interp))
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !strings.Contains(got, "INTERPOLATED PERSONA") {
		t.Errorf("interpolated @include should expand, got %q", got)
	}
	if len(srcs) != 1 {
		t.Errorf("expected 1 Source, got %d", len(srcs))
	}
}
