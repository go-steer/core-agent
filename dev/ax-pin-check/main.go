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
//
// Four GitHub API calls per run (#1418):
//
//   - compare google/ax .ax-ref...main: upstream movement and the watched
//     files that changed;
//   - compare mastersingh24/ax .ax-fork-ref...main: fork movement;
//   - compare mastersingh24/ax .ax-ref...main: the fork's patch stack on
//     top of the upstream pin. This one 404s if the fork has not fetched
//     the upstream commit named by .ax-ref, and the run errors rather than
//     filing an issue. That is deliberate: a fork that does not contain
//     the upstream pin is a pin problem, not a drift report;
//   - GET /repos/{repo}/commits/heads/{branch} for each head SHA. The
//     compare payload's commits[] is capped at 250 and its files[] at
//     300; GitHub documents that on an unpaged call the last listed
//     commit is still the tip, but that is a property of the pagination
//     defaults, not of the endpoint. The branch tip is read from the
//     endpoint whose job that is, and a failed lookup is an error rather
//     than a silent fallback to the pin.
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

	// GitHub's compare endpoint returns at most this many files; past the
	// ceiling the payload is silently partial. Commits are capped too (250
	// unpaged), but total_commits is reported alongside, so that side is
	// detected exactly rather than by a hardcoded count.
	compareFilesCeiling = 300

	// maxResponseBytes bounds a single API response. A compare payload
	// carries a unified diff per changed file, so 300 files of a large
	// upstream range can run to tens of megabytes; a bound that is too
	// small fails the weekly run exactly when the report matters most.
	maxResponseBytes = 64 << 20
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

// truncated reports whether GitHub cut either list in this payload, in
// which case counts are trustworthy but contents are not.
func (c compareResponse) truncated() bool {
	return c.TotalCommits > len(c.Commits) || len(c.Files) >= compareFilesCeiling
}

// commitResponse is the subset of GET /repos/{owner}/{repo}/commits/{ref}
// that ax-pin-check inspects.
type commitResponse struct {
	SHA string `json:"sha"`
}

// Report is the evaluated state of the AX pins against upstream and fork.
type Report struct {
	UpstreamPin       string   `json:"upstream_pin"`
	ForkPin           string   `json:"fork_pin"`
	UpstreamStatus    string   `json:"upstream_status"`
	UpstreamAheadBy   int      `json:"upstream_ahead_by"`
	UpstreamBehindBy  int      `json:"upstream_behind_by"`
	UpstreamHeadSHA   string   `json:"upstream_head_sha,omitempty"`
	WatchedFiles      []string `json:"watched_files,omitempty"`
	CompareTruncated  bool     `json:"compare_truncated,omitempty"`
	ForkAheadBy       int      `json:"fork_ahead_by"`
	ForkBehindBy      int      `json:"fork_behind_by"`
	ForkHeadSHA       string   `json:"fork_head_sha,omitempty"`
	ForkWatchedFiles  []string `json:"fork_watched_files,omitempty"`
	PatchStackCommits int      `json:"patch_stack_commits"`
	PatchStackBehind  int      `json:"patch_stack_behind"`
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

	c := apiClient{base: apiBase, token: token}
	upCmp, err := c.compare(ctx, upstreamRepo, upPin, targetBranch)
	if err != nil {
		return Report{}, fmt.Errorf("compare upstream %s: %w", upstreamRepo, err)
	}
	forkCmp, err := c.compare(ctx, forkRepo, forkPin, targetBranch)
	if err != nil {
		return Report{}, fmt.Errorf("compare fork %s against .ax-fork-ref: %w", forkRepo, err)
	}
	stackCmp, err := c.compare(ctx, forkRepo, upPin, targetBranch)
	if err != nil {
		return Report{}, fmt.Errorf("compare fork %s against .ax-ref: %w", forkRepo, err)
	}
	upHead, err := c.branchHead(ctx, upstreamRepo, targetBranch)
	if err != nil {
		return Report{}, fmt.Errorf("head of %s:%s: %w", upstreamRepo, targetBranch, err)
	}
	forkHead, err := c.branchHead(ctx, forkRepo, targetBranch)
	if err != nil {
		return Report{}, fmt.Errorf("head of %s:%s: %w", forkRepo, targetBranch, err)
	}

	return Report{
		UpstreamPin:       upPin,
		ForkPin:           forkPin,
		UpstreamStatus:    upCmp.Status,
		UpstreamAheadBy:   upCmp.AheadBy,
		UpstreamBehindBy:  upCmp.BehindBy,
		UpstreamHeadSHA:   upHead,
		WatchedFiles:      filterWatchedFiles(upCmp),
		CompareTruncated:  upCmp.truncated() || forkCmp.truncated() || stackCmp.truncated(),
		ForkAheadBy:       forkCmp.AheadBy,
		ForkBehindBy:      forkCmp.BehindBy,
		ForkHeadSHA:       forkHead,
		ForkWatchedFiles:  filterWatchedFiles(forkCmp),
		PatchStackCommits: stackCmp.AheadBy,
		PatchStackBehind:  stackCmp.BehindBy,
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

// apiClient is the thin GitHub REST client ax-pin-check needs.
type apiClient struct {
	base  string
	token string
}

func (c apiClient) get(ctx context.Context, path string, out any) error {
	url := strings.TrimRight(c.base, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if len(body) >= maxResponseBytes {
		return fmt.Errorf("GET %s: response exceeded %d bytes", url, maxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", url, err)
	}
	return nil
}

// compare fetches GET /repos/{repo}/compare/{base}...{head}.
func (c apiClient) compare(ctx context.Context, repo, base, head string) (compareResponse, error) {
	var out compareResponse
	err := c.get(ctx, fmt.Sprintf("/repos/%s/compare/%s...%s", repo, base, head), &out)
	return out, err
}

// branchHead fetches the current tip of repo's branch via
// GET /repos/{repo}/commits/heads/{branch}. The `heads/` form is the
// documented unambiguous spelling (a tag could share the name). This is
// the endpoint whose purpose is "where is the branch now"; the compare
// payload's commits[] is a capped list whose ordering guarantees depend
// on pagination defaults (#1418).
func (c apiClient) branchHead(ctx context.Context, repo, branch string) (string, error) {
	var out commitResponse
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/commits/heads/%s", repo, branch), &out); err != nil {
		return "", err
	}
	if !sha40Re.MatchString(out.SHA) {
		return "", fmt.Errorf("%s:%s: head sha %q is not a 40-char hex SHA", repo, branch, out.SHA)
	}
	return out.SHA, nil
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

	fmt.Fprintf(&b, "**Fork patch stack (`%s:main` vs `.ax-ref`):** %d commit(s) ahead, %d behind. **Upstream movement:** `%s:main` is %d commit(s) ahead of `.ax-ref`.\n\n",
		forkRepo, r.PatchStackCommits, r.PatchStackBehind, upstreamRepo, r.UpstreamAheadBy)

	if r.CompareTruncated {
		fmt.Fprintf(&b, "> **Warning:** a GitHub Compare payload was cut (more than %d files, or fewer commits listed than `total_commits`); the counts above are right but the watched-file list may be incomplete. Inspect the full diff before bumping pins.\n\n",
			compareFilesCeiling)
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
