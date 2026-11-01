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

// Package evals is the v0.3 parity harness: it loads the scenario
// corpus and the intent table, and (as the workstream lands) scores a
// recorded run against them.
//
// It lives under internal/ deliberately. The harness measures mast
// against a specific external agent; it is a repo quality gate, not one
// of mast's embedding contracts. Promoting it to pkg/ later is additive,
// whereas shipping an unproven API and retracting it is not.
//
// Everything here is a pure function over on-disk fixtures and a
// recorded event log: no provider, no credentials, no cluster.
package evals

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Scenario is one evaluation example. The inputs/outputs shape is
// carried through from upstream verbatim so the corpus stays diffable
// against its source; ID and Category are mast-side additions, needed
// because upstream examples are positional and an expected-fail
// allowlist has to name them.
type Scenario struct {
	ID       string          `json:"id"`
	Category string          `json:"category"`
	Inputs   ScenarioInputs  `json:"inputs"`
	Outputs  ScenarioOutputs `json:"outputs"`
}

// ScenarioInputs is the prompt side of an example: a cluster
// observation in prose. Scenarios are text, not live clusters, which is
// what lets the suite run anywhere.
type ScenarioInputs struct {
	Scenario string `json:"scenario"`
}

// ScenarioOutputs is the expectation side of an example.
type ScenarioOutputs struct {
	ExpectedTools    []string `json:"expected_tools"`
	ExpectedActions  []string `json:"expected_actions"`
	ExpectedResponse string   `json:"expected_response"`
}

// Provenance is the fixture header — the first record of every
// scenario file. It is required, not optional: the corpus records a
// contested source choice (which of two drifted upstream copies is
// authoritative) and known defects in the upstream evaluators, and a
// fixture that can be copied without carrying that context loses it.
type Provenance struct {
	Fixture               string `json:"fixture"`
	UpstreamRepo          string `json:"upstream_repo"`
	UpstreamPath          string `json:"upstream_path"`
	ScenarioCount         int    `json:"scenario_count"`
	Ported                string `json:"ported"`
	SourceChoice          string `json:"source_choice"`
	DriftDirection        string `json:"drift_direction"`
	ExpectedToolsNote     string `json:"expected_tools_note"`
	UpstreamEvaluatorNote string `json:"upstream_evaluator_note"`
}

// Dataset is one loaded scenario file.
type Dataset struct {
	Meta      Provenance
	Scenarios []Scenario
}

// header is the wrapper shape of the first JSONL record.
type header struct {
	Meta *Provenance `json:"_meta"`
}

// LoadDataset reads a scenario JSONL file: a required `{"_meta": {...}}`
// header record followed by one Scenario per line.
//
// Every parse failure is fatal rather than a skipped line. A silently
// dropped scenario would shrink the denominator of every score in the
// harness, which reads as progress.
func LoadDataset(path string) (Dataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return Dataset{}, fmt.Errorf("evals: open %q: %w", path, err)
	}
	defer f.Close()
	ds, err := parseDataset(f)
	if err != nil {
		return Dataset{}, fmt.Errorf("evals: %q: %w", path, err)
	}
	return ds, nil
}

func parseDataset(r io.Reader) (Dataset, error) {
	var ds Dataset
	sc := bufio.NewScanner(r)
	// Expected responses run long; the 64KiB default token size is not
	// enough headroom to rely on.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	seen := make(map[string]bool)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if lineNo == 1 {
			var h header
			if err := json.Unmarshal([]byte(line), &h); err != nil {
				return Dataset{}, fmt.Errorf("line 1: parse header: %w", err)
			}
			if h.Meta == nil {
				return Dataset{}, fmt.Errorf("line 1: missing the required _meta header record")
			}
			ds.Meta = *h.Meta
			continue
		}
		var s Scenario
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			return Dataset{}, fmt.Errorf("line %d: parse scenario: %w", lineNo, err)
		}
		if s.ID == "" {
			return Dataset{}, fmt.Errorf("line %d: scenario has no id", lineNo)
		}
		if seen[s.ID] {
			return Dataset{}, fmt.Errorf("line %d: duplicate scenario id %q", lineNo, s.ID)
		}
		seen[s.ID] = true
		ds.Scenarios = append(ds.Scenarios, s)
	}
	if err := sc.Err(); err != nil {
		return Dataset{}, fmt.Errorf("read: %w", err)
	}
	if ds.Meta.Fixture == "" {
		return Dataset{}, fmt.Errorf("empty dataset: no _meta header record")
	}
	// The header states how many scenarios the file should carry. If a
	// line is lost in an edit, this is what catches it.
	if n := ds.Meta.ScenarioCount; n != 0 && n != len(ds.Scenarios) {
		return Dataset{}, fmt.Errorf("header declares %d scenarios, file has %d", n, len(ds.Scenarios))
	}
	return ds, nil
}
