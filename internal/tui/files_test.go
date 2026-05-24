// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListProjectFiles_FilterAndExclude(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// Layout:
	//   root/main.go
	//   root/internal/tui/view.go
	//   root/node_modules/junk.js   (excluded)
	//   root/.git/HEAD              (excluded)
	for _, p := range []string{
		"main.go",
		"internal/tui/view.go",
		"node_modules/junk.js",
		".git/HEAD",
	} {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	all := listProjectFiles(root, "")
	got := map[string]bool{}
	for _, it := range all {
		got[it.Display] = true
	}
	if !got["main.go"] || !got["internal/tui/view.go"] {
		t.Errorf("missing expected files: %+v", all)
	}
	if got["node_modules/junk.js"] || got[".git/HEAD"] {
		t.Errorf("excluded path leaked through: %+v", all)
	}

	// Filter narrows to just view.go (substring match).
	filtered := listProjectFiles(root, "view")
	if len(filtered) != 1 || filtered[0].Display != "internal/tui/view.go" {
		t.Errorf("filter 'view' = %+v", filtered)
	}
	// Insertion value carries the @ prefix.
	if filtered[0].Value != "@internal/tui/view.go" {
		t.Errorf("Value = %q, want @-prefixed path", filtered[0].Value)
	}
}
