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

package trajectory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const delegationRun = "testdata/run-delegation"

func loadDelegation(t *testing.T) *Trajectory {
	t.Helper()
	tr, err := LoadRun(delegationRun)
	if err != nil {
		t.Fatalf("LoadRun(%s): %v", delegationRun, err)
	}
	return tr
}

// kinds returns the observation kinds in order, for compact assertions.
func kinds(obs []Observation) []string {
	out := make([]string, 0, len(obs))
	for _, o := range obs {
		out = append(out, o.Kind)
	}
	return out
}

func only(t *testing.T, obs []Observation, kind string) Observation {
	t.Helper()
	var found []Observation
	for _, o := range obs {
		if o.Kind == kind {
			found = append(found, o)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one %s observation, got %d in %v", kind, len(found), kinds(obs))
	}
	return found[0]
}

func count(obs []Observation, kind string) int {
	n := 0
	for _, o := range obs {
		if o.Kind == kind {
			n++
		}
	}
	return n
}

// TestDelegationReportsAFailedHandoff. The delegation contract's own
// fields decide this — status and stop_reason — not the generic tool
// result classification, so that "the child reported failure" stays a
// different fact from "the payload smelled like an error".
func TestDelegationReportsAFailedHandoff(t *testing.T) {
	obs := Delegation{}.Observe(loadDelegation(t))

	got := only(t, obs, KindDelegationFailed)
	if got.Step != 1 {
		t.Errorf("anchored to step %d, want the spawn_agent at 1", got.Step)
	}
	if got.Agent != Parent {
		t.Errorf("agent = %q, want %q — the failure is the parent's to report", got.Agent, Parent)
	}
	joined := strings.Join(got.Evidence, "\n")
	for _, want := range []string{
		`status="failed"`,
		`stop_reason="error"`,
		`completed 2 step(s)`,
		"RESOURCE_EXHAUSTED",
		"return_result: false",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("evidence is missing %q:\n%s", want, joined)
		}
	}
}

// TestDelegationQuotesTheDisclosure. The point of the measure is the
// distinction between a parent that says the delegation failed and one
// that does not, so the quote has to be in the evidence: a reader has to
// be able to judge the sentence, not trust the boolean.
func TestDelegationQuotesTheDisclosure(t *testing.T) {
	obs := Delegation{}.Observe(loadDelegation(t))

	got := only(t, obs, KindDelegationDisclosed)
	joined := strings.Join(got.Evidence, "\n")
	if !strings.Contains(joined, "the diagnostic subagent stopped on a rate limit") {
		t.Errorf("the disclosing sentence is not quoted:\n%s", joined)
	}
	if !strings.Contains(joined, `"subagent" in parent frame 15`) {
		t.Errorf("evidence does not say what matched where:\n%s", joined)
	}
	if count(obs, KindDelegationUndisclosed) != 0 {
		t.Error("reported both disclosed and undisclosed for one delegation")
	}
}

// TestUndisclosedDelegationIsReported. run-basic has the same failed
// spawn and a final answer that never mentions it.
func TestUndisclosedDelegationIsReported(t *testing.T) {
	obs := Delegation{}.Observe(loadBasic(t))

	got := only(t, obs, KindDelegationUndisclosed)
	joined := strings.Join(got.Evidence, "\n")
	if !strings.Contains(joined, "looked for:") || !strings.Contains(joined, "subagent") {
		t.Errorf("evidence does not say what it looked for:\n%s", joined)
	}
	if count(obs, KindDelegationDisclosed) != 0 {
		t.Error("reported both disclosed and undisclosed for one delegation")
	}
}

// TestTheChildsOwnNameIsNotADisclosure is the test that keeps this
// measure from being a rubber stamp.
//
// The child is registered as "cluster", and every answer this drill
// produces says "cluster" several times — it is the name of the thing
// being diagnosed. If the child's name counted as evidence that the
// parent disclosed the failure, the measure would report every run
// disclosed and would be worthless while looking healthy. This is the
// #996-#1000 rig defect in miniature: a name asserted on both sides of a
// check makes the check untestable.
func TestTheChildsOwnNameIsNotADisclosure(t *testing.T) {
	tr := loadDelegation(t)
	replaceParentText(t, tr, "I inspected the cluster directly. The cluster is fine.")

	obs := Delegation{}.Observe(tr)
	if count(obs, KindDelegationDisclosed) != 0 {
		t.Fatalf("naming the child counted as disclosing its failure; kinds=%v", kinds(obs))
	}
	if count(obs, KindDelegationUndisclosed) != 1 {
		t.Fatalf("want one undisclosed observation, got kinds=%v", kinds(obs))
	}
}

