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

// Command ax-pin-check compares examples/deploy/ax's pinned AX runner
// commits (.ax-ref on google/ax and .ax-fork-ref on mastersingh24/ax)
// against upstream and fork main branches, and renders the job summary
// and rolling-issue body used by .github/workflows/ax-pin-check.yml (#1410).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	defaultAPIBase  = "https://api.github.com"
	upstreamRepo    = "google/ax"
	forkRepo        = "mastersingh24/ax"
	targetBranch    = "main"
	issueBodyMarker = "<!-- ax-pin-check-rolling-issue -->"
)

var sha40Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

// compareResponse is the subset of GitHub's GET /repos/{owner}/{repo}/compare/{base}...{head}
// payload that ax-pin-check inspects.
type compareResponse struct {
	Status       string `json:"status"`
	AheadBy      int    `json:"ahead_by"`
	BehindBy     int    `json:"behind_by"`
	TotalCommits int    `json:"total_commits"`
	Commits      []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
	} `json:"commits"`
	Files []struct {
		Filename string `json:"filename"`
		Status   string `json:"status"`
	} `json:"files"`
}

// Report is the evaluated state of the AX pins against upstream and fork.
type Report struct {
	UpstreamPin        string   `json:"upstream_pin"`
	ForkPin            string   `json:"fork_pin"`
	UpstreamStatus     string   `json:"upstream_status"`
	UpstreamAheadBy    int      `json:"upstream_ahead_by"`
	UpstreamBehindBy   int      `json:"upstream_behind_by"`
	UpstreamHeadSHA    string   `json:"upstream_head_sha,omitempty"`
	WatchedFiles       []string `json:"watched_files,omitempty"`
	CompareTruncated   bool     `json:"compare_truncated,omitempty"`
	ForkAheadBy        int      `json:"fork_ahead_by"`
	ForkBehindBy       int      `json:"fork_behind_by"`
	ForkHeadSHA        string   `json:"fork_head_sha,omitempty"`
	ForkWatchedFiles   []string `json:"fork_watched_files,omitempty"`
	PatchStackCommits  int      `json:"patch_stack_commits"`
	PatchStackBehind   int      `json:"patch_stack_behind"`
	ForkBehindUpstream int      `json:"fork_behind_upstream"`
}

// Drift reports whether either pin has relevant upstream or fork movement
// that warrants opening or updating the rolling issue.
func (r Report) Drift() bool {
	return len(r.WatchedFiles) > 0 || r.CompareTruncated || r.ForkAheadBy > 0 || r.PatchStackBehind > 0
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ax-pin-check: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	axDir := flag.String("ax-dir", filepath.Join("examples", "deploy", "ax"), "path to examples/deploy/ax")
	apiBase := flag.String("api-url", defaultAPIBase, "GitHub API base URL")
	summaryPath := flag.String("summary", "", "write Markdown summary to this file")
	issueBodyPath := flag.String("issue-body", "", "write rolling issue body to this file")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rep, err := evaluate(ctx, *axDir, *apiBase, os.Getenv("GITHUB_TOKEN"))
	if err != nil {
		return err
	}

	md := renderMarkdown(rep)
	if *summaryPath != "" {
		if err := os.WriteFile(*summaryPath, []byte(md), 0o600); err != nil {
			return fmt.Errorf("write summary: %w", err)
		}
	}
	if *issueBodyPath != "" {
		if err := os.WriteFile(*issueBodyPath, []byte(md), 0o600); err != nil {
			return fmt.Errorf("write issue body: %w", err)
		}
	}
	fmt.Printf("drift=%t\n", rep.Drift())
	return nil
}

func evaluate(ctx context.Context, axDir, apiBase, token string) (Report, error) {
	upPin, forkPin, err := readAndVerifyPins(axDir)
	if err != nil {
		return Report{}, err
	}

	upCmp, err := fetchCompare(ctx, apiBase, token, upstreamRepo, upPin, targetBranch)
	if err != nil {
		return Report{}, fmt.Errorf("compare upstream %s: %w", upstreamRepo, err)
	}
	forkCmp, err := fetchCompare(ctx, apiBase, token, forkRepo, forkPin, targetBranch)
	if err != nil {
		return Report{}, fmt.Errorf("compare fork %s against .ax-fork-ref: %w", forkRepo, err)
	}
	stackCmp, err := fetchCompare(ctx, apiBase, token, forkRepo, upPin, targetBranch)
	if err != nil {
		return Report{}, fmt.Errorf("compare fork %s against .ax-ref: %w", forkRepo, err)
	}

	return Report{
		UpstreamPin:        upPin,
		ForkPin:            forkPin,
		UpstreamStatus:     upCmp.Status,
		UpstreamAheadBy:    upCmp.AheadBy,
		UpstreamBehindBy:   upCmp.BehindBy,
		UpstreamHeadSHA:    headSHA(upCmp, upPin),
		WatchedFiles:       filterWatchedFiles(upCmp),
		CompareTruncated:   len(upCmp.Files) >= 300 || len(forkCmp.Files) >= 300,
		ForkAheadBy:        forkCmp.AheadBy,
		ForkBehindBy:       forkCmp.BehindBy,
		ForkHeadSHA:        headSHA(forkCmp, forkPin),
		ForkWatchedFiles:   filterWatchedFiles(forkCmp),
		PatchStackCommits:  stackCmp.AheadBy,
		PatchStackBehind:   stackCmp.BehindBy,
		ForkBehindUpstream: upCmp.AheadBy + stackCmp.BehindBy,
	}, nil
}

