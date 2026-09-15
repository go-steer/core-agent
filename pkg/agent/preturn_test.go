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

package agent

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
	"github.com/go-steer/core-agent/v2/pkg/usage"
	"github.com/go-steer/core-agent/v2/pkg/watchdog"
)

// The pre-turn pipeline's ordering tests (#659).
//
// Every test below has the same shape, and the shape is the point: run
// the real pipeline and assert the correct behaviour, then run a
// pipeline with exactly two steps transposed and assert the historical
// bug comes back. The second half is what gives the first half teeth —
// an ordering assertion that still passes under a reordering is not an
// ordering assertion, it is a coincidence, and that is precisely the
// state Run was in before this file existed.
//
// One bug per test, named for its issue, so a future failure says which
// incident is about to recur rather than "pipeline broken".

// transpose returns a copy of steps with the two named steps swapped.
//
// It fails the test when a name is missing rather than silently
// returning an unpermuted slice. That matters more than it looks: a
// rename in preturn.go would otherwise turn every test in this file
// into a tautology that runs the correct pipeline twice and passes.
func transpose(t *testing.T, steps []preTurnStep, a, b string) []preTurnStep {
	t.Helper()
	out := append([]preTurnStep(nil), steps...)
	i, j := stepIndex(t, out, a), stepIndex(t, out, b)
	out[i], out[j] = out[j], out[i]
	return out
}

func stepIndex(t *testing.T, steps []preTurnStep, name string) int {
	t.Helper()
	for i, s := range steps {
		if s.name == name {
			return i
		}
	}
	t.Fatalf("no pre-turn step named %q — it was renamed or removed, and this test is no longer testing anything", name)
	return -1
}

// preTurnAgent is a fully-wired agent over an in-memory session
// service: the pipeline touches the session service, the inbox and the
// summarizer, none of which are nil in production.
func preTurnAgent(t *testing.T, opts ...Option) *Agent {
	t.Helper()
	base := []Option{
		WithSessionService(session.InMemoryService()),
		WithSession("preturn-user", "preturn-sid"),
	}
	a, err := New(&recordingLLM{}, append(base, opts...)...)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}

// runPrep runs the given pipeline over a fresh turnPrep and hands back
// both, so a test can assert on the error and on the prompt it built.
func runPrep(t *testing.T, a *Agent, steps []preTurnStep, prompt string) (*turnPrep, error) {
	t.Helper()
	tp := &turnPrep{ctx: context.Background(), prompt: prompt}
	return tp, a.runPreTurn(tp, steps)
}

// alertingManager is a SubagentManager whose only behaviour is the one
// prepend the pipeline asks it for.
type alertingManager struct {
	pendingAlertManager
	alert string
}

func (m *alertingManager) PrependPendingAlerts(p string) string {
	if m.alert == "" {
		return p
	}
	if p == "" {
		return m.alert
	}
	return m.alert + "\n\n" + p
}

// turnCountingWatchdog is a watchdog that implements TurnObserver, so
// the turn-boundary step is observable.
type turnCountingWatchdog struct {
	fakeWatchdog
	starts int
}

func (w *turnCountingWatchdog) ObserveTurnStart() { w.starts++ }

// --- the list itself -------------------------------------------------

// The permutation helper addresses steps by name, and every test in
// this file names its `enforces` claim as the reason it exists. Both
// properties are load-bearing enough to assert.
func TestPreTurnSteps_EveryStepIsNamedUniquelyAndSaysWhatItsPositionBuys(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for i, s := range preTurnSteps {
		if s.name == "" {
			t.Errorf("step %d has no name; the permutation tests address steps by name", i)
		}
		if seen[s.name] {
			t.Errorf("duplicate step name %q — transpose() cannot address it unambiguously", s.name)
		}
		seen[s.name] = true
		if strings.TrimSpace(s.enforces) == "" {
			t.Errorf("step %q does not say what its position enforces; if it genuinely has no ordering constraint, say so", s.name)
		}
		if s.run == nil {
			t.Errorf("step %q has no run func", s.name)
		}
	}
}

