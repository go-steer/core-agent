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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tagFixture is a mirror whose release tags sit before, at and after
// the base the working copy is made at, with the IDs of every object a
// careless copy could leak.
type tagFixture struct {
	r *rig
	// leaks are objects that must never reach a copy, by what they are.
	leaks map[string]string
}

// newTagFixture tags the history newMirror makes (seed list, then its
// removal), plus two more commits on main:
//
//	v0.9.0       lightweight, on the "seed list" commit (its tree holds seeds.md)
//	v1.0.0       lightweight, on an ancestor of base that has a parent
//	v1.1.0       annotated, on the base itself
//	v1.1.1-rc.1  annotated pre-release, on the base (not a release tag)
//	notes        lightweight, on the base (not a release tag)
//	v1.2.0       annotated, on a commit AFTER the base, on another branch
func newTagFixture(t *testing.T) *tagFixture {
	t.Helper()
	r := newRig(t, commitAs())
	mirror := r.mirror
	work := filepath.Join(t.TempDir(), "w")
	gitT(t, "", "clone", "--quiet", mirror, work)
	gitT(t, work, "config", "user.name", "Pat Maintainer")
	gitT(t, work, "config", "user.email", "pat@example.com")
	leaks := map[string]string{
		"the seed-list commit":        gitT(t, work, "rev-parse", "HEAD~1"),
		"the seed-list commit's tree": gitT(t, work, "rev-parse", "HEAD~1^{tree}"),
		"the seed list's blob":        gitT(t, work, "rev-parse", "HEAD~1:seeds.md"),
	}
	gitT(t, work, "tag", "v0.9.0", "HEAD~1")
	for _, name := range []string{"one", "two"} {
		writeFile(t, filepath.Join(work, name+".txt"), name+"\n")
		gitT(t, work, "add", name+".txt")
		gitT(t, work, "commit", "--quiet", "-m", "add "+name)
		if name == "one" {
			gitT(t, work, "tag", "v1.0.0", "HEAD")
			leaks["v1.0.0's commit"] = gitT(t, work, "rev-parse", "HEAD")
		}
	}
	gitT(t, work, "push", "--quiet", mirror, "main")
	gitT(t, work, "tag", "-a", "v1.1.0", "-m", "release 1.1.0", "HEAD")
	// Annotated tags ON the base: a clone sends the tag object of every
	// refs/tags/* entry that points at a commit it sends (include-tag,
	// even with --no-tags), so a private repo keeping the fetched tags
	// under refs/tags would leak these messages into every copy.
	leaks["v1.1.0's tag object (on the base)"] = gitT(t, work, "rev-parse", "v1.1.0")
	gitT(t, work, "tag", "-a", "v1.1.1-rc.1", "-m", "rc", "HEAD")
	leaks["v1.1.1-rc.1's tag object (on the base)"] = gitT(t, work, "rev-parse", "v1.1.1-rc.1")
	gitT(t, work, "tag", "notes", "HEAD")
	gitT(t, work, "checkout", "--quiet", "-b", "upstream-fix")
	writeFile(t, filepath.Join(work, "fix.txt"), "the answer\n")
	gitT(t, work, "add", "fix.txt")
	gitT(t, work, "commit", "--quiet", "-m", "fix the seeded bug")
	leaks["the commit after the base"] = gitT(t, work, "rev-parse", "HEAD")
	leaks["the fix's blob"] = gitT(t, work, "rev-parse", "HEAD:fix.txt")
	gitT(t, work, "tag", "-a", "v1.2.0", "-m", "release 1.2.0", "HEAD")
	leaks["v1.2.0's tag object"] = gitT(t, work, "rev-parse", "v1.2.0")
	gitT(t, work, "push", "--quiet", mirror, "upstream-fix", "--tags")
	return &tagFixture{r: r, leaks: leaks}
}

