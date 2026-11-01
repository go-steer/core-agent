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

package outcome

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/go-steer/core-agent/v3/internal/evals"
)

// CatalogFile and CasesDir are the corpus layout, relative to the
// directory handed to [Load].
const (
	CatalogFile = "fixtures.yaml"
	CasesDir    = "cases"
)

// demotionDate is the layout a demotion's date takes. Date only: the
// hour a case was demoted is not a thing anyone reads.
const demotionDate = "2006-01-02"

// kebab is the shape an id and a check name take. Enforced because a
// check name reads as a claim in a report, and mixed conventions in one
// column read as two different kinds of thing.
var kebab = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Load reads a corpus directory: the role catalog and every case file
// under cases/.
//
// The intent table is required rather than optional. Every intent a case
// names is checked against it, and — more to the point — the table is
// what decides whether an intent_satisfied check can measure anything at
// all. Loading without it would let the one class of vacuity this corpus
// already contains pass unnoticed, which is the thing §6 exists to stop.
//
// Every failure is fatal. A skipped case shrinks the roster, and a
// smaller roster reads as a faster gate rather than as a weaker one.
func Load(dir string, tbl evals.IntentTable) (Corpus, error) {
	cat, err := loadCatalog(filepath.Join(dir, CatalogFile))
	if err != nil {
		return Corpus{}, err
	}
	cases, err := loadCases(filepath.Join(dir, CasesDir))
	if err != nil {
		return Corpus{}, err
	}
	c := Corpus{Catalog: cat, Cases: cases}
	if err := c.validate(tbl); err != nil {
		return Corpus{}, fmt.Errorf("outcome: %s: %w", dir, err)
	}
	return c, nil
}

func loadCatalog(path string) (Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Catalog{}, fmt.Errorf("outcome: read catalog: %w", err)
	}
	var cat Catalog
	if err := decodeStrict(data, &cat); err != nil {
		return Catalog{}, fmt.Errorf("outcome: %s: %w", path, err)
	}
	if cat.SchemaVersion != 1 {
		return Catalog{}, fmt.Errorf("outcome: %s: schema_version %d, want 1", path, cat.SchemaVersion)
	}
	if len(cat.Roles) == 0 {
		return Catalog{}, fmt.Errorf("outcome: %s: no roles", path)
	}
	return cat, nil
}

func loadCases(dir string) ([]Case, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("outcome: read cases: %w", err)
	}
	var cases []Case
	seen := make(map[string]string)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("outcome: read case: %w", err)
		}
		var cs Case
		if err := decodeStrict(data, &cs); err != nil {
			return nil, fmt.Errorf("outcome: %s: %w", path, err)
		}
		// The filename is how a human finds a red row, so it is the id
		// and not merely near it.
		if want := strings.TrimSuffix(e.Name(), ".yaml"); cs.ID != want {
			return nil, fmt.Errorf("outcome: %s: id %q does not match the filename", path, cs.ID)
		}
		if prev, dup := seen[cs.ID]; dup {
			return nil, fmt.Errorf("outcome: %s: duplicate id %q, already loaded from %s", path, cs.ID, prev)
		}
		seen[cs.ID] = path
		cases = append(cases, cs)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("outcome: %s: no cases", dir)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return cases, nil
}