func TestRunPreTurn_StopsAtTheFirstError(t *testing.T) {
	t.Parallel()
	a := preTurnAgent(t)
	var ran []string
	boom := &costCeilingError{reason: "refused"}
	steps := []preTurnStep{
		{name: "a", enforces: "x", run: func(*Agent, *turnPrep) error { ran = append(ran, "a"); return nil }},
		{name: "b", enforces: "x", run: func(*Agent, *turnPrep) error { ran = append(ran, "b"); return boom }},
		{name: "c", enforces: "x", run: func(*Agent, *turnPrep) error { ran = append(ran, "c"); return nil }},
	}
	if _, err := runPrep(t, a, steps, ""); err != boom {
		t.Fatalf("err = %v, want the refusal from step b", err)
	}
	if got := strings.Join(ran, ","); got != "a,b" {
		t.Errorf("ran %q; a refused turn must not keep running steps", got)
	}
}

// --- #362 / #144: the settle pass and the snapshot ------------------

// settleAgent is the #362 setup: a per-turn ceiling, a prior turn whose
// baseline was snapshotted at $0, and the harness's main-model append
// for that turn landing AFTER its post-turn hook — which is why the
// hook's delta missed it and this turn's pre-turn pass has to catch it.
func settleAgent(t *testing.T, turnUSD float64) (*Agent, *terminalRecorder) {
	t.Helper()
	tr := usage.NewTracker()
	a := preTurnAgent(t, WithUsageTracker(tr), WithCostCeiling(CostCeiling{MaxTurnUSD: 0.10}))
	var rec terminalRecorder
	rec.attachTo(a)
	a.snapshotTurnStartCost() // the prior turn's baseline: $0
	// The late harness append. Pricing is per-million tokens.
	tr.Append("harness", int(turnUSD*10_000_000), 0, usage.Pricing{InputPerMTok: 0.10})
	return a, &rec
}

// TestPreTurn_362_TheSettlePassMustRunBeforeTheSnapshot is the ordering
// half of #362. snapshot-turn-start-cost overwrites the very baseline
// settle-cost-ceiling subtracts from, so transposing the two does not
// make the check wrong — it makes it read zero, forever, silently.
func TestPreTurn_362_TheSettlePassMustRunBeforeTheSnapshot(t *testing.T) {
	t.Parallel()

	t.Run("in order: the prior turn's overspend is caught", func(t *testing.T) {
		a, rec := settleAgent(t, 0.15)
		if _, err := runPrep(t, a, preTurnSteps, "next turn"); err != nil {
			t.Fatalf("runPreTurn: %v", err)
		}
		if got := len(rec.guardrailTrips()); got != 1 {
			t.Fatalf("guardrail trips = %d, want 1 — the $0.15 turn blew the $0.10 per-turn cap", got)
		}
		if reason := rec.guardrailTrips()[0].Reason; !strings.Contains(reason, "per-turn") {
			t.Errorf("trip is not the per-turn one: %q", reason)
		}
	})

	t.Run("transposed: the delta reads zero and nothing trips", func(t *testing.T) {
		a, rec := settleAgent(t, 0.15)
		steps := transpose(t, preTurnSteps, "settle-cost-ceiling", "snapshot-turn-start-cost")
		if _, err := runPrep(t, a, steps, "next turn"); err != nil {
			t.Fatalf("runPreTurn: %v", err)
		}
		if got := len(rec.guardrailTrips()); got != 0 {
			t.Fatalf("guardrail trips = %d, want 0 — this subtest exists to prove the one above can fail", got)
		}
	})
}