func (f *tagFixture) prepare(t *testing.T) (dir, base string) {
	t.Helper()
	ctx := context.Background()
	g := f.r.d.git
	base, err := g.fetchBase(ctx, "")
	if err != nil {
		t.Fatalf("fetchBase: %v", err)
	}
	dir, err = g.prepareClone(ctx, 1)
	if err != nil {
		t.Fatalf("prepareClone: %v", err)
	}
	return dir, base
}

// gitIn runs git in the copy with the user's config isolated and lazy
// fetching off, returning its combined output and error.
func gitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_NO_LAZY_FETCH=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestCopyHasAncestorReleaseTags: every release tag that is an ancestor
// of the base is in the copy, by name, and explains itself.
func TestCopyHasAncestorReleaseTags(t *testing.T) {
	f := newTagFixture(t)
	dir, _ := f.prepare(t)
	got := strings.Fields(gitT(t, dir, "tag", "--list"))
	for _, want := range []string{"v0.1.0", "v0.9.0", "v1.0.0", "v1.1.0"} {
		if !slices.Contains(got, want) {
			t.Errorf("copy tags = %v, want %s (an ancestor of the base)", got, want)
		}
	}
	if typ := gitT(t, dir, "cat-file", "-t", "v1.0.0"); typ != "blob" {
		t.Errorf("v1.0.0 names a %s in the copy, want the placeholder blob", typ)
	}
	if out := gitT(t, dir, "show", "v1.1.0"); !strings.Contains(out, "release tag NAMES only") {
		t.Errorf("git show v1.1.0 in the copy = %q, want the placeholder's explanation", out)
	}
}

