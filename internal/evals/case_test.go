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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCase(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "case.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write case: %v", err)
	}
	return path
}

const goodCase = `{
  "id": "c1",
  "fixture": "f1",
  "planted_defect": "one workload carries a tag the registry does not have",
  "prompt": "find the thing that is broken",
  "checks": [
    {"name": "named-it", "why": "the evidence chain", "source": "answer",
     "all_of": ["${fact.workload}"]}
  ]
}`

func TestLoadCaseAcceptsAWellFormedCase(t *testing.T) {
	c, err := LoadCase(writeCase(t, goodCase))
	if err != nil {
		t.Fatalf("LoadCase: %v", err)
	}
	if c.ID != "c1" || len(c.Checks) != 1 {
		t.Fatalf("unexpected case: %+v", c)
	}
}

// The validator's job is to make a badly-shaped case impossible to run
// rather than to make it fail interestingly at grading time, so each of
// these is a separate refusal with its own reason.
func TestLoadCaseRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "no planted defect",
			body: `{"id":"c","fixture":"f","prompt":"p","checks":[{"name":"n","why":"w","source":"answer","all_of":["x"]}]}`,
			want: "planted_defect is required",
		},
		{
			name: "no checks",
			body: `{"id":"c","fixture":"f","planted_defect":"d","prompt":"p","checks":[]}`,
			want: "at least one check is required",
		},
		{
			name: "check with no assertion",
			body: `{"id":"c","fixture":"f","planted_defect":"d","prompt":"p","checks":[{"name":"n","why":"w","source":"answer"}]}`,
			want: "asserts nothing",
		},
		{
			name: "single-term any_of",
			body: `{"id":"c","fixture":"f","planted_defect":"d","prompt":"p","checks":[{"name":"n","why":"w","source":"answer","any_of":["x"]}]}`,
			want: "all_of spelled misleadingly",
		},
		{
			name: "empty term",
			body: `{"id":"c","fixture":"f","planted_defect":"d","prompt":"p","checks":[{"name":"n","why":"w","source":"answer","all_of":["  "]}]}`,
			want: "found in every haystack",
		},
		{
			name: "unknown source",
			body: `{"id":"c","fixture":"f","planted_defect":"d","prompt":"p","checks":[{"name":"n","why":"w","source":"transcript","all_of":["x"]}]}`,
			want: "source must be",
		},
		{
			name: "duplicate check names",
			body: `{"id":"c","fixture":"f","planted_defect":"d","prompt":"p","checks":[
			  {"name":"n","why":"w","source":"answer","all_of":["x"]},
			  {"name":"n","why":"w","source":"answer","all_of":["y"]}]}`,
			want: "duplicate check name",
		},
		{
			name: "no why",
			body: `{"id":"c","fixture":"f","planted_defect":"d","prompt":"p","checks":[{"name":"n","source":"answer","all_of":["x"]}]}`,
			want: "why is required",
		},
		{
			name: "unknown field",
			body: `{"id":"c","fixture":"f","planted_defect":"d","prompt":"p","rubric":"nope","checks":[{"name":"n","why":"w","source":"answer","all_of":["x"]}]}`,
			want: "unknown field",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadCase(writeCase(t, tt.body))
			if err == nil {
				t.Fatal("want an error, got none")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not mention %q", err, tt.want)
			}
		})
	}
}

func TestBindExpandsFactReferences(t *testing.T) {
	c, err := LoadCase(writeCase(t, goodCase))
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixture{
		Name:      "f1",
		Facts:     map[string]string{"workload": "ledger-writer"},
		Witnesses: map[string]string{"w": "witness/w.log"},
	}
	bound, err := c.Bind(f)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if got := bound.Checks[0].AllOf[0]; got != "ledger-writer" {
		t.Fatalf("all_of[0] = %q, want the expanded fact", got)
	}
	// The receiver is untouched, so binding one case against two
	// fixtures cannot leak the first fixture's facts into the second.
	if got := c.Checks[0].AllOf[0]; got != "${fact.workload}" {
		t.Fatalf("Bind mutated the receiver: %q", got)
	}
}

// An unknown fact key must not expand to the empty string: an empty
// needle is found in every haystack, so the typo would silently turn an
// all_of check into one that can never fail.
func TestBindRejectsUnknownFact(t *testing.T) {
	c, err := LoadCase(writeCase(t, goodCase))
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixture{Name: "f1", Facts: map[string]string{"namespace": "payments-prod"}}
	_, err = c.Bind(f)
	if err == nil || !strings.Contains(err.Error(), "unknown fact") {
		t.Fatalf("want an unknown-fact error, got %v", err)
	}
}

// The withhold-the-location rule, mechanised. A prompt naming the fact
// turns the case into a copying exercise the no-access baseline scores.
func TestBindRejectsAPromptThatLeaksAFact(t *testing.T) {
	body := strings.Replace(goodCase,
		`"prompt": "find the thing that is broken"`,
		`"prompt": "look in payments-prod and find the thing that is broken"`, 1)
	c, err := LoadCase(writeCase(t, body))
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixture{
		Name:      "f1",
		Facts:     map[string]string{"workload": "ledger-writer", "namespace": "payments-prod"},
		Witnesses: map[string]string{"w": "witness/w.log"},
	}
	if _, err := c.Bind(f); err == nil || !strings.Contains(err.Error(), "withhold the location") {
		t.Fatalf("want a leak error, got %v", err)
	}
}

// Case-insensitively, because a prompt that says "Payments-Prod" leaks
// exactly as much as one that says "payments-prod".
func TestBindLeakDetectionIsCaseInsensitive(t *testing.T) {
	body := strings.Replace(goodCase,
		`"prompt": "find the thing that is broken"`,
		`"prompt": "check the Payments-Prod namespace"`, 1)
	c, err := LoadCase(writeCase(t, body))
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixture{
		Name:      "f1",
		Facts:     map[string]string{"workload": "ledger-writer", "namespace": "payments-prod"},
		Witnesses: map[string]string{"w": "witness/w.log"},
	}
	if _, err := c.Bind(f); err == nil || !strings.Contains(err.Error(), "withhold the location") {
		t.Fatalf("want a leak error, got %v", err)
	}
}

func TestBindRejectsAWitnessTheFixtureDoesNotDeclare(t *testing.T) {
	body := strings.Replace(goodCase, `"source": "answer"`, `"source": "witness:ghost"`, 1)
	c, err := LoadCase(writeCase(t, body))
	if err != nil {
		t.Fatal(err)
	}
	f := &Fixture{
		Name:      "f1",
		Facts:     map[string]string{"workload": "ledger-writer"},
		Witnesses: map[string]string{"cluster-reads": "witness/reads.log"},
	}
	_, err = c.Bind(f)
	if err == nil || !strings.Contains(err.Error(), "declares no witness") {
		t.Fatalf("want a missing-witness error, got %v", err)
	}
	if !strings.Contains(err.Error(), "cluster-reads") {
		t.Fatalf("error should name the witnesses the fixture does have: %v", err)
	}
}

func TestWitnessName(t *testing.T) {
	if _, ok := (Check{Source: SourceAnswer}).WitnessName(); ok {
		t.Fatal("answer is not a witness")
	}
	name, ok := (Check{Source: "witness:cluster-reads"}).WitnessName()
	if !ok || name != "cluster-reads" {
		t.Fatalf("WitnessName = %q, %v", name, ok)
	}
}
