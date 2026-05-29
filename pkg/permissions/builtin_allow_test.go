// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package permissions

import (
	"strings"
	"testing"
)

// TestBundles_NoMutatingVerbs pins the conservative-by-construction
// invariant: the read_only bundle must never include a verb that can
// mutate the filesystem. If you're tempted to add `rm`, `mv`, `cp`,
// `chmod`, `chown`, `sudo`, `dd`, or `mkfs`/`shred`/`wipefs`,
// reconsider — the whole point of this bundle is that users can trust
// it without thinking. DO NOT delete this test to silence a CI failure
// after adding a new entry; either drop the entry or move it to a
// non-default bundle.
func TestBundles_ReadOnly_NoMutatingVerbs(t *testing.T) {
	t.Parallel()
	banned := []string{
		"bash:rm", "bash:rm ",
		"bash:mv", "bash:mv ",
		"bash:cp", "bash:cp ",
		"bash:chmod", "bash:chmod ",
		"bash:chown", "bash:chown ",
		"bash:sudo", "bash:sudo ",
		"bash:dd", "bash:dd ",
		"bash:mkfs", "bash:mkfs ",
		"bash:shred", "bash:shred ",
		"bash:wipefs", "bash:wipefs ",
	}
	for _, entry := range Bundles[BundleReadOnly] {
		for _, bad := range banned {
			if entry == strings.TrimSpace(bad) || strings.HasPrefix(entry, bad) {
				t.Errorf("read_only bundle contains mutating verb %q (matched %q)", entry, bad)
			}
		}
	}
}

// TestBundles_KnownNames keeps Bundles, KnownBundles(), and the
// exported bundle-name constants in lockstep. If a new bundle is added
// and this fails, update both Bundles and the constants together.
func TestBundles_KnownNames(t *testing.T) {
	t.Parallel()
	want := []string{BundleCoreAgentTools, BundleDevTools, BundleReadOnly} // sorted
	got := KnownBundles()
	if len(got) != len(want) {
		t.Fatalf("KnownBundles len = %d, want %d (%v vs %v)", len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("KnownBundles()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveBuiltinAllow_OffDropsEverything(t *testing.T) {
	t.Parallel()
	got, err := ResolveBuiltinAllow(false, []string{BundleDevTools, BundleCoreAgentTools})
	if err != nil {
		t.Fatalf("ResolveBuiltinAllow: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("use_builtin_allow=false must drop every entry, got %d", len(got))
	}
}

func TestResolveBuiltinAllow_DefaultIsReadOnly(t *testing.T) {
	t.Parallel()
	got, err := ResolveBuiltinAllow(true, nil)
	if err != nil {
		t.Fatalf("ResolveBuiltinAllow: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("read_only baseline must be non-empty")
	}
	// Sanity: contains pwd, no git (dev_tools is opt-in).
	want := map[string]bool{"bash:pwd": false, "bash:git status*": false}
	for _, e := range got {
		if _, ok := want[e]; ok {
			want[e] = true
		}
	}
	if !want["bash:pwd"] {
		t.Errorf("missing bash:pwd in read_only baseline")
	}
	if want["bash:git status*"] {
		t.Errorf("dev_tools entry leaked into read_only baseline")
	}
}

func TestResolveBuiltinAllow_ExtrasDedupe(t *testing.T) {
	t.Parallel()
	got, err := ResolveBuiltinAllow(true, []string{BundleDevTools, BundleDevTools})
	if err != nil {
		t.Fatalf("ResolveBuiltinAllow: %v", err)
	}
	count := 0
	for _, e := range got {
		if e == "bash:git status*" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected one bash:git status*, got %d (dedupe broken)", count)
	}
}

func TestResolveBuiltinAllow_UnknownBundleErrors(t *testing.T) {
	t.Parallel()
	_, err := ResolveBuiltinAllow(true, []string{"not_a_real_bundle"})
	if err == nil {
		t.Fatal("unknown bundle name must error")
	}
	if !strings.Contains(err.Error(), "not_a_real_bundle") {
		t.Errorf("error should mention the bad name; got %v", err)
	}
}

// TestResolveBuiltinAllow_DevToolsAdds confirms dev_tools layers on top
// of read_only without dropping it — earlier prototypes treated extras
// as replacement, which broke common bundles.
func TestResolveBuiltinAllow_DevToolsAdds(t *testing.T) {
	t.Parallel()
	got, err := ResolveBuiltinAllow(true, []string{BundleDevTools})
	if err != nil {
		t.Fatalf("ResolveBuiltinAllow: %v", err)
	}
	hasPwd, hasGit := false, false
	for _, e := range got {
		if e == "bash:pwd" {
			hasPwd = true
		}
		if e == "bash:git status*" {
			hasGit = true
		}
	}
	if !hasPwd || !hasGit {
		t.Errorf("dev_tools extras should layer on read_only: pwd=%v git=%v", hasPwd, hasGit)
	}
}
