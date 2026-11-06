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

package evals

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Scenario is one evaluation example.
type Scenario struct {
	ID       string          `json:"id"`
	Category string          `json:"category"`
	Inputs   ScenarioInputs  `json:"inputs"`
	Outputs  ScenarioOutputs `json:"outputs"`
}

// ScenarioInputs is the prompt side of an example: a cluster
// observation in prose.
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
// scenario file.
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

type header struct {
	Meta *Provenance `json:"_meta"`
}

// LoadDataset reads a scenario JSONL file: a required `{"_meta": {...}}`
// header record followed by one Scenario per line.
func LoadDataset(path string) (Dataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return Dataset{}, fmt.Errorf("evals: open %q: %w", path, err)
	}
	defer f.Close()
	ds, err := ParseDataset(f)
	if err != nil {
		return Dataset{}, fmt.Errorf("evals: %q: %w", path, err)
	}
	return ds, nil
}

// ParseDataset parses a scenario JSONL stream: a required `{"_meta": {...}}`
// header record followed by one Scenario per line.
func ParseDataset(r io.Reader) (Dataset, error) {
	var ds Dataset
	sc := bufio.NewScanner(r)
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
	if n := ds.Meta.ScenarioCount; n != 0 && n != len(ds.Scenarios) {
		return Dataset{}, fmt.Errorf("header declares %d scenarios, file has %d", n, len(ds.Scenarios))
	}
	return ds, nil
}
