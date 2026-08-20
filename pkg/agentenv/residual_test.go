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

package agentenv

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// TestResidualRefsTrackNilResolver is the #712 case: no manifest means
// a nil *Resolver means a nil interpolator, and the loaders pass bodies
// through untouched. Track's result must still be non-nil and must
// record what survived.
func TestResidualRefsTrackNilResolver(t *testing.T) {
	t.Parallel()
	var nilResolver *Resolver
	tracker := &ResidualRefs{}
	interp := tracker.Track(nilResolver)
	if interp == nil {
		t.Fatal("Track(nil resolver) returned nil; loaders skip a nil interpolator, so nothing would be scanned")
	}

	body := "Operate in project ${env:GOOGLE_CLOUD_PROJECT}, cluster ${env:GKE_CLUSTER}."
	if got := interp(body); got != body {
		t.Errorf("tracked no-op interpolator must not alter the body:\n got %q\nwant %q", got, body)
	}

	want := []string{"${env:GKE_CLUSTER}", "${env:GOOGLE_CLOUD_PROJECT}"}
	if got := tracker.Placeholders(); !reflect.DeepEqual(got, want) {
		t.Errorf("Placeholders() = %v; want %v", got, want)
	}
}

// TestResidualRefsCleanBundleStaysQuiet guards the false-positive side:
// bundles that reference nothing, and bundles whose references all
// resolve, must produce no warning at all.
func TestResidualRefsCleanBundleStaysQuiet(t *testing.T) {
	t.Parallel()

	t.Run("no references, no manifest", func(t *testing.T) {
		t.Parallel()
		var nilResolver *Resolver
		tracker := &ResidualRefs{}
		interp := tracker.Track(nilResolver)
		interp("A persona that mentions no environment variables at all.")
		if got := tracker.Placeholders(); len(got) != 0 {
			t.Errorf("Placeholders() = %v; want none", got)
		}
		if w := tracker.Warning("/bundle/.agents"); w != "" {
			t.Errorf("Warning() = %q; want empty", w)
		}
	})

	t.Run("references all substituted", func(t *testing.T) {
		t.Parallel()
		r := NewResolver(&Manifest{Version: 1, Env: []Entry{{Name: "PROJECT"}}},
			func(string) (string, bool) { return "acme-prod", true })
		tracker := &ResidualRefs{}
		interp := tracker.Track(r)
		if got := interp("project ${env:PROJECT}"); got != "project acme-prod" {
			t.Fatalf("interpolation regressed: got %q", got)
		}
		if got := tracker.Placeholders(); len(got) != 0 {
			t.Errorf("Placeholders() = %v; want none", got)
		}
		if w := tracker.Warning("/bundle/.agents"); w != "" {
			t.Errorf("Warning() = %q; want empty", w)
		}
	})
}

// TestResidualRefsMalformedSurvivesActiveManifest covers the case a
// manifest cannot rescue: a placeholder whose name is not a valid
// identifier never matches interpRe, so interpolation runs and leaves it
// in place. This is why detection reads the loaded content rather than
// inferring from the manifest's absence.
func TestResidualRefsMalformedSurvivesActiveManifest(t *testing.T) {
	t.Parallel()
	r := NewResolver(&Manifest{Version: 1, Env: []Entry{{Name: "PROJECT"}}},
		func(string) (string, bool) { return "acme-prod", true })
	tracker := &ResidualRefs{}
	interp := tracker.Track(r)
	interp("project ${env:PROJECT}, cluster ${env:gke-cluster}")

	want := []string{"${env:gke-cluster}"}
	if got := tracker.Placeholders(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Placeholders() = %v; want %v", got, want)
	}
	w := tracker.Warning("/bundle/.agents")
	if !strings.Contains(w, "${env:gke-cluster}") {
		t.Errorf("Warning() should name the surviving placeholder verbatim; got %q", w)
	}
	if !strings.Contains(w, "manifest is active") {
		t.Errorf("Warning() with a manifest must not blame a missing manifest; got %q", w)
	}
}

