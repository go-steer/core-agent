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

// The upstream half: what has k8s-lookout released, and is the image
// for a given release actually pullable.
//
// Everything here sits behind [Resolver] so that not one test in this
// package touches the network. That is not tidiness — this tool's
// package is compiled and tested by dev/ci/presubmits/test-unit, and a
// unit suite that reaches GitHub is a unit suite that goes red when
// GitHub does.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Release is one upstream release, in GitHub's own field names so a
// captured API response can be replayed through --releases verbatim.
type Release struct {
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	URL         string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
}

// Resolver answers the two questions this tool cannot answer from the
// working tree.
type Resolver interface {
	// Releases lists upstream releases, newest first. Drafts and
	// pre-releases are the implementation's to filter, not the caller's.
	Releases(ctx context.Context) ([]Release, error)
	// ImagePublished reports whether the container image for a release
	// tag can actually be pulled.
	ImagePublished(ctx context.Context, tag string) (bool, error)
}

// githubResolver reads releases from the GitHub API and image
// availability from the registry.
type githubResolver struct {
	repo   string // "owner/name"
	image  string // "ghcr.io/owner/name"
	token  string // optional; raises the anonymous rate limit
	client *http.Client
}

func newGitHubResolver(repo, image, token string) *githubResolver {
	return &githubResolver{
		repo: repo, image: image, token: token,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// Releases returns the newest page of published releases, newest
// semver first.
//
// One page (100) is deliberate: the question is "what is the latest",
// and a project that has cut more than 100 releases since the pin was
// last touched has problems this tool is not going to help with.
func (g *githubResolver) Releases(ctx context.Context) ([]Release, error) {
	url := "https://api.github.com/repos/" + g.repo + "/releases?per_page=100"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.token != "" {
		req.Header.Set("Authorization", "Bearer "+g.token)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", url, resp.Status, firstLine(body))
	}
	var all []Release
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("decode releases: %w", err)
	}
	return publishedReleases(all), nil
}

// publishedReleases drops drafts, pre-releases and anything whose tag
// is not a plain semver, then orders by version.
//
// Ordering by version rather than by date is the safer read: a
// backported patch release cut after a newer minor would otherwise
// present itself as "latest" and walk the pin backwards.
func publishedReleases(all []Release) []Release {
	var out []Release
	for _, r := range all {
		if r.Draft || r.Prerelease {
			continue
		}
		if _, ok := parseTag(r.Tag); !ok {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := parseTag(out[i].Tag)
		b, _ := parseTag(out[j].Tag)
		return b.less(a)
	})
	return out
}

// ghcrAccept is the set of manifest media types a HEAD has to declare
// it understands. Omit the index types and the registry answers 404 for
// a multi-arch image that is perfectly present.
const ghcrAccept = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// ImagePublished asks the registry whether <image>:<tag> exists, using
// an anonymous pull token. Public GHCR images need no credential, and
// deliberately not sending one keeps this reading what an operator
// following the recipe would see.
func (g *githubResolver) ImagePublished(ctx context.Context, tag string) (bool, error) {
	repoPath := strings.TrimPrefix(g.image, "ghcr.io/")
	token, err := g.ghcrToken(ctx, repoPath)
	if err != nil {
		return false, err
	}
	url := "https://ghcr.io/v2/" + repoPath + "/manifests/" + tag
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", ghcrAccept)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := g.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("HEAD %s: %s", url, resp.Status)
	}
}

func (g *githubResolver) ghcrToken(ctx context.Context, repoPath string) (string, error) {
	url := "https://ghcr.io/token?service=ghcr.io&scope=repository:" + repoPath + ":pull"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s: %s", url, resp.Status, firstLine(body))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode ghcr token: %w", err)
	}
	if out.Token == "" {
		return "", fmt.Errorf("ghcr returned an empty pull token for %s", repoPath)
	}
	return out.Token, nil
}

// snapshot is the offline replacement for the two live calls: a
// captured releases response plus the tags whose image is NOT yet
// published.
type snapshot struct {
	Releases    []Release `json:"releases"`
	Unpublished []string  `json:"unpublished"`
}

// stubResolver replays a snapshot. It is what --releases installs, and
// what every test in this package uses.
type stubResolver struct {
	releases    []Release
	unpublished map[string]bool
}

