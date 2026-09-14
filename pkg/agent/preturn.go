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

// The pre-turn pipeline, as data rather than as statement order (#659).
//
// Everything Run does before it hands a request to the runner is a
// sequence of small steps whose *order* is the whole design. That order
// used to live in two places that cannot be checked: the physical
// sequence of statements in Run, and a trail of comments naming the bugs
// each position prevents — #362, #145, #144, #623, #537, and since then
// #655. Six defects at one seam is a structural signal, not a style one,
// and critically it has nothing to do with how long Run is: a
// twenty-line version with the same implicit ordering would be exactly
// as dangerous, because nothing except production detects a transposed
// pair.
//
// So the sequence is a slice. Each step carries the constraint its
// position encodes, in the `enforces` field, next to the test that now
// holds it — and `preturn_test.go` runs the pipeline against permuted
// slices, so a reordering fails in CI instead of in an incident. That is
// the point of the extraction; the shorter Run is a side effect.
//
// Deliberately NOT done here: chopping Run into runPart1/runPart2. That
// preserves the implicit ordering while spreading it across more call
// sites, which is strictly worse than one long function — the constraint
// becomes invisible *and* non-local.
//
// Two shapes of step share one type. Most are side effects on the agent
// (restore state, enforce a ceiling, repair a history), and a few build
// the prompt the turn will actually send. They are one list because the
// ordering constraints cross the boundary: the raw-prompt capture has to
// happen before the alert prepend buries the operator's text, and the
// cost snapshot has to happen after the settle-time enforcement that
// reads the previous turn's baseline. A pipeline split in two by shape
// would let exactly those pairs be transposed.

package agent

import (
	"context"
	"strings"
)

// turnPrep is the state a pre-turn step reads and writes. One value per
// Run call, never shared, so no synchronization: the agent fields the
// steps touch have their own locks where they need them.
type turnPrep struct {
	// ctx is the caller's context, before any per-turn wrapping. Steps
	// that persist or summarize use it; the wrapped runCtx is built
	// after the pipeline, from values the pipeline produced.
	ctx context.Context

	// prompt accumulates the prepends. Starts as the caller's text and
	// ends as what actually goes to the model.
	prompt string

	// rawPrompt is the operator's own text, captured before the prepends
	// bury it under alerts, inbox framing and watchdog feedback. What the
	// session gets named after, and what decides the inbox framing.
	rawPrompt string

	// drained is this turn's inbox batch — texts, per-message senders,
	// the turn originator and the OTel span links. Produced by the
	// drain-inbox step and read by three later ones.
	drained inboxDrain
}

// preTurnStep is one ordered stage of the pipeline.
//
// enforces is not decoration. It states what this step's *position*
// buys, which is the thing that was previously only in a comment and is
// now also in a test. A step with no ordering constraint says so.
type preTurnStep struct {
	name     string
	enforces string
	run      func(a *Agent, tp *turnPrep) error
}

