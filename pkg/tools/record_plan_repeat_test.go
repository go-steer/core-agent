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

// The repeat guard (#906). Live on GKE 2.9.0-dev.4 a model in
// completion-reporting mode called record_plan eight times in one turn,
// seven of them consecutively, and each call ran nextPlanSeq and wrote
// another file: plan-5 through plan-11, eight "Plan recorded" successes,
// and the same "Now unblocked for this session" announcement re-read
// seven times after the gate had already opened.
//
// Every test in this file holds ONE handler across its calls, because
// that is the deployed shape — tools.Build runs once per process and the
// resulting tool serves every turn of every session. Helpers that build
// a fresh handler per call (invokeRecordPlan, recordPlanAs) cannot see
// this guard at all.

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// planDriver drives one long-lived record_plan handler the way a daemon
// does: many calls, several authors, several turns.
type planDriver struct {
	t   *testing.T
	fn  functiontool.Func[recordPlanArgs, recordPlanResult]
	dir string
}

func newPlanDriver(t *testing.T, gate *permissions.Gate) *planDriver {
	t.Helper()
	dir := t.TempDir()
	return &planDriver{t: t, fn: recordPlanFunc(gate, dir), dir: dir}
}

// try runs one record_plan as (agent, session) inside turn `invocation`
// and returns the error rather than failing the test, so it is safe to
// call from a goroutine (t.Fatalf from a non-test goroutine is UB).
func (d *planDriver) try(agent, session, invocation, plan string) (recordPlanResult, error) {
	ctx := &planToolCtx{
		Context:    context.Background(),
		agent:      agent,
		session:    session,
		invocation: invocation,
	}
	res, err := d.fn(ctx, recordPlanArgs{Plan: plan})
	if err != nil {
		return res, fmt.Errorf("record_plan(%s/%s, turn %s): %w", agent, session, invocation, err)
	}
	return res, nil
}

// call is try for the test goroutine, where a failure should stop.
func (d *planDriver) call(agent, session, invocation, plan string) recordPlanResult {
	d.t.Helper()
	res, err := d.try(agent, session, invocation, plan)
	if err != nil {
		d.t.Fatal(err)
	}
	return res
}

