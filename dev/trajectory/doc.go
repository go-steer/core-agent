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

// Package trajectory reads what an agent DID, in order, out of a
// recorded run — and reports on the shape of it.
//
// # Why this exists
//
// Everything else we measure asks whether the agent ended up in the
// right state. The GKE drill's six boxes grade an answer; #652's eval
// runner grades effects on the world. Neither asks how the agent got
// there, and that is a separate question with separate failures (#994):
//
//   - The #711 run reached a correct-looking end state in 44 turns and
//     1.4M input tokens for $1.33, against $0.26 for the comparable
//     run. An outcome grader scores that a pass.
//   - The list_agents runaway behind the #623–#627 guardrail train was
//     a shape failure: the same call, forever, with a fine end state.
//   - On 2026-09-11 a drill run scored six boxes out of six while its
//     `cluster` subagent was dead of a Vertex 429. The parent re-did the
//     reads itself and still produced a grounded answer. Nothing in the
//     rubric can see any of that: the run's evidence.md never mentions
//     the delegation, the 429, or the subagent at all, so a run where
//     half the plan did not execute is indistinguishable on the sheet
//     from one where it did.
//
// # What the first backfill showed
//
// The whole archive, 15 runs, read as step tables (2026-09-11). Three
// things were visible that no evidence sheet says, and none of them
// needed a rubric — they are counts and row order:
//
//   - The dead delegation is a NUMBER. Scenario A runs make 11-12 steps
//     with 9-10 of them in the `cluster` subagent. The 2026-09-11 run
//     that scored 6/6 makes 6 steps with 1 in `cluster`. The column
//     says what the scorecard had to be told by hand.
//   - #1014 is a ROW ORDER. In run 20260909T232522Z-c the child reads a
//     resource and the parent, right after the handoff, reads it again.
//     "A parent cannot cite its subagent's reads, so it re-does them"
//     stops being an anecdote and becomes a shape you can point at.
//   - `return_result` is present in every healthy delegation and absent
//     from the broken one. A single column tells you whether the
//     handoff completed, which is cheaper than any prose scan.
//
// What it did NOT show is a loop or a redundant-call problem WITHIN one
// agent: no run in the archive has one. That matters for what gets built
// next — such a measure would report zero across the entire corpus and
// would therefore be untested. Delegation shape fires today.
//
// # What the first measure showed
//
// [Delegation], run over the same 15 runs, puts numbers on all three
// bullets above:
//
//   - 10 repeated reads across 8 of the 15 runs. That is #1014's headline
//     finding as a rate rather than an anecdote: in more than half the
//     archive the parent re-issues a read its child already made.
//   - 1 failed delegation, in 20260911T110503Z-a, and the parent
//     DISCLOSED it — unprompted, in the second line of its answer. See
//     the correction below.
//   - 0 undisclosed failures. On this corpus the agent is not the thing
//     hiding the failure.
//
// Three design decisions in [Delegation] came from the corpus refusing to
// agree with what had been written about it, and each is worth more than
// the code it changed:
//
//   - A "result dropped" measure was planned around #641 (`wait:true`
//     returns a summary, not findings). #641 is CLOSED, and the archive
//     confirms the fix: spawn_agent's `output` equals the child's
//     return_result `result` byte-for-byte in all 14 healthy runs. The
//     measure would have reported a defect that no longer exists.
//   - The repeated-read signature originally matched arguments exactly
//     and found 5 repeats in 5 runs — and MISSED the case that motivated
//     it, where the child read a resource as YAML and the parent re-read
//     it unformatted. Ignoring presentational arguments moved it to 10
//     across 8. See presentationalArgs.
//   - The 2026-09-11 run's parent was described here, and in a signed
//     evidence sheet, as having absorbed the 429 silently. It did not:
//     it said so in its own answer. What was actually silent was
//     evidence.md, and the rubric's blindness got read as the agent's.
//     The bullet above is the corrected version.
//
// # What it is not
//
// It is not a grader and it does not return a verdict. Measures emit
// Observations — "here is a thing that happened, at this step, with
// this evidence" — and a human reads them. Two reasons, and both are
// load-bearing:
//
// The rubric in dev/uat/gke-drill/SCORECARD.md is still moving, and
// automating a moving rubric encodes whichever version happened to be
// current. #652 says the same thing about the eval corpus.
//
// And the failure mode this package is most likely to have is not being
// wrong, it is being ignored. #790 + #844 spent 6,146 lines on a drift
// detector that detected correctly and changed nothing, because
// attention was the bottleneck rather than detection. dev/tools/focus
// carries that lesson in its own header. So: no gate, no CI wiring, no
// threshold that goes red. If this ever needs a fix, deleting it is a
// correct response.
//
// # The substrate
//
// A trajectory is the ordered sequence of (agent, tool, arguments,
// outcome) across a parent and its subagents. The drill already records
// exactly that, and the recording turns out to be free to read: the
// `event` object in a run's transcript.jsonl is a marshalled
// google.golang.org/adk/session.Event — the same type pkg/eventlog
// persists. All fifteen runs archived as of 2026-09-11 decode into it
// with DisallowUnknownFields set and no failures, so this package gets
// typed FunctionCall.Args, FunctionResponse.Response, UsageMetadata,
// ErrorCode and Branch for the cost of a json.Unmarshal, where
// score.py hand-walks the same dicts.
//
// That also settles, in the narrow sense, the open question #994 parks:
// #881 (a general transcript export) is frozen while the drill, the
// eval runner and this package each reach for their own rendering of
// the same rows. They do not disagree about the data MODEL — all three
// already speak session.Event. They differ only in transport, SSE
// capture against a SQLite eventlog. So the seam here is the loader:
// [LoadRun] turns one recorded run into [Frame]s, and everything above
// it works on Frames. A pkg/eventlog loader is additive, needing no
// change to [Step] or to anything reading Steps.
//
// # Relationship to score.py
//
// None, deliberately. score.py decides the drill's six rubric boxes and
// renders evidence.md. This answers "what did it do". They are not
// merged and this does not feed that, at least until the rubric stops
// changing.
//
// The one place they must NOT drift is what counts as a failed tool
// call — see [Status] and responseStatus, which mirror score.py's
// three-way rule on purpose, down to reproducing Python's JSON encoding
// so that the rule's 200-character window cuts in the same place (see
// pyDumps). That mirroring is checked, not asserted:
// TestAgreesWithScorePyOnTheArchive compares this package's five counts
// against the tally score.py wrote into each run's own evidence.md.
// Fifteen runs, seventy-five numbers, zero disagreements as of
// 2026-09-11.
package trajectory
