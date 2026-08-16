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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// canned LiteLLM-shaped JSON. Keeps the test hermetic — no network,
// no fixture files. Rates are deliberately chosen to exercise the
// binary-repr rounding path (0.00000015 * 1_000_000 = 0.14999... in
// naive float math).
const cannedLiteLLM = `{
  "model-with-cache": {
    "input_cost_per_token": 0.0000015,
    "output_cost_per_token": 0.000009,
    "cache_read_input_token_cost": 0.00000015,
    "litellm_provider": "fake-vertex"
  },
  "model-without-cache": {
    "input_cost_per_token": 0.000001,
    "output_cost_per_token": 0.000005,
    "litellm_provider": "fake-anthropic"
  },
  "model-with-zero-cost": {
    "input_cost_per_token": 0,
    "output_cost_per_token": 0
  },
  "unrelated-model-not-in-allowlist": {
    "input_cost_per_token": 0.5,
    "output_cost_per_token": 1
  }
}`

func TestParse_MalformedEntryIsDropped(t *testing.T) {
	t.Parallel()
	body := []byte(`{"good": {"input_cost_per_token": 0.001}, "bad": "not-an-object"}`)
	out, err := parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := out["good"]; !ok {
		t.Errorf("good entry missing")
	}
	if _, ok := out["bad"]; ok {
		t.Errorf("malformed entry should have been dropped")
	}
}

func TestFilter_KeepsAllowedDropsMissingAndZero(t *testing.T) {
	t.Parallel()
	all, err := parse([]byte(cannedLiteLLM))
	if err != nil {
		t.Fatalf("parse canned: %v", err)
	}
	allow := []string{
		"model-with-cache",
		"model-without-cache",
		"model-with-zero-cost", // must be reported missing
		"absent-model",         // must be reported missing
	}
	kept, missing := filter(all, allow)

	if len(kept) != 2 {
		t.Fatalf("kept %d, want 2: %+v", len(kept), kept)
	}
	// Kept entries are sorted by name.
	if kept[0].Name != "model-with-cache" || kept[1].Name != "model-without-cache" {
		t.Errorf("kept order wrong: %+v", kept)
	}

	// Verify the binary-repr rounding actually fired: 0.00000015 * 1e6
	// = 0.15000000000000002 in naive float; round6 must snap it to 0.15.
	if kept[0].CachedInputPerMTok != 0.15 {
		t.Errorf("cached rate not rounded cleanly: %v (want 0.15)", kept[0].CachedInputPerMTok)
	}
	if kept[0].InputPerMTok != 1.5 || kept[0].OutputPerMTok != 9 {
		t.Errorf("input/output rates wrong: %+v", kept[0])
	}

	// Model without cache: CachedInputPerMTok stays 0 and the
	// generator emits the shorter literal (no cache field). We assert
	// on the input field only here — the format check lives below.
	if kept[1].CachedInputPerMTok != 0 {
		t.Errorf("no-cache entry should have zero cached rate: %+v", kept[1])
	}

	// Both zero-cost and absent-from-catalog must be reported so
	// operators regenerating notice the allowlist has drifted.
	missingJoined := strings.Join(missing, ",")
	if !strings.Contains(missingJoined, "absent-model") {
		t.Errorf("missing report should include absent-model: %v", missing)
	}
	if !strings.Contains(missingJoined, "model-with-zero-cost") {
		t.Errorf("missing report should include zero-cost model: %v", missing)
	}
}

