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

package judge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v3/internal/evals"
)

const (
	corpusPath    = "../../../testdata/evals/scenarios/langchain-sre.jsonl"
	overridesPath = "../../../testdata/evals/judge/overrides.yaml"
)

func loadCorpus(t *testing.T) evals.Dataset {
	t.Helper()
	ds, err := evals.LoadDataset(corpusPath)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	return ds
}

func loadOverrides(t *testing.T) OverrideFile {
	t.Helper()
	ovf, err := LoadOverrides(overridesPath)
	if err != nil {
		t.Fatalf("load overrides: %v", err)
	}
	return ovf
}

// TestFixtures_CoversWholeCorpus is the tier's precondition: every
// scenario has something to read. Fixtures errors on any starved row, so
// this failing means the judge board would carry a score attributable to
// the fixture rather than to mast.
func TestFixtures_CoversWholeCorpus(t *testing.T) {
	ds := loadCorpus(t)
	fx, err := Fixtures(ds, loadOverrides(t))
	if err != nil {
		t.Fatalf("Fixtures: %v", err)
	}
	if len(fx) != len(ds.Scenarios) {
		t.Fatalf("fixtures cover %d scenarios, corpus has %d", len(fx), len(ds.Scenarios))
	}
	for _, s := range ds.Scenarios {
		if fx[s.ID].Subject == "" {
			t.Errorf("%s: fixture has no subject", s.ID)
		}
	}
}