func readAndVerifyPins(axDir string) (upPin, forkPin string, err error) {
	upBytes, err := os.ReadFile(filepath.Join(axDir, ".ax-ref")) // #nosec G304 -- operator-supplied directory flag
	if err != nil {
		return "", "", fmt.Errorf("read .ax-ref: %w", err)
	}
	upPin = strings.TrimSpace(string(upBytes))
	if !sha40Re.MatchString(upPin) {
		return "", "", fmt.Errorf(".ax-ref %q is not a 40-char hex SHA", upPin)
	}

	forkBytes, err := os.ReadFile(filepath.Join(axDir, ".ax-fork-ref")) // #nosec G304 -- operator-supplied directory flag
	if err != nil {
		return "", "", fmt.Errorf("read .ax-fork-ref: %w", err)
	}
	forkPin = strings.TrimSpace(string(forkBytes))
	if !sha40Re.MatchString(forkPin) {
		return "", "", fmt.Errorf(".ax-fork-ref %q is not a 40-char hex SHA", forkPin)
	}

	dfBytes, err := os.ReadFile(filepath.Join(axDir, "Dockerfile")) // #nosec G304 -- operator-supplied directory flag
	if err != nil {
		return "", "", fmt.Errorf("read Dockerfile: %w", err)
	}
	if !strings.Contains(string(dfBytes), "ARG AX_REF="+forkPin) {
		return "", "", fmt.Errorf("dockerfile ARG AX_REF does not match .ax-fork-ref (%s)", forkPin)
	}
	return upPin, forkPin, nil
}

func fetchCompare(ctx context.Context, apiBase, token, repo, base, head string) (compareResponse, error) {
	url := fmt.Sprintf("%s/repos/%s/compare/%s...%s", strings.TrimRight(apiBase, "/"), repo, base, head)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return compareResponse{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return compareResponse{}, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return compareResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return compareResponse{}, fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out compareResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return compareResponse{}, fmt.Errorf("decode %s: %w", url, err)
	}
	return out, nil
}

func headSHA(cmp compareResponse, fallback string) string {
	if n := len(cmp.Commits); n > 0 && cmp.Commits[n-1].SHA != "" {
		return cmp.Commits[n-1].SHA
	}
	return fallback
}

// isWatchedPath reports whether a changed file in AX affects the runner
// contract, task-spec schema, or task-runner binary that examples/deploy/ax
// depends on (#1410).
func isWatchedPath(path string) bool {
	if path == "docs/runner.md" || path == "Dockerfile.task-runner" {
		return true
	}
	for _, prefix := range []string{
		"cmd/ax-task-runner/",
		"runner/",
		"pkg/apis/",
	} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func filterWatchedFiles(cmp compareResponse) []string {
	var out []string
	for _, f := range cmp.Files {
		if isWatchedPath(f.Filename) {
			out = append(out, f.Filename)
		}
	}
	sort.Strings(out)
	return out
}

func renderMarkdown(r Report) string {
	var b strings.Builder
	b.WriteString(issueBodyMarker + "\n")
	b.WriteString("## AX runner pin check (`examples/deploy/ax`)\n\n")
	fmt.Fprintf(&b, "| Pin | Repository | Pinned SHA | `%s` status | Watched files changed |\n", targetBranch)
	b.WriteString("|---|---|---|---|---|\n")
	fmt.Fprintf(&b, "| `.ax-ref` | [`%s`](https://github.com/%s) | `%s` | %d ahead / %d behind (`%s`) | %d |\n",
		upstreamRepo, upstreamRepo, shortSHA(r.UpstreamPin), r.UpstreamAheadBy, r.UpstreamBehindBy, shortSHA(r.UpstreamHeadSHA), len(r.WatchedFiles))
	fmt.Fprintf(&b, "| `.ax-fork-ref` | [`%s`](https://github.com/%s) | `%s` | %d ahead / %d behind (`%s`) | %d |\n\n",
		forkRepo, forkRepo, shortSHA(r.ForkPin), r.ForkAheadBy, r.ForkBehindBy, shortSHA(r.ForkHeadSHA), len(r.ForkWatchedFiles))

	fmt.Fprintf(&b, "**Fork patch stack (`%s` vs `.ax-ref`):** %d commit(s) ahead, %d behind (`%s:main` is %d commit(s) ahead of `.ax-ref`).\n\n",
		forkRepo, r.PatchStackCommits, r.PatchStackBehind, upstreamRepo, r.ForkBehindUpstream)

	if r.CompareTruncated {
		b.WriteString("> **Warning:** GitHub Compare file list reached the 300-file pagination ceiling; inspect the full diff before bumping pins.\n\n")
	}

	if len(r.WatchedFiles) > 0 {
		b.WriteString("### Upstream runner / task-spec changes since `.ax-ref`\n\n")
		for _, f := range r.WatchedFiles {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
		b.WriteString("\n")
	}
	if len(r.ForkWatchedFiles) > 0 {
		b.WriteString("### Fork runner / task-spec changes since `.ax-fork-ref`\n\n")
		for _, f := range r.ForkWatchedFiles {
			fmt.Fprintf(&b, "- `%s`\n", f)
		}
		b.WriteString("\n")
	}

	b.WriteString("### How to update\n\n")
	b.WriteString("1. Verify the new runner pin against `go-steer/shipmind`'s `ax-runner` entry in `versions.yaml`.\n")
	b.WriteString("2. Update `examples/deploy/ax/.ax-ref`, `examples/deploy/ax/.ax-fork-ref`, and `ARG AX_REF=` in `examples/deploy/ax/Dockerfile`.\n")
	b.WriteString("3. Run `go test ./examples/deploy/ax/...`.\n")
	return b.String()
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
