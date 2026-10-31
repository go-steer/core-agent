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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fixtureUpSHA    = "8888435d7e275f1e7ad1c127af9048b2d97ebc13"
	fixtureForkSHA  = "e8bc98d87d9e3dcdee4aafa899bc802186d67dcb"
	fixtureUpHead   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	fixtureForkHead = "cccccccccccccccccccccccccccccccccccccccc"
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

// fixtureAPI is a GitHub API stand-in keyed by request path. Paths not in
// the map 404, which is what the real API does for an unknown ref.
func fixtureAPI(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, ok := routes[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// headRoutes are the two branch-head lookups every evaluate() makes.
func headRoutes() map[string]string {
	return map[string]string{
		"/repos/google/ax/commits/heads/main":        `{"sha": "` + fixtureUpHead + `"}`,
		"/repos/mastersingh24/ax/commits/heads/main": `{"sha": "` + fixtureForkHead + `"}`,
	}
}

func TestReadAndVerifyPins_DetectsDockerfileMismatch(t *testing.T) {
	t.Parallel()
	const other = "1111111111111111111111111111111111111111"
	dir := writeFixtureDir(t, fixtureUpSHA, fixtureForkSHA, other)
	if _, _, err := readAndVerifyPins(dir); err == nil || !strings.Contains(err.Error(), "dockerfile ARG AX_REF") {
		t.Fatalf("readAndVerifyPins err = %v, want Dockerfile mismatch error", err)
	}
}

func TestEvaluate_DetectsUpstreamRunnerAndSchemaDrift(t *testing.T) {
	t.Parallel()
	dir := writeFixtureDir(t, fixtureUpSHA, fixtureForkSHA, fixtureForkSHA)

	routes := headRoutes()
	routes["/repos/google/ax/compare/"+fixtureUpSHA+"...main"] = `{
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
	}`
	routes["/repos/mastersingh24/ax/compare/"+fixtureForkSHA+"...main"] = `{"status": "identical", "ahead_by": 0, "behind_by": 0}`
	routes["/repos/mastersingh24/ax/compare/"+fixtureUpSHA+"...main"] = `{"status": "ahead", "ahead_by": 21, "behind_by": 0}`
	srv := fixtureAPI(t, routes)

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
	dir := writeFixtureDir(t, fixtureUpSHA, fixtureForkSHA, fixtureForkSHA)

	quiet := `{
		"status": "ahead",
		"ahead_by": 0,
		"behind_by": 0,
		"files": [{"filename": "README.md", "status": "modified"}]
	}`
	routes := headRoutes()
	routes["/repos/google/ax/compare/"+fixtureUpSHA+"...main"] = quiet
	routes["/repos/mastersingh24/ax/compare/"+fixtureForkSHA+"...main"] = quiet
	routes["/repos/mastersingh24/ax/compare/"+fixtureUpSHA+"...main"] = quiet
	srv := fixtureAPI(t, routes)

	rep, err := evaluate(context.Background(), dir, srv.URL, "")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if rep.Drift() {
		t.Errorf("rep.Drift() = true, want false when no watched files changed and fork is current: %+v", rep)
	}
}

// The head SHA comes from GET /commits/heads/{branch}, not from the
// compare payload's commits[]. The compare list is capped and its "last
// element is the tip" property holds only for an unpaged call; the
// report must name whatever the branch endpoint says, whatever the
// compare lists (#1418).
func TestEvaluate_HeadSHAComesFromBranchNotCompare(t *testing.T) {
	t.Parallel()
	dir := writeFixtureDir(t, fixtureUpSHA, fixtureForkSHA, fixtureForkSHA)

	routes := headRoutes()
	routes["/repos/google/ax/compare/"+fixtureUpSHA+"...main"] = `{
		"status": "ahead", "ahead_by": 300, "behind_by": 0,
		"commits": [{"sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]
	}`
	routes["/repos/mastersingh24/ax/compare/"+fixtureForkSHA+"...main"] = `{
		"status": "ahead", "ahead_by": 1, "behind_by": 0,
		"commits": [{"sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]
	}`
	routes["/repos/mastersingh24/ax/compare/"+fixtureUpSHA+"...main"] = `{"status": "ahead", "ahead_by": 1, "behind_by": 0}`
	srv := fixtureAPI(t, routes)

	rep, err := evaluate(context.Background(), dir, srv.URL, "")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if rep.UpstreamHeadSHA != fixtureUpHead {
		t.Errorf("UpstreamHeadSHA = %q, want branch tip %q (not the compare payload's last commit)", rep.UpstreamHeadSHA, fixtureUpHead)
	}
	if rep.ForkHeadSHA != fixtureForkHead {
		t.Errorf("ForkHeadSHA = %q, want branch tip %q", rep.ForkHeadSHA, fixtureForkHead)
	}
}

// A compare payload that lists fewer commits than its total_commits is
// as truncated as one with 300 files; both must flip CompareTruncated
// and therefore Drift (#1418). GitHub caps the unpaged list at 250; the
// fixture lists 250 of 900, with only an unwatched file, so nothing else
// can make this drift.
func TestEvaluate_TruncatedAtCommitCeiling(t *testing.T) {
	t.Parallel()
	dir := writeFixtureDir(t, fixtureUpSHA, fixtureForkSHA, fixtureForkSHA)

	const listed = 250
	var commits strings.Builder
	for i := range listed {
		if i > 0 {
			commits.WriteString(",")
		}
		fmt.Fprintf(&commits, `{"sha": "%040x"}`, i)
	}
	quiet := `{"status": "ahead", "ahead_by": 0, "behind_by": 0}`
	routes := headRoutes()
	routes["/repos/google/ax/compare/"+fixtureUpSHA+"...main"] = `{
		"status": "ahead", "ahead_by": 900, "behind_by": 0, "total_commits": 900,
		"commits": [` + commits.String() + `],
		"files": [{"filename": "README.md", "status": "modified"}]
	}`
	routes["/repos/mastersingh24/ax/compare/"+fixtureForkSHA+"...main"] = quiet
	routes["/repos/mastersingh24/ax/compare/"+fixtureUpSHA+"...main"] = quiet
	srv := fixtureAPI(t, routes)

	rep, err := evaluate(context.Background(), dir, srv.URL, "")
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !rep.CompareTruncated {
		t.Errorf("CompareTruncated = false with %d of 900 commits listed, want true", listed)
	}
	if !rep.Drift() {
		t.Error("Drift() = false on a truncated compare, want true")
	}
}

// A comparison of exactly as many commits as it lists is not truncated,
// whatever the count: the predicate is total_commits versus the list,
// not a hardcoded 250.
func TestCompareResponse_FullListIsNotTruncated(t *testing.T) {
	t.Parallel()
	var c compareResponse
	c.TotalCommits = 3
	c.Commits = make([]struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	}, 3)
	if c.truncated() {
		t.Error("truncated() = true for a payload listing all 3 of its 3 commits")
	}
	c.TotalCommits = 4
	if !c.truncated() {
		t.Error("truncated() = false for a payload listing 3 of 4 commits")
	}
}

// The "ahead of .ax-ref" figure is upstream's ahead_by alone. The sum
// upCmp.AheadBy + stackCmp.BehindBy that used to be rendered under that
// label double-counts upstream commits the fork has already merged
// (#1418).
func TestRenderMarkdown_UpstreamAheadIsNotSummedWithStackBehind(t *testing.T) {
	t.Parallel()
	md := renderMarkdown(Report{
		UpstreamAheadBy:   4,
		PatchStackCommits: 21,
		PatchStackBehind:  2,
	})
	if !strings.Contains(md, "`google/ax:main` is 4 commit(s) ahead of `.ax-ref`") {
		t.Errorf("renderMarkdown should report upstream ahead_by (4) under the upstream label:\n%s", md)
	}
	if strings.Contains(md, "is 6 commit(s) ahead") {
		t.Errorf("renderMarkdown still sums upstream ahead_by with patch-stack behind_by:\n%s", md)
	}
}

// A branch-head lookup that fails is an error, not a silent fallback to
// the pin: a report that names the pin as "main" would tell the operator
// nothing moved.
func TestEvaluate_BranchHeadFailureIsAnError(t *testing.T) {
	t.Parallel()
	dir := writeFixtureDir(t, fixtureUpSHA, fixtureForkSHA, fixtureForkSHA)

	quiet := `{"status": "ahead", "ahead_by": 0, "behind_by": 0}`
	routes := map[string]string{
		"/repos/google/ax/compare/" + fixtureUpSHA + "...main":          quiet,
		"/repos/mastersingh24/ax/compare/" + fixtureForkSHA + "...main": quiet,
		"/repos/mastersingh24/ax/compare/" + fixtureUpSHA + "...main":   quiet,
		// No /commits/heads/main routes: both 404.
	}
	srv := fixtureAPI(t, routes)

	if _, err := evaluate(context.Background(), dir, srv.URL, ""); err == nil || !strings.Contains(err.Error(), "head of google/ax:main") {
		t.Fatalf("evaluate err = %v, want a branch-head error", err)
	}
}

func TestReport_DriftOnCompareTruncation(t *testing.T) {
	t.Parallel()
	r := Report{CompareTruncated: true}
	if !r.Drift() {
		t.Error("Report{CompareTruncated: true}.Drift() = false, want true")
	}
	md := renderMarkdown(r)
	if !strings.Contains(md, "Compare payload was cut") {
		t.Errorf("renderMarkdown missing truncation warning:\n%s", md)
	}
}