func TestRender_ProducesCompilableGoWithExpectedShape(t *testing.T) {
	t.Parallel()
	kept := []generatedEntry{
		{Name: "cached-model", InputPerMTok: 1.5, CachedInputPerMTok: 0.15, OutputPerMTok: 9, Provider: "fake"},
		{Name: "no-cache-model", InputPerMTok: 1, OutputPerMTok: 5, Provider: "fake"},
	}
	when := time.Date(2026, 7, 16, 12, 34, 56, 0, time.UTC)
	src, err := render(kept, when, "test-source")
	if err != nil {
		// format.Source failure = uncompilable output; the whole point
		// of the render step is to guarantee this doesn't happen.
		t.Fatalf("render: %v", err)
	}
	got := string(src)

	// Header carries the regen date + source.
	if !strings.Contains(got, "Regenerated 2026-07-16 from test-source") {
		t.Errorf("header missing regen line:\n%s", got)
	}
	// Both models present, alphabetically ordered in the input, and
	// each carries a UpdatedAt time.Date literal. gofmt column-aligns
	// map entries, so match key + prefix separately rather than
	// pinning exact whitespace.
	if !strings.Contains(got, `"cached-model":`) || !strings.Contains(got, "InputPerMTok: 1.5, CachedInputPerMTok: 0.15") {
		t.Errorf("cached-model line missing/wrong shape:\n%s", got)
	}
	if !strings.Contains(got, `"no-cache-model":`) || !strings.Contains(got, "InputPerMTok: 1, OutputPerMTok: 5") {
		t.Errorf("no-cache-model line missing/wrong shape (should omit CachedInputPerMTok):\n%s", got)
	}
	// The no-cache entry must NOT carry the CachedInputPerMTok field.
	if strings.Contains(got, `"no-cache-model":`) {
		// Slice just the no-cache line to check.
		i := strings.Index(got, `"no-cache-model":`)
		line := got[i : i+strings.Index(got[i:], "\n")]
		if strings.Contains(line, "CachedInputPerMTok") {
			t.Errorf("no-cache entry should not emit CachedInputPerMTok: %s", line)
		}
	}
	if !strings.Contains(got, "time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)") {
		t.Errorf("UpdatedAt time.Date literal missing (date must be truncated):\n%s", got)
	}
	// Belt-and-suspenders: the wall-clock time from `when` must NOT
	// leak into the output. Same-day regens should be byte-identical
	// regardless of when they ran.
	if strings.Contains(got, "12, 34, 56") {
		t.Errorf("wall-clock leaked into output — same-day regens will produce diff noise")
	}
}

