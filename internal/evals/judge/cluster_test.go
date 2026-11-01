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
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v3/internal/evals"
)

const intentsPath = "../../../testdata/evals/intents.yaml"

func loadIntents(t *testing.T) evals.IntentTable {
	t.Helper()
	tbl, err := evals.LoadIntentTable(intentsPath)
	if err != nil {
		t.Fatalf("load intent table: %v", err)
	}
	return tbl
}

// TestNewCluster_EveryScenarioIsReachable is the second precondition for
// the tier, after the fixtures existing: the evidence has to be
// reachable through a tool the scenario's own expected intents name.
// A scenario failing here would score low for a reason that has nothing
// to do with mast.
func TestNewCluster_EveryScenarioIsReachable(t *testing.T) {
	ds := loadCorpus(t)
	tbl := loadIntents(t)
	fx, err := Fixtures(ds, loadOverrides(t))
	if err != nil {
		t.Fatalf("Fixtures: %v", err)
	}
	widths := map[int]int{}
	for _, s := range ds.Scenarios {
		c, err := NewCluster(tbl, s, fx[s.ID])
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		tools := c.AnsweringTools()
		if len(tools) == 0 {
			t.Errorf("%s: no tool answers, so the agent has nothing to read", s.ID)
		}
		widths[len(tools)]++
	}
	t.Logf("answering-tool counts across the corpus: %v", widths)
}

// TestCluster_NonAnsweringToolFindsNothing pins the shape of a wrong
// turn: an empty reading, not an error. Erroring would tell the agent it
// had guessed wrong, which a real cluster does not volunteer.
func TestCluster_NonAnsweringToolFindsNothing(t *testing.T) {
	ds := loadCorpus(t)
	tbl := loadIntents(t)
	fx, err := Fixtures(ds, loadOverrides(t))
	if err != nil {
		t.Fatalf("Fixtures: %v", err)
	}

	var checked int
	for _, s := range ds.Scenarios {
		c, err := NewCluster(tbl, s, fx[s.ID])
		if err != nil {
			continue
		}
		answering := map[string]bool{}
		for _, name := range c.AnsweringTools() {
			answering[name] = true
		}
		for name := range tbl.LookoutTools {
			if answering[name] {
				continue
			}
			got := c.Read(name, "default/some-pod")
			if !strings.Contains(got, "no abnormal findings") {
				t.Errorf("%s: non-answering tool %q returned data:\n%s", s.ID, name, got)
			}
			for _, m := range fx[s.ID].Messages {
				if strings.Contains(got, m) {
					t.Errorf("%s: non-answering tool %q leaked an observation", s.ID, name)
				}
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("every tool answered every scenario — tool choice is not consequential and intent_coverage measures nothing")
	}
	t.Logf("checked %d non-answering tool calls", checked)
}

// TestCluster_AnsweringToolServesItsHalf checks the message/field split
// reaches the agent, and that a single-sided tool stays single-sided.
func TestCluster_AnsweringToolServesItsHalf(t *testing.T) {
	obs := Observations{
		Subject:  "pods are crashlooping",
		Messages: []string{"Error: DATABASE_URL not set"},
		Fields:   []string{"api-server-secrets"},
	}
	sc := evals.Scenario{ID: "LC-TEST"}
	sc.Outputs.ExpectedTools = []string{"kubectl_get_pod_logs", "kubectl_describe_pod"}
	c, err := NewCluster(loadIntents(t), sc, obs)
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}

	logs := c.Read("k8s_triage_logs", "default/api")
	if !strings.Contains(logs, "DATABASE_URL not set") {
		t.Errorf("log tool did not serve its messages:\n%s", logs)
	}
	if strings.Contains(logs, "api-server-secrets") {
		t.Errorf("log tool served resource fields, which makes tool choice cosmetic:\n%s", logs)
	}

	spec := c.Read("k8s_resource_spec", "default/api")
	if !strings.Contains(spec, "api-server-secrets") {
		t.Errorf("spec tool did not serve its fields:\n%s", spec)
	}
	if strings.Contains(spec, "DATABASE_URL not set") {
		t.Errorf("spec tool served log lines:\n%s", spec)
	}

	both := c.Read("k8s_triage_workload", "default/api")
	if !strings.Contains(both, "DATABASE_URL not set") || !strings.Contains(both, "api-server-secrets") {
		t.Errorf("the consolidator did not serve both halves:\n%s", both)
	}
}

// TestCluster_AnsweringToolWithAnEmptyHalfSaysNothingFound is the
// regression pin for a defect the first live board surfaced: a tool that
// answers the scenario but whose half of the fixture is empty used to
// return its header and the echoed alert, and nothing else. That is not
// a clean reading — the model read it as broken tooling, said "the tools
// are returning only the subject echo", and declined to diagnose, which
// the board then scored as mast reasoning badly.
func TestCluster_AnsweringToolWithAnEmptyHalfSaysNothingFound(t *testing.T) {
	// Fields only, and expected tools that reach both halves.
	obs := Observations{Subject: "pods are crashlooping", Fields: []string{"api-server-secrets"}}
	sc := evals.Scenario{ID: "LC-TEST"}
	sc.Outputs.ExpectedTools = []string{"kubectl_get_pod_logs", "kubectl_describe_pod"}
	c, err := NewCluster(loadIntents(t), sc, obs)
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}

	logs := c.Read("k8s_triage_logs", "default/api")
	if !strings.Contains(logs, "no abnormal findings") {
		t.Errorf("a log tool with no messages to serve returned something other than a clean reading:\n%s", logs)
	}
	if strings.Contains(logs, obs.Subject) {
		t.Errorf("the reading echoed the alert back with no observations attached:\n%s", logs)
	}

	// The half that does have content is unaffected.
	if spec := c.Read("k8s_resource_spec", "default/api"); !strings.Contains(spec, "api-server-secrets") {
		t.Errorf("the populated half stopped serving:\n%s", spec)
	}
}