// decodeStrict rejects a key the schema does not describe. A typo'd key
// is otherwise a silently dropped assertion, and a case with a dropped
// assertion still runs and still reports green.
func decodeStrict(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

// UnmarshalYAML decodes the `check:` block, which is a union keyed on
// `type`.
//
// The body is re-marshalled and decoded through a strict decoder rather
// than through node.Decode, because node.Decode does not carry
// KnownFields — so a typo inside a check body would survive the strict
// decode of everything around it. That is the worst place to lose it:
// `required_phrase` for `required_phrases` yields a check with no
// phrases, which is precisely the empty-list vacuity §6 names.
func (s *Spec) UnmarshalYAML(node *yaml.Node) error {
	var probe struct {
		Type SpecType `yaml:"type"`
	}
	if err := node.Decode(&probe); err != nil {
		return fmt.Errorf("check body: %w", err)
	}
	if probe.Type == "" {
		return fmt.Errorf("check body declares no type")
	}
	if why, unavailable := unbuilt[probe.Type]; unavailable {
		return fmt.Errorf("check type %q is named in the design but not built: %s", probe.Type, why)
	}

	body, err := yaml.Marshal(node)
	if err != nil {
		return fmt.Errorf("check body %q: %w", probe.Type, err)
	}

	s.Type = probe.Type
	switch probe.Type {
	case TypeReportContains:
		s.ReportContains = &ReportContains{}
		err = decodeStrict(body, s.ReportContains)
	case TypeIntentSatisfied:
		s.IntentSatisfied = &IntentSatisfied{}
		err = decodeStrict(body, s.IntentSatisfied)
	case TypeToolCalled:
		s.ToolCalled = &ToolCalled{}
		err = decodeStrict(body, s.ToolCalled)
	case TypeClusterResourceProperty:
		s.ClusterResourceProperty = &ClusterResourceProperty{}
		err = decodeStrict(body, s.ClusterResourceProperty)
	case TypeApprovalRequested:
		s.ApprovalRequested = &ApprovalRequested{}
		err = decodeStrict(body, s.ApprovalRequested)
	default:
		return fmt.Errorf("unknown check type %q", probe.Type)
	}
	if err != nil {
		return fmt.Errorf("check type %q: %w", probe.Type, err)
	}
	return nil
}

// validate applies every rule the package doc lists, and fills the
// defaults. It mutates the corpus in place, so a caller reads
// Repetitions and Requirement without re-deriving what "unset" meant.
func (c *Corpus) validate(tbl evals.IntentTable) error {
	if len(tbl.Intents) == 0 {
		return fmt.Errorf("no intent table: an intent_satisfied check could not be told from one that names nothing")
	}

	probes, err := c.parseProbes()
	if err != nil {
		return err
	}
	// Kept, so the provisioner reads the same parse the validation did
	// rather than a second one that could disagree with it.
	c.probes = probes

	// asserted accumulates every (role, subject) a check addresses, for
	// the reverse half of the probe corollary.
	asserted := make(map[string]map[string]bool)
	// usedRoles is the forward half at role granularity: a role no case
	// declares is fixture nobody reads.
	usedRoles := make(map[string]bool)

	for i := range c.Cases {
		cs := &c.Cases[i]
		if err := c.validateCase(cs, tbl, probes, asserted, usedRoles); err != nil {
			return fmt.Errorf("case %s: %w", cs.ID, err)
		}
	}

	for name := range c.Catalog.Roles {
		if !usedRoles[name] {
			return fmt.Errorf("role %q is declared by no case: a fixture nobody reads is provisioning time bought for nothing", name)
		}
	}
	for name, ps := range probes {
		for _, p := range ps {
			if !asserted[name][p.subject()] {
				return fmt.Errorf(
					"role %q probes %s and no check asserts on it: a probe nothing reads confirms the fixture was planted and measures nothing after",
					name, p)
			}
		}
	}
	return c.validateRestore()
}

// parseProbes reads every role's probe list into subjects.
func (c *Corpus) parseProbes() (map[string][]Probe, error) {
	out := make(map[string][]Probe, len(c.Catalog.Roles))
	names := sortedKeys(c.Catalog.Roles)
	for _, name := range names {
		role := c.Catalog.Roles[name]
		if len(role.Probes) == 0 {
			return nil, fmt.Errorf(
				"role %q has no probes: nothing would entitle a later absence to be called a violation rather than an environment that was never provisioned",
				name)
		}
		seen := make(map[string]bool)
		for _, raw := range role.Probes {
			p, err := parseProbe(raw)
			if err != nil {
				return nil, fmt.Errorf("role %q: %w", name, err)
			}
			if seen[p.subject()] {
				return nil, fmt.Errorf("role %q probes %s twice", name, p)
			}
			seen[p.subject()] = true
			out[name] = append(out[name], p)
		}
	}
	return out, nil
}

func parseProbe(raw string) (Probe, error) {
	p := Probe{Raw: raw}
	switch {
	case strings.Count(raw, "?") == 1:
		kind, sel, _ := strings.Cut(raw, "?")
		p.Kind, p.Selector = strings.ToLower(kind), sel
		if p.Kind == "" || p.Selector == "" {
			return Probe{}, fmt.Errorf("probe %q: want <kind>?<label-selector>", raw)
		}
	case strings.Count(raw, "/") == 1:
		kind, name, _ := strings.Cut(raw, "/")
		p.Kind, p.Name = strings.ToLower(kind), name
		if p.Kind == "" || p.Name == "" {
			return Probe{}, fmt.Errorf("probe %q: want <kind>/<name>", raw)
		}
	default:
		return Probe{}, fmt.Errorf("probe %q: want <kind>/<name> or <kind>?<label-selector>", raw)
	}
	return p, nil
}

func (c *Corpus) validateCase(
	cs *Case,
	tbl evals.IntentTable,
	probes map[string][]Probe,
	asserted map[string]map[string]bool,
	usedRoles map[string]bool,
) error {
	if !kebab.MatchString(cs.ID) {
		return fmt.Errorf("id %q is not kebab-case", cs.ID)
	}
	if strings.TrimSpace(cs.Name) == "" {
		return fmt.Errorf("no name: a red cell with no name is a row a reader cannot act on")
	}
	if strings.TrimSpace(cs.Prompt) == "" {
		return fmt.Errorf("no prompt")
	}
	if strings.TrimSpace(cs.ExpectedOutput) == "" {
		return fmt.Errorf("no expected_output: nothing parses it, and a human reading a red cell has nothing else that says what right looked like")
	}
	switch {
	case cs.Repetitions == 0:
		cs.Repetitions = DefaultRepetitions
	case cs.Repetitions < 1:
		return fmt.Errorf("repetitions %d", cs.Repetitions)
	}
	if len(cs.Fixtures) == 0 {
		return fmt.Errorf("declares no fixtures")
	}
	declared := make(map[string]bool, len(cs.Fixtures))
	for _, role := range cs.Fixtures {
		if _, ok := c.Catalog.Roles[role]; !ok {
			return fmt.Errorf("fixture role %q is not in %s", role, CatalogFile)
		}
		if declared[role] {
			return fmt.Errorf("declares fixture role %q twice", role)
		}
		declared[role] = true
		usedRoles[role] = true
	}
	if len(cs.VerificationSpec) == 0 {
		return fmt.Errorf("has no verification_spec: expected_output is prose, so this case grades nothing")
	}
	if d := cs.Demoted; d != nil {
		// Both fields required. A demotion with no date cannot be aged
		// out, and one with no measurement is indistinguishable six
		// months on from a case nobody ever looked at again.
		if _, err := time.Parse(demotionDate, d.Date); err != nil {
			return fmt.Errorf("demoted.date %q is not %s", d.Date, demotionDate)
		}
		if strings.TrimSpace(d.Measurement) == "" {
			return fmt.Errorf("demoted.measurement is empty: record which repetitions failed and how, or this row cannot be promoted or deleted later on any evidence")
		}
	}

	seenCheck := make(map[string]bool, len(cs.VerificationSpec))
	for i := range cs.VerificationSpec {
		ck := &cs.VerificationSpec[i]
		if !kebab.MatchString(ck.Name) {
			return fmt.Errorf("check %d: name %q is not kebab-case", i, ck.Name)
		}
		if seenCheck[ck.Name] {
			return fmt.Errorf("two checks named %q", ck.Name)
		}
		seenCheck[ck.Name] = true
		if err := c.validateCheck(cs, ck, tbl, declared, probes, asserted); err != nil {
			return fmt.Errorf("check %q: %w", ck.Name, err)
		}
	}
	return nil
}

func (c *Corpus) validateCheck(
	cs *Case,
	ck *Check,
	tbl evals.IntentTable,
	declared map[string]bool,
	probes map[string][]Probe,
	asserted map[string]map[string]bool,
) error {
	switch ck.Role {
	case RoleObjective, RoleSafeguard:
	case "":
		return fmt.Errorf("no role")
	default:
		return fmt.Errorf("role %q, want %s or %s", ck.Role, RoleObjective, RoleSafeguard)
	}

	switch ck.Requirement {
	case "":
		ck.Requirement = Required
	case Required, Diagnostic:
	default:
		return fmt.Errorf("requirement %q, want %s or %s", ck.Requirement, Required, Diagnostic)
	}
	// A safeguard exists to red the gate. One that reports and does not
	// gate is a safeguard in name only, and the name is what a reader
	// trusts.
	if ck.Role == RoleSafeguard && ck.Requirement == Diagnostic {
		return fmt.Errorf("a safeguard cannot be diagnostic")
	}

	switch {
	case ck.Severity == "":
	case ck.Severity != Catastrophic:
		return fmt.Errorf("severity %q, want %s", ck.Severity, Catastrophic)
	case ck.Role != RoleSafeguard:
		return fmt.Errorf("severity %s on an %s check", Catastrophic, RoleObjective)
	}

	switch ck.Mode {
	case "":
		ck.Mode = ModeAssert
	case ModeAssert, ModeConverge:
	default:
		return fmt.Errorf("mode %q, want %s or %s", ck.Mode, ModeAssert, ModeConverge)
	}
	if ck.Mode == ModeConverge && ck.Spec.Type != TypeClusterResourceProperty {
		return fmt.Errorf(
			"mode %s on a %s check: the transcript is immutable once the run ends, so polling only waits out the timeout",
			ModeConverge, ck.Spec.Type)
	}

	switch ck.Spec.Type {
	case TypeReportContains:
		return validateReportContains(ck.Spec.ReportContains)
	case TypeIntentSatisfied:
		return validateIntentSatisfied(ck, tbl)
	case TypeToolCalled:
		return validateToolCalled(ck)
	case TypeClusterResourceProperty:
		return c.validateClusterCheck(cs, ck, declared, probes, asserted)
	case TypeApprovalRequested:
		return validateApprovalRequested(cs, ck)
	}
	return fmt.Errorf("unhandled check type %q", ck.Spec.Type)
}

// validateApprovalRequested refuses the two ways a park assertion can
// be written and measure nothing.
//
// Note what is deliberately *not* refused: role: safeguard. It is the
// only mutation safeguard in the schema, which is the whole of §3.1 —
// tool_called is refused as one because the trace is the agent's
// account of itself, and this reads a record mast wrote at the gate.
func validateApprovalRequested(cs *Case, ck *Check) error {
	spec := ck.Spec.ApprovalRequested
	if strings.TrimSpace(spec.ForEffect) == "" {
		return fmt.Errorf("names no for_effect: the check has no subject, and a run that mutates nothing would report the same as one that mutates ungated")
	}
	// A run can only park a call it can make. The tier's tool surface is
	// read-only on every non-mutating case, so the tool named here is
	// one the model is not shown — the check cannot fire, and under the
	// default requirement it would red every pass on a fact about the
	// corpus rather than about the agent (§6).
	if !cs.Mutating {
		return fmt.Errorf(
			"asserts a park for %q on a case that is not mutating: true — the surface a read-only case runs against carries no mutating tool, so the call can never be made and the check can never fire",
			spec.ForEffect)
	}
	return nil
}

func validateReportContains(spec *ReportContains) error {
	if len(spec.RequiredPhrases)+len(spec.AnyOfPhrases)+len(spec.ForbiddenPhrases) == 0 {
		return fmt.Errorf("names no phrases: it would pass on every report ever written")
	}
	// Matching is case-insensitive, so two phrases differing only in
	// case are one phrase written twice. Within a list that is a list
	// longer than the assertion it makes; across two lists it is a
	// contradiction — a phrase both required and forbidden is a check no
	// report can pass.
	//
	// The three lists are walked in a fixed order rather than over a map
	// literal, so a corpus with two problems always reports the same one.
	lists := []struct {
		label string
		items []string
	}{
		{"required_phrases", spec.RequiredPhrases},
		{"any_of_phrases", spec.AnyOfPhrases},
		{"forbidden_phrases", spec.ForbiddenPhrases},
	}
	seen := make(map[string]string)
	for _, l := range lists {
		for _, p := range l.items {
			key := strings.ToLower(strings.TrimSpace(p))
			if key == "" {
				return fmt.Errorf("%s carries an empty phrase, which every report contains", l.label)
			}
			if prev, dup := seen[key]; dup {
				if prev == l.label {
					return fmt.Errorf("%s names %q twice", l.label, p)
				}
				return fmt.Errorf("phrase %q is in both %s and %s", p, prev, l.label)
			}
			seen[key] = l.label
		}
	}
	return nil
}

// validateIntentSatisfied is where §3.4 stops being a paragraph.
//
// Two static conditions make an intent check unable to measure what it
// is written as, and both are decidable from the intent table alone:
//
//   - an intent no lookout tool satisfies can never be reached, so the
//     check is a rung that cannot fire;
//   - a `mode: all` set that one single tool satisfies whole cannot tell
//     "the agent read both" from "the agent made one call", which is the
//     only thing such a check is ever written to say.
//
// The second is not a defect in intents.yaml. The table is deliberately
// indifferent to how many calls produced an answer — that indifference
// is the consolidation thesis (docs/README.md:149) — so the check is
// vacuous by construction and permanently. It is admitted as a
// diagnostic and refused as required, which keeps the record that the
// two-object read is unmeasured without reddening the gate forever on a
// decision the project made on purpose.
func validateIntentSatisfied(ck *Check, tbl evals.IntentTable) error {
	spec := ck.Spec.IntentSatisfied
	if len(spec.Intents) == 0 {
		return fmt.Errorf("names no intents")
	}
	switch spec.Mode {
	case IntentAll, IntentAny:
	case "":
		return fmt.Errorf("no mode: all and any are different claims and neither is the safe default")
	default:
		return fmt.Errorf("mode %q, want %s or %s", spec.Mode, IntentAll, IntentAny)
	}

	defined := make(map[string]bool, len(tbl.Intents))
	for _, in := range tbl.Intents {
		defined[in.ID] = true
	}
	reachable := make(map[string]bool)
	for _, lt := range tbl.LookoutTools {
		for _, id := range lt.Satisfies {
			reachable[id] = true
		}
	}

	seen := make(map[string]bool, len(spec.Intents))
	var unreachable []string
	for _, id := range spec.Intents {
		if !defined[id] {
			return fmt.Errorf("intent %q is not in the intent table", id)
		}
		if seen[id] {
			return fmt.Errorf("names intent %q twice", id)
		}
		seen[id] = true
		if !reachable[id] {
			unreachable = append(unreachable, id)
		}
	}

	// A check that no run can satisfy is refused outright, whatever its
	// requirement: reporting a rung that always reds is not the same
	// service as reporting one that cannot distinguish.
	switch {
	case spec.Mode == IntentAll && len(unreachable) > 0:
		return fmt.Errorf("mode all, but no tool this runtime can call satisfies %s", strings.Join(unreachable, ", "))
	case spec.Mode == IntentAny && len(unreachable) == len(spec.Intents):
		return fmt.Errorf("no tool this runtime can call satisfies any of %s", strings.Join(spec.Intents, ", "))
	}

	if spec.Mode == IntentAll && len(spec.Intents) > 1 {
		if tool := singleToolSatisfying(tbl, spec.Intents); tool != "" && ck.Requirement != Diagnostic {
			return fmt.Errorf(
				"mode all over %s, but one %s call satisfies all of them, so this cannot tell two reads from one — the intent layer is indifferent to call count by design (docs/README.md:149), so mark it %s rather than deleting it",
				strings.Join(spec.Intents, " + "), tool, Diagnostic)
		}
	}
	return nil
}

// singleToolSatisfying names a lookout tool whose one call satisfies
// every intent in want, or "" when the conjunction needs more than one.
func singleToolSatisfying(tbl evals.IntentTable, want []string) string {
	for _, name := range sortedKeys(tbl.LookoutTools) {
		covers := make(map[string]bool)
		for _, id := range tbl.LookoutTools[name].Satisfies {
			covers[id] = true
		}
		all := true
		for _, id := range want {
			if !covers[id] {
				all = false
				break
			}
		}
		if all {
			return name
		}
	}
	return ""
}

func validateToolCalled(ck *Check) error {
	if len(ck.Spec.ToolCalled.ToolNames) == 0 {
		return fmt.Errorf("names no tools")
	}
	// The source's own tool_called section forbids this and its
	// crashloop-remediate-and-verify case does it anyway. The trace
	// shows the delegating turn, a planner-dispatched specialist's calls
	// may not be in it (DESIGN.md), and a check reading the agent's own
	// account of itself is not a boundary.
	if ck.Role == RoleSafeguard {
		return fmt.Errorf(
			"a %s check cannot be a safeguard: it reads the agent's own account of itself, and a planner-dispatched specialist's calls need not appear in the trace at all",
			TypeToolCalled)
	}
	return nil
}

func (c *Corpus) validateClusterCheck(
	cs *Case,
	ck *Check,
	declared map[string]bool,
	probes map[string][]Probe,
	asserted map[string]map[string]bool,
) error {
	spec := ck.Spec.ClusterResourceProperty

	if spec.FixtureRole == "" {
		return fmt.Errorf("no fixture_role: a check with no role has no address")
	}
	if !declared[spec.FixtureRole] {
		return fmt.Errorf("fixture_role %q is not one of the case's fixtures", spec.FixtureRole)
	}
	// The source schema carries both, and rule one of the same document
	// says a case may never name a location. Two addresses for one
	// object can disagree, and the one that loses is the role — which is
	// the one the provisioner actually used.
	if spec.Namespace != "" {
		return fmt.Errorf(
			"carries both fixture_role %q and a literal namespace %q: the role addresses the cluster, and a check with two locations has one that can go stale",
			spec.FixtureRole, spec.Namespace)
	}
	if spec.Kind == "" {
		return fmt.Errorf("no kind")
	}
	if spec.ResourceName != "" && spec.Selector != "" {
		return fmt.Errorf("carries both resource_name and selector")
	}

	role := c.Catalog.Roles[spec.FixtureRole]
	kind := strings.ToLower(spec.Kind)

	// A set assertion reads every object of a kind in the role's
	// namespace, so a role with no namespace gives it no bound.
	if spec.ResourceName == "" && spec.Selector == "" && role.Namespace == "" {
		return fmt.Errorf(
			"is a set assertion over %q, which is cluster-scoped: without a namespace the matched set is unbounded",
			spec.FixtureRole)
	}

	if spec.ChangedCountEq != nil {
		if *spec.ChangedCountEq < 0 {
			return fmt.Errorf("changed_count_eq %d", *spec.ChangedCountEq)
		}
		if spec.Op != "" || spec.Path != "" || spec.Value != nil {
			return fmt.Errorf("changed_count_eq is a blast-radius ceiling over the matched set, not a property assertion: drop op/path/value")
		}
		if spec.StableFor != "" {
			return fmt.Errorf("changed_count_eq with stable_for: one counts what changed, the other watches what did not")
		}
	} else {
		if !spec.Op.valid() {
			return fmt.Errorf("op %q, want one of eq, ne, gte, lte, absent, present", spec.Op)
		}
		switch {
		case spec.Op.pathless() && spec.Path != "":
			return fmt.Errorf("op %s reads an object, not a field: drop path", spec.Op)
		case !spec.Op.pathless() && spec.Path == "":
			return fmt.Errorf("op %s needs a path", spec.Op)
		case spec.Op.pathless() && spec.Value != nil:
			return fmt.Errorf("op %s takes no value", spec.Op)
		case !spec.Op.pathless() && spec.Value == nil:
			return fmt.Errorf("op %s needs a value", spec.Op)
		}
		if spec.Op.ordered() {
			switch spec.Value.(type) {
			case int, float64:
			default:
				return fmt.Errorf("op %s compares magnitudes, and value %v is not a number", spec.Op, spec.Value)
			}
		}
	}

	if spec.StableFor != "" && ck.Mode != ModeConverge {
		return fmt.Errorf("stable_for reads a settle window and needs mode %s", ModeConverge)
	}

	// The forward half of the probe corollary. An assertion on an object
	// no probe confirmed cannot tell "the agent deleted it" from "it was
	// never planted", and the second reads as the first.
	if spec.ResourceName != "" || spec.Selector != "" {
		p := Probe{Kind: kind, Name: spec.ResourceName, Selector: spec.Selector}
		if !hasProbe(probes[spec.FixtureRole], p.subject()) {
			return fmt.Errorf(
				"asserts on %s, which role %q does not probe: an absence there could not be told from an environment that was never provisioned",
				p.subject(), spec.FixtureRole)
		}
		if asserted[spec.FixtureRole] == nil {
			asserted[spec.FixtureRole] = make(map[string]bool)
		}
		asserted[spec.FixtureRole][p.subject()] = true
	}

	// A read-only case that does not pin the fixture is a case whose
	// safeguard is the absence of a safeguard.
	if !cs.Mutating && ck.Role == RoleSafeguard && spec.ChangedCountEq != nil {
		return fmt.Errorf("a read-only case has no changed set to bound")
	}
	return nil
}

func hasProbe(ps []Probe, subject string) bool {
	for _, p := range ps {
		if p.subject() == subject {
			return true
		}
	}
	return false
}

// validateRestore enforces the restore obligation in both directions.
//
// The forward direction stops a dangling name: a role naming a case that
// is not in the corpus has an obligation with nothing behind it, which
// is the shape a typo takes and the shape that disarms it silently.
//
// The reverse direction is the one that matters. A mutating case must be
// named by every role it declares, so admitting crashloop-remediate-and-
// verify — which shares crashloop-workload with all three read-only
// cases and rewrites the exact field they pin at 64Mi as a catastrophic
// safeguard — cannot happen without the same commit stating that the
// runner has to put the fixture back.
func (c *Corpus) validateRestore() error {
	byID := make(map[string]Case, len(c.Cases))
	for _, cs := range c.Cases {
		byID[cs.ID] = cs
	}
	for _, name := range sortedKeys(c.Catalog.Roles) {
		role := c.Catalog.Roles[name]
		for _, id := range role.RestoreRequiredAfter {
			cs, ok := byID[id]
			if !ok {
				return fmt.Errorf(
					"role %q requires a restore after case %q, which is not in the corpus: an obligation naming nothing is an obligation nothing enforces",
					name, id)
			}
			if !cs.Mutating {
				return fmt.Errorf("role %q requires a restore after case %q, which does not mutate", name, id)
			}
		}
	}
	for _, cs := range c.Cases {
		if !cs.Mutating {
			continue
		}
		for _, roleName := range cs.Fixtures {
			if !contains(c.Catalog.Roles[roleName].RestoreRequiredAfter, cs.ID) {
				return fmt.Errorf(
					"case %q mutates and declares role %q, which does not list it under restore_required_after: the next case sharing that fixture would read the leftovers as the agent's work",
					cs.ID, roleName)
			}
		}
	}
	return nil
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// sortedKeys keeps every error message and every iteration order stable,
// so a corpus with two problems always reports the same one first.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
