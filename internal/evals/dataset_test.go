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
	"path/filepath"
	"strings"
	"testing"
)

const (
	scenarioDir   = "../../testdata/evals/scenarios"
	intentsPath   = "../../testdata/evals/intents.yaml"
	langchainFile = "langchain-sre.jsonl"
)

func loadLangChain(t *testing.T) Dataset {
	t.Helper()
	ds, err := LoadDataset(filepath.Join(scenarioDir, langchainFile))
	if err != nil {
		t.Fatalf("LoadDataset: %v", err)
	}
	return ds
}

// TestLoadDataset_PortedCorpus is the W0.1 done-when: the corpus loads
// and nothing is silently dropped. The count is pinned to 31 in two
// independent places (here and the fixture header) because a lost line
// would shrink every score's denominator and read as progress.
func TestLoadDataset_PortedCorpus(t *testing.T) {
	ds := loadLangChain(t)

	if got := len(ds.Scenarios); got != 31 {
		t.Fatalf("loaded %d scenarios, want 31", got)
	}
	if ds.Meta.ScenarioCount != 31 {
		t.Errorf("header ScenarioCount = %d, want 31", ds.Meta.ScenarioCount)
	}
	if !strings.Contains(ds.Meta.UpstreamPath, ".jsonl") {
		t.Errorf("header UpstreamPath = %q, want the .jsonl copy", ds.Meta.UpstreamPath)
	}
	// The source choice between the two drifted upstream copies is
	// contested enough that losing the rationale is a real risk.
	for _, field := range []struct{ name, val string }{
		{"SourceChoice", ds.Meta.SourceChoice},
		{"DriftDirection", ds.Meta.DriftDirection},
		{"UpstreamEvaluatorNote", ds.Meta.UpstreamEvaluatorNote},
	} {
		if strings.TrimSpace(field.val) == "" {
			t.Errorf("header %s is empty; provenance must travel with the fixture", field.name)
		}
	}

	for _, s := range ds.Scenarios {
		if strings.TrimSpace(s.Inputs.Scenario) == "" {
			t.Errorf("%s: empty scenario prompt", s.ID)
		}
		if len(s.Outputs.ExpectedTools) == 0 {
			t.Errorf("%s: no expected_tools", s.ID)
		}
		if len(s.Outputs.ExpectedActions) == 0 {
			t.Errorf("%s: no expected_actions", s.ID)
		}
		if strings.TrimSpace(s.Outputs.ExpectedResponse) == "" {
			t.Errorf("%s: empty expected_response", s.ID)
		}
	}
}

// TestLoadDataset_Rejects covers the ways a fixture can go wrong in an
// edit. Each of these would otherwise degrade quietly into a smaller
// corpus rather than a failure.
func TestLoadDataset_Rejects(t *testing.T) {
	const hdr = `{"_meta":{"fixture":"t","scenario_count":1}}`
	row := func(id string) string {
		return `{"id":"` + id + `","category":"c","inputs":{"scenario":"s"},` +
			`"outputs":{"expected_tools":["kubectl_get_pods"],` +
			`"expected_actions":["a"],"expected_response":"OK: fine"}}`
	}

	tests := []struct {
		name string
		body string
		want string
	}{
		{"no header", row("A") + "\n", "missing the required _meta header"},
		{"empty file", "", "no _meta header record"},
		{"count mismatch", hdr + "\n" + row("A") + "\n" + row("B") + "\n", "header declares 1"},
		{"duplicate id", `{"_meta":{"fixture":"t","scenario_count":2}}` + "\n" + row("A") + "\n" + row("A") + "\n", "duplicate scenario id"},
		{"missing id", hdr + "\n" + `{"category":"c","inputs":{"scenario":"s"},"outputs":{}}` + "\n", "no id"},
		{"unknown field", hdr + "\n" + `{"id":"A","surprise":1}` + "\n", "unknown field"},
		{"malformed json", hdr + "\n" + `{"id":` + "\n", "parse scenario"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseDataset(strings.NewReader(tc.body))
			if err == nil {
				t.Fatalf("parseDataset accepted a bad fixture, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestLoadDataset_ScenarioIDsAreStable guards the identifiers the W0.4
// expected-fail allowlist will reference. Renaming one silently
// un-allowlists a red scenario.
func TestLoadDataset_ScenarioIDsAreStable(t *testing.T) {
	ds := loadLangChain(t)
	want := map[string]string{
		"LC-01-crashloopbackoff":          "CrashLoopBackOff",
		"LC-20-full-cluster-health-check": "Full cluster health check (green)",
		"LC-31-cni-plugin-failure-pods":   "CNI plugin failure — pods can't communicate",
	}
	got := make(map[string]string, len(ds.Scenarios))
	for _, s := range ds.Scenarios {
		got[s.ID] = s.Category
	}
	for id, cat := range want {
		if got[id] != cat {
			t.Errorf("scenario %q: category = %q, want %q", id, got[id], cat)
		}
	}
}
