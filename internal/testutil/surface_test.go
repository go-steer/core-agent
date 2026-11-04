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

// TestReviewGateWorkflowRequiresHeadingAndFixVerification exercises the
// awk + bash logic in .github/workflows/review-gate.yml against representative
// PR bodies and titles so "no adversarial review done", a heading with an
// empty body, or a fix: PR without pre-fix verification are rejected (#1385).
func TestReviewGateWorkflowRequiresHeadingAndFixVerification(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := RepoRoot(thisFile)
	if err != nil {
		t.Fatal(err)
	}
	wfPath := filepath.Join(root, ".github", "workflows", "review-gate.yml")
	wfBytes, err := os.ReadFile(wfPath)
	if err != nil {
		t.Fatalf("read %s: %v", wfPath, err)
	}
	wfText := string(wfBytes)
	idx := strings.Index(wfText, "# Extract the \"## Adversarial review\"")
	if idx < 0 {
		t.Fatalf("could not find section extractor in %s", wfPath)
	}
	var snippet strings.Builder
	snippet.WriteString("set -euo pipefail\n")
	for _, line := range strings.Split(wfText[idx:], "\n") {
		trimmed := strings.TrimPrefix(line, "          ")
		snippet.WriteString(trimmed)
		snippet.WriteByte('\n')
	}
	script := snippet.String()

	validBody := "## Summary\n\nFixes race.\n\n## Adversarial review\n\nSkeptic subagent over the staged diff. No correctness defects found after checking lock ordering and SQLite pragmas. Pre-fix failure verified.\n"

	cases := []struct {
		name    string
		title   string
		body    string
		wantErr bool
	}{
		{
			name:    "bare phrase without heading is rejected",
			title:   "feat: add thing",
			body:    "no adversarial review done",
			wantErr: true,
		},
		{
			name:    "heading with trivial body is rejected",
			title:   "feat: add thing",
			body:    "## Adversarial review\n\nLGTM\n## Next section\nMore text here that should not count toward the adversarial review section.",
			wantErr: true,
		},
		{
			name:    "fix PR without pre-fix mention is rejected",
			title:   "fix(attach): close race",
			body:    "## Adversarial review\n\nSkeptic subagent over the staged diff. Checked lock ordering, channel lifecycles, and goroutine teardown across all callers.",
			wantErr: true,
		},
		{
			name:    "feat PR with substantive section passes without pre-fix mention",
			title:   "feat(attach): add endpoint",
			body:    "## Adversarial review\n\nSkeptic subagent over the staged diff. Checked lock ordering, channel lifecycles, and goroutine teardown across all callers.",
			wantErr: false,
		},
		{
			name:    "fix PR with substantive section and pre-fix mention passes",
			title:   "fix(attach): close race",
			body:    validBody,
			wantErr: false,
		},
		{
			name:    "soak dispatcher PR with Adversarial review: commit paragraph passes",
			title:   "fix(agent): fall back on empty summary",
			body:    "## Commits\n\nAdversarial review: skeptic subagent checked lock ordering, nil-slice returns, and pre-fix test failure before the fix landed.\n",
			wantErr: false,
		},
	}

	for _, tc := range cases {
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = append(os.Environ(), "PR_TITLE="+tc.title, "PR_BODY="+tc.body)
		out, runErr := cmd.CombinedOutput()
		if tc.wantErr && runErr == nil {
			t.Errorf("%s: expected failure, got exit 0:\n%s", tc.name, out)
		}
		if !tc.wantErr && runErr != nil {
			t.Errorf("%s: expected exit 0, got %v:\n%s", tc.name, runErr, out)
		}
	}
}

// TestEveryPresubmitHasACIStep enforces that every script in
// dev/ci/presubmits/ (other than the credential-gated e2e-* scripts)
// has a corresponding entry in dev/tools/ci's STEPS array (#1394).
func TestEveryPresubmitHasACIStep(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root, err := RepoRoot(thisFile)
	if err != nil {
		t.Fatal(err)
	}

	ciPath := filepath.Join(root, "dev", "tools", "ci")
	ciBytes, err := os.ReadFile(ciPath)
	if err != nil {
		t.Fatalf("read %s: %v", ciPath, err)
	}
	ciText := string(ciBytes)

	presubmitDir := filepath.Join(root, "dev", "ci", "presubmits")
	entries, err := os.ReadDir(presubmitDir)
	if err != nil {
		t.Fatalf("readdir %s: %v", presubmitDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "e2e-") {
			continue
		}
		target := "dev/tools/" + name
		if name == "verify-docs-lint" {
			target = "dev/tools/docs-lint"
		}
		if _, statErr := os.Stat(filepath.Join(root, target)); statErr != nil {
			t.Errorf("presubmit %s has no corresponding tool script %s: %v", name, target, statErr)
		}
		if !strings.Contains(ciText, target) {
			t.Errorf("presubmit dev/ci/presubmits/%s is not invoked in dev/tools/ci (expected %q in STEPS)", name, target)
		}
	}
}
