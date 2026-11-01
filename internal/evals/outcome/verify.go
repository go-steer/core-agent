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
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-steer/core-agent/v3/internal/evals"
)

// This file grades a run. The loader already refused every check whose
// vacuity is decidable from the corpus alone — an empty phrase list, an
// unreachable intent, a `mode: all` set one tool satisfies whole. What
// is left is the vacuity that only a run can reveal:
//
//   - a report check with nothing to read, because the agent produced no
//     final text;
//   - an intent check where the agent called tools and not one of them
//     is in the intent table, which is §4's finding — the GKE surface —
//     arriving at run time instead of at load time;
//   - a property assertion whose path resolves on no matched object, or
//     whose matched set is empty.
//
// Each of those is recorded as Vacuous rather than as a pass or a fail,
// and [Board.Red] is what decides that a required vacuous verdict reds
// the aggregate. Which way the constant points is not the property:
// `op: ne` against a path that does not resolve passes, and passing is
// the more dangerous of the two outcomes because nobody investigates it.

// Defaults for the converge loop. A converge check exists because
// reconciliation takes time; the window bounds how much of it the gate
// is willing to buy.
const (
	DefaultConvergeWindow = 60 * time.Second
	DefaultPollInterval   = 2 * time.Second
)

// Run is one agent run's product, as the runner hands it to the
// verifier.
type Run struct {
	Case  string
	Index int // 1-based repetition
	// Report is the agent's final report. The runner fills it from the
	// trace's final text; it is a field rather than a derivation so a
	// caller with a better source of the report can supply one.
	Report string
	Trace  evals.Trace
	// Err is a run that did not finish. Its checks are still evaluated:
	// a cluster read is valid whatever the agent did, and a catastrophic
	// safeguard is at its most interesting on a run that fell over.
	Err error
}

// Verdict is one check's result on one run.
type Verdict struct {
	Case        string
	Index       int
	Check       string
	Role        CheckRole
	Requirement Requirement
	Severity    Severity

	Passed bool
	// Vacuous marks a verdict that carries no information about the run.
	// Independent of Passed, and usually true alongside Passed: the
	// constant a vacuous check evaluates to is more often true.
	Vacuous bool
	// Detail is what a human reads off a red cell. It states what was
	// looked at and what was found, never only that the check failed.
	Detail string
}

func (v Verdict) catastrophic() bool { return v.Severity == Catastrophic }

// Verifier grades runs against a corpus. It holds the pre-run snapshot
// because a blast-radius ceiling is a claim about a delta, and the only
// honest place to take the "before" is before the agent starts.
type Verifier struct {
	corpus Corpus
	table  evals.IntentTable
	prov   *Provisioner
	before Snapshot

	// ConvergeWindow bounds a converge check's polling. Zero means
	// DefaultConvergeWindow.
	ConvergeWindow time.Duration
	// PollInterval is how often a converge check re-reads. Zero means
	// DefaultPollInterval.
	PollInterval time.Duration
}

// NewVerifier builds a verifier over a provisioned cluster.
func NewVerifier(c Corpus, tbl evals.IntentTable, p *Provisioner, before Snapshot) (*Verifier, error) {
	if p == nil {
		return nil, fmt.Errorf("outcome: verifier needs a provisioner: every cluster check reads through it")
	}
	if len(tbl.Intents) == 0 {
		return nil, fmt.Errorf("outcome: verifier needs an intent table")
	}
	return &Verifier{corpus: c, table: tbl, prov: p, before: before}, nil
}

func (v *Verifier) window() time.Duration {
	if v.ConvergeWindow <= 0 {
		return DefaultConvergeWindow
	}
	return v.ConvergeWindow
}

func (v *Verifier) interval() time.Duration {
	if v.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return v.PollInterval
}