func newStubResolver(s snapshot) *stubResolver {
	missing := map[string]bool{}
	for _, tag := range s.Unpublished {
		missing[tag] = true
	}
	return &stubResolver{releases: publishedReleases(s.Releases), unpublished: missing}
}

func loadSnapshot(path string) (snapshot, error) {
	var s snapshot
	body, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return s, err
	}
	// A bare JSON array is a raw `gh api .../releases` capture. Accept
	// it, so replaying a live response takes no hand-editing.
	if trimmed := strings.TrimSpace(string(body)); strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(body, &s.Releases); err != nil {
			return s, fmt.Errorf("decode %s: %w", path, err)
		}
		return s, nil
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return s, fmt.Errorf("decode %s: %w", path, err)
	}
	return s, nil
}

// writeSnapshot records a resolution so a later invocation can replay
// it through --releases instead of asking upstream again.
//
// The weekly job runs this tool twice — once to decide whether there is
// drift, once to rewrite — and a release cut between the two calls
// would otherwise write the tree to a tag the verdict never evaluated,
// producing a pull request whose title and diff disagree. Handing the
// second run the first run's answer makes the whole job reason about
// one release. It is the same file format --releases already replays,
// so this adds a flag rather than a code path.
func writeSnapshot(path string, state upstreamState) error {
	snap := snapshot{Releases: state.Releases}
	for _, s := range state.Skipped {
		snap.Unpublished = append(snap.Unpublished, s.Tag)
	}
	body, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0o600)
}

func (r *stubResolver) Releases(context.Context) ([]Release, error) { return r.releases, nil }

func (r *stubResolver) ImagePublished(_ context.Context, tag string) (bool, error) {
	return !r.unpublished[tag], nil
}

// upstreamState is everything the upstream side of the question
// answers: the whole release list (the pull-request body quotes it),
// the release the pins should be at, and any newer ones skipped
// because their image is not pullable yet.
type upstreamState struct {
	Releases []Release
	Target   Release
	Skipped  []Release
}

// resolveUpstream walks the release list newest first and stops at the
// first one whose image is actually published.
//
// The skip exists because a GitHub release and its container image are
// two events, not one: k8s-lookout's release workflow tags first and
// pushes after, so for a few minutes "latest release" names an image
// that does not exist. Bumping to it would open a pull request whose
// kind e2e fails on ImagePullBackOff, and the next week's run would
// open another one. Checking is one HEAD against an anonymous
// registry token, which is cheap enough that there is no reason to
// guess instead.
//
// The skip is REPORTED, never silent: a release that stays imageless
// for a week is an upstream bug someone should see.
func resolveUpstream(ctx context.Context, r Resolver) (upstreamState, error) {
	releases, err := r.Releases(ctx)
	if err != nil {
		return upstreamState{}, err
	}
	if len(releases) == 0 {
		return upstreamState{}, fmt.Errorf("upstream published no releases with a plain semver tag")
	}
	state := upstreamState{Releases: releases}
	for _, rel := range releases {
		ok, pubErr := r.ImagePublished(ctx, rel.Tag)
		if pubErr != nil {
			return upstreamState{}, fmt.Errorf("check image for %s: %w", rel.Tag, pubErr)
		}
		if ok {
			state.Target = rel
			return state, nil
		}
		state.Skipped = append(state.Skipped, rel)
	}
	return upstreamState{}, fmt.Errorf(
		"none of the %d published releases (newest %s) has a pullable image — that is an "+
			"upstream problem, not drift", len(releases), releases[0].Tag)
}

// version is a plain major.minor.patch. Lookout tags nothing else, and
// a tag this cannot parse is one this tool declines to reason about
// rather than guesses at.
type version struct{ major, minor, patch int }

func parseTag(tag string) (version, bool) {
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if len(parts) != 3 || !strings.HasPrefix(tag, "v") {
		return version{}, false
	}
	var v version
	for i, dst := range []*int{&v.major, &v.minor, &v.patch} {
		n, err := strconv.Atoi(parts[i])
		if err != nil || n < 0 {
			return version{}, false
		}
		*dst = n
	}
	return v, true
}

func (a version) less(b version) bool {
	if a.major != b.major {
		return a.major < b.major
	}
	if a.minor != b.minor {
		return a.minor < b.minor
	}
	return a.patch < b.patch
}

func firstLine(body []byte) string {
	s := strings.TrimSpace(string(body))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
