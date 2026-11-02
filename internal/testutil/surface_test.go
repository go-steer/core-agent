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

package testutil

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const modulePath = "github.com/go-steer/core-agent/v3"

// TestEveryImportablePackageIsADecision enforces that every importable
// library package in the module (outside cmd/, dev/, examples/, and
// internal/) is explicitly classified as either `promised` or
// `unsupported` in dev/api-surface.txt, and that every entry in
// dev/api-surface.txt corresponds to a real package directory.
func TestEveryImportablePackageIsADecision(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := RepoRoot(thisFile)
	if err != nil {
		t.Fatal(err)
	}
	surfaceFile := filepath.Join(root, "dev", "api-surface.txt")

	f, err := os.Open(surfaceFile)
	if err != nil {
		t.Fatalf("open %s: %v", surfaceFile, err)
	}
	defer f.Close()

	decisions := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || (fields[0] != "promised" && fields[0] != "unsupported") {
			t.Errorf("dev/api-surface.txt:%d: want '<promised|unsupported> <pkg>', got %q", lineNo, line)
			continue
		}
		kind, pkg := fields[0], fields[1]
		if prev, dup := decisions[pkg]; dup {
			t.Errorf("dev/api-surface.txt:%d: duplicate entry for %s (already %s)", lineNo, pkg, prev)
			continue
		}
		decisions[pkg] = kind
		if info, statErr := os.Stat(filepath.Join(root, pkg)); statErr != nil || !info.IsDir() {
			t.Errorf("dev/api-surface.txt:%d: listed package directory %q does not exist", lineNo, pkg)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", surfaceFile, err)
	}

	cmd := exec.Command("go", "list", "./...")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list ./...: %v\n%s", err, out)
	}

	importable := make(map[string]bool)
	for _, full := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		full = strings.TrimSpace(full)
		if full == "" || full == modulePath {
			continue
		}
		rel := strings.TrimPrefix(full, modulePath+"/")
		if isExemptFromAPISurface(rel) {
			continue
		}
		importable[rel] = true
		if _, ok := decisions[rel]; !ok {
			t.Errorf("importable package %q is not classified in dev/api-surface.txt (add `promised %s` or `unsupported %s`; see docs/compatibility-policy.md)", rel, rel, rel)
		}
	}
	for pkg := range decisions {
		if !importable[pkg] {
			t.Errorf("dev/api-surface.txt entry %q is not a non-exempt importable Go package in `go list ./...`", pkg)
		}
	}

	checkSurfaceDocsInSync(t, root, decisions)
}

func checkSurfaceDocsInSync(t *testing.T, root string, decisions map[string]string) {
	t.Helper()
	policyPath := filepath.Join(root, "docs", "compatibility-policy.md")
	policyBytes, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatalf("read %s: %v", policyPath, err)
	}
	policyText := string(policyBytes)
	promisedIdx := strings.Index(policyText, "### `promised` packages")
	unsupportedIdx := strings.Index(policyText, "### `unsupported` packages")
	if promisedIdx < 0 || unsupportedIdx < promisedIdx {
		t.Fatalf("docs/compatibility-policy.md missing expected `### \\`promised\\` packages` / `### \\`unsupported\\` packages` subsections")
	}
	policyPromised := policyText[promisedIdx:unsupportedIdx]
	policyUnsupported := policyText[unsupportedIdx:]
	if sep := strings.Index(policyUnsupported, "\n---"); sep >= 0 {
		policyUnsupported = policyUnsupported[:sep]
	}

	changelogPath := filepath.Join(root, "CHANGELOG.md")
	changelogBytes, err := os.ReadFile(changelogPath)
	if err != nil {
		t.Fatalf("read %s: %v", changelogPath, err)
	}
	changelogHeader := string(changelogBytes)
	if idx := strings.Index(changelogHeader, "\n## ["); idx >= 0 {
		changelogHeader = changelogHeader[:idx]
	}
	remIdx := strings.Index(changelogHeader, "\nRemaining packages under ")
	if remIdx < 0 {
		t.Fatalf("CHANGELOG.md Stability promise header missing `Remaining packages under ` split")
	}
	changelogPromised := changelogHeader[:remIdx]
	changelogUnsupported := changelogHeader[remIdx:]

	for pkg, kind := range decisions {
		backtickedFull := "`" + pkg + "`"
		short := strings.TrimPrefix(pkg, "pkg/")
		backtickedShort := "`" + short + "`"
		policySection := policyPromised
		changelogSection := changelogPromised
		if kind == "unsupported" {
			policySection = policyUnsupported
			changelogSection = changelogUnsupported
		}
		if !strings.Contains(policySection, backtickedFull) {
			t.Errorf("docs/compatibility-policy.md %s section does not list %s (keep in sync with dev/api-surface.txt)", kind, backtickedFull)
		}
		if !strings.Contains(changelogSection, backtickedShort) && !strings.Contains(changelogSection, backtickedFull) {
			t.Errorf("CHANGELOG.md Stability promise %s section does not list %s (keep in sync with dev/api-surface.txt)", kind, backtickedShort)
		}
	}
}

func isExemptFromAPISurface(rel string) bool {
	if strings.HasPrefix(rel, "cmd/") || strings.HasPrefix(rel, "dev/") || strings.HasPrefix(rel, "examples/") {
		return true
	}
	if rel == "internal" || strings.HasPrefix(rel, "internal/") || strings.Contains(rel, "/internal/") || strings.HasSuffix(rel, "/internal") {
		return true
	}
	return false
}
