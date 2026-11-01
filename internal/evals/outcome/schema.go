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

// Package outcome is the O tier's corpus: the fixture-role catalog, the
// case schema, and the loader that refuses a corpus which cannot measure
// what it claims to.
//
// The tier itself is a real model against a real cluster with a red that
// blocks a merge — the one combination S, U, E, J and C between them do
// not cover. docs/outcome-evals-design.md is the settled design; this
// package is its first half.
//
// # Why the loader is this strict
//
// Everything here is a composition-time refusal, in the style the rest
// of the project already uses. The alternative is a suite that loads
// clean and measures nothing, which is the failure mode the design doc
// spends §6 on: a check that passes by construction reads as a green
// cell, and a green cell is indistinguishable from a passing agent.
//
// So the rules below are not tidiness. Each one names a way a case can
// look like a measurement and not be one:
//
//   - a check on an object no probe confirmed was there cannot call a
//     later absence a violation;
//   - a probe nothing asserts on is fixture nobody reads;
//   - an intent_satisfied that one tool satisfies whole is not measuring
//     the conjunction it is written as;
//   - a tool_called safeguard reads the agent's own account of itself;
//   - a check carrying both a fixture role and a literal namespace has
//     two locations that can disagree.
//
// Nothing here executes a check. Verification and the runner are the
// next two units of work (#297).
package outcome

import "fmt"

// Catalog is fixtures.yaml: the only file in the corpus allowed to know
// where anything is.
type Catalog struct {
	SchemaVersion int             `yaml:"schema_version"`
	ProbeSyntax   string          `yaml:"probe_syntax"`
	Roles         map[string]Role `yaml:"roles"`
}

// Role is one planted fixture, addressed by name.
type Role struct {
	// Namespace is empty for a cluster-scoped role.
	Namespace string `yaml:"namespace"`
	// Probes are confirmed present before the agent starts. Syntax is
	// `<kind>/<name>` or `<kind>?<label-selector>`.
	Probes []string `yaml:"probes"`
	// Fixture describes what is planted, in prose, for a human.
	Fixture string `yaml:"fixture"`
	// NegativeSpace is what is deliberately *not* planted. Prose, and
	// load-bearing: crashloop-misleading-symptom is only a test because
	// the two hypotheses it hands the agent are checkably false.
	NegativeSpace []string `yaml:"negative_space"`
	// Exclusive names kinds no other fixture may plant in this role's
	// namespace, because a pathless absent check reads the whole set.
	Exclusive []string `yaml:"exclusive"`
	// RestoreRequiredAfter names the mutating cases that oblige the
	// runner to restore this role. See Corpus validation for why the
	// enforcement that matters runs in the other direction.
	RestoreRequiredAfter []string `yaml:"restore_required_after"`
}

// Probe is one parsed entry of [Role.Probes].
type Probe struct {
	// Raw is the entry as written, for error messages.
	Raw string
	// Kind is lowercased; a probe and a check that disagree only in case
	// are the same subject.
	Kind string
	// Name is set for a `<kind>/<name>` probe.
	Name string
	// Selector is set for a `<kind>?<label-selector>` probe.
	Selector string
}

// Case is one outcome-eval case: one file under cases/.
type Case struct {
	ID     string `yaml:"id"`
	Name   string `yaml:"name"`
	Domain string `yaml:"domain"`
	Prompt string `yaml:"prompt"`
	// ExpectedOutput is prose for a human reading a red cell. It is
	// never parsed, and the loader must not be tempted to: the risk it
	// carries is a rich expected_output beside a thin spec, read as
	// though the rich thing were tested.
	ExpectedOutput string `yaml:"expected_output"`
	// Fixtures are role names from the catalog.
	Fixtures []string `yaml:"fixtures"`
	// Repetitions defaults to [DefaultRepetitions]. A case reds the gate
	// only if all of them fail; the catastrophic rung reds on one.
	Repetitions int `yaml:"repetitions"`
	// Mutating governs sequencing and the restore obligation. Honoured
	// by the runner from day one, before any case sets it: a mutating
	// case shares crashloop-workload with all three admitted cases and
	// rewrites the field they pin as a catastrophic safeguard.
	Mutating bool `yaml:"mutating"`
	// Demoted takes a case off the blocking roster. Nil for a blocking
	// case.
	Demoted *Demotion `yaml:"demoted"`

	VerificationSpec []Check `yaml:"verification_spec"`
}

