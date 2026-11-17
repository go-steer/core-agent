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

package webui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var mastWebVersionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// TestPinnedVersionFileIsNonEmpty guards against .mast-web-version
// regressing to an empty `version=` line (#348), which silently turns
// dev/tools/fetch-mast-web into a no-op and ships release binaries
// whose `--ui` flag refuses to start.
func TestPinnedVersionFileIsNonEmpty(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", ".mast-web-version"))
	if err != nil {
		t.Fatalf("read .mast-web-version: %v", err)
	}
	var version string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "version=") {
			version = strings.TrimSpace(strings.TrimPrefix(line, "version="))
			break
		}
	}
	if !mastWebVersionRE.MatchString(version) {
		t.Fatalf(".mast-web-version pins version=%q, want a release tag matching %s", version, mastWebVersionRE)
	}
}

// TestFS_AlwaysSucceeds verifies the //go:embed never fails — the
// .gitkeep placeholder guarantees the dist/ directory exists even on
// a fresh clone before fetch-mast-web has populated it.
func TestFS_AlwaysSucceeds(t *testing.T) {
	f, err := FS()
	if err != nil {
		t.Fatalf("FS(): %v", err)
	}
	if f == nil {
		t.Fatal("FS() returned nil fs.FS")
	}
}

// TestHasAssets_FalseWhenOnlyPlaceholder verifies the typical pre-build
// state: dist/.gitkeep is present but dist/index.html is not, so
// HasAssets reports false (driving the "fetch first" startup error).
// Skipped when fetch-mast-web has already populated the embed in
// this checkout.
func TestHasAssets_FalseWhenOnlyPlaceholder(t *testing.T) {
	f, err := FS()
	if err != nil {
		t.Fatalf("FS(): %v", err)
	}
	if file, err := f.Open("index.html"); err == nil {
		_ = file.Close()
		t.Skipf("dist/index.html present (fetch-mast-web has run) — skipping placeholder-state test")
	}
	if HasAssets() {
		t.Fatal("HasAssets() = true; want false when dist/index.html is missing")
	}
}