// TestPreTurn_144_TheReadFileLoopIsOnlyBoundedBecauseTheSettlePassRuns
// is the incident #362 was found in, and the stronger consequence of
// the same transposition. #1049 made a per-turn trip end its turn and
// nothing more, so what actually stops a re-driving loop is the
// three-in-a-row escalation to a session halt. Every one of those three
// trips is recorded by the settle pass. Move it after the snapshot and
// the streak never advances past zero: the loop is unbounded again, and
// no ceiling ever fires to say so.
func TestPreTurn_144_TheReadFileLoopIsOnlyBoundedBecauseTheSettlePassRuns(t *testing.T) {
	t.Parallel()

	// Each iteration is one turn of the loop: the pre-turn pipeline
	// runs, then the harness appends that turn's runaway spend late.
	drive := func(t *testing.T, steps []preTurnStep) *Agent {
		t.Helper()
		tr := usage.NewTracker()
		a := preTurnAgent(t, WithUsageTracker(tr), WithCostCeiling(CostCeiling{MaxTurnUSD: 0.10}))
		for i := 0; i < maxConsecutiveTurnCeilingTrips+1; i++ {
			if _, err := runPrep(t, a, steps, ""); err != nil {
				return a // refused: the loop is bounded, which is the point
			}
			tr.Append("harness", 1_500_000, 0, usage.Pricing{InputPerMTok: 0.10}) // $0.15
		}
		return a
	}

	t.Run("in order: the streak escalates and the session halts", func(t *testing.T) {
		a := drive(t, preTurnSteps)
		if tripped, _ := a.CostCeilingTripped(); !tripped {
			t.Fatalf("after %d runaway turns in a row the session must halt; it did not", maxConsecutiveTurnCeilingTrips)
		}
		if err := a.preflightCostCeiling(); err == nil {
			t.Error("the halted session still accepts turns")
		}
	})

	t.Run("transposed: the loop runs forever", func(t *testing.T) {
		a := drive(t, transpose(t, preTurnSteps, "settle-cost-ceiling", "snapshot-turn-start-cost"))
		if tripped, _ := a.CostCeilingTripped(); tripped {
			t.Fatal("the session halted under the transposed pipeline; this subtest exists to prove the one above can fail")
		}
	})
}

// --- #145: refuse before doing any work ------------------------------

// TestPreTurn_145_ACostCeilingRefusalMustNotConsumeTheOperatorsMessage
// is what "refuse at the very top" is for. A ceiling checked after the
// work is a report, not a cap — and the specific work that hurts here
// is the inbox drain, because a message consumed by a turn that is
// then refused is a message the operator never gets an answer to and
// cannot re-read. It is gone.
func TestPreTurn_145_ACostCeilingRefusalMustNotConsumeTheOperatorsMessage(t *testing.T) {
	t.Parallel()

	halted := func(t *testing.T) *Agent {
		t.Helper()
		a := preTurnAgent(t)
		a.costCeilingExceeded = true
		a.costCeilingReason = "per-session cost ceiling exceeded"
		if err := a.Inject("did the rollout finish?"); err != nil {
			t.Fatalf("Inject: %v", err)
		}
		return a
	}

	t.Run("in order: refused, and the message is still queued", func(t *testing.T) {
		a := halted(t)
		_, err := runPrep(t, a, preTurnSteps, "")
		if !IsCostCeilingExceeded(err) {
			t.Fatalf("err = %v, want a cost-ceiling refusal", err)
		}
		if got := a.PendingInboxCount(); got != 1 {
			t.Errorf("pending inbox = %d, want 1 — the refused turn ate the operator's steer", got)
		}
	})

	t.Run("transposed: the refused turn eats the message", func(t *testing.T) {
		a := halted(t)
		steps := transpose(t, preTurnSteps, "preflight-cost-ceiling", "drain-inbox")
		_, err := runPrep(t, a, steps, "")
		if !IsCostCeilingExceeded(err) {
			t.Fatalf("err = %v, want a cost-ceiling refusal", err)
		}
		if got := a.PendingInboxCount(); got != 0 {
			t.Fatalf("pending inbox = %d, want 0 — this subtest exists to prove the one above can fail", got)
		}
	})
}

// --- #623: the watchdog refusal is what breaks the loop --------------

