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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *store {
	t.Helper()
	root := t.TempDir()
	st, err := newStore(root, filepath.Join(root, "library"), "session-1")
	if err != nil {
		t.Fatalf("newStore: %v", err)
	}
	return st
}

// TestSlugifyContainsModelAuthoredNames is the other security test:
// the preflight name arrives straight off a model tool call and is
// used to build a filename, so it must not be able to escape the
// library directory.
func TestSlugifyContainsModelAuthoredNames(t *testing.T) {
	cases := map[string]string{
		"maxtext-v5e-256":            "maxtext-v5e-256",
		"MaxText v5e 16x16":          "maxtext-v5e-16x16",
		"../../etc/cron.d/pwn":       "etc-cron-d-pwn",
		"/absolute/path":             "absolute-path",
		"..":                         "",
		"   ":                        "",
		"trailing---":                "trailing",
		"emoji 🚀 name":               "emoji-name",
		strings.Repeat("long", 40):   strings.Repeat("long", 20),
		"name\nwith\nnewlines":       "name-with-newlines",
		"semi;colon && rm -rf /":     "semi-colon-rm-rf",
		"dots.and.dashes-and_scores": "dots-and-dashes-and-scores",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"../../etc/passwd", "/absolute/path", "a/b/c"} {
		if strings.ContainsAny(slugify(in), `/\.`) {
			t.Errorf("slugify(%q) = %q still contains path characters", in, slugify(in))
		}
	}
}

func TestSaveLibraryEntryWritesManifestAndMetadata(t *testing.T) {
	st := newTestStore(t)
	const manifest = "apiVersion: batch/v1\nkind: Job\n"
	path, err := st.saveLibraryEntry(libraryEntry{
		Name:        "MaxText v5e 16x16",
		Features:    []string{"jax", "v5e"},
		TargetLabel: "tpu-v5-lite-podslice/16x16",
		Metadata:    "derived from prod",
		SourceJob:   "prod-jobset.yaml",
	}, manifest)
	if err != nil {
		t.Fatalf("saveLibraryEntry: %v", err)
	}
	if filepath.Base(path) != "maxtext-v5e-16x16.yaml" {
		t.Errorf("manifest path = %q, want the slugified name", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved manifest: %v", err)
	}
	if string(body) != manifest {
		t.Errorf("saved manifest = %q, want it byte-identical to what was verified", body)
	}

	metaBody, err := os.ReadFile(strings.TrimSuffix(path, ".yaml") + ".json")
	if err != nil {
		t.Fatalf("read metadata sidecar: %v", err)
	}
	var got libraryEntry
	if err := json.Unmarshal(metaBody, &got); err != nil {
		t.Fatalf("parse metadata: %v", err)
	}
	if got.TargetLabel != "tpu-v5-lite-podslice/16x16" || got.SourceJob != "prod-jobset.yaml" {
		t.Errorf("metadata lost fields: %+v", got)
	}
	if got.SavedAt.IsZero() {
		t.Error("metadata has no saved_at timestamp")
	}

	names, err := st.listLibrary()
	if err != nil {
		t.Fatalf("listLibrary: %v", err)
	}
	if len(names) != 1 || names[0] != "maxtext-v5e-16x16" {
		t.Errorf("listLibrary = %v", names)
	}
}

func TestSaveLibraryEntryRejectsBadInput(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.saveLibraryEntry(libraryEntry{Name: "ok"}, "  \n"); err == nil {
		t.Error("an empty manifest must be rejected")
	}
	if _, err := st.saveLibraryEntry(libraryEntry{Name: "../.."}, "kind: Job\n"); err == nil {
		t.Error("a name that slugifies to nothing must be rejected")
	}
}

func TestStripCheckerInstructions(t *testing.T) {
	in := "metadata:\n  name: preflight\n  checker-instruction: look for jax.devices()\nspec:\n" +
		"    checker-instruction: and this indented one too\n  replicas: 1\n"
	got := stripCheckerInstructions(in)
	if strings.Contains(got, "checker-instruction") {
		t.Errorf("checker instructions survived stripping:\n%s", got)
	}
	for _, keep := range []string{"name: preflight", "replicas: 1", "spec:"} {
		if !strings.Contains(got, keep) {
			t.Errorf("stripping removed %q:\n%s", keep, got)
		}
	}
	// A value that merely mentions the word is left alone.
	const benign = "  note: the checker-instruction key is stripped\n"
	if stripCheckerInstructions(benign) != benign {
		t.Error("only whole checker-instruction lines should be removed")
	}
}

func TestSourceAndCandidateRoundTrip(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.readSource(); err == nil {
		t.Error("reading a missing source must fail rather than return empty")
	}
	if err := st.writeSource("kind: JobSet\n"); err != nil {
		t.Fatalf("writeSource: %v", err)
	}
	if err := st.writeCandidate("kind: Job\n"); err != nil {
		t.Fatalf("writeCandidate: %v", err)
	}
	src, err := st.readSource()
	if err != nil || src != "kind: JobSet\n" {
		t.Errorf("readSource = %q, %v", src, err)
	}
	cand, err := st.readCandidate()
	if err != nil || cand != "kind: Job\n" {
		t.Errorf("readCandidate = %q, %v", cand, err)
	}
}

func TestExperienceLogAndGrep(t *testing.T) {
	st := newTestStore(t)
	if err := st.appendExperience("jax-oom", "reduce per_device_batch_size before blaming the topology"); err != nil {
		t.Fatalf("appendExperience: %v", err)
	}
	if err := st.appendExperience("gke", "admission webhook rejects hostNetwork in the test namespace"); err != nil {
		t.Fatalf("appendExperience: %v", err)
	}
	if _, err := st.saveLibraryEntry(libraryEntry{
		Name:     "maxtext-v5e-16x16",
		Features: []string{"jax", "maxtext"},
	}, "kind: Job\nmetadata:\n  name: preflight-maxtext\n"); err != nil {
		t.Fatalf("saveLibraryEntry: %v", err)
	}

	matches := st.grep("maxtext", 40)
	if len(matches) == 0 {
		t.Fatal("grep found nothing for a term that is in the library")
	}
	var fromLibrary bool
	for _, m := range matches {
		if strings.HasPrefix(m, "library/") {
			fromLibrary = true
		}
	}
	if !fromLibrary {
		t.Errorf("grep did not search the library: %v", matches)
	}

	exp := st.grep("webhook", 40)
	if len(exp) != 1 || !strings.HasPrefix(exp[0], "experience: ") {
		t.Errorf("grep of the experience log = %v", exp)
	}
	if got := st.grep("   ", 40); got != nil {
		t.Errorf("an empty query should match nothing, got %v", got)
	}
	if got := st.grep("maxtext jax", 1); len(got) != 1 {
		t.Errorf("grep ignored its limit: %v", got)
	}
}