// Demotion is a flaky case taken off the blocking roster: it keeps
// running and keeps reporting, and stops blocking.
//
// A committed diff, never a runtime flag and never an environment
// variable — the sibling project that ran this experiment demoted 23% of
// its roster inside 72 hours, and what made that survivable at all was
// that each demotion was a reviewable change with a reason attached
// rather than a setting somebody flipped.
//
// The catastrophic rung is not demotable and this struct cannot make it
// so: [Board.Red] reads Demoted only for the all-repetitions rule and
// the vacuity rung.
type Demotion struct {
	// Date is when it was demoted, YYYY-MM-DD.
	Date string `yaml:"date"`
	// Measurement is what was measured, in enough detail to decide later
	// whether to promote or delete: which repetitions failed, and how.
	// Required, because "flaky" six months on is indistinguishable from
	// "nobody looked".
	Measurement string `yaml:"measurement"`
}

// DefaultRepetitions is the design's five: the difference between an
// agent that diagnoses an OOM 3 times in 5 and one that does it 5 in 5
// is the product, and a single-shot suite cannot see it.
const DefaultRepetitions = 5

// Check is one graded claim about a run. Its name reads as a claim, so a
// failure reads as its negation.
type Check struct {
	Name        string      `yaml:"name"`
	Role        CheckRole   `yaml:"role"`
	Requirement Requirement `yaml:"requirement"`
	Severity    Severity    `yaml:"severity"`
	Mode        Mode        `yaml:"mode"`
	Spec        Spec        `yaml:"check"`
}

// CheckRole separates what the agent was asked to achieve from what it
// was required not to break.
type CheckRole string

const (
	RoleObjective CheckRole = "objective"
	RoleSafeguard CheckRole = "safeguard"
)

// Requirement is mast's addition to the source schema, and the whole of
// §6. It classifies a check by whether a vacuous verdict gates.
//
// The mapping is deliberate and mirrors harness.CorpusSummary, whose two
// lists already draw this line: Dead gates, DeadDiagnostics reports.
//
//	required   <-> Dead            (a vacuous verdict reds the aggregate)
//	diagnostic <-> DeadDiagnostics (a vacuous verdict is reported only)
//
// The default is required, and it has to be per-check and explicit: a
// check that is vacuous *by construction* is not a defect to fix, and
// marking it required would red the aggregate forever on a fact about
// the runtime rather than about the agent.
type Requirement string

const (
	Required   Requirement = "required"
	Diagnostic Requirement = "diagnostic"
)

// Severity marks the never-demotable rung.
type Severity string

// Catastrophic trips on one violation in one repetition, always, and is
// never demotable.
const Catastrophic Severity = "catastrophic"

// Mode is when a check is read.
//
// A transcript is immutable once the run ends, so polling a failed
// report check only waits out the timeout: anything reading the
// transcript is assert. Converge exists for cluster reads after a
// mutating case, where reconciliation takes time.
type Mode string

const (
	ModeAssert   Mode = "assert"
	ModeConverge Mode = "converge"
)

// SpecType is the verifier a check uses.
type SpecType string

const (
	TypeReportContains          SpecType = "report_contains"
	TypeIntentSatisfied         SpecType = "intent_satisfied"
	TypeToolCalled              SpecType = "tool_called"
	TypeClusterResourceProperty SpecType = "cluster_resource_property"
	TypeApprovalRequested       SpecType = "approval_requested"

	// The two the design doc names and the loader still refuses, each
	// with the reason and the issue. Refused rather than
	// accepted-and-ignored: a case written against a verifier nothing
	// runs is a rung that cannot fire, and it reads green.
	TypeEffectRecorded SpecType = "effect_recorded"
	TypeManifestDryRun SpecType = "manifest_dry_run"
)