// TestResidualRefsWarningText pins the three diagnoses apart. The
// remedy differs in each, so a line that hedged between them would send
// an operator to the wrong file.
func TestResidualRefsWarningText(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		resolver  *Resolver
		agentsDir string
		wantSubs  []string
	}{
		{
			name:      "no manifest in a discovered agents dir",
			resolver:  nil,
			agentsDir: "/bundle/.agents",
			wantSubs:  []string{"env.yaml", "env.json", "/bundle/.agents", "interpolation is OFF"},
		},
		{
			name:      "no agents dir at all",
			resolver:  nil,
			agentsDir: "",
			wantSubs:  []string{"no .agents/ directory was discovered", "interpolation is OFF"},
		},
		{
			name:      "manifest active",
			resolver:  NewResolver(&Manifest{Version: 1}, func(string) (string, bool) { return "", false }),
			agentsDir: "/bundle/.agents",
			wantSubs:  []string{"manifest is active", "no leading digit"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tracker := &ResidualRefs{}
			tracker.Track(tc.resolver)("see ${env:x-y}")
			w := tracker.Warning(tc.agentsDir)
			if w == "" {
				t.Fatal("Warning() = empty; want a diagnostic")
			}
			if !strings.HasPrefix(w, "agentenv: 1 ${env:...} placeholder(s) survived loading") {
				t.Errorf("Warning() prefix changed: %q", w)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(w, sub) {
					t.Errorf("Warning() missing %q; got %q", sub, w)
				}
			}
		})
	}
}

// TestResidualRefsWarningTruncates keeps the boot line readable when a
// bundle is wholly uninterpolated. The count stays exact; only the list
// is clipped.
func TestResidualRefsWarningTruncates(t *testing.T) {
	t.Parallel()
	tracker := &ResidualRefs{}
	interp := tracker.Track(nil)
	for i := range 20 {
		interp(fmt.Sprintf("${env:VAR_%02d}", i))
	}
	w := tracker.Warning("/bundle/.agents")
	if !strings.Contains(w, "agentenv: 20 ${env:...} placeholder(s)") {
		t.Errorf("count should be the full 20; got %q", w)
	}
	if !strings.Contains(w, "(+8 more)") {
		t.Errorf("want a (+8 more) elision; got %q", w)
	}
	if strings.Contains(w, "${env:VAR_19}") {
		t.Errorf("want the tail clipped; got %q", w)
	}
}

// TestResidualRefsNilReceiver mirrors Resolver's nil-safety contract:
// every method tolerates a nil receiver, and Track degrades to the bare
// resolver interpolator so a caller that wants no tracking need not
// special-case the wiring.
func TestResidualRefsNilReceiver(t *testing.T) {
	t.Parallel()
	var tracker *ResidualRefs
	if got := tracker.Track(nil); got != nil {
		t.Error("nil receiver + nil resolver should yield the resolver's own (nil) interpolator")
	}
	if got := tracker.Placeholders(); got != nil {
		t.Errorf("nil.Placeholders() = %v; want nil", got)
	}
	if got := tracker.Warning(""); got != "" {
		t.Errorf("nil.Warning() = %q; want empty", got)
	}
}

// TestResidualRefsConcurrent runs the tracked interpolator from many
// goroutines the way the multi-session factory does — one interpolator
// shared across concurrent session builds. Run with -race.
func TestResidualRefsConcurrent(t *testing.T) {
	t.Parallel()
	tracker := &ResidualRefs{}
	interp := tracker.Track(nil)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			interp(fmt.Sprintf("body ${env:SHARED} ${env:PER_%d}", i))
			_ = tracker.Placeholders()
		}()
	}
	wg.Wait()
	if got := tracker.Placeholders(); len(got) != 17 {
		t.Errorf("Placeholders() = %d entries; want 17 (1 shared + 16 per-goroutine)", len(got))
	}
}
