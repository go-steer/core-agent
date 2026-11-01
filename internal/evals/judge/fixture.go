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

// Package judge is the metered tier of the v0.3 parity eval suite
// (docs/v0.3-plan.md W0.5): the 31-scenario corpus scored against a real
// model, which is the only tier that produces a LangChain-comparable
// number.
//
// # Why this tier needs a fixture cluster at all
//
// The corpus's expected responses assert facts that are not in the
// prompt. LC-01's scenario says only that a container "exits with code 1
// within seconds of starting"; its expected response names the log line
// `Error: DATABASE_URL not set` and the Secret `api-server-secrets`.
// Those are only reachable by calling a tool. Upstream's judge prompt
// puts the expected response in as ground truth and defines
// correct_diagnosis as matching it, so an agent with no cluster to read
// cannot score above the floor no matter how well it reasons — the
// metric would measure fixture starvation. The fixture therefore serves
// observations the agent can read.
//
// # The rule that keeps the measurement honest
//
// A fixture authored freely from the expected response would hand the
// model its own answer, and the judge would be grading transcription.
// So exactly one mechanical rule governs what may cross from the answer
// side into the cluster:
//
//	Only quoted spans of the expected response become observations.
//
// Unquoted prose never crosses. That is what makes the boundary
// checkable rather than a matter of authorial restraint: the diagnosis
// in LC-01 ("the pod references a secret key that does not exist"), the
// severity, and the remediation are all unquoted, so none of them can
// reach the agent. The raw artifacts they were inferred *from* — the log
// line, the Secret's name — are quoted, so they can. The agent still has
// to do the inference and grade the severity itself, which is the work
// being measured.
//
// [Guard] enforces the rule against both derived and hand-written
// observations, and TestGuard_RejectsConclusions pins it.
//
// # What this measures, and what it does not
//
// It measures: given faithful cluster data, does the agent read the
// right things, reach the right diagnosis, grade severity correctly, and
// write something an operator could act on. It does *not* measure
// whether mast's real lookout read path surfaces those facts — that is
// lookout's own CI (scoreboard lead row L6). A green judge board with a
// broken read path is possible and would not be caught here.
package judge

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"github.com/go-steer/core-agent/v3/internal/evals"
)

// Observations is what the fixture cluster knows about one scenario. It
// is deliberately small: a fixture rich enough to be interesting is a
// fixture rich enough to hide the answer in.
type Observations struct {
	// Subject is the alert text, carried verbatim from the corpus
	// input. This is not a leak — it is the prompt the agent already
	// has, repeated where a real tool would echo the object it read.
	Subject string
	// Messages are free-text artifacts: log lines, event messages,
	// controller errors. Reachable through the log and event tools.
	Messages []string
	// Fields are identifiers and values: resource names, selectors,
	// storage classes, image references. Reachable through the spec and
	// edge tools.
	Fields []string
	// Derived reports whether Messages/Fields came from the mechanical
	// rule or from an override, so the report can say how much of the
	// board rests on hand-authored fixtures.
	Derived bool
}

// Empty reports whether the fixture has nothing beyond the alert text.
// A scenario in this state cannot support a diagnosis and needs an
// override.
func (o Observations) Empty() bool { return len(o.Messages) == 0 && len(o.Fields) == 0 }

// quotedSpans returns the single-quoted spans of an expected response.
// The corpus writes artifacts in single quotes throughout — log lines,
// resource names, selectors — which is what makes the rule mechanical
// rather than a heuristic over prose.
//
// This is a scanner rather than the obvious `'([^']{2,})'` regex
// because the corpus also writes possessives, and the naive pattern
// pairs the apostrophe with the next real opening quote. In LC-21 that
// took "the app's DB_HOST config to 'postgres…'" and yielded the span
// "s DB_HOST config to" — a fragment of the *remediation sentence*
// crossing into the fixture, which is the one thing the rule exists to
// prevent. TestDerive_UnquotedProseNeverCrosses caught it and pins it.
//
// The distinguishing rule is one-sided: an apostrophe is preceded by a
// word character ("app's", "pods'") and an opening quote is not. A
// closing quote is almost always preceded by one, so the test applies
// only while looking for an opener.
func quotedSpans(s string) []string {
	var out []string
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		if rs[i] != '\'' || (i > 0 && isWordRune(rs[i-1])) {
			continue
		}
		for j := i + 1; j < len(rs); j++ {
			if rs[j] != '\'' {
				continue
			}
			if j-i > 2 { // at least two runes between the quotes
				out = append(out, string(rs[i+1:j]))
			}
			i = j
			break
		}
	}
	return out
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

