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