func TestRound6_HandlesBinaryReprArtifacts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want float64
	}{
		{0.09999999999999999, 0.1},
		{0.14999999999999997, 0.15},
		{1.5, 1.5},
		{0, 0},
		{1_000_000.0000001, 1_000_000},
	}
	for _, c := range cases {
		if got := round6(c.in); got != c.want {
			t.Errorf("round6(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// --- --check / drift detection --------------------------------------

// renderDay renders entries stamped with a given date, failing the test
// on render error. Used by the drift tests to produce two snapshots
// that differ only in their UpdatedAt.
func renderDay(t *testing.T, entries []generatedEntry, y int, m time.Month, d int) []byte {
	t.Helper()
	src, err := render(entries, time.Date(y, m, d, 0, 0, 0, 0, time.UTC), "canned://litellm")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return src
}

// threeEntries is a small sorted table. Names are equal-length so any
// column padding in the rendered output comes from the rate values,
// which is what the alignment test below needs.
func threeEntries() []generatedEntry {
	return []generatedEntry{
		{Name: "aaa", InputPerMTok: 1, OutputPerMTok: 2, Provider: "fake"},
		{Name: "bbb", InputPerMTok: 3, OutputPerMTok: 4, Provider: "fake"},
		{Name: "ccc", InputPerMTok: 5, OutputPerMTok: 6, Provider: "fake"},
	}
}

// The headline regression. Regenerating on a later day rewrites every
// UpdatedAt, so a byte comparison — which is what all four callers used
// to do via `git diff --quiet` — reports drift even though no rate
// moved. That false positive would have opened a no-op PR every Monday
// and made the release guards fail unless you regenerated on the same
// UTC day you tagged.
func TestNormalize_DateOnlyRegenIsNotDrift(t *testing.T) {
	t.Parallel()
	entries := threeEntries()
	day1 := renderDay(t, entries, 2026, time.August, 15)
	day2 := renderDay(t, entries, 2026, time.August, 16)

	// Precondition: this is exactly the drift the old check reported.
	if bytes.Equal(day1, day2) {
		t.Fatal("precondition failed: same-rate renders on different days should differ byte-wise")
	}
	if !bytes.Equal(normalize(day1), normalize(day2)) {
		t.Errorf("date-only regen reported as drift:\n--- %s\n+++ %s",
			normalize(day1), normalize(day2))
	}
	if got := diffEntries(normalize(day1), normalize(day2)); len(got) != 0 {
		t.Errorf("diffEntries on a date-only regen = %q, want none", got)
	}
}

// Checking a pinned local snapshot is a documented offline workflow.
// The provenance string in the header differs from the committed
// file's URL, which must not by itself read as a price change.
func TestNormalize_SourceProvenanceIsNotDrift(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, time.August, 15, 0, 0, 0, 0, time.UTC)
	fromURL, err := render(threeEntries(), day, defaultLiteLLMSource)
	if err != nil {
		t.Fatalf("render url: %v", err)
	}
	fromFile, err := render(threeEntries(), day, "/tmp/litellm-snapshot.json")
	if err != nil {
		t.Fatalf("render file: %v", err)
	}
	if bytes.Equal(fromURL, fromFile) {
		t.Fatal("precondition failed: differing --source should change the header")
	}
	if !bytes.Equal(normalize(fromURL), normalize(fromFile)) {
		t.Error("a different --source was reported as rate drift")
	}
}

// The other half: normalization must not swallow a genuine rate move.
func TestNormalize_RateChangeIsStillDrift(t *testing.T) {
	t.Parallel()
	before := renderDay(t, threeEntries(), 2026, time.August, 15)

	moved := threeEntries()
	moved[1].OutputPerMTok = 40 // bbb: 4 -> 40
	after := renderDay(t, moved, 2026, time.August, 15)

	if bytes.Equal(normalize(before), normalize(after)) {
		t.Fatal("a changed output rate was normalized away")
	}
	got := diffEntries(normalize(before), normalize(after))
	if len(got) == 0 || !strings.Contains(got[0], "changed bbb") {
		t.Errorf("diffEntries = %q, want a 'changed bbb' report", got)
	}
}

// gofmt column-aligns the map literal, so one entry gaining digits
// re-pads its neighbours' trailing comments. Without collapsing runs of
// spaces the report blames models whose rates never moved — noise that
// would train reviewers to skim the very diff they must read carefully.
func TestDiffEntries_AlignmentChurnDoesNotBlameUnchangedModels(t *testing.T) {
	t.Parallel()
	before := renderDay(t, threeEntries(), 2026, time.August, 15)

	widened := threeEntries()
	widened[1].InputPerMTok = 1234.5678 // much wider than "3"
	after := renderDay(t, widened, 2026, time.August, 15)

	var changed []string
	for _, line := range diffEntries(normalize(before), normalize(after)) {
		if strings.HasPrefix(line, "changed ") ||
			strings.HasPrefix(line, "added ") ||
			strings.HasPrefix(line, "removed ") {
			changed = append(changed, line)
		}
	}
	if len(changed) != 1 || !strings.Contains(changed[0], "bbb") {
		t.Errorf("alignment churn leaked into the report: got %q, want only a bbb change", changed)
	}
}

func TestDiffEntries_ReportsAddedAndRemoved(t *testing.T) {
	t.Parallel()
	before := renderDay(t, threeEntries(), 2026, time.August, 15)

	// Drop "bbb", append "ddd" — filter() emits sorted output, so the
	// rendered table stays in name order.
	churned := []generatedEntry{
		{Name: "aaa", InputPerMTok: 1, OutputPerMTok: 2, Provider: "fake"},
		{Name: "ccc", InputPerMTok: 5, OutputPerMTok: 6, Provider: "fake"},
		{Name: "ddd", InputPerMTok: 7, OutputPerMTok: 8, Provider: "fake"},
	}
	after := renderDay(t, churned, 2026, time.August, 15)

	joined := strings.Join(diffEntries(normalize(before), normalize(after)), "\n")
	if !strings.Contains(joined, "removed bbb") {
		t.Errorf("missing removal report in:\n%s", joined)
	}
	if !strings.Contains(joined, "added   ddd") {
		t.Errorf("missing addition report in:\n%s", joined)
	}
}

// checkDrift's stdout line is a machine contract: pricing-regen.yml,
// release.yml, cut-dev-tag.sh and cut-ga-tag.sh all branch on the exact
// strings "drift=true" / "drift=false". Drift can't ride on the exit
// code because `go run` collapses every non-zero child status to 1,
// which would make a failed LiteLLM fetch look like a price change.
func TestCheckDrift_StdoutVerdictIsTheCallerContract(t *testing.T) {
	entries := threeEntries()
	onDisk := renderDay(t, entries, 2026, time.August, 15)

	moved := threeEntries()
	moved[0].InputPerMTok = 99
	generatedMoved := renderDay(t, moved, 2026, time.August, 16)

	// Same rates, later day — must NOT be drift.
	generatedSameRates := renderDay(t, entries, 2026, time.August, 16)

	for _, tc := range []struct {
		name      string
		generated []byte
		want      string
	}{
		{"same rates, newer stamp", generatedSameRates, "drift=false\n"},
		{"moved rate", generatedMoved, "drift=true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "builtin.go")
			if err := os.WriteFile(path, onDisk, 0o600); err != nil {
				t.Fatalf("seed %s: %v", path, err)
			}
			if got := captureStdout(t, func() { checkDrift(path, tc.generated) }); got != tc.want {
				t.Errorf("stdout = %q, want %q", got, tc.want)
			}
		})
	}
}

// captureStdout redirects os.Stdout for the duration of fn. checkDrift
// writes its verdict with fmt.Println, so there is no injectable writer
// to hook instead. Not parallel-safe — the tests using it must not call
// t.Parallel().
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r)
		done <- buf.String()
	}()
	fn()
	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	return <-done
}
