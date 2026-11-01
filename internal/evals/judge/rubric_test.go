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
	"regexp"
	"strings"
	"testing"
)

// rubricCondition is one of the twelve conditions systemInstruction
// enumerates under a severity, paired with a pattern for the way the
// corpus would have to write it.
//
// The patterns are deliberately generous — "probe" for missing probes,
// bare "cronjob" for suspended CronJobs, "down|unavailable|outage" for
// service down. A tight pattern would make the finding below an artifact
// of the matching; a loose one that still finds nothing does not.
type rubricCondition struct {
	severity string
	name     string
	pattern  *regexp.Regexp
}

var rubricConditions = []rubricCondition{
	{"CRITICAL", "service down", regexp.MustCompile(`(?i)\bdown\b|unavailable|outage|cannot serve`)},
	{"CRITICAL", "crash loops", regexp.MustCompile(`(?i)crash.?loop`)},
	{"CRITICAL", "OOM kills", regexp.MustCompile(`(?i)oom.?kill|out of memory|exit code 137`)},
	{"CRITICAL", "0 ready endpoints", regexp.MustCompile(`(?i)0 endpoints|no endpoints|0/\d+\s*(pods\s*)?ready`)},
	{"WARNING", "no PDB", regexp.MustCompile(`(?i)\bpdb\b|poddisruptionbudget|disruption budget`)},
	{"WARNING", "missing probes", regexp.MustCompile(`(?i)\bprobe`)},
	{"WARNING", ":latest images", regexp.MustCompile(`(?i):latest`)},
	{"WARNING", "wildcard RBAC", regexp.MustCompile(`(?i)\brbac\b|clusterrole|wildcard`)},
	{"INFO", "right-sizing", regexp.MustCompile(`(?i)right.?siz|over-?provision|oversiz|under-?utili`)},
	{"INFO", "orphaned PVs", regexp.MustCompile(`(?i)orphan`)},
	{"INFO", "suspended CronJobs", regexp.MustCompile(`(?i)suspend|cronjob`)},
}

// TestSeverityRubricIsVerbatim guards the pairing above: every condition
// named in rubricConditions has to still be in the prompt, or the
// measurement below is quietly checking a rubric mast no longer ships.
func TestSeverityRubricIsVerbatim(t *testing.T) {
	// The prompt is hard-wrapped, so "OOM kills" reaches the model as
	// "OOM\n    kills". Collapse whitespace before looking for a phrase.
	flat := strings.Join(strings.Fields(systemInstruction), " ")
	for _, c := range rubricConditions {
		if !strings.Contains(flat, c.name) {
			t.Errorf("systemInstruction no longer enumerates %q under %s; re-derive rubricConditions from the prompt",
				c.name, c.severity)
		}
	}
}

// TestSeverityRubricDoesNotSpanCorpus is the standing evidence for
// #179's disposition: it measures how much of the corpus the severity
// definitions actually decide.
//
// The answer is a fifth of it, one-sided. The four CRITICAL conditions
// occur in 5 of the 31 scenarios; the four WARNING conditions and the
// three INFO conditions occur in none. Twenty rows are nonetheless
// labelled WARNING or INFO, so on those the label rests on judgement the
// rubric never states — and an agent given only the rubric has nothing
// to place them with. That is why severity_accuracy is diagnostic
// (evals.SeverityAccuracy) rather than partitioned into "the corpus
// label is wrong" and "the agent escalated": on the rows that miss there
// is no stated definition to partition against, so the two readings are
// indistinguishable in principle rather than merely by this metric.
//
// The test fails if that stops being true in either direction. A corpus
// that grew rows the WARNING or INFO conditions describe, or a rubric
// rewritten to define the buckets this corpus actually uses, would break
// it — and both are grounds to re-promote the metric, which is the
// point of finding out mechanically instead of re-litigating it each
// release.
func TestSeverityRubricDoesNotSpanCorpus(t *testing.T) {
	ds := loadCorpus(t)
	if len(ds.Scenarios) != 31 {
		t.Fatalf("corpus has %d scenarios, want the 31 this measurement is stated over", len(ds.Scenarios))
	}

	hit := map[string]map[string]bool{}
	for _, sc := range ds.Scenarios {
		for _, c := range rubricConditions {
			if !c.pattern.MatchString(sc.Inputs.Scenario) {
				continue
			}
			if hit[c.severity] == nil {
				hit[c.severity] = map[string]bool{}
			}
			hit[c.severity][sc.ID] = true
		}
	}

	if n := len(hit["CRITICAL"]); n != 5 {
		t.Errorf("CRITICAL conditions occur in %d scenarios, want 5 — the corpus or the rubric moved, so re-read #179", n)
	}
	for _, severity := range []string{"WARNING", "INFO"} {
		if n := len(hit[severity]); n != 0 {
			var ids []string
			for id := range hit[severity] {
				ids = append(ids, id)
			}
			t.Errorf("%s conditions now occur in %d scenarios (%s); the rubric has started to describe the buckets the corpus labels with, so severity_accuracy may be promotable again",
				severity, n, strings.Join(ids, ", "))
		}
	}

	// The other half of the finding: the undefined buckets are where the
	// labels are. A corpus weighted the other way would make the gap
	// academic.
	labelled := 0
	for _, sc := range ds.Scenarios {
		switch severityOf(sc.Outputs.ExpectedResponse) {
		case "WARNING", "INFO":
			labelled++
		}
	}
	if labelled != 20 {
		t.Errorf("%d of 31 rows are labelled WARNING or INFO, want 20 — #179's numbers are stated over that split", labelled)
	}
}

// severityOf reads the leading severity token of an expected response.
// Deliberately not evals.extractSeverity: the corpus writes a bare
// "WARNING: " prefix and nothing else, and this measurement should not
// move when the tolerant extractor does.
func severityOf(expected string) string {
	line, _, _ := strings.Cut(expected, "\n")
	token, _, ok := strings.Cut(strings.TrimSpace(line), ":")
	if !ok {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(token))
}