// TestCluster_ReadResultAddsASignalWithoutMovingTheBoard is the
// non-perturbation check for #169.
//
// The empty-read signal exists so the board can tell "never called the
// tool" from "called it against a scope that held nothing". Every way
// of surfacing that through the tool's own payload — a found flag in
// the JSON, a different wording for a clean reading — changes what the
// model reads and moves every score on the board, which would make the
// new column and the old ones un-comparable in the same run that
// introduced it. So the prose is asserted byte-identical to what Read
// has always returned, and the signal rides beside it.
func TestCluster_ReadResultAddsASignalWithoutMovingTheBoard(t *testing.T) {
	ds := loadCorpus(t)
	tbl := loadIntents(t)
	fx, err := Fixtures(ds, loadOverrides(t))
	if err != nil {
		t.Fatalf("Fixtures: %v", err)
	}

	var found, empty int
	for _, s := range ds.Scenarios {
		c, err := NewCluster(tbl, s, fx[s.ID])
		if err != nil {
			continue
		}
		for name := range tbl.LookoutTools {
			for _, scope := range []string{"", "default/some-pod"} {
				reading, ok := c.ReadResult(name, scope)
				if plain := c.Read(name, scope); plain != reading {
					t.Fatalf("%s/%s: Read and ReadResult disagree on what the agent sees:\n%q\nvs\n%q",
						s.ID, name, plain, reading)
				}
				// The two halves have to agree, or the signal is worse
				// than nothing: a clean reading reported as a find would
				// hide exactly the runs #169 is looking for.
				clean := strings.Contains(reading, "no abnormal findings")
				if ok == clean {
					t.Errorf("%s/%s: found=%v for a reading that %s:\n%s",
						s.ID, name, ok, map[bool]string{true: "found nothing", false: "carried observations"}[clean], reading)
				}
				if ok {
					found++
				} else {
					empty++
				}
			}
		}
	}
	if found == 0 || empty == 0 {
		t.Fatalf("found=%d empty=%d — one side never happened, so the signal is not being exercised", found, empty)
	}
	t.Logf("readings across the corpus: %d with observations, %d empty", found, empty)
}

// TestNewCluster_RefusesUnreachableEvidence is the neutralize-verification
// for the reachability check: a scenario whose expected intents reach
// only spec-shaped tools must not be allowed to carry log-line evidence.
func TestNewCluster_RefusesUnreachableEvidence(t *testing.T) {
	tbl := loadIntents(t)

	// inspect.rollout_history is served only by k8s_recent_changes,
	// which is field-shaped. Storage would not do: it also reaches
	// k8s_cluster_health, which does return abnormal-event text.
	sc := evals.Scenario{ID: "LC-TEST"}
	sc.Outputs.ExpectedTools = []string{"kubectl_rollout_history"}
	_, err := NewCluster(tbl, sc, Observations{Messages: []string{"Error: connection refused"}})
	if err == nil {
		t.Fatal("NewCluster accepted a scenario whose log evidence no tool can return")
	}
	if !strings.Contains(err.Error(), "message observation") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// TestNewCluster_RefusesUnknownLookoutTool guards the other direction:
// lookout growing a tool must not silently narrow the fixture's read
// path.
func TestNewCluster_RefusesUnknownLookoutTool(t *testing.T) {
	tbl := loadIntents(t)
	tbl.LookoutTools["k8s_brand_new_check"] = evals.LookoutTool{Satisfies: []string{"inspect.logs"}}

	sc := evals.Scenario{ID: "LC-TEST"}
	sc.Outputs.ExpectedTools = []string{"kubectl_get_pod_logs"}
	_, err := NewCluster(tbl, sc, Observations{Messages: []string{"boom"}})
	if err == nil {
		t.Fatal("NewCluster accepted an intent table naming a tool it has no shape for")
	}
	if !strings.Contains(err.Error(), "no shape") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}
