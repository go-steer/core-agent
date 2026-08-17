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

package imagepin

import (
	"path/filepath"
	"regexp"
	"testing"
)

// TestResolve_IsScopedByFamily is the claim the whole refactor rests
// on: the image-name predicate is a PARAMETER, not a constant the
// walker knows about.
//
// One overlay, two images in the same `images:` block, two callers
// asking about different ones. If the family ever stopped being a
// parameter this test is what says so — and #680's gate, which passes
// its own daemon family through the same call, would be silently
// answering about somebody else's image.
func TestResolve_IsScopedByFamily(t *testing.T) {
	root := writeTree(t, map[string]string{
		"overlay/kustomization.yaml": "images:\n" +
			"  - name: ghcr.io/example/daemon\n    newTag: \"2.9.0\"\n" +
			"  - name: example.test/widget\n    newTag: \"v1.0.0\"\n",
	})
	dir := filepath.Join(root, "overlay")

	daemon := NewFamily(nil, "ghcr.io/example/daemon")
	widget := NewFamily(nil, "example.test/widget")
	absent := NewFamily(nil, "ghcr.io/example/nothing-here")

	for _, tc := range []struct {
		name    string
		fam     *Family
		wantTag string // "" means "no pin at all"
	}{
		{"daemon family sees the daemon tag", daemon, "2.9.0"},
		{"widget family sees the widget tag", widget, "v1.0.0"},
		{"an unrelated family sees nothing", absent, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pin, err := Resolve(dir, tc.fam)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			switch {
			case tc.wantTag == "" && pin != nil:
				t.Fatalf("Resolve found %+v, want no pin", pin)
			case tc.wantTag == "":
				return
			case pin == nil:
				t.Fatalf("Resolve found no pin, want tag %q", tc.wantTag)
			case pin.Tag != tc.wantTag:
				t.Errorf("Resolve tag = %q, want %q", pin.Tag, tc.wantTag)
			}
		})
	}
}

// TestSites_IsScopedByFamily is the same claim for the enumeration
// entry point, which is a separate code path.
func TestSites_IsScopedByFamily(t *testing.T) {
	root := writeTree(t, map[string]string{
		"recipes/demo/deploy.yaml": "image: ghcr.io/example/daemon:2.9.0\n" +
			"image: example.test/widget:v1.0.0\n",
	})
	tr := testTracked("recipes")
	if got, want := siteKeys(mustSites(t, tr, root)),
		[]string{"recipes/demo/deploy.yaml:image reference:v1.0.0"}; !equal(got, want) {
		t.Errorf("widget family found %v, want %v", got, want)
	}

	tr.Family = NewFamily(nil, "ghcr.io/example/daemon")
	tr.ImageRefRe = regexp.MustCompile(`ghcr\.io/example/daemon:([0-9]+\.[0-9]+\.[0-9]+)`)
	tr.TagRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	if got, want := siteKeys(mustSites(t, tr, root)),
		[]string{"recipes/demo/deploy.yaml:image reference:2.9.0"}; !equal(got, want) {
		t.Errorf("daemon family found %v, want %v", got, want)
	}
}

// TestNewFamily_PrefixShapedNamesDoNotShadowEachOther: core-agent and
// core-agent-slim are a real pair in this repo, and a leftmost-first
// alternation that tried the short name first would read the longer
// image as `core-agent` with a tag of `-slim:2.9.0`.
func TestNewFamily_PrefixShapedNamesDoNotShadowEachOther(t *testing.T) {
	fam := NewFamily(nil, "ghcr.io/example/agent", "ghcr.io/example/agent-slim")
	got := fam.refRe.FindString("image: ghcr.io/example/agent-slim:2.9.0")
	if want := "ghcr.io/example/agent-slim:2.9.0"; got != want {
		t.Errorf("refRe matched %q, want %q", got, want)
	}
	image, tag, _ := SplitImageRef(got)
	if image != "ghcr.io/example/agent-slim" || tag != "2.9.0" {
		t.Errorf("SplitImageRef = (%q, %q), want (%q, %q)",
			image, tag, "ghcr.io/example/agent-slim", "2.9.0")
	}
}