// planFiles lists the plan artifacts on disk, revoked ones included —
// the count is the whole assertion in most of these tests.
func (d *planDriver) planFiles() []string {
	d.t.Helper()
	entries, err := os.ReadDir(filepath.Join(d.dir, recordPlanDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		d.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if recordPlanFilenameRegex.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names
}

func armedGate() *permissions.Gate {
	return permissions.New(permissions.Options{Mode: permissions.ModeYolo, RequirePlanArtifact: true})
}

// The acceptance criterion, in the shape the live session produced:
// eight calls in one turn, each with a reworded plan (which is what
// defeated loop detection — distinct args every time). One file.
func TestRecordPlan_LoopInOneTurnMintsExactlyOnePlanFile(t *testing.T) {
	t.Parallel()
	d := newPlanDriver(t, armedGate())

	var results []recordPlanResult
	for i := 1; i <= 8; i++ {
		results = append(results, d.call("core_agent", "s1", "inv-1",
			fmt.Sprintf("## Goal\nTriage the incident.\n\nRevision %d.", i)))
	}

	if files := d.planFiles(); len(files) != 1 {
		t.Fatalf("eight record_plan calls in one turn produced %d plan files (%v), want 1", len(files), files)
	}
	first := results[0]
	if first.Outcome != planOutcomeRecorded || first.Sequence != 1 {
		t.Errorf("first call = outcome %q seq %d, want recorded/1", first.Outcome, first.Sequence)
	}
	for i, res := range results[1:] {
		if res.Outcome != planOutcomeUpdated {
			t.Errorf("call %d: outcome %q, want %q", i+2, res.Outcome, planOutcomeUpdated)
		}
		if res.Path != first.Path || res.Sequence != first.Sequence {
			t.Errorf("call %d: wrote %s (seq %d), want the first plan %s (seq %d)",
				i+2, res.Path, res.Sequence, first.Path, first.Sequence)
		}
	}
	// The surviving artifact is the LAST revision, not the first: an
	// in-place update that kept stale content would be a different bug.
	body, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Revision 8.") {
		t.Errorf("artifact does not hold the final revision:\n%s", body)
	}
	if strings.Contains(string(body), "Revision 7.") {
		t.Errorf("artifact appended instead of overwriting:\n%s", body)
	}
}

// An identical plan is not a revision. Nothing is written at all, and
// the model is told so rather than handed another success.
func TestRecordPlan_IdenticalPlanWritesNothing(t *testing.T) {
	t.Parallel()
	d := newPlanDriver(t, armedGate())
	const plan = "## Goal\nRestart the deployment."

	first := d.call("core_agent", "s1", "inv-1", plan)
	// A sentinel proves the second call did not rewrite the file: if it
	// had, the artifact would be back to the rendered plan.
	if err := os.WriteFile(first.Path, []byte("sentinel\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repeat := d.call("core_agent", "s1", "inv-1", plan)

	if repeat.Outcome != planOutcomeUnchanged {
		t.Errorf("repeat outcome = %q, want %q", repeat.Outcome, planOutcomeUnchanged)
	}
	if repeat.Path != first.Path {
		t.Errorf("repeat path = %s, want the existing %s", repeat.Path, first.Path)
	}
	if files := d.planFiles(); len(files) != 1 {
		t.Errorf("plan files = %v, want just the one", files)
	}
	body, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "sentinel\n" {
		t.Errorf("unchanged plan rewrote the artifact:\n%s", body)
	}
	if strings.Contains(repeat.Message, "Plan recorded at") {
		t.Errorf("a no-op still reports a fresh recording: %q", repeat.Message)
	}
	if !strings.Contains(repeat.Message, "No plan file was written") {
		t.Errorf("message does not say nothing was written: %q", repeat.Message)
	}
}

// Whitespace-only differences are the same plan: the handler trims and
// newline-normalises before comparing, so a model that re-sends its plan
// with a trailing blank line does not get a second artifact.
func TestRecordPlan_WhitespaceOnlyDifferenceIsUnchanged(t *testing.T) {
	t.Parallel()
	d := newPlanDriver(t, armedGate())

	d.call("core_agent", "s1", "inv-1", "## Goal\nShip it.")
	repeat := d.call("core_agent", "s1", "inv-1", "  ## Goal\nShip it.\n\n  ")

	if repeat.Outcome != planOutcomeUnchanged {
		t.Errorf("outcome = %q, want %q", repeat.Outcome, planOutcomeUnchanged)
	}
}

// A later turn with a genuinely different plan still files a new
// artifact — the guard is a repeat guard, not a one-plan-per-session
// rule. The audit trail across turns is the point of the directory.
func TestRecordPlan_NewTurnWithANewPlanStillIncrementsSeq(t *testing.T) {
	t.Parallel()
	d := newPlanDriver(t, armedGate())

	first := d.call("core_agent", "s1", "inv-1", "## Goal\nTriage the incident.")
	second := d.call("core_agent", "s1", "inv-2", "## Goal\nRoll the deployment back.")
	third := d.call("core_agent", "s1", "inv-3", "## Goal\nWrite the postmortem.")

	for i, res := range []recordPlanResult{first, second, third} {
		if res.Sequence != i+1 || res.Outcome != planOutcomeRecorded {
			t.Errorf("turn %d: seq %d outcome %q, want %d/recorded", i+1, res.Sequence, res.Outcome, i+1)
		}
	}
	if files := d.planFiles(); len(files) != 3 {
		t.Errorf("three turns, three plans: got %v", files)
	}
}

// /replan archives the artifact and clears the gate. The next
// record_plan must mint a fresh file — resurrecting the revoked
// sequence number would put a live plan back at a path the operator
// already retired.
func TestRecordPlan_AfterReplanTheRedraftGetsANewFile(t *testing.T) {
	t.Parallel()
	gate := armedGate()
	d := newPlanDriver(t, gate)
	const plan = "## Goal\nDrain the node."

	first := d.call("core_agent", "s1", "inv-1", plan)
	if _, err := RevokePlanBy(gate, d.dir, PlanOwner{Agent: "core_agent", Session: "s1"}); err != nil {
		t.Fatal(err)
	}
	// Same turn, same text: the only thing that changed is the operator
	// took the plan away.
	redraft := d.call("core_agent", "s1", "inv-1", plan)

	if redraft.Outcome != planOutcomeRecorded {
		t.Errorf("post-/replan outcome = %q, want %q", redraft.Outcome, planOutcomeRecorded)
	}
	if redraft.Path == first.Path {
		t.Errorf("redraft reused the revoked path %s", redraft.Path)
	}
	if _, err := os.Stat(filepath.Join(d.dir, recordPlanDir, "plan-1-revoked.md")); err != nil {
		t.Errorf("the archived plan was disturbed: %v", err)
	}
}

// The #747 shape: a parent and its declarative subagent plan inside one
// incident. They are different authors, so they get different files even
// when the turn is the same — collapsing them would make the parent's
// delegation plan and the specialist's investigation notes one artifact.
//
// Note what this does NOT claim. The synchronous subagent door derives a
// session ID per delegation (pkg/agent/subagent.go), so the same
// subagent delegated twice is two authors and files two plans. The unit
// the guard collapses repeats within is one delegated run, which is the
// unit a loop happens inside; it is not a cap on plans per incident.
func TestRecordPlan_ParentAndSubagentKeepSeparatePlansInOneTurn(t *testing.T) {
	t.Parallel()
	d := newPlanDriver(t, armedGate())

	parent := d.call("core_agent", "s1", "inv-1", "## Goal\nDelegate to the cluster specialist.")
	sub := d.call("cluster", "s1", "inv-1", "## Goal\nRead the pod events.")
	tenant := d.call("core_agent", "s2", "inv-1", "## Goal\nSomeone else's incident entirely.")

	seqs := map[int]bool{parent.Sequence: true, sub.Sequence: true, tenant.Sequence: true}
	if len(seqs) != 3 {
		t.Errorf("authors collapsed onto one plan: %d/%d/%d", parent.Sequence, sub.Sequence, tenant.Sequence)
	}
	if files := d.planFiles(); len(files) != 3 {
		t.Errorf("plan files = %v, want three", files)
	}
}

// The unblock list is an announcement of a state change, so it may only
// appear when the state changed. Seven re-reads of "Now unblocked for
// this session: ..." was the loop congratulating itself.
func TestRecordPlanMessage_UnblockListOnlyOnTheGateTransition(t *testing.T) {
	t.Parallel()
	gate := armedGate()
	gate.RegisterPlanGatedTools("mcp", "spawn_agent")
	d := newPlanDriver(t, gate)

	first := d.call("core_agent", "s1", "inv-1", "## Goal\nTriage.")
	if !strings.Contains(first.Message, "Now unblocked for this session: mcp, spawn_agent") {
		t.Errorf("the call that opened the gate did not announce it: %q", first.Message)
	}

	repeat := d.call("core_agent", "s1", "inv-1", "## Goal\nTriage, reworded.")
	nextTurn := d.call("core_agent", "s1", "inv-2", "## Goal\nA genuinely different plan.")
	for name, msg := range map[string]string{"repeat": repeat.Message, "next turn": nextTurn.Message} {
		if strings.Contains(msg, "Now unblocked for this session") {
			t.Errorf("%s re-announces a transition that did not happen: %q", name, msg)
		}
		if !strings.Contains(msg, "already satisfied") {
			t.Errorf("%s does not say the gate was already open: %q", name, msg)
		}
	}
}

// Per-session sub-gates are what the checks actually consult (#214), so
// the transition has to be read there too: a session recording its first
// plan gets the announcement even though the process-wide template gate
// was flipped by someone else's session long ago.
func TestRecordPlanMessage_TransitionIsReadOnTheSessionGate(t *testing.T) {
	t.Parallel()
	template := armedGate()
	template.RegisterPlanGatedTools("mcp")
	template.MarkPlanRecorded() // another session already planned
	dir := t.TempDir()
	fn := recordPlanFunc(template, dir)

	session := template.DeriveForSession("s-new", nil)
	ctx := &planToolCtx{
		Context:    permissions.WithSessionGate(context.Background(), session),
		agent:      "core_agent",
		session:    "s-new",
		invocation: "inv-1",
	}
	res, err := fn(ctx, recordPlanArgs{Plan: "## Goal\nMy own first plan."})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Message, "Now unblocked for this session: mcp") {
		t.Errorf("a session opening its own gate was told nothing changed: %q", res.Message)
	}
}

// A handler driven without an invocation context — library callers and
// the pre-#906 unit tests — has no turn boundary to read. Treat it as
// one turn rather than as permission to mint unbounded files.
func TestRecordPlan_NoInvocationContextStillGuardsRepeats(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fn := recordPlanFunc(armedGate(), dir)

	for i := 1; i <= 5; i++ {
		if _, err := fn(tool.Context(nil), recordPlanArgs{Plan: fmt.Sprintf("plan rev %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, recordPlanDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("five context-free calls produced %d artifacts, want 1", len(entries))
	}
}

// Concurrent authors race nextPlanSeq-then-write over one directory.
// Each must come away with its own sequence number; before #906 the
// read-modify-write was unsynchronised and two sessions could compute
// the same seq and clobber each other. Run under -race.
func TestRecordPlan_ConcurrentAuthorsGetDistinctSequences(t *testing.T) {
	t.Parallel()
	d := newPlanDriver(t, armedGate())

	const authors = 16
	var wg sync.WaitGroup
	seqs := make([]int, authors)
	errs := make([]error, authors)
	for i := range authors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := d.try("core_agent", fmt.Sprintf("tenant-%d", i), "inv-1", fmt.Sprintf("plan for %d", i))
			seqs[i], errs[i] = res.Sequence, err
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	seen := map[int]bool{}
	for _, s := range seqs {
		if seen[s] {
			t.Errorf("sequence %d handed to two authors: %v", s, seqs)
		}
		seen[s] = true
	}
	if files := d.planFiles(); len(files) != authors {
		t.Errorf("%d concurrent authors produced %d files: %v", authors, len(files), files)
	}
}

// The memory is bounded, so a daemon with more live authors than
// planTurnMemoryLimit degrades to the pre-#906 behavior for the evicted
// ones — a needless plan file, never a wrong one. Stated as a test so
// the degradation is a decision rather than a surprise.
func TestRecordPlan_MemoryEvictsLeastRecentlyUsedAuthor(t *testing.T) {
	t.Parallel()
	d := newPlanDriver(t, armedGate())
	const plan = "## Goal\nThe same plan throughout."

	first := d.call("core_agent", "tenant-0", "inv-1", plan)
	for i := 1; i <= planTurnMemoryLimit; i++ {
		d.call("core_agent", fmt.Sprintf("tenant-%d", i), "inv-1", plan)
	}
	evicted := d.call("core_agent", "tenant-0", "inv-1", plan)

	if evicted.Outcome != planOutcomeRecorded {
		t.Errorf("outcome = %q, want %q once the author was evicted", evicted.Outcome, planOutcomeRecorded)
	}
	if evicted.Path == first.Path {
		t.Errorf("evicted author overwrote its old artifact at %s", evicted.Path)
	}
}

// Eviction is least-recently-*used*, not insertion-ordered, and the
// difference decides whether the guard works at all in the deployment it
// was written for. A synchronous subagent gets a session ID derived per
// delegation, so a busy parent produces a stream of single-use keys.
// Under insertion order the primary session — inserted once, at the
// start, then only ever re-read — is the FIRST thing that churn evicts,
// and the looping agent this whole change exists to stop would drop
// straight back to a new file per call.
func TestRecordPlan_MemoryKeepsTheBusyAuthorThroughChurn(t *testing.T) {
	t.Parallel()
	d := newPlanDriver(t, armedGate())
	const plan = "## Goal\nThe primary session's plan."

	primary := d.call("core_agent", "primary", "inv-1", plan)
	// Twice the table's capacity in one-shot delegations, with the
	// primary session touching its plan in between each.
	for i := range planTurnMemoryLimit * 2 {
		d.call("cluster", fmt.Sprintf("primary/cluster/fc-%d", i), "inv-1", "## Goal\nOne delegated look.")
		if got := d.call("core_agent", "primary", "inv-1", plan); got.Outcome != planOutcomeUnchanged {
			t.Fatalf("after %d delegations the primary session's repeat = %q, want %q",
				i+1, got.Outcome, planOutcomeUnchanged)
		}
	}
	if got := d.call("core_agent", "primary", "inv-1", plan); got.Path != primary.Path {
		t.Errorf("primary session was evicted: repeat wrote %s, want %s", got.Path, primary.Path)
	}
}

// An operator who deletes plan-N.md by hand has withdrawn the artifact
// as surely as /replan does, so the next call has to file a real plan
// rather than reporting "unchanged" against a path with nothing at it.
// This is why the guard stats the remembered path instead of trusting
// its own digest.
func TestRecordPlan_DeletedArtifactIsRecordedAgain(t *testing.T) {
	t.Parallel()
	d := newPlanDriver(t, armedGate())
	const plan = "## Goal\nRotate the credentials."

	first := d.call("core_agent", "s1", "inv-1", plan)
	if err := os.Remove(first.Path); err != nil {
		t.Fatal(err)
	}
	again := d.call("core_agent", "s1", "inv-1", plan)

	if again.Outcome != planOutcomeRecorded {
		t.Errorf("outcome = %q, want %q after the artifact was deleted", again.Outcome, planOutcomeRecorded)
	}
	if _, err := os.Stat(again.Path); err != nil {
		t.Errorf("no artifact on disk after the redraft: %v", err)
	}
}

// Advisory mode has no gate to open, so the repeat path must be the
// behaviour on its own — the guard is about the plans directory, not
// about permissions, and a recipe running plan_mode: advisory gets the
// same loop protection with none of the unblock prose.
func TestRecordPlan_AdvisoryModeStillCollapsesRepeats(t *testing.T) {
	t.Parallel()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	d := newPlanDriver(t, gate)
	const plan = "## Goal\nAdvisory plan."

	first := d.call("core_agent", "s1", "inv-1", plan)
	repeat := d.call("core_agent", "s1", "inv-1", plan)

	if repeat.Outcome != planOutcomeUnchanged {
		t.Errorf("outcome = %q, want %q", repeat.Outcome, planOutcomeUnchanged)
	}
	if files := d.planFiles(); len(files) != 1 {
		t.Errorf("plan files = %v, want just the one", files)
	}
	for _, msg := range []string{first.Message, repeat.Message} {
		if strings.Contains(msg, "unblocked") || strings.Contains(msg, "already satisfied") {
			t.Errorf("advisory mode claims a gate effect: %q", msg)
		}
	}
}