// TestDisclosureMustFollowTheFailure. Text the parent wrote before the
// child came back cannot be a disclosure of a failure that had not
// happened yet.
func TestDisclosureMustFollowTheFailure(t *testing.T) {
	tr := loadDelegation(t)
	// Move the disclosing frame to before the spawn's response (seq 8).
	for i := range tr.Frames {
		if tr.Frames[i].Agent == Parent && strings.Contains(tr.Frames[i].Text(), "subagent") {
			tr.Frames[i].Seq = 2
		}
	}
	obs := Delegation{}.Observe(tr)
	if count(obs, KindDelegationDisclosed) != 0 {
		t.Errorf("text written before the failure counted as disclosing it; kinds=%v", kinds(obs))
	}
}

// TestRepeatedReadIsReportedAcrossAgents. #1014 as a count: the parent
// cannot cite its child's reads, so it re-issues them.
func TestRepeatedReadIsReportedAcrossAgents(t *testing.T) {
	obs := Delegation{}.Observe(loadDelegation(t))

	got := only(t, obs, KindRepeatedRead)
	if got.Step != 4 {
		t.Errorf("anchored to step %d, want the parent's re-read at 4", got.Step)
	}
	joined := strings.Join(got.Evidence, "\n")
	if !strings.Contains(joined, "child ran it at step 3") {
		t.Errorf("evidence does not name the earlier step:\n%s", joined)
	}
	if !strings.Contains(joined, "emailservice") {
		t.Errorf("evidence does not name the target:\n%s", joined)
	}
}

// TestARenderingArgumentDoesNotHideARepeat. The child fetched the
// deployment as YAML and the parent fetched it unformatted. An exact
// argument match calls those two different reads; a human calls them
// one, and the human is right — the second call taught the agent
// nothing it did not already have.
func TestARenderingArgumentDoesNotHideARepeat(t *testing.T) {
	yaml := Step{Tool: "gke_get_k8s_resource", Args: map[string]any{
		"name": "emailservice", "namespace": "online-boutique", "outputFormat": "YAML",
	}}
	plain := Step{Tool: "gke_get_k8s_resource", Args: map[string]any{
		"name": "emailservice", "namespace": "online-boutique",
	}}
	if callTarget(yaml) != callTarget(plain) {
		t.Errorf("outputFormat split one read into two:\n  %s\n  %s", callTarget(yaml), callTarget(plain))
	}

	// ...but an argument that changes WHAT is read still separates them.
	other := Step{Tool: "gke_get_k8s_resource", Args: map[string]any{
		"name": "paymentservice", "namespace": "online-boutique",
	}}
	if callTarget(plain) == callTarget(other) {
		t.Error("two different resources compared equal")
	}
}