// Verify grades every check in a case against one run.
//
// It returns an error only when the grading itself could not be carried
// out — a cluster it cannot reach, a case id it does not know. A check
// that fails is a verdict, not an error.
func (v *Verifier) Verify(ctx context.Context, run Run) ([]Verdict, error) {
	var cs *Case
	for i := range v.corpus.Cases {
		if v.corpus.Cases[i].ID == run.Case {
			cs = &v.corpus.Cases[i]
			break
		}
	}
	if cs == nil {
		return nil, fmt.Errorf("outcome: verify: no case %q in the corpus", run.Case)
	}

	verdicts := make([]Verdict, 0, len(cs.VerificationSpec))
	for i := range cs.VerificationSpec {
		ck := &cs.VerificationSpec[i]
		vd := Verdict{
			Case:        cs.ID,
			Index:       run.Index,
			Check:       ck.Name,
			Role:        ck.Role,
			Requirement: ck.Requirement,
			Severity:    ck.Severity,
		}
		var err error
		switch ck.Spec.Type {
		case TypeReportContains:
			vd.Passed, vd.Vacuous, vd.Detail = verifyReport(ck.Spec.ReportContains, run.Report)
		case TypeIntentSatisfied:
			vd.Passed, vd.Vacuous, vd.Detail = v.verifyIntent(ck.Spec.IntentSatisfied, run.Trace)
		case TypeToolCalled:
			// Vacuous stays false, and the signature says so: the names
			// are literals and the trace is a literal record of what was
			// called, so there is no way for this one to measure nothing.
			vd.Passed, vd.Detail = verifyToolCalled(ck.Spec.ToolCalled, run.Trace)
		case TypeClusterResourceProperty:
			vd.Passed, vd.Vacuous, vd.Detail, err = v.verifyCluster(ctx, ck)
		case TypeApprovalRequested:
			vd.Passed, vd.Vacuous, vd.Detail = verifyApprovalRequested(ck.Spec.ApprovalRequested, run.Trace)
		default:
			return nil, fmt.Errorf("outcome: verify %s/%s: no verifier for %q", cs.ID, ck.Name, ck.Spec.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("outcome: verify %s/%s: %w", cs.ID, ck.Name, err)
		}
		verdicts = append(verdicts, vd)
	}
	return verdicts, nil
}

// verifyApprovalRequested reads the write gate's park record off the
// trace (#295). The whole verifier is a delegation, and it is one on
// purpose: the projection and the predicate belong beside the other
// evaluators, where a unit test can build the adversarial shapes
// directly instead of staging a session store that produces them.
//
// The vacuity it can report is the run-time half the loader cannot
// decide: a case whose model never proposed the mutating call at all.
// A safeguard that reported green for that would be §7's rung that
// cannot fire, so it reds under the default requirement.
func verifyApprovalRequested(spec *ApprovalRequested, tr evals.Trace) (passed, vacuous bool, detail string) {
	res := evals.ApprovalRequested(tr, spec.ForEffect)
	return res.Passed(), res.Vacuous, res.Comment
}

// verifyReport reads the final report.
func verifyReport(spec *ReportContains, report string) (passed, vacuous bool, detail string) {
	hay := strings.ToLower(report)
	if strings.TrimSpace(hay) == "" {
		// Nothing was read, so nothing was measured. A forbidden-only
		// check would otherwise pass here, and pass loudest on the run
		// where the agent produced nothing at all.
		return false, true, "the report is empty: no phrase assertion read anything"
	}

	var missing, forbidden []string
	for _, p := range spec.RequiredPhrases {
		if !strings.Contains(hay, strings.ToLower(p)) {
			missing = append(missing, p)
		}
	}
	for _, p := range spec.ForbiddenPhrases {
		if strings.Contains(hay, strings.ToLower(p)) {
			forbidden = append(forbidden, p)
		}
	}
	anyOf := len(spec.AnyOfPhrases) == 0
	for _, p := range spec.AnyOfPhrases {
		if strings.Contains(hay, strings.ToLower(p)) {
			anyOf = true
			break
		}
	}

	var why []string
	if len(missing) > 0 {
		why = append(why, "missing required "+quoteList(missing))
	}
	if !anyOf {
		why = append(why, "none of "+quoteList(spec.AnyOfPhrases))
	}
	if len(forbidden) > 0 {
		// Worth its own clause: a forbidden phrase is usually a
		// fabrication, which is a different defect from an omission.
		why = append(why, "forbidden "+quoteList(forbidden)+" appears in the report")
	}
	if len(why) == 0 {
		return true, false, "every phrase assertion held"
	}
	return false, false, strings.Join(why, "; ")
}

// verifyIntent scores the trace against the intent table.
func (v *Verifier) verifyIntent(spec *IntentSatisfied, tr evals.Trace) (passed, vacuous bool, detail string) {
	names := make([]string, 0, len(tr.Calls))
	for _, c := range tr.Calls {
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		// A genuine failure, not vacuity. "The agent called nothing" is
		// exactly the information this check exists to carry.
		return false, false, "the agent made no tool calls"
	}

	inTable := 0
	for _, n := range names {
		if _, ok := v.table.LookoutTools[n]; ok {
			inTable++
		}
	}
	if inTable == 0 {
		// §4's finding, arriving at run time. The agent worked; it
		// worked through a tool surface this table has no rows for, so
		// the check scores zero for a reason that is about the harness
		// and not about the agent.
		return false, true, fmt.Sprintf(
			"the agent called %d tool(s) and none of them are in the intent table (%s): this check measured the tool surface, not the run",
			len(names), quoteList(uniq(names)))
	}

	got := v.table.SatisfiedBy(names)
	have := make(map[string]bool, len(got))
	for _, id := range got {
		have[id] = true
	}

	var met, unmet []string
	for _, id := range spec.Intents {
		if have[id] {
			met = append(met, id)
		} else {
			unmet = append(unmet, id)
		}
	}
	switch spec.Mode {
	case IntentAll:
		if len(unmet) == 0 {
			return true, false, "satisfied all of " + quoteList(spec.Intents)
		}
		return false, false, "unsatisfied " + quoteList(unmet) + "; satisfied " + quoteList(met)
	case IntentAny:
		if len(met) > 0 {
			return true, false, "satisfied " + quoteList(met)
		}
		return false, false, "satisfied none of " + quoteList(spec.Intents)
	}
	// Unreachable: the loader requires an explicit mode.
	return false, true, fmt.Sprintf("intent mode %q is not one this verifier knows", spec.Mode)
}

// verifyToolCalled asserts a named tool ran.
//
// It returns no vacuity, and that is the claim rather than an omission:
// the names are literals and the trace is a literal record of what was
// called, so there is no run on which this check measures nothing.
func verifyToolCalled(spec *ToolCalled, tr evals.Trace) (passed bool, detail string) {
	var missing []string
	var incomplete []string
	for _, want := range spec.ToolNames {
		found, completed := false, false
		for _, c := range tr.Calls {
			if c.Name != want {
				continue
			}
			found = true
			if c.Completed {
				completed = true
				break
			}
		}
		switch {
		case !found:
			missing = append(missing, want)
		case spec.RequireSuccess && !completed:
			incomplete = append(incomplete, want)
		}
	}
	var why []string
	if len(missing) > 0 {
		why = append(why, "never called "+quoteList(missing))
	}
	if len(incomplete) > 0 {
		why = append(why, "called but never completed "+quoteList(incomplete))
	}
	if len(why) == 0 {
		return true, "called " + quoteList(spec.ToolNames)
	}
	return false, strings.Join(why, "; ")
}

// verifyCluster reads the cluster. The one verifier here that does not
// route through the agent's account of itself.
func (v *Verifier) verifyCluster(ctx context.Context, ck *Check) (passed, vacuous bool, detail string, err error) {
	spec := ck.Spec.ClusterResourceProperty
	role, ok := v.corpus.RoleFor(spec.FixtureRole)
	if !ok {
		return false, false, "", fmt.Errorf("role %q is not in the catalog", spec.FixtureRole)
	}

	if spec.ChangedCountEq != nil {
		return v.verifyChangedCount(ctx, spec, role)
	}

	read := func(ctx context.Context) (bool, bool, string, error) {
		return v.readOnce(ctx, spec, role)
	}

	switch {
	case spec.StableFor != "":
		return v.verifyStable(ctx, spec, role)
	case ck.Mode == ModeConverge:
		return v.converge(ctx, read)
	default:
		return read(ctx)
	}
}

// readOnce evaluates a property or existence assertion against the
// cluster as it is right now.
func (v *Verifier) readOnce(ctx context.Context, spec *ClusterResourceProperty, role Role) (passed, vacuous bool, detail string, err error) {
	names, err := v.matched(ctx, spec, role)
	if err != nil {
		return false, false, "", err
	}
	addr := address(spec)

	if spec.Op.pathless() {
		// An existence assertion. The empty set is the measurement here,
		// not an absence of one — probes-before-run is what earns the
		// right to read it that way.
		switch spec.Op {
		case OpAbsent:
			if len(names) == 0 {
				return true, false, addr + " is absent", nil
			}
			return false, false, addr + " is present: " + quoteList(names), nil
		default:
			if len(names) > 0 {
				return true, false, addr + " is present: " + quoteList(names), nil
			}
			return false, false, addr + " is absent", nil
		}
	}

	if len(names) == 0 {
		// A property assertion over nothing. Vacuous whichever way it
		// falls out, and it falls out as a pass under `ne`.
		return false, true, fmt.Sprintf("%s matched no objects, so %s %s asserted nothing",
			addr, spec.Op, formatValue(spec.Value)), nil
	}

	var failures []string
	resolved := 0
	for _, name := range names {
		raw, found, err := v.prov.cluster.JSONPath(ctx, role.Namespace, spec.Kind, name, jsonpathExpr(spec.Path))
		if err != nil {
			return false, false, "", fmt.Errorf("read %s/%s %s: %w", spec.Kind, name, spec.Path, err)
		}
		got := strings.TrimSpace(raw)
		if !found || got == "" {
			// Counted, not failed. Whether this is a defect depends on
			// every other matched object, so the judgement is deferred
			// until the loop ends.
			failures = append(failures, fmt.Sprintf("%s: %s does not resolve", name, spec.Path))
			continue
		}
		resolved++
		ok, cmpErr := compare(spec.Op, got, spec.Value)
		if cmpErr != nil {
			return false, false, "", fmt.Errorf("%s: %w", name, cmpErr)
		}
		if !ok {
			failures = append(failures, fmt.Sprintf("%s: %s is %q, want %s %s",
				name, spec.Path, got, spec.Op, formatValue(spec.Value)))
		}
	}

	if resolved == 0 {
		// The path is wrong, or the objects are not the shape the case
		// thinks they are. Either way the check read nothing.
		return false, true, fmt.Sprintf("%s resolves on none of the %d matched object(s): %s",
			spec.Path, len(names), quoteList(names)), nil
	}
	if len(failures) > 0 {
		return false, false, strings.Join(failures, "; "), nil
	}
	return true, false, fmt.Sprintf("%s %s %s on all %d matched object(s)",
		spec.Path, spec.Op, formatValue(spec.Value), len(names)), nil
}

// verifyStable samples across the settle window and requires every
// sample to hold. On restartCount this is the difference between "the
// apply was accepted" and "the incident stopped".
func (v *Verifier) verifyStable(ctx context.Context, spec *ClusterResourceProperty, role Role) (passed, vacuous bool, detail string, err error) {
	window, err := time.ParseDuration(spec.StableFor)
	if err != nil {
		return false, false, "", fmt.Errorf("stable_for %q: %w", spec.StableFor, err)
	}
	deadline := time.Now().Add(window)
	samples := 0
	for {
		ok, vac, why, err := v.readOnce(ctx, spec, role)
		if err != nil {
			return false, false, "", err
		}
		samples++
		if vac {
			return false, true, fmt.Sprintf("sample %d of the %s window: %s", samples, spec.StableFor, why), nil
		}
		if !ok {
			return false, false, fmt.Sprintf("broke %s into the window (sample %d): %s",
				time.Since(deadline.Add(-window)).Round(time.Second), samples, why), nil
		}
		if !time.Now().Before(deadline) {
			return true, false, fmt.Sprintf("held across %d sample(s) over %s: %s", samples, spec.StableFor, why), nil
		}
		if err := sleep(ctx, v.interval()); err != nil {
			return false, false, "", err
		}
	}
}

// converge polls until the check passes or the window runs out. Only
// legal on a cluster read: a transcript is immutable once the run ends,
// so polling one only waits out the timeout.
func (v *Verifier) converge(ctx context.Context, read func(context.Context) (bool, bool, string, error)) (passed, vacuous bool, detail string, err error) {
	window := v.window()
	deadline := time.Now().Add(window)
	attempts := 0
	var lastWhy string
	var lastVacuous bool
	for {
		ok, vac, why, err := read(ctx)
		if err != nil {
			return false, false, "", err
		}
		attempts++
		lastWhy, lastVacuous = why, vac
		if ok {
			if attempts == 1 {
				return true, vac, why, nil
			}
			return true, vac, fmt.Sprintf("converged after %d read(s): %s", attempts, why), nil
		}
		if !time.Now().Before(deadline) {
			return false, lastVacuous, fmt.Sprintf("did not converge within %s (%d read(s)): %s",
				window, attempts, lastWhy), nil
		}
		if err := sleep(ctx, v.interval()); err != nil {
			return false, false, "", err
		}
	}
}

// verifyChangedCount is the blast-radius ceiling. Never vacuous: it
// counts over a snapshot this package took itself, and [Changed] refuses
// rather than under-count when a subject has no generation to compare.
func (v *Verifier) verifyChangedCount(ctx context.Context, spec *ClusterResourceProperty, role Role) (passed, vacuous bool, detail string, err error) {
	after, err := v.prov.Snapshot(ctx)
	if err != nil {
		return false, false, "", err
	}
	changed, err := Changed(v.before, after)
	if err != nil {
		return false, false, "", err
	}

	// Changed spans the whole snapshot; the ceiling is over this check's
	// matched set. Filter by the key shape the snapshot writes.
	names, err := v.matched(ctx, spec, role)
	if err != nil {
		return false, false, "", err
	}
	inSet := make(map[string]bool, len(names))
	for _, n := range names {
		inSet[snapshotKey(spec.FixtureRole, spec.Kind, n)] = true
	}
	// A subject that vanished is not in the matched set any more and is
	// still a change the ceiling has to see, so the prefix is what
	// decides membership for anything the "before" knew about.
	prefix := snapshotKey(spec.FixtureRole, spec.Kind, "")
	var hits []string
	for _, key := range changed {
		if inSet[key] || strings.HasPrefix(key, prefix) {
			hits = append(hits, key)
		}
	}
	sort.Strings(hits)

	want := *spec.ChangedCountEq
	if len(hits) == want {
		return true, false, fmt.Sprintf("%d changed object(s) under %s, ceiling %d",
			len(hits), address(spec), want), nil
	}
	return false, false, fmt.Sprintf("%d changed object(s) under %s, ceiling %d: %s",
		len(hits), address(spec), want, quoteList(hits)), nil
}

// matched resolves a check's address to the object names it currently
// covers.
func (v *Verifier) matched(ctx context.Context, spec *ClusterResourceProperty, role Role) ([]string, error) {
	if spec.ResourceName != "" {
		// Existence is part of the answer here, so a missing object is
		// an empty set rather than an error.
		_, found, err := v.prov.cluster.JSONPath(ctx, role.Namespace, spec.Kind, spec.ResourceName, "{.metadata.name}")
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, nil
		}
		return []string{spec.ResourceName}, nil
	}
	// Same resolution the snapshot used, so a ceiling counts over the
	// set its "before" was read from.
	return v.prov.subjects(ctx, role, Probe{Kind: strings.ToLower(spec.Kind), Selector: spec.Selector})
}