// TestPreTurn_623_AWatchdogRefusalMustPrecedeThePendingCompaction is
// the watchdog arm of the same structural rule, with a different cost.
// An auto-continue re-drive of the halted turn calls Run again; if a
// pending compaction runs before the refusal, that re-drive pays for a
// summarizer call — a real model call, at real cost — for a turn that
// is refused a microsecond later. A halted runaway that keeps spending
// is the failure #623 was filed about.
func TestPreTurn_623_AWatchdogRefusalMustPrecedeThePendingCompaction(t *testing.T) {
	t.Parallel()

	halted := func(t *testing.T) (*Agent, *recordingLLM) {
		t.Helper()
		llm := &recordingLLM{}
		a, err := New(llm,
			WithSessionService(session.InMemoryService()),
			WithSession("preturn-user", "preturn-sid"),
			WithCompactor(NewDefaultCompactor()),
		)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		seedPreTurnHistory(t, a, &genai.Part{Text: "read_file, again"})
		a.watchdogTripped = true
		a.watchdogReason = "watchdog halted the agent (repeated-tool-call)"
		a.compactionPending = true
		return a, llm
	}

	t.Run("in order: refused before the summarizer is paid for", func(t *testing.T) {
		a, llm := halted(t)
		_, err := runPrep(t, a, preTurnSteps, "")
		if !IsWatchdogTripped(err) {
			t.Fatalf("err = %v, want a watchdog refusal", err)
		}
		if got := len(llm.requests); got != 0 {
			t.Errorf("summarizer calls = %d, want 0 — a refused turn paid for a compaction", got)
		}
	})

	t.Run("transposed: the refused turn pays for a compaction", func(t *testing.T) {
		a, llm := halted(t)
		steps := transpose(t, preTurnSteps, "preflight-watchdog", "pending-compaction")
		_, err := runPrep(t, a, steps, "")
		if !IsWatchdogTripped(err) {
			t.Fatalf("err = %v, want a watchdog refusal", err)
		}
		if got := len(llm.requests); got == 0 {
			t.Fatal("no summarizer call under the transposed pipeline; this subtest exists to prove the one above can fail")
		}
	})
}

// --- #655: a refused turn is not a turn boundary ---------------------

// TestPreTurn_655_ARefusedTurnIsNotATurnBoundary pins the placement the
// seventh watchdog signal depends on. Signals whose evidence is scoped
// to one turn clear it at the boundary; a refused turn never ran, so it
// is not a boundary. Move the boundary above the preflights and an
// auto-continue re-drive gets a way to launder a stall one refusal at a
// time — each refusal resets the very evidence that produced it.
func TestPreTurn_655_ARefusedTurnIsNotATurnBoundary(t *testing.T) {
	t.Parallel()

	halted := func(t *testing.T) (*Agent, *turnCountingWatchdog) {
		t.Helper()
		w := &turnCountingWatchdog{}
		a := preTurnAgent(t, WithWatchdog(w, nil), WithWatchdogEnforce())
		a.watchdogTripped = true
		a.watchdogReason = "watchdog halted the agent (no-progress)"
		return a, w
	}

	t.Run("in order: the refusal clears nothing", func(t *testing.T) {
		a, w := halted(t)
		if _, err := runPrep(t, a, preTurnSteps, ""); !IsWatchdogTripped(err) {
			t.Fatalf("err = %v, want a watchdog refusal", err)
		}
		if w.starts != 0 {
			t.Errorf("ObserveTurnStart calls = %d, want 0 — the refused turn was counted as a boundary", w.starts)
		}
	})

	t.Run("transposed: the refusal launders the stall", func(t *testing.T) {
		a, w := halted(t)
		steps := transpose(t, preTurnSteps, "turn-boundary", "preflight-watchdog")
		if _, err := runPrep(t, a, steps, ""); !IsWatchdogTripped(err) {
			t.Fatalf("err = %v, want a watchdog refusal", err)
		}
		if w.starts == 0 {
			t.Fatal("no turn-start observed under the transposed pipeline; this subtest exists to prove the one above can fail")
		}
	})
}

// --- #1074: the gate's refusals clear on the same boundary -----------

// denyingPrompter refuses everything and counts how many times it was
// asked. The count is the whole assertion: #1074 is about how many
// times an operator is paged for one request.
type denyingPrompter struct{ asked int }

func (p *denyingPrompter) AskApproval(context.Context, permissions.PromptRequest) (permissions.Decision, error) {
	p.asked++
	return permissions.DecisionDeny, nil
}