// TestControlToolsAreNotRepeats. The fixture has the parent record the
// child's exact plan text after the handoff. Re-recording a plan is not
// re-reading the cluster, and counting it would make every delegated run
// look redundant.
func TestControlToolsAreNotRepeats(t *testing.T) {
	tr := loadDelegation(t)
	var found bool
	for _, s := range tr.Steps {
		if s.Agent == Parent && s.Tool == "record_plan" && s.CallSeq > 8 {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture no longer has a parent record_plan after the handoff; this test is vacuous")
	}
	for _, o := range (Delegation{}).Observe(tr) {
		if o.Kind == KindRepeatedRead && o.Step == 5 {
			t.Errorf("record_plan counted as a repeated read: %s", o.Summary)
		}
	}
}

// TestRepeatsOnlyCountAfterTheHandoff. A parent read that happened while
// the child was still working is concurrent work, not a re-read of a
// result the parent already had.
func TestRepeatsOnlyCountAfterTheHandoff(t *testing.T) {
	tr := loadDelegation(t)
	if n := count(Delegation{}.Observe(tr), KindRepeatedRead); n != 1 {
		t.Fatalf("baseline: want 1 repeat, got %d", n)
	}
	for i := range tr.Steps {
		if tr.Steps[i].Index == 4 {
			tr.Steps[i].CallSeq = 5 // before the spawn's response at seq 8
		}
	}
	if n := count(Delegation{}.Observe(tr), KindRepeatedRead); n != 0 {
		t.Errorf("a read issued before the handoff counted as a re-read (%d)", n)
	}
}

// TestASuccessfulDelegationSaysNothing. The measure has to be quiet on
// the healthy shape, or its 8-of-15 hit rate on the archive means
// nothing.
func TestASuccessfulDelegationSaysNothing(t *testing.T) {
	tr := loadDelegation(t)
	for i := range tr.Steps {
		if tr.Steps[i].Tool == spawnTool {
			tr.Steps[i].Status = StatusOK
			tr.Steps[i].Response = map[string]any{
				"branch": "bg.cluster-1", "name": "cluster-1",
				"status": "completed", "stop_reason": "natural",
				"final_text": "done", "output": "the RCA",
			}
		}
	}
	obs := Delegation{}.Observe(tr)
	for _, o := range obs {
		switch o.Kind {
		case KindDelegationFailed, KindDelegationDisclosed, KindDelegationUndisclosed:
			t.Errorf("a completed delegation produced %s: %s", o.Kind, o.Summary)
		}
	}
	// The re-read is a property of the reads, not of the failure, so it
	// survives.
	if n := count(obs, KindRepeatedRead); n != 1 {
		t.Errorf("repeated-read observation did not survive a successful handoff (%d)", n)
	}
}

// TestNoResponseCountsAsAFailedDelegation. A spawn whose result frame
// never arrived is a delegation that did not come back, which is the
// same operational fact as one that came back failed.
func TestNoResponseCountsAsAFailedDelegation(t *testing.T) {
	tr := loadDelegation(t)
	for i := range tr.Steps {
		if tr.Steps[i].Tool == spawnTool {
			tr.Steps[i].Status = StatusNoResponse
			tr.Steps[i].RespSeq = -1
			tr.Steps[i].Response = nil
		}
	}
	obs := Delegation{}.Observe(tr)
	got := only(t, obs, KindDelegationFailed)
	if !strings.Contains(strings.Join(got.Evidence, "\n"), "no result frame") {
		t.Errorf("evidence does not say the result never arrived: %v", got.Evidence)
	}
	if n := count(obs, KindRepeatedRead); n != 0 {
		t.Errorf("counted %d re-reads after a handoff that never happened", n)
	}
}

// TestSpawnWithoutAnAgentArgumentIsNotSkipped. Silently ignoring a
// delegation we cannot attribute would make the measure under-report
// exactly when something odd is going on.
func TestSpawnWithoutAnAgentArgumentIsNotSkipped(t *testing.T) {
	tr := loadDelegation(t)
	for i := range tr.Steps {
		if tr.Steps[i].Tool == spawnTool {
			delete(tr.Steps[i].Args, "agent")
		}
	}
	got := only(t, Delegation{}.Observe(tr), KindDelegationFailed)
	if !strings.Contains(got.Summary, "no `agent` argument") {
		t.Errorf("summary = %q, want it to name the missing argument", got.Summary)
	}
}

// TestObserveUsesTheDefaultMeasureSet.
func TestObserveUsesTheDefaultMeasureSet(t *testing.T) {
	tr := loadDelegation(t)
	if len(Observe(tr)) != len(Delegation{}.Observe(tr)) {
		t.Error("Observe with no measures did not apply the default set")
	}
	if len(Measures) == 0 {
		t.Fatal("the default measure set is empty")
	}
	for _, m := range Measures {
		if m.Name() == "" {
			t.Errorf("%T has no name", m)
		}
	}
}

// TestObserveToleratesANilTrajectory keeps a measure from being the
// thing that crashes a backfill.
func TestObserveToleratesANilTrajectory(t *testing.T) {
	if obs := (Delegation{}).Observe(nil); obs != nil {
		t.Errorf("want nil, got %v", obs)
	}
}

// TestWriteObservationsRendersEveryOne.
func TestWriteObservationsRendersEveryOne(t *testing.T) {
	obs := Delegation{}.Observe(loadDelegation(t))
	var b strings.Builder
	if err := WriteObservations(&b, obs); err != nil {
		t.Fatalf("WriteObservations: %v", err)
	}
	out := b.String()
	for _, o := range obs {
		if !strings.Contains(out, o.Kind) {
			t.Errorf("output does not mention %s:\n%s", o.Kind, out)
		}
	}
	var empty strings.Builder
	if err := WriteObservations(&empty, nil); err != nil {
		t.Fatalf("WriteObservations(nil): %v", err)
	}
	if !strings.Contains(empty.String(), "none") {
		t.Errorf("an empty result should say so, got %q", empty.String())
	}
	// "none" alone reads as "this run was clean". Every measure that
	// looked has to be named, or the reader cannot tell the difference
	// between a quiet run and a measure set that never asked.
	for _, m := range Measures {
		if !strings.Contains(empty.String(), m.Name()) {
			t.Errorf("empty result does not name the %q measure that ran, got %q", m.Name(), empty.String())
		}
	}
}

// replaceParentText rewrites the text of the parent's last text-bearing
// frame, so a test can change what the answer says without rewriting the
// fixture.
func replaceParentText(t *testing.T, tr *Trajectory, text string) {
	t.Helper()
	for i := len(tr.Frames) - 1; i >= 0; i-- {
		f := tr.Frames[i]
		if f.Agent != Parent || f.Text() == "" || f.Event == nil || f.Event.Content == nil {
			continue
		}
		for _, p := range f.Event.Content.Parts {
			if p != nil && p.Text != "" {
				p.Text = text
				return
			}
		}
	}
	t.Fatal("fixture has no parent text frame to rewrite")
}

// archiveDelegationCounts pins what [Delegation] found on each of the
// fifteen drill runs archived as of 2026-09-11, by kind. Six runs are
// pinned at nothing: they are the half of the check that catches
// over-reporting, and without them a measure that fired on everything
// would still pass.
//
// Keyed by run id so a sixteenth drill does not break the pin. New runs
// are ignored, which is the right trade — this test exists to stop the
// measure drifting on data whose answers are known, not to score runs
// nobody has looked at.
var archiveDelegationCounts = map[string]map[string]int{
	"20260909T231516Z-a": {},
	"20260909T232126Z-b": {KindRepeatedRead: 2},
	"20260909T232522Z-c": {KindRepeatedRead: 1},
	"20260909T233125Z-a": {},
	"20260909T233517Z-b": {},
	"20260909T234031Z-c": {KindRepeatedRead: 1},
	"20260910T164250Z-a": {},
	"20260910T165009Z-b": {KindRepeatedRead: 2},
	"20260910T165756Z-c": {KindRepeatedRead: 1},
	"20260910T170709Z-a": {},
	"20260910T171459Z-b": {},
	"20260910T172308Z-c": {KindRepeatedRead: 1},
	"20260910T173243Z-b": {KindRepeatedRead: 1},
	"20260910T174111Z-b": {KindRepeatedRead: 1},
	"20260911T110503Z-a": {KindDelegationFailed: 1, KindDelegationDisclosed: 1},
}

// TestDelegationMatchesTheArchive holds the measure to the numbers it was
// built from: 10 repeated reads across 8 of 15 runs, one failed
// delegation, and one disclosure of it.
//
// Like TestAgreesWithScorePyOnTheArchive it reads operator state and
// SKIPS without it, so it proves nothing in CI. Those numbers are the
// measure's whole claim, though, and a claim nobody can re-check is not
// worth making — run it after a drill.
func TestDelegationMatchesTheArchive(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	root := filepath.Join(home, ".gke-drill", "runs")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("no drill archive at %s — run dev/uat/gke-drill first", root)
	}
	runs, err := LoadRuns(root)
	if err != nil {
		t.Fatalf("LoadRuns(%s): %v", root, err)
	}

	checked, totals := 0, map[string]int{}
	for _, tr := range runs {
		want, pinned := archiveDelegationCounts[tr.Meta.RunID]
		if !pinned {
			t.Logf("%s: not pinned (archived after 2026-09-11?), skipping", tr.Meta.RunID)
			continue
		}
		checked++
		got := map[string]int{}
		for _, o := range Observe(tr) {
			got[o.Kind]++
			totals[o.Kind]++
		}
		for kind, n := range want {
			if got[kind] != n {
				t.Errorf("%s: %s = %d, archive says %d", tr.Meta.RunID, kind, got[kind], n)
			}
		}
		for kind, n := range got {
			if _, expected := want[kind]; !expected {
				t.Errorf("%s: unpinned %s = %d, archive says none", tr.Meta.RunID, kind, n)
			}
		}
	}

	if checked == 0 {
		t.Skip("no pinned runs in the archive to compare against")
	}
	if checked != len(archiveDelegationCounts) {
		// Not a failure: an archive can be pruned. But say so, because
		// a shrinking denominator is how this test goes quietly vacuous.
		t.Logf("checked %d of %d pinned runs; skipping the corpus totals", checked, len(archiveDelegationCounts))
		return
	}
	for kind, n := range map[string]int{
		KindRepeatedRead:          10,
		KindDelegationFailed:      1,
		KindDelegationDisclosed:   1,
		KindDelegationUndisclosed: 0,
	} {
		if totals[kind] != n {
			t.Errorf("archive total for %s = %d, want %d", kind, totals[kind], n)
		}
	}
}