// preTurnSteps is the pipeline, in order. The ordering here is the
// contract; preturn_test.go permutes it and asserts each historical bug
// comes back.
var preTurnSteps = []preTurnStep{
	{
		name: "await-resume",
		enforces: "First. A parked agent starts no turn and — just as importantly — " +
			"drains no inbox: the steer an operator typed while parked has to survive " +
			"until the turn that resume starts. Any step that consumed state before " +
			"this one would consume it on behalf of a turn that is not happening.",
		run: func(a *Agent, tp *turnPrep) error { return a.awaitResume(tp.ctx) },
	},
	{
		name: "restore-guardrails",
		enforces: "#643: before anything can refuse or permit this turn. A halt a " +
			"restart clears is not a halt — a runaway that trips the watchdog and then " +
			"kills the pod would come back to a disarmed backstop and loop again. Must " +
			"therefore precede both preflights, which are the things that read the " +
			"restored state.",
		run: func(a *Agent, tp *turnPrep) error { a.ensureGuardrailsRestored(tp.ctx); return nil },
	},
	{
		name: "settle-cost-ceiling",
		enforces: "#362, and #144 is the incident. Must run BEFORE snapshot-turn-start-cost, " +
			"which resets the baseline this reads. In harness-driven deployments the " +
			"harness appends the main-model cost AFTER the prior turn's cleanup hook, so " +
			"that turn's post-hook saw only in-turn internal spend and missed the model " +
			"entirely; re-running here, while a.turnStartCost still holds the prior " +
			"turn's baseline, is what lets a single runaway turn trip the per-turn cap " +
			"at all. Transposed with the snapshot, the delta is always ~0 and the cap " +
			"never fires.",
		run: func(a *Agent, tp *turnPrep) error { a.maybeEnforceCostCeiling(false); return nil },
	},
	{
		name: "drain-out-of-band-events",
		enforces: "#643: flushes any guardrail row still queued, and must come BEFORE the " +
			"two preflights. They return early, so a trip left unwritten here would sit " +
			"unwritten until some later turn that the preflight will never allow to " +
			"start — and a halt that dies with the process is not a halt. The rows that " +
			"actually reach this step are the ones queued while a turn WAS in flight " +
			"(queueOutOfBandEvent writes inline otherwise), i.e. the in-turn guardrail arm " +
			"that cut its own turn. No turn is in flight yet, so this is a safe write " +
			"window.",
		run: func(a *Agent, tp *turnPrep) error { a.drainOutOfBandEvents(); return nil },
	},
	{
		name: "preflight-cost-ceiling",
		enforces: "#145: refuses the turn before any tracker write, model call or " +
			"pending-cleanup work. Position is the enforcement — a ceiling checked after " +
			"the work has been done is a report, not a cap.",
		run: func(a *Agent, tp *turnPrep) error { return a.preflightCostCeiling() },
	},
	{
		name: "preflight-watchdog",
		enforces: "#623: the same structural refusal, and the thing that actually breaks " +
			"a tool-call loop. An auto-continue re-drive of the interrupted turn calls " +
			"Run again and is refused HERE rather than re-issuing the looping call, so " +
			"this must precede every step that does work.",
		run: func(a *Agent, tp *turnPrep) error { return a.preflightWatchdog() },
	},
	{
		name: "turn-boundary",
		enforces: "#655: signals whose evidence is scoped to one turn clear it here. " +
			"Deliberately AFTER both preflights — a refused turn never ran, so it is not " +
			"a boundary, and letting it clear state would hand an auto-continue re-drive " +
			"a way to launder a stall one refusal at a time.",
		run: func(a *Agent, tp *turnPrep) error { a.observeTurnStartForWatchdog(); return nil },
	},
	{
		name: "repair-dangling-tool-calls",
		enforces: "#537: heals a history whose previous turn died between a persisted " +
			"functionCall and its functionResponse; providers reject an unanswered call, " +
			"so without this the session is poisoned for every subsequent turn. Must " +
			"precede the checkpoint and compaction drains, which APPEND A BOUNDARY every " +
			"later window is sliced from — repair after one of them and the synthesized " +
			"response lands on the far side of the cut from its call, which is the same " +
			"rejection with the operands swapped. (The original reason was that the drains " +
			"would choke on the dangling tail themselves; #541 later gave summarizerHistory " +
			"its own normalization, so that half is now covered twice and this half is not.)",
		run: func(a *Agent, tp *turnPrep) error { a.repairDanglingToolCalls(tp.ctx); return nil },
	},
	{
		name: "pending-checkpoint",
		enforces: "Before pending-compaction: a checkpoint subsumes the slicing baseline, " +
			"making any pending compaction redundant for the same span. Reversed, the " +
			"turn pays for a summarization it is about to discard.",
		run: func(a *Agent, tp *turnPrep) error { a.runPendingCheckpoint(tp.ctx); return nil },
	},
	{
		name:     "pending-compaction",
		enforces: "After pending-checkpoint, and before the request is built against this history.",
		run:      func(a *Agent, tp *turnPrep) error { a.runPendingCompaction(tp.ctx); return nil },
	},
	{
		name: "snapshot-turn-start-cost",
		enforces: "The other half of #362's constraint: records the baseline the post-turn " +
			"hook diffs against, so it must come AFTER settle-cost-ceiling has finished " +
			"reading the previous one.",
		run: func(a *Agent, tp *turnPrep) error { a.snapshotTurnStartCost(); return nil },
	},
	{
		name: "capture-raw-prompt",
		enforces: "Before every prepend below. The operator's own text is what the session " +
			"is named after and what picks the inbox framing; once the alert and inbox " +
			"blocks are in front of it there is no way to recover which part they typed. " +
			"Naming a session after a watchdog observation would be worse than leaving " +
			"it unnamed.",
		run: func(a *Agent, tp *turnPrep) error { tp.rawPrompt = tp.prompt; return nil },
	},
	{
		name: "prepend-alerts",
		enforces: "Alerts first of the two prepends: they are internal state changes, and " +
			"inbox messages are external input, which belongs closer to the prompt.",
		run: func(a *Agent, tp *turnPrep) error {
			if a.bgMgr != nil {
				tp.prompt = a.bgMgr.PrependPendingAlerts(tp.prompt)
			}
			return nil
		},
	},
	{
		name: "drain-inbox",
		enforces: "Produces tp.drained, which three later steps read. Emits the same " +
			"inbox/dequeued events the public DrainInbox does, so the SSE stream stays " +
			"consistent with what /inject produced on the way in.",
		run: func(a *Agent, tp *turnPrep) error { tp.drained = a.drainInboxFull(); return nil },
	},
	{
		name: "prepend-inbox",
		enforces: "After drain-inbox, obviously, but the load-bearing part is that it reads " +
			"tp.rawPrompt and not tp.prompt: the alert prepend may already have filled " +
			"prompt in, and a wake-driven turn carrying a subagent report still has no " +
			"operator asking anything. Whether the operator typed something is what picks " +
			"the framing (#697).",
		run: func(a *Agent, tp *turnPrep) error {
			tp.prompt = prependInboxMessages(tp.prompt, tp.drained.texts, tp.drained.senders,
				strings.TrimSpace(tp.rawPrompt) == "")
			return nil
		},
	},
	{
		name: "title-session",
		enforces: "After drain-inbox, because a daemon-driven turn arrives as Run(\"\") with " +
			"the text in the inbox — keying only on the prompt argument would leave every " +
			"attach-mode session permanently unnamed. Before the watchdog prepend, so the " +
			"title cannot be drawn from an observation about the model.",
		run: func(a *Agent, tp *turnPrep) error {
			if src := titleSource(tp.rawPrompt, tp.drained.texts); src != "" {
				a.maybeTitleSession(tp.ctx, src)
			}
			return nil
		},
	},
	{
		name: "prepend-watchdog-feedback",
		enforces: "#159: last, so it reads first. It is an observation about the model's own " +
			"immediately-preceding turn, and a correction buried under a page of inbox " +
			"traffic is a correction the model can skim past.",
		run: func(a *Agent, tp *turnPrep) error {
			tp.prompt = a.prependWatchdogFeedback(tp.prompt)
			return nil
		},
	},
}

// runPreTurn executes steps in order against tp, stopping at the first
// error — which means the turn is refused and the caller yields the
// error rather than starting one.
//
// steps is a parameter rather than a read of the package var so the
// tests can hand it a permuted pipeline. Nothing in production passes
// anything but preTurnSteps.
func (a *Agent) runPreTurn(tp *turnPrep, steps []preTurnStep) error {
	for _, step := range steps {
		if err := step.run(a, tp); err != nil {
			return err
		}
	}
	return nil
}
