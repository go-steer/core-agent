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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixtureDir(t *testing.T, upSHA, forkSHA, dfRef string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".ax-ref"), []byte(upSHA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".ax-fork-ref"), []byte(forkSHA+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	df := "ARG AX_REF=" + dfRef + "\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadAndVerifyPins_DetectsDockerfileMismatch(t *testing.T) {
	t.Parallel()
	const (
		upSHA   = "8888435d7e275f1e7ad1c127af9048b2d97ebc13"
		forkSHA = "e8bc98d87d9e3dcdee4aafa899bc802186d67dcb"
		other   = "1111111111111111111111111111111111111111"
	)
	dir := writeFixtureDir(t, upSHA, forkSHA, other)
	if _, _, err := readAndVerifyPins(dir); err == nil || !strings.Contains(err.Error(), "dockerfile ARG AX_REF") {
		t.Fatalf("readAndVerifyPins err = %v, want Dockerfile mismatch error", err)
	}
}

func TestEvaluate_DetectsUpstreamRunnerAndSchemaDrift(t *testing.T) {
	t.Parallel()
	const (
		upSHA   = "8888435d7e275f1e7ad1c127af9048b2d97ebc13"
		forkSHA = "e8bc98d87d9e3dcdee4aafa899bc802186d67dcb"
	)
	dir := writeFixtureDir(t, upSHA, forkSHA, forkSHA)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/google/ax/compare/" + upSHA + "...main":
			_, _ = io.WriteString(w, `{
				"status": "ahead",
				"ahead_by": 4,
				"behind_by": 0,
				"commits": [{"sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],
				"files": [
					{"filename": "README.md", "status": "modified"},
					{"filename": "docs/runner.md", "status": "modified"},
					{"filename": "cmd/ax-task-runner/main.go", "status": "modified"},
					{"filename": "pkg/apis/v1alpha1/types.go", "status": "modified"}
				]
			}`)
		case "/repos/mastersingh24/ax/compare/" + forkSHA + "...main":
			_, _ = io.WriteString(w, `{"status": "identical", "ahead_by": 0, "behind_by": 0}`)
		case "/repos/mastersingh24/ax/compare/" + upSHA + "...main":
			_, _ = io.WriteString(w, `{"status": "ahead", "ahead_by": 21, "behind_by": 0}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	rep, err := evaluate(context.Background(), dir, srv.URL, "test-token")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !rep.Drift() {
		t.Error("rep.Drift() = false, want true when watched upstream files changed")
	}
	if len(rep.WatchedFiles) != 3 {
		t.Errorf("WatchedFiles = %v, want 3 watched files (docs/runner.md, cmd/ax-task-runner/main.go, pkg/apis/v1alpha1/types.go)", rep.WatchedFiles)
	}
	if rep.PatchStackCommits != 21 {
		t.Errorf("PatchStackCommits = %d, want 21", rep.PatchStackCommits)
	}
	md := renderMarkdown(rep)
	for _, want := range []string{
		issueBodyMarker,
		"docs/runner.md",
		"cmd/ax-task-runner/main.go",
		"pkg/apis/v1alpha1/types.go",
		"21 commit(s) ahead",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("renderMarkdown missing %q:\n%s", want, md)
		}
	}
}

func TestEvaluate_NoDriftWhenOnlyUnwatchedUpstreamFilesChange(t *testing.T) {
	t.Parallel()
	const (
		upSHA   = "8888435d7e275f1e7ad1c127af9048b2d97ebc13"
		forkSHA = "e8bc98d87d9e3dcdee4aafa899bc802186d67dcb"
	)
	dir := writeFixtureDir(t, upSHA, forkSHA, forkSHA)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"status": "ahead",
			"ahead_by": 0,
			"behind_by": 0,
			"files": [{"filename": "README.md", "status": "modified"}]
		}`)
	}))
	defer srv.Close()

	rep, err := evaluate(context.Background(), dir, srv.URL, "")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if rep.Drift() {
		t.Errorf("rep.Drift() = true, want false when no watched files changed and fork is current: %+v", rep)
	}
}

func TestReport_DriftOnCompareTruncation(t *testing.T) {
	t.Parallel()
	r := Report{CompareTruncated: true}
	if !r.Drift() {
		t.Error("Report{CompareTruncated: true}.Drift() = false, want true")
	}
	md := renderMarkdown(r)
	if !strings.Contains(md, "300-file pagination ceiling") {
		t.Errorf("renderMarkdown missing truncation warning:\n%s", md)
	}
}