// unbuilt maps a named-but-unavailable verifier to why.
var unbuilt = map[SpecType]string{
	TypeEffectRecorded: "the capture record it reads shipped with #296, but nothing verifies it yet (#298)",
	TypeManifestDryRun: "deferred until the admitted roster is stable (docs/outcome-evals-design.md §3.3)",
}

// Spec is the verifier body: the `check:` block. Exactly one of the
// pointers is set, and Type says which.
type Spec struct {
	Type SpecType

	ReportContains          *ReportContains
	IntentSatisfied         *IntentSatisfied
	ToolCalled              *ToolCalled
	ClusterResourceProperty *ClusterResourceProperty
	ApprovalRequested       *ApprovalRequested
}

// ApprovalRequested asserts that a mutating call was put to an operator
// by the write gate before it could run — §3.1's replacement for the
// source's `request_approval` tool_called check.
//
// It is the one verifier here that reads a record mast wrote about
// itself at the moment it mattered, rather than the agent's account of
// what it did. That is why it is the only safeguard the loader lets
// carry a mutation claim.
type ApprovalRequested struct {
	Type SpecType `yaml:"type"`
	// ForEffect names the mutating tool. Every call the run makes to it
	// must be parked, and the park's recorded arguments must match the
	// call's: a gate that asked about a proxy is not a gate.
	//
	// One tool, not a list. The check's name reads as a claim about one
	// effect, and a list would let a failure on either of two tools
	// print under a name that mentions the other.
	ForEffect string `yaml:"for_effect"`
}

// ReportContains reads the final report. The weakest verifier here, and
// strong only when it demands a conjunction a real read produces: it is
// weak when it accepts any one plausible phrase.
//
// Matching is case-insensitive.
type ReportContains struct {
	Type SpecType `yaml:"type"`
	// RequiredPhrases must all appear. Only fair on nouns we planted:
	// we chose payments-api, so demanding it back is not a vocabulary
	// test.
	RequiredPhrases []string `yaml:"required_phrases"`
	// AnyOfPhrases covers legitimate spelling variance.
	AnyOfPhrases []string `yaml:"any_of_phrases"`
	// ForbiddenPhrases is the underused half: the agent has no
	// vulnerability feed, so a CVE id in an upgrade report is a
	// fabrication and cheap to catch.
	ForbiddenPhrases []string `yaml:"forbidden_phrases"`
}

// IntentSatisfied scores the trace against testdata/evals/intents.yaml
// rather than against tool names, so a consolidated read path that
// answers four upstream tools in one call scores as satisfaction rather
// than as a regression.
//
// Use it to assert the agent *read* something, never to assert it did
// not mutate.
type IntentSatisfied struct {
	Type    SpecType   `yaml:"type"`
	Intents []string   `yaml:"intents"`
	Mode    IntentMode `yaml:"mode"`
}

// IntentMode is all-of or any-of over the named intents. Required
// explicitly: the difference between the two is the whole meaning of the
// check, and a default would let a conjunction be written as a
// disjunction by omission.
type IntentMode string

const (
	IntentAll IntentMode = "all"
	IntentAny IntentMode = "any"
)

// ToolCalled asserts a specific named tool ran, where the intent table
// has no opinion.
//
// Never a mutation safeguard, and the loader enforces that: the trace
// shows the delegating turn, a planner-dispatched specialist's calls may
// not be in it, and a check that reads the agent's own account of itself
// is not a boundary.
type ToolCalled struct {
	Type           SpecType `yaml:"type"`
	ToolNames      []string `yaml:"tool_names"`
	RequireSuccess bool     `yaml:"require_success"`
}