// TestPreTurn_1074_ARefusedTurnDoesNotClearTheGatesRefusals is the
// #655 argument applied to the gate's turn-scoped refusal memory, and
// it is the reason that memory hangs off this step rather than off Run
// directly. The memory exists so a denied call cannot re-open its own
// prompt for the rest of the turn; if a refused turn counted as a
// boundary, an auto-continue re-drive would clear the denial and put
// the operator back in front of the prompt they just closed — once per
// re-drive, which is the loop #1074 exists to stop.
func TestPreTurn_1074_ARefusedTurnDoesNotClearTheGatesRefusals(t *testing.T) {
	t.Parallel()

	halted := func(t *testing.T) (*Agent, *permissions.Gate, *denyingPrompter) {
		t.Helper()
		p := &denyingPrompter{}
		g := permissions.New(permissions.Options{Mode: permissions.ModeAsk, Prompter: p})
		a := preTurnAgent(t, WithGate(g), WithWatchdog(&fakeWatchdog{}, nil), WithWatchdogEnforce())
		a.watchdogTripped = true
		a.watchdogReason = "watchdog halted the agent (no-progress)"
		// The turn that got denied, before the re-drive.
		if err := g.CheckBash(context.Background(), "kubectl delete ns prod"); err == nil {
			t.Fatal("expected the prompter's denial to fail the call")
		}
		if p.asked != 1 {
			t.Fatalf("setup: prompts = %d, want 1", p.asked)
		}
		return a, g, p
	}

	t.Run("in order: the refusal stands", func(t *testing.T) {
		a, g, p := halted(t)
		if _, err := runPrep(t, a, preTurnSteps, ""); !IsWatchdogTripped(err) {
			t.Fatalf("err = %v, want a watchdog refusal", err)
		}
		if err := g.CheckBash(context.Background(), "kubectl delete ns prod"); err == nil {
			t.Fatal("expected the repeat to be refused")
		}
		if p.asked != 1 {
			t.Errorf("prompts = %d, want 1 — the refused turn cleared the gate's refusal memory", p.asked)
		}
	})

	t.Run("transposed: the re-drive re-opens the prompt", func(t *testing.T) {
		a, g, p := halted(t)
		steps := transpose(t, preTurnSteps, "turn-boundary", "preflight-watchdog")
		if _, err := runPrep(t, a, steps, ""); !IsWatchdogTripped(err) {
			t.Fatalf("err = %v, want a watchdog refusal", err)
		}
		if err := g.CheckBash(context.Background(), "kubectl delete ns prod"); err == nil {
			t.Fatal("expected the repeat to be denied by the prompter")
		}
		if p.asked == 1 {
			t.Fatal("the refusal survived the transposed pipeline; this subtest exists to prove the one above can fail")
		}
	})
}

// A turn that actually starts IS a boundary: the model now has a tool
// result in its history saying it was refused, and the operator's
// circumstances may have changed. Without this half, the memory is
// session-scoped by accident and one misclick disables a tool for the
// rest of the session.
func TestPreTurn_1074_ATurnThatRunsClearsTheGatesRefusals(t *testing.T) {
	t.Parallel()
	p := &denyingPrompter{}
	g := permissions.New(permissions.Options{Mode: permissions.ModeAsk, Prompter: p})
	a := preTurnAgent(t, WithGate(g))

	if err := g.CheckBash(context.Background(), "kubectl delete ns prod"); err == nil {
		t.Fatal("expected the prompter's denial to fail the call")
	}
	if _, err := runPrep(t, a, preTurnSteps, "carry on"); err != nil {
		t.Fatalf("pre-turn pipeline: %v", err)
	}
	if err := g.CheckBash(context.Background(), "kubectl delete ns prod"); err == nil {
		t.Fatal("expected the new turn's call to be denied by the prompter")
	}
	if p.asked != 2 {
		t.Errorf("prompts = %d, want 2 — the turn boundary did not clear the gate's refusal memory", p.asked)
	}
}

// --- #537: heal the history before anything reads it -----------------