// TestDerive_UnquotedProseNeverCrosses is the honesty check on the
// mechanical path. It asserts the rule in the package doc directly: a
// derived observation must appear single-quoted in the expected
// response. Anything else means unquoted prose — the diagnosis, the
// grade, the remedy — reached the agent, and the judge is grading
// transcription.
func TestDerive_UnquotedProseNeverCrosses(t *testing.T) {
	ds := loadCorpus(t)
	ovf := loadOverrides(t)
	checked := 0
	for _, s := range ds.Scenarios {
		if _, overridden := ovf.Overrides[s.ID]; overridden {
			continue
		}
		obs := Derive(s, nil)
		for _, got := range append(append([]string{}, obs.Messages...), obs.Fields...) {
			if !strings.Contains(s.Outputs.ExpectedResponse, "'"+got+"'") {
				t.Errorf("%s: observation %q is not a quoted span of the expected response", s.ID, got)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no derived observations were checked — the rule is untested, which is how it would pass while doing nothing")
	}
	t.Logf("checked %d derived observations across %d mechanically-served scenarios",
		checked, len(ds.Scenarios)-len(ovf.Overrides))
}

// TestOverrides_EnrichButNeverDisplace pins the hand-authored share of
// the board. An override may add to what the quoting rule established —
// that is the whole reason it exists — but every span the rule vouched
// for must still reach the agent. Dropping one is the move that turns
// the set into an answer key, because it lets a hand-written fixture
// quietly stand in for a derived one.
func TestOverrides_EnrichButNeverDisplace(t *testing.T) {
	ds := loadCorpus(t)
	ovf := loadOverrides(t)
	byID := make(map[string]evals.Scenario, len(ds.Scenarios))
	for _, s := range ds.Scenarios {
		byID[s.ID] = s
	}
	for id, ov := range ovf.Overrides {
		s, ok := byID[id]
		if !ok {
			t.Errorf("override %q matches no scenario", id)
			continue
		}
		if dropped := displaced(Derive(s, nil), ov); len(dropped) > 0 {
			t.Errorf("override %q drops derived observation(s) %s", id, strings.Join(dropped, ", "))
		}
	}
	t.Logf("%d of %d scenarios rest on hand-written fixtures", len(ovf.Overrides), len(ds.Scenarios))
}

// TestDisplaced_CatchesADroppedSpan is the neutralize-verification for
// the containment check: the shipped overrides all pass it, so without
// a case that fails the check could be `return nil` and nothing would
// notice.
func TestDisplaced_CatchesADroppedSpan(t *testing.T) {
	derived := Observations{Messages: []string{"Error: DATABASE_URL not set"}, Fields: []string{"worker"}}

	// Contextualised, not verbatim — the case the substring rule exists
	// for. An equality check would reject this and force a bare
	// duplicate entry beside every useful one.
	keeps := Override{
		Messages: []string{"app: Error: DATABASE_URL not set (repeated 14x)"},
		Fields:   []string{"container worker: resources.limits.memory=256Mi"},
	}
	if dropped := displaced(derived, keeps); len(dropped) > 0 {
		t.Errorf("containment rejected an override that carries both spans in context: %v", dropped)
	}

	drops := Override{Fields: []string{"container worker: resources.limits.memory=256Mi"}}
	dropped := displaced(derived, drops)
	if len(dropped) != 1 || !strings.Contains(dropped[0], "DATABASE_URL") {
		t.Errorf("dropped = %v, want the one message the override does not carry", dropped)
	}
}

// TestGuard_RejectsConclusions is the neutralize-verification for the
// guard. Each case is a fixture that has stopped reporting and started
// explaining; the last two are the false positives the guard must not
// produce, since a guard that rejects legitimate log lines gets loosened
// and then protects nothing.
func TestGuard_RejectsConclusions(t *testing.T) {
	cases := []struct {
		name   string
		obs    Observations
		reject bool
	}{
		{
			name:   "severity grade in a message",
			obs:    Observations{Messages: []string{"CRITICAL: node ip-10-0-1-42 is NotReady"}},
			reject: true,
		},
		{
			name:   "severity grade in a field",
			obs:    Observations{Fields: []string{"severity=WARNING"}},
			reject: true,
		},
		{
			name:   "lowercase severity still grades",
			obs:    Observations{Messages: []string{"pod status: critical"}},
			reject: true,
		},
		{
			name:   "corpus remediation phrasing",
			obs:    Observations{Messages: []string{"Recommended action: raise the memory limit to 1Gi"}},
			reject: true,
		},
		{
			name:   "diagnosis phrasing",
			obs:    Observations{Fields: []string{"the problem is a selector mismatch"}},
			reject: true,
		},
		{
			name:   "root cause phrasing",
			obs:    Observations{Messages: []string{"root cause: DNS resolution failure"}},
			reject: true,
		},
		{
			name:   "critical section is not a grade",
			obs:    Observations{Messages: []string{"lock held in criticalsection for 4m"}},
			reject: false,
		},
		{
			// The Kubernetes event type, not a grade — the carve-out.
			name:   "warning events is an event type",
			obs:    Observations{Messages: []string{"No Warning events recorded in the last 60 minutes"}},
			reject: false,
		},
		{
			// The other half of the carve-out: it must stay narrow.
			name:   "warning as a grade still trips it",
			obs:    Observations{Messages: []string{"WARNING: disk filling on ip-10-0-1-42"}},
			reject: true,
		},
		{
			name:   "an ordinary artifact passes",
			obs:    Observations{Messages: []string{"Error: DATABASE_URL not set"}, Fields: []string{"api-server-secrets"}},
			reject: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Guard("LC-TEST", tc.obs)
			if tc.reject && err == nil {
				t.Fatalf("Guard accepted a fixture that states a conclusion: %+v", tc.obs)
			}
			if !tc.reject && err != nil {
				t.Fatalf("Guard rejected a legitimate observation: %v", err)
			}
		})
	}
}

// TestGuard_HoldsOverTheShippedOverrides runs the guard over the real
// override file. The overrides are authored from the expected responses
// by construction, so this is the check that keeps them raw.
func TestGuard_HoldsOverTheShippedOverrides(t *testing.T) {
	for id, ov := range loadOverrides(t).Overrides {
		if err := Guard(id, Observations{Messages: ov.Messages, Fields: ov.Fields}); err != nil {
			t.Errorf("%v", err)
		}
	}
}

func TestLoadOverrides_RejectsMalformedFiles(t *testing.T) {
	cases := map[string]string{
		"unsupported version": "version: 2\noverrides: {}\n",
		"override with no reason": "version: 1\noverrides:\n" +
			"  LC-03-oomkilled:\n    messages: [\"exit code 137\"]\n",
		"override with a blank reason": "version: 1\noverrides:\n" +
			"  LC-03-oomkilled:\n    reason: \"   \"\n    messages: [\"exit code 137\"]\n",
		"not yaml": "{{{\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "overrides.yaml")
			writeFile(t, path, body)
			if _, err := LoadOverrides(path); err == nil {
				t.Fatal("LoadOverrides accepted a file it should have refused")
			}
		})
	}
}