// ClusterResourceProperty reads the cluster. The one that matters: it is
// the only verifier here that does not route through the agent's account
// of itself.
type ClusterResourceProperty struct {
	Type SpecType `yaml:"type"`
	// FixtureRole addresses the cluster. Required, and it is the only
	// address a check gets.
	FixtureRole string `yaml:"fixture_role"`
	// Namespace is rejected by the loader. It exists as a field so the
	// refusal can explain itself: the source schema carries both this
	// and fixture_role, which are two locations that can disagree.
	Namespace string `yaml:"namespace"`

	Kind string `yaml:"kind"`
	// ResourceName addresses one object. Omitting both it and Selector
	// turns the check into a set assertion over the role's namespace —
	// `kind: poddisruptionbudget, op: absent`, no name, means no PDB of
	// any name appeared here.
	ResourceName string `yaml:"resource_name"`
	Selector     string `yaml:"selector"`

	Path  string `yaml:"path"`
	Op    Op     `yaml:"op"`
	Value any    `yaml:"value"`

	// StableFor asserts the value did not change over a settle window.
	// On restartCount it is the difference between "the apply was
	// accepted" and "the incident stopped".
	StableFor string `yaml:"stable_for"`
	// ChangedCountEq is a blast-radius ceiling: over the matched set,
	// exactly n objects have a changed metadata.generation. mast gates
	// on verb and nothing gates on scope; this is the smallest thing
	// that does.
	ChangedCountEq *int `yaml:"changed_count_eq"`
}

// Op is the comparison a property assertion makes.
type Op string

const (
	OpEq      Op = "eq"
	OpNe      Op = "ne"
	OpGte     Op = "gte"
	OpLte     Op = "lte"
	OpAbsent  Op = "absent"
	OpPresent Op = "present"
)

// ordered reports whether an op compares magnitudes, which constrains
// what a value may be.
func (o Op) ordered() bool { return o == OpGte || o == OpLte }

// pathless reports whether an op asserts about an object rather than a
// field within one.
func (o Op) pathless() bool { return o == OpAbsent || o == OpPresent }

func (o Op) valid() bool {
	switch o {
	case OpEq, OpNe, OpGte, OpLte, OpAbsent, OpPresent:
		return true
	}
	return false
}

// Corpus is a loaded fixtures.yaml plus every case beside it.
type Corpus struct {
	Catalog Catalog
	// Cases are sorted by id, so a report's row order does not depend on
	// directory iteration.
	Cases []Case

	// probes is every role's probe list, parsed once during validation
	// and reused. Unexported: a Corpus that did not come from Load has
	// not been validated, and this is the field that would make it look
	// as though it had.
	probes map[string][]Probe
}

// Runs is the total number of agent runs a full pass costs. The wall
// clock is budgeted before the roster, not after (§7), and this is the
// number that budget is stated against.
func (c Corpus) Runs() int {
	n := 0
	for _, cs := range c.Cases {
		n += cs.Repetitions
	}
	return n
}

// ProbesFor returns a role's parsed probes, in the order the catalog
// wrote them. Unknown role names return nil.
//
// These are the subjects confirmed present before the agent starts, and
// they are the only ones a check is allowed to address — which is what
// makes them the right set for the pre-run generation snapshot too.
func (c Corpus) ProbesFor(name string) []Probe { return c.probes[name] }

// RoleFor resolves a role name, reporting whether the catalog has it.
func (c Corpus) RoleFor(name string) (Role, bool) {
	r, ok := c.Catalog.Roles[name]
	return r, ok
}

// Mutating returns the ids of the cases that change the cluster. They
// run last and alone, never concurrently with a case sharing one of
// their fixture roles.
func (c Corpus) Mutating() []string {
	var out []string
	for _, cs := range c.Cases {
		if cs.Mutating {
			out = append(out, cs.ID)
		}
	}
	return out
}

// String renders a probe as it was written.
func (p Probe) String() string { return p.Raw }

// subject is the (kind, name-or-selector) a probe or a check addresses,
// used to match the two.
func (p Probe) subject() string {
	if p.Selector != "" {
		return fmt.Sprintf("%s?%s", p.Kind, p.Selector)
	}
	return fmt.Sprintf("%s/%s", p.Kind, p.Name)
}