// TestCopyLacksTagsAfterBase: a tag on a commit after the base is not in
// the copy, nor is any object behind it, nor any historical object
// behind the tags that did cross: no older commit, tree, blob or tag
// message (the seed list sits in v0.9.0's tree).
func TestCopyLacksTagsAfterBase(t *testing.T) {
	f := newTagFixture(t)
	dir, base := f.prepare(t)
	got := strings.Fields(gitT(t, dir, "tag", "--list"))
	for _, bad := range []string{"v1.2.0", "v1.1.1-rc.1", "notes"} {
		if slices.Contains(got, bad) {
			t.Errorf("copy has tag %s; only release tags merged into the base may cross", bad)
		}
	}
	for what, id := range f.leaks {
		if _, err := gitIn(dir, "cat-file", "-e", id); err == nil {
			t.Errorf("copy holds %s (%s)", what, id)
		}
	}
	if n := gitT(t, dir, "rev-list", "--all", "--count"); n != "1" {
		t.Errorf("the copy reaches %s commits from all refs, want the depth-1 base only", n)
	}
	shallow, err := os.ReadFile(filepath.Join(dir, ".git", "shallow"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Fields(string(shallow)); !slices.Equal(lines, []string{base}) {
		t.Errorf(".git/shallow = %v, want only the base", lines)
	}
	if err := inspectCopy(dir, base); err != nil {
		t.Errorf("inspectCopy refused the copy the dispatcher made: %v", err)
	}
}

// TestVersionFallbackFindsTagInCopy runs the real presubmit inside the
// copy: its tag lookup must find the latest release (v1.1.0), not fail
// with "no release tags found".
func TestVersionFallbackFindsTagInCopy(t *testing.T) {
	f := newTagFixture(t)
	dir, _ := f.prepare(t)
	if err := os.MkdirAll(filepath.Join(dir, "internal", "version"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "internal", "version", "version.go"), "package version\n\nvar Version = \"v1.2.0-dev\"\n")
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "ci", "presubmits", "verify-version-fallback"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("verify-version-fallback in the copy: %v\n%s", err, out)
	}
}

// TestCopiedTagsDontBreakEverydayGit: the agent reads history and the
// repository maintains itself with ordinary commands. None may fail on
// a copied tag, or the agent chases what looks like corruption.
func TestCopiedTagsDontBreakEverydayGit(t *testing.T) {
	f := newTagFixture(t)
	dir, _ := f.prepare(t)
	for _, args := range [][]string{
		{"log", "--all", "--oneline", "--stat"},
		{"log", "--tags", "--oneline"},
		{"rev-list", "--all", "--objects"},
		{"show", "v1.0.0"},
		{"for-each-ref", "refs/tags"},
		{"status", "--short"},
		{"fsck", "--no-progress"},
		{"gc", "--quiet"},
	} {
		out, err := gitIn(dir, args...)
		if err != nil {
			t.Errorf("git %v in the copy: %v\n%s", args, err, out)
		}
		// A dangling tag is a tag object no ref names: history text that
		// came along with the clone.
		if args[0] == "fsck" && strings.Contains(out, "dangling tag") {
			t.Errorf("git fsck in the copy found a dangling tag object:\n%s", out)
		}
	}
}

// TestFetchBackWorksWithCopiedTags: the copied tags must not break the
// copy checks, the fetch-back, or a person's retry onto a fresh copy.
func TestFetchBackWorksWithCopiedTags(t *testing.T) {
	f := newTagFixture(t)
	dir, base := f.prepare(t)
	commitAs()(t, dir)
	tip, commits, err := f.r.d.git.collect(context.Background(), 1, dir, base)
	if err != nil {
		t.Fatalf("collect with copied tags: %v", err)
	}
	if len(commits) != 1 || tip == base {
		t.Errorf("collect = %s with %d commits, want the agent's one commit", tip, len(commits))
	}
	if _, err := f.r.d.git.prepareClone(context.Background(), 1); err != nil {
		t.Fatalf("a fresh copy after a fetch-back: %v", err)
	}
}

// TestFetchBaseUnshallowsAnOldPrivateRepo: a private repo left depth-1
// by an earlier dispatcher can't answer "which tags are ancestors";
// fetchBase deepens it once.
func TestFetchBaseUnshallowsAnOldPrivateRepo(t *testing.T) {
	f := newTagFixture(t)
	g := f.r.d.git
	ctx := context.Background()
	if err := g.ensurePrivate(ctx); err != nil {
		t.Fatal(err)
	}
	ref := "refs/heads/" + g.base
	gitT(t, g.privateDir, "fetch", "--quiet", "--no-tags", "--depth=1", g.remote, "+"+ref+":"+ref)
	if _, err := os.Stat(filepath.Join(g.privateDir, "shallow")); err != nil {
		t.Fatalf("setup: the private repo is not shallow: %v", err)
	}
	dir, _ := f.prepare(t)
	if got := strings.Fields(gitT(t, dir, "tag", "--list")); !slices.Contains(got, "v1.0.0") {
		t.Errorf("copy tags = %v after an old shallow private repo, want v1.0.0", got)
	}
}

// TestNoReachableReleaseTagIsAnError: a tags remote that shares no
// history with the mirror would hand the agent the red presubmit this
// exists to prevent; the copy is refused with a reason instead.
func TestNoReachableReleaseTagIsAnError(t *testing.T) {
	r := newRig(t, commitAs())
	other := filepath.Join(t.TempDir(), "other.git")
	src := filepath.Join(t.TempDir(), "src")
	gitT(t, "", "init", "--quiet", "--bare", other)
	gitT(t, "", "init", "--quiet", "-b", "main", src)
	writeFile(t, filepath.Join(src, "x"), "x\n")
	gitT(t, src, "add", "x")
	gitT(t, src, "-c", "user.name=U", "-c", "user.email=u@example.com", "commit", "--quiet", "-m", "unrelated")
	gitT(t, src, "tag", "v9.9.9")
	gitT(t, src, "push", "--quiet", other, "main", "--tags")
	r.d.git.tagsRemote = other
	ctx := context.Background()
	if _, err := r.d.git.fetchBase(ctx, ""); err != nil {
		t.Fatal(err)
	}
	// The mirror's own v0.1.0 was never fetched: tags come only from the
	// tags remote.
	if _, err := r.d.git.prepareClone(ctx, 1); err == nil || !strings.Contains(err.Error(), "no release tag") {
		t.Fatalf("prepareClone = %v, want a refusal naming the missing release tags", err)
	}
}