// TestFixtures_RefusesStarvedAndStale covers the two ways the fixture
// set can silently stop describing the corpus: a scenario nothing
// serves, and an override that has stopped applying.
func TestFixtures_RefusesStarvedAndStale(t *testing.T) {
	ds := loadCorpus(t)

	// Each case asserts the reason as well as the refusal: three
	// different defects reach Fixtures through one error return, and a
	// test that only checks err != nil passes when the wrong guard
	// fires.
	t.Run("starved scenario", func(t *testing.T) {
		_, err := Fixtures(ds, OverrideFile{Version: 1})
		if err == nil {
			t.Fatal("Fixtures accepted a corpus with nine unserved scenarios")
		}
		if !strings.Contains(err.Error(), "no observations beyond the alert text") {
			t.Fatalf("refused for the wrong reason: %v", err)
		}
	})

	t.Run("stale override", func(t *testing.T) {
		ovf := loadOverrides(t)
		ovf.Overrides["LC-99-does-not-exist"] = Override{Reason: "test", Fields: []string{"x"}}
		_, err := Fixtures(ds, ovf)
		if err == nil {
			t.Fatal("Fixtures accepted an override matching no scenario")
		}
		if !strings.Contains(err.Error(), "matches no scenario") {
			t.Fatalf("refused for the wrong reason: %v", err)
		}
	})

	t.Run("override displacing a derived fixture", func(t *testing.T) {
		ovf := loadOverrides(t)
		var served string
		for _, s := range ds.Scenarios {
			if _, ok := ovf.Overrides[s.ID]; !ok && !Derive(s, nil).Empty() {
				served = s.ID
				break
			}
		}
		if served == "" {
			t.Fatal("no mechanically-served scenario to test with")
		}
		ovf.Overrides[served] = Override{Reason: "test", Fields: []string{"x"}}
		_, err := Fixtures(ds, ovf)
		if err == nil {
			t.Fatalf("Fixtures accepted an override displacing derived observations for %s", served)
		}
		if !strings.Contains(err.Error(), "never displace a derived fact") {
			t.Fatalf("refused for the wrong reason: %v", err)
		}
	})
}

// TestDerive_OverrideReplacesRatherThanMerges pins the choice in Derive.
// A merge would make it ambiguous which half of a fixture the quoting
// rule vouched for.
func TestDerive_OverrideReplacesRatherThanMerges(t *testing.T) {
	s := evals.Scenario{ID: "LC-TEST"}
	s.Inputs.Scenario = "an alert"
	s.Outputs.ExpectedResponse = "the pod logs show 'Error: connection refused' from 'cache-svc'"

	derived := Derive(s, nil)
	if len(derived.Messages) != 1 || len(derived.Fields) != 1 {
		t.Fatalf("derived split is wrong: %+v", derived)
	}
	if !derived.Derived {
		t.Error("derived observations are not marked Derived")
	}

	got := Derive(s, &Override{Reason: "test", Fields: []string{"only-this"}})
	if got.Derived {
		t.Error("overridden observations are marked Derived")
	}
	if len(got.Messages) != 0 {
		t.Errorf("override merged with derived messages: %v", got.Messages)
	}
	if len(got.Fields) != 1 || got.Fields[0] != "only-this" {
		t.Errorf("override fields = %v", got.Fields)
	}
	if got.Subject != "an alert" {
		t.Errorf("override dropped the subject: %q", got.Subject)
	}
}

// TestDerive_DedupesRepeatedSpans covers the corpus habit of quoting
// both sides of a mismatch. Listing a selector twice would read as two
// findings.
func TestDerive_DedupesRepeatedSpans(t *testing.T) {
	s := evals.Scenario{ID: "LC-TEST"}
	s.Outputs.ExpectedResponse = "selector 'app=payment-api' does not match 'app=payment', and 'app=payment' has no pods"
	got := Derive(s, nil)
	if len(got.Fields) != 2 {
		t.Fatalf("fields = %v, want the two distinct selectors", got.Fields)
	}
	if got.Fields[0] != "app=payment-api" {
		t.Errorf("dedupe did not preserve first-seen order: %v", got.Fields)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