// seedPreTurnHistory creates the agent's session and appends a user
// turn plus one model event carrying the given parts.
func seedPreTurnHistory(t *testing.T, a *Agent, modelParts ...*genai.Part) {
	t.Helper()
	ctx := context.Background()
	svc := a.sessionService
	if _, err := svc.Create(ctx, &session.CreateRequest{
		AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
	}); err != nil {
		t.Fatalf("session Create: %v", err)
	}
	resp, err := svc.Get(ctx, &session.GetRequest{
		AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	user := session.NewEvent("preturn")
	user.Author = "user"
	user.Content = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "list the pods"}}}
	if err := svc.AppendEvent(ctx, resp.Session, user); err != nil {
		t.Fatalf("AppendEvent(user): %v", err)
	}
	mdl := session.NewEvent("preturn")
	mdl.Author = defaultAgentName
	mdl.Content = &genai.Content{Role: genai.RoleModel, Parts: modelParts}
	if err := svc.AppendEvent(ctx, resp.Session, mdl); err != nil {
		t.Fatalf("AppendEvent(model): %v", err)
	}
}

// sessionShape labels the session's events in order: "repair" for a
// synthesized tool response, "boundary" for a compaction marker, "" for
// everything else. Position is what these two tests are about.
func sessionShape(t *testing.T, a *Agent) []string {
	t.Helper()
	resp, err := a.sessionService.Get(context.Background(), &session.GetRequest{
		AppName: a.appName, UserID: a.userID, SessionID: a.sessionID,
	})
	if err != nil {
		t.Fatalf("session Get: %v", err)
	}
	var out []string
	for ev := range resp.Session.Events().All() {
		switch {
		case ev.CustomMetadata["kind"] == tailRepairKind:
			out = append(out, "repair")
		case ev.CustomMetadata[CompactionMetadataKey] == CompactionEventTag:
			out = append(out, "boundary")
		default:
			out = append(out, "")
		}
	}
	return out
}

func indexOf(kinds []string, want string) int {
	for i, k := range kinds {
		if k == want {
			return i
		}
	}
	return -1
}

// TestPreTurn_537_TheRepairLandsInsideTheWindowItBelongsTo is the
// ordering claim tail repair rests on. A turn that died between a
// persisted functionCall and its functionResponse leaves an unanswered
// call; providers reject that outright, so the session is poisoned for
// every subsequent turn until the repair runs.
//
// The failure the transposition produces is not the one the original
// comment in Run described. It said the drains would trip on the
// dangling tail themselves — true when #537 landed, but #541 later gave
// summarizerHistory its own call/response normalization, so the
// summarizer is now shielded independently. What the ordering still
// buys is where the repair LANDS. Compaction appends a boundary event,
// and every later window is sliced from it. Run the drain first and the
// synthesized response is appended after that boundary: the next turn's
// window opens with a functionResponse whose call is on the far side of
// the cut, which is the same provider rejection with the operands
// swapped. Repairing before the boundary is drawn keeps the pair on the
// same side of it.
func TestPreTurn_537_TheRepairLandsInsideTheWindowItBelongsTo(t *testing.T) {
	t.Parallel()

	dangling := func(t *testing.T) *Agent {
		t.Helper()
		a, err := New(&recordingLLM{},
			WithSessionService(session.InMemoryService()),
			WithSession("preturn-user", "preturn-sid"),
			WithCompactor(NewDefaultCompactor()),
		)
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		seedPreTurnHistory(t, a, &genai.Part{FunctionCall: &genai.FunctionCall{
			ID: "call-537", Name: "read_file", Args: map[string]any{"path": "main.go"},
		}})
		a.compactionPending = true
		return a
	}

	assertBoth := func(t *testing.T, kinds []string) (repair, boundary int) {
		t.Helper()
		repair, boundary = indexOf(kinds, "repair"), indexOf(kinds, "boundary")
		if repair < 0 {
			t.Fatalf("no tail-repair event was written; session shape %v", kinds)
		}
		if boundary < 0 {
			t.Fatalf("the pending compaction wrote no boundary; session shape %v", kinds)
		}
		return repair, boundary
	}

	t.Run("in order: the repair precedes the boundary", func(t *testing.T) {
		a := dangling(t)
		if _, err := runPrep(t, a, preTurnSteps, "carry on"); err != nil {
			t.Fatalf("runPreTurn: %v", err)
		}
		kinds := sessionShape(t, a)
		repair, boundary := assertBoth(t, kinds)
		if repair > boundary {
			t.Errorf("the repair landed after the compaction boundary (%v) — the next window opens on an orphaned response", kinds)
		}
	})

	t.Run("transposed: the repair is stranded past the boundary", func(t *testing.T) {
		a := dangling(t)
		steps := transpose(t, preTurnSteps, "repair-dangling-tool-calls", "pending-compaction")
		if _, err := runPrep(t, a, steps, "carry on"); err != nil {
			t.Fatalf("runPreTurn: %v", err)
		}
		kinds := sessionShape(t, a)
		repair, boundary := assertBoth(t, kinds)
		if repair < boundary {
			t.Fatalf("the repair still preceded the boundary (%v); this subtest exists to prove the one above can fail", kinds)
		}
	})
}