// messageLike splits quoted spans into free text and identifiers. A
// span is a message when it reads as a sentence fragment: it contains
// whitespace and some marker of a report — a colon, or a word that
// names a failure. Everything else is an identifier.
//
// The split is what makes tool choice consequential. If every tool
// returned everything, calling one would be as good as calling the
// right one and the trajectory metric would be measuring nothing.
var messageLike = regexp.MustCompile(`(?i)(:\s|\berror\b|\bfailed\b|\bdenied\b|\bunauthorized\b|\bdetected\b|\bnot set\b|\btimeout\b)`)

// Override is a hand-written fixture for a scenario the quoting rule
// cannot serve well enough. Nine of the 31 corpus rows quote nothing at
// all, and several more quote a single identifier; their diagnostic
// facts are numeric or structural, and prose is the only place they
// exist.
//
// An override may enrich a thin derived fixture, but never displace a
// derived fact — see [Fixtures].
type Override struct {
	// Reason is why the mechanical rule is insufficient here. Required:
	// an override without a stated reason is indistinguishable from one
	// added to make a scenario score better.
	Reason string `yaml:"reason"`
	// Messages and Fields replace the derived ones entirely. They are
	// held to the same Guard as derived observations.
	Messages []string `yaml:"messages"`
	Fields   []string `yaml:"fields"`
}

// OverrideFile is the on-disk override set.
type OverrideFile struct {
	Version   int                 `yaml:"version"`
	Overrides map[string]Override `yaml:"overrides"`
}

// LoadOverrides reads the override file.
func LoadOverrides(path string) (OverrideFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return OverrideFile{}, fmt.Errorf("judge: read overrides %q: %w", path, err)
	}
	var f OverrideFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return OverrideFile{}, fmt.Errorf("judge: parse overrides %q: %w", path, err)
	}
	if f.Version != 1 {
		return OverrideFile{}, fmt.Errorf("judge: overrides %q: unsupported version %d", path, f.Version)
	}
	for id, ov := range f.Overrides {
		if strings.TrimSpace(ov.Reason) == "" {
			return OverrideFile{}, fmt.Errorf("judge: override %q has no reason", id)
		}
	}
	return f, nil
}

// Derive builds the fixture cluster's view of one scenario.
//
// The mechanical path takes only quoted spans of the expected response.
// An override replaces them wholesale — it does not merge, because a
// merge would make it ambiguous which facts the rule vouched for.
func Derive(s evals.Scenario, ov *Override) Observations {
	obs := Observations{Subject: s.Inputs.Scenario, Derived: true}
	if ov != nil {
		obs.Derived = false
		obs.Messages = dedupe(ov.Messages)
		obs.Fields = dedupe(ov.Fields)
		return obs
	}
	var msgs, fields []string
	for _, raw := range quotedSpans(s.Outputs.ExpectedResponse) {
		span := strings.TrimSpace(raw)
		if span == "" {
			continue
		}
		if strings.Contains(span, " ") && messageLike.MatchString(span) {
			msgs = append(msgs, span)
			continue
		}
		fields = append(fields, span)
	}
	obs.Messages = dedupe(msgs)
	obs.Fields = dedupe(fields)
	return obs
}