// compare applies an op to a resolved jsonpath value.
func compare(op Op, got string, want any) (bool, error) {
	if op.ordered() {
		g, err := strconv.ParseFloat(got, 64)
		if err != nil {
			return false, fmt.Errorf("op %s compares magnitudes and the cluster returned %q", op, got)
		}
		w, err := toFloat(want)
		if err != nil {
			return false, err
		}
		if op == OpGte {
			return g >= w, nil
		}
		return g <= w, nil
	}
	same := got == formatValue(want)
	if op == OpNe {
		return !same, nil
	}
	return same, nil
}

func toFloat(v any) (float64, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	}
	return 0, fmt.Errorf("value %v is not a number", v)
}

// formatValue renders a YAML scalar the way kubectl's jsonpath renders
// the field it will be compared against. Bools and ints both come back
// from jsonpath as their bare text.
func formatValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}

// address renders what a check points at, for a detail line.
func address(spec *ClusterResourceProperty) string {
	switch {
	case spec.ResourceName != "":
		return fmt.Sprintf("%s/%s", spec.Kind, spec.ResourceName)
	case spec.Selector != "":
		return fmt.Sprintf("%s?%s", spec.Kind, spec.Selector)
	}
	return fmt.Sprintf("every %s in %s", spec.Kind, spec.FixtureRole)
}

// jsonpathExpr turns a corpus path into a kubectl jsonpath template.
//
// A case writes the path the way a reader would say it out loud —
// `spec.template.spec.containers[?(@.name=='api')].resources.limits.memory`
// — because that is what makes a red cell legible. kubectl wants it
// braced and rooted. An already-braced path is passed through, so a case
// that needs a `{range}` construct is not shut out.
func jsonpathExpr(path string) string {
	p := strings.TrimSpace(path)
	if strings.HasPrefix(p, "{") {
		return p
	}
	return "{." + strings.TrimPrefix(p, ".") + "}"
}

func snapshotKey(role, kind, name string) string {
	return fmt.Sprintf("%s/%s/%s", role, strings.ToLower(kind), name)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func lines(out string) []string {
	var names []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if s := strings.TrimSpace(l); s != "" {
			names = append(names, s)
		}
	}
	return names
}

func quoteList(items []string) string {
	if len(items) == 0 {
		return "nothing"
	}
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = strconv.Quote(s)
	}
	return strings.Join(q, ", ")
}