// --- #643: restore, then flush, then refuse --------------------------

// TestPreTurn_643_TheOutOfBandFlushMustPrecedeTheRefusal pins the
// second half of the durable-guardrail ordering. The settle pass can
// queue a durable trip row; the preflights return early. Flush after
// them and the row sits unwritten until some later turn — which that
// very preflight will never allow to start. The halt then dies with the
// process, which is the whole failure #643 exists to prevent.
func TestPreTurn_643_TheOutOfBandFlushMustPrecedeTheRefusal(t *testing.T) {
	t.Parallel()

	halted := func(t *testing.T) *Agent {
		t.Helper()
		h, cleanup := openTestEventLog(t)
		t.Cleanup(cleanup)
		createTestSession(t, h, DefaultAppName, "u", "s-659-643")
		a, err := New(&recordingLLM{}, WithEventLog(h), WithSession("u", "s-659-643"))
		if err != nil {
			t.Fatalf("agent.New: %v", err)
		}
		// A halt the previous turn tripped, with its durable row still
		// queued. The queue is filled directly rather than through
		// queueOutOfBandEvent, which drains inline when no turn is in
		// flight: the rows this step exists for are the ones queued
		// while a turn WAS in flight — the in-turn guardrail arm that
		// cuts its own turn — and whose post-turn flush did not land.
		a.costCeilingExceeded = true
		a.costCeilingReason = "per-session cost ceiling exceeded"
		a.pendingOutOfBandEvents = []*session.Event{
			attach.NewGuardrailTripEvent(attach.GuardrailCostCeiling, a.costCeilingReason),
		}
		return a
	}

	t.Run("in order: the halt is durable before the turn is refused", func(t *testing.T) {
		a := halted(t)
		if _, err := runPrep(t, a, preTurnSteps, ""); !IsCostCeilingExceeded(err) {
			t.Fatalf("err = %v, want a cost-ceiling refusal", err)
		}
		if got := countGuardrailRows(t, a, attach.GuardrailTripEventAuthor); got != 1 {
			t.Errorf("durable trip rows = %d, want 1 — a restart would hand the runaway a fresh budget", got)
		}
	})

	t.Run("transposed: the refusal strands the row", func(t *testing.T) {
		a := halted(t)
		steps := transpose(t, preTurnSteps, "drain-out-of-band-events", "preflight-cost-ceiling")
		if _, err := runPrep(t, a, steps, ""); !IsCostCeilingExceeded(err) {
			t.Fatalf("err = %v, want a cost-ceiling refusal", err)
		}
		if got := countGuardrailRows(t, a, attach.GuardrailTripEventAuthor); got != 0 {
			t.Fatalf("durable trip rows = %d, want 0 — this subtest exists to prove the one above can fail", got)
		}
	})
}

// --- #697: the framing reads the operator's own text -----------------