// dedupe removes repeats while preserving first-seen order. The corpus
// repeats selectors on both sides of a mismatch ("'app=payment-api'
// versus 'app=payment'"), and a fixture that listed each twice would
// read as two findings.
func dedupe(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// displaced returns the derived observations an override does not carry
// forward.
//
// Containment is by substring, not equality, and that is the point: the
// quoting rule yields bare spans ('worker'), while a useful override
// states them in context ("container worker: resources.limits.memory=
// 256Mi"). Requiring equality would force a bare duplicate entry beside
// every contextualised one, which is a worse fixture for the same
// guarantee. What must hold is that the vouched-for span still reaches
// the agent.
func displaced(derived Observations, ov Override) []string {
	entries := append(append([]string{}, ov.Messages...), ov.Fields...)
	covered := func(span string) bool {
		for _, e := range entries {
			if strings.Contains(e, span) {
				return true
			}
		}
		return false
	}
	var out []string
	for _, span := range append(append([]string{}, derived.Messages...), derived.Fields...) {
		if !covered(span) {
			out = append(out, fmt.Sprintf("%q", span))
		}
	}
	return out
}

// severityTokens are the grades the corpus uses. A fixture that
// contains one has graded the incident for the agent, which is half of
// what the tier measures.
var severityTokens = []string{"CRITICAL", "WARNING", "INFO"}

// eventTypeSense is a vocabulary collision, not a loophole. Kubernetes
// types every event as Normal or Warning, so "no Warning events in the
// last hour" is a reading a real event tool returns, not a grade the
// fixture assigned — and LC-20, the corpus's only all-clear row, is
// precisely the scenario that needs to state it.
//
// The carve-out is deliberately narrow: the token counts as an event
// type only when the word "event" or "events" follows it. "WARNING:
// disk filling" still trips the guard, which
// TestGuard_RejectsConclusions pins in both directions.
var eventTypeSense = regexp.MustCompile(`\b(WARNING|NORMAL)\s+EVENTS?\b`)

// remedialPhrases mark a fixture that has moved from reporting to
// advising. "Recommended action" is the corpus's own phrasing; the rest
// catch a hand-written override drifting the same way.
var remedialPhrases = []string{
	"recommended action",
	"root cause",
	"you should",
	"remediation:",
	"fix:",
	"to resolve",
	"the problem is",
}

// Guard rejects observations that state a conclusion instead of an
// observation.
//
// It runs over derived and overridden fixtures alike. The derived ones
// are already protected by the quoting rule and should never trip it —
// which is the point: the guard's job is to catch the day someone
// loosens the rule, or writes an override that explains rather than
// reports. A fixture that grades severity or names the remedy turns
// response_quality into a transcription score and severity_accuracy into
// a constant, and both would look like progress.
func Guard(id string, o Observations) error {
	for _, s := range append(append([]string{}, o.Messages...), o.Fields...) {
		upper := eventTypeSense.ReplaceAllString(strings.ToUpper(s), "EVENT-TYPE")
		for _, tok := range severityTokens {
			// Anchored on a word boundary: a log line may legitimately
			// contain "critical section" without grading anything.
			if regexp.MustCompile(`\b` + tok + `\b`).MatchString(upper) {
				return fmt.Errorf("judge: %s: fixture states severity %q — the agent must grade the incident, not read the grade: %q", id, tok, s)
			}
		}
		lower := strings.ToLower(s)
		for _, phrase := range remedialPhrases {
			if strings.Contains(lower, phrase) {
				return fmt.Errorf("judge: %s: fixture contains remedial phrasing %q — observations report, they do not advise: %q", id, phrase, s)
			}
		}
	}
	return nil
}

// Fixtures derives the whole corpus, applying overrides and guarding
// every result.
//
// A scenario left with nothing but its alert text is an error, not a
// warning. Scoring it would produce a low number attributable to the
// fixture rather than to mast, and a board that mixes those with real
// results is not a measurement.
//
// An override may enrich a thin derived fixture, but it may not drop a
// fact the quoting rule vouched for: every derived observation must
// still be reachable in the override's own entries. That containment is
// the safeguard, and it replaced a stricter one — "an override is only
// admissible where the rule is silent" — that the first live board
// showed was drawing the line in the wrong place. Four rows quote a
// single bare identifier ('worker', 'team-a'), which clears "the rule
// said something" while leaving the agent with an identifier and no
// facts; the model read the resulting cluster as broken tooling and
// declined to diagnose, and the board scored that as mast doing badly.
// Silence was never the property worth testing. Displacement was: a
// hand-written fixture quietly replacing a vouched-for one is the move
// that turns a fixture set into an answer key, and containment refuses
// exactly that while letting a starved row be fed.
func Fixtures(ds evals.Dataset, ovf OverrideFile) (map[string]Observations, error) {
	out := make(map[string]Observations, len(ds.Scenarios))
	var starved []string
	for _, s := range ds.Scenarios {
		var ov *Override
		if o, ok := ovf.Overrides[s.ID]; ok {
			ov = &o
			if dropped := displaced(Derive(s, nil), o); len(dropped) > 0 {
				return nil, fmt.Errorf(
					"judge: override %q drops observation(s) the quoting rule vouched for (%s) — an override may enrich a thin fixture, never displace a derived fact",
					s.ID, strings.Join(dropped, ", "))
			}
		}
		obs := Derive(s, ov)
		if err := Guard(s.ID, obs); err != nil {
			return nil, err
		}
		if obs.Empty() {
			starved = append(starved, s.ID)
			continue
		}
		out[s.ID] = obs
	}
	if len(starved) > 0 {
		sort.Strings(starved)
		return nil, fmt.Errorf(
			"judge: %d scenario(s) have no observations beyond the alert text, so a low score would measure the fixture and not mast — add an override with a stated reason for: %s",
			len(starved), strings.Join(starved, ", "))
	}
	// Every override must match a real scenario. A stale entry is an
	// override that silently stopped applying, which reads as a derived
	// fixture that happens to work.
	for id := range ovf.Overrides {
		if _, ok := out[id]; !ok {
			return nil, fmt.Errorf("judge: override %q matches no scenario in the corpus", id)
		}
	}
	return out, nil
}