// TestPreTurn_697_TheRawPromptIsCapturedBeforeTheAlertPrepend is why
// capture-raw-prompt sits above every prepend. A wake-driven turn
// arrives as Run("") carrying a subagent report; nobody asked anything,
// so the inbox bundle IS the turn and gets the handling guidance. Let
// the alert prepend land first and rawPrompt is the alert text —
// non-empty — so the pipeline concludes an operator is waiting for an
// answer and drops the guidance.
func TestPreTurn_697_TheRawPromptIsCapturedBeforeTheAlertPrepend(t *testing.T) {
	t.Parallel()

	woken := func(t *testing.T) *Agent {
		t.Helper()
		a := preTurnAgent(t)
		a.bgMgr = &alertingManager{alert: "[Background] researcher finished."}
		if err := a.Inject("node pool is at capacity"); err != nil {
			t.Fatalf("Inject: %v", err)
		}
		return a
	}

	t.Run("in order: the bundle is the turn", func(t *testing.T) {
		a := woken(t)
		tp, err := runPrep(t, a, preTurnSteps, "")
		if err != nil {
			t.Fatalf("runPreTurn: %v", err)
		}
		if tp.rawPrompt != "" {
			t.Errorf("rawPrompt = %q, want empty — nobody typed anything this turn", tp.rawPrompt)
		}
		if !strings.Contains(tp.prompt, inboxHandlingGuidance) {
			t.Errorf("the inbox bundle lost its handling guidance:\n%s", tp.prompt)
		}
	})

	t.Run("transposed: the alert is mistaken for the operator", func(t *testing.T) {
		a := woken(t)
		steps := transpose(t, preTurnSteps, "capture-raw-prompt", "prepend-alerts")
		tp, err := runPrep(t, a, steps, "")
		if err != nil {
			t.Fatalf("runPreTurn: %v", err)
		}
		if strings.Contains(tp.prompt, inboxHandlingGuidance) {
			t.Fatal("the guidance survived the transposed pipeline; this subtest exists to prove the one above can fail")
		}
	})
}

// --- #159: the correction goes on last so it reads first -------------

// TestPreTurn_159_WatchdogFeedbackGoesOnLastSoItReadsFirst pins the far
// end of the pipeline. The feedback is an observation about the model's
// own immediately-preceding turn; buried under a page of inbox traffic
// it is a correction the model can skim past.
func TestPreTurn_159_WatchdogFeedbackGoesOnLastSoItReadsFirst(t *testing.T) {
	t.Parallel()

	looping := func(t *testing.T) *Agent {
		t.Helper()
		a := preTurnAgent(t)
		a.watchdogPending = []watchdog.Alert{{
			Signal:   "repeated-tool-call",
			Severity: watchdog.SeverityWarn,
			Reason:   "read_file on main.go five times with identical args.",
		}}
		if err := a.Inject("any update?"); err != nil {
			t.Fatalf("Inject: %v", err)
		}
		return a
	}

	block := watchdog.FormatFeedback([]watchdog.Alert{{
		Signal:   "repeated-tool-call",
		Severity: watchdog.SeverityWarn,
		Reason:   "read_file on main.go five times with identical args.",
	}})

	t.Run("in order: the correction leads", func(t *testing.T) {
		a := looping(t)
		tp, err := runPrep(t, a, preTurnSteps, "")
		if err != nil {
			t.Fatalf("runPreTurn: %v", err)
		}
		if !strings.HasPrefix(tp.prompt, block) {
			t.Errorf("the watchdog correction is not the first thing the model reads:\n%s", tp.prompt)
		}
	})

	t.Run("transposed: the inbox buries it", func(t *testing.T) {
		a := looping(t)
		steps := transpose(t, preTurnSteps, "prepend-watchdog-feedback", "prepend-inbox")
		tp, err := runPrep(t, a, steps, "")
		if err != nil {
			t.Fatalf("runPreTurn: %v", err)
		}
		if strings.HasPrefix(tp.prompt, block) {
			t.Fatal("the correction still led under the transposed pipeline; this subtest exists to prove the one above can fail")
		}
	})
}

// --- the pause gate --------------------------------------------------

// TestPreTurn_TheParkedAgentDrainsNothing is the await-resume
// placement. A parked agent starting no turn is the obvious half; the
// half that bites is that it must also consume nothing, because the
// steer an operator typed while parked has to survive until the turn
// that resume starts.
func TestPreTurn_TheParkedAgentDrainsNothing(t *testing.T) {
	t.Parallel()
	a := preTurnAgent(t)
	a.Pause("operator parked the agent")
	if err := a.Inject("stop the rollout"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller gives up rather than the gate opening
	tp := &turnPrep{ctx: ctx, prompt: ""}
	if err := a.runPreTurn(tp, preTurnSteps); err == nil {
		t.Fatal("a parked agent with a dead context started a turn")
	}
	if got := a.PendingInboxCount(); got != 1 {
		t.Errorf("pending inbox = %d, want 1 — the parked agent ate the operator's steer", got)
	}
}
