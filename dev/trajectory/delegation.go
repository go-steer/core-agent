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
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Observation kinds emitted by [Delegation].
const (
	// KindDelegationFailed is a spawn_agent whose child did not finish.
	KindDelegationFailed = "delegation-failed"
	// KindDelegationDisclosed is a failed delegation the parent told the
	// operator about, in its own answer, unprompted.
	KindDelegationDisclosed = "delegation-disclosed"
	// KindDelegationUndisclosed is a failed delegation the parent's
	// answer does not mention.
	KindDelegationUndisclosed = "delegation-undisclosed"
	// KindRepeatedRead is the parent re-issuing, after the handoff, a
	// call its child already made.
	KindRepeatedRead = "delegation-repeated-read"
)

// spawnTool is the delegation door this measure watches. The other door
// — a declarative subagent invoked async — does not block a parent turn
// and so cannot produce the shapes below; #758 gated both doors, and if
// async delegation ever needs measuring it is a separate measure with
// separate evidence, not a second name in this constant.
const spawnTool = "spawn_agent"

// returnResultTool is the child's side of the contract shipped by
// #727-#732.
const returnResultTool = "return_result"

// Delegation reports on the shape of parent-to-subagent handoffs.
//
// It is the first measure in this package, and it is first because of
// what the archive showed rather than what was easiest to write. The
// obvious candidate was redundancy inside one agent — #967's "loop rate"
// — and no run in the archive contains a loop, so it would have reported
// zero fifteen times and proven nothing. Every kind below fires on the
// archive as it stood on 2026-09-11.
//
// What it does NOT do is decide whether any of it was wrong. A parent
// that re-reads a resource its child already read may be double-checking
// something load-bearing; a failed delegation the parent recovered from
// cleanly is a good outcome, not a bad one. Those are judgements about
// intent, and the package doc explains why they are not made here.
type Delegation struct{}

// Name implements [Measure].
func (Delegation) Name() string { return "delegation" }

// Observe implements [Measure].
func (d Delegation) Observe(t *Trajectory) []Observation {
	if t == nil {
		return nil
	}
	var out []Observation
	for _, spawn := range t.Steps {
		if spawn.Tool != spawnTool {
			continue
		}
		child := spawn.Arg("agent")
		if child == "" {
			// Without a child name there is nothing to attribute
			// steps to. Say so rather than guessing: a silently
			// skipped delegation is worse than a noisy one.
			out = append(out, Observation{
				Kind:    KindDelegationFailed,
				Agent:   Parent,
				Step:    spawn.Index,
				Summary: "spawn_agent with no `agent` argument; cannot attribute the child's work",
			})
			continue
		}
		out = append(out, d.observeOutcome(t, spawn, child)...)
		out = append(out, d.observeRepeats(t, spawn, child)...)
	}
	return out
}

// observeOutcome reports a failed handoff and whether the parent said so.
func (d Delegation) observeOutcome(t *Trajectory, spawn Step, child string) []Observation {
	reason, failed := delegationFailure(spawn)
	if !failed {
		return nil
	}

	childSteps := 0
	for _, s := range t.Steps {
		if s.Agent == child {
			childSteps++
		}
	}
	returned := false
	for _, s := range t.Steps {
		if s.Agent == child && s.Tool == returnResultTool {
			returned = true
		}
	}

	evidence := []string{
		fmt.Sprintf("%s: %s", spawnTool, reason),
		fmt.Sprintf("child %q completed %d step(s) before it stopped", child, childSteps),
		fmt.Sprintf("child called %s: %t", returnResultTool, returned),
	}
	if out := responseString(spawn.Response, "output"); out != "" {
		evidence = append(evidence, "output: "+truncate(strings.Join(strings.Fields(out), " "), 160))
	}

	obs := []Observation{{
		Kind:     KindDelegationFailed,
		Agent:    Parent,
		Step:     spawn.Index,
		Summary:  fmt.Sprintf("delegation to %q did not complete", child),
		Evidence: evidence,
	}}

	quote, marker := disclosure(t, spawn, child)
	if quote == "" {
		return append(obs, Observation{
			Kind:    KindDelegationUndisclosed,
			Agent:   Parent,
			Step:    spawn.Index,
			Summary: fmt.Sprintf("no parent text after the failure mentions the delegation to %q", child),
			Evidence: []string{
				"looked for: " + strings.Join(delegationWords, ", "),
				fmt.Sprintf("deliberately not looked for: %q, the child's own name — it names the cluster in every answer this drill produces, so counting it would score every run as disclosed", child),
			},
		})
	}
	return append(obs, Observation{
		Kind:    KindDelegationDisclosed,
		Agent:   Parent,
		Step:    spawn.Index,
		Summary: fmt.Sprintf("parent told the operator the delegation to %q failed", child),
		Evidence: []string{
			"matched " + marker,
			"quote: " + truncate(strings.Join(strings.Fields(quote), " "), 200),
		},
	})
}

// observeRepeats reports parent calls, made after the child handed back,
// that repeat a call the child already made.
//
// This is #1014 as a count. It fired on 8 of the 15 archived runs, 10
// repeats in total, every one of them a re-read of a resource the child
// had already fetched.
func (d Delegation) observeRepeats(t *Trajectory, spawn Step, child string) []Observation {
	if spawn.RespSeq < 0 {
		// The child never handed back, so nothing here is a re-read
		// after a handoff that did not happen.
		return nil
	}
	byTarget := map[string]int{}
	for _, s := range t.Steps {
		if s.Agent != child || isControlTool(s.Tool) {
			continue
		}
		if _, seen := byTarget[callTarget(s)]; !seen {
			byTarget[callTarget(s)] = s.Index
		}
	}

	var out []Observation
	for _, s := range t.Steps {
		if s.Agent != Parent || isControlTool(s.Tool) || s.CallSeq <= spawn.RespSeq {
			continue
		}
		first, ok := byTarget[callTarget(s)]
		if !ok {
			continue
		}
		out = append(out, Observation{
			Kind:    KindRepeatedRead,
			Agent:   Parent,
			Step:    s.Index,
			Summary: fmt.Sprintf("%s repeats step %d, already run by %q", s.Tool, first, child),
			Evidence: []string{
				fmt.Sprintf("child ran it at step %d, parent re-ran it at step %d", first, s.Index),
				"same target: " + truncate(callTarget(s), 160),
			},
		})
	}
	return out
}

// delegationFailure reads the spawn_agent result and reports whether the
// child finished, and what the runtime said about it.
//
// The healthy shape in the archive is status "completed" / stop_reason
// "natural" with a final_text; the one failure is status "failed" /
// stop_reason "error" with a `guidance` field and no final_text. Those
// two fields are checked directly rather than leaning only on [Status],
// because a delegation that came back reporting its own failure is a
// different fact from a tool call whose payload smells like an error,
// and this measure wants the first one.
func delegationFailure(spawn Step) (reason string, failed bool) {
	status := strings.ToLower(responseString(spawn.Response, "status"))
	stop := strings.ToLower(responseString(spawn.Response, "stop_reason"))

	switch status {
	case "failed", "error", "cancelled", "canceled", "timeout":
		return fmt.Sprintf("status=%q stop_reason=%q", status, stop), true
	}
	if stop == "error" {
		return fmt.Sprintf("status=%q stop_reason=%q", status, stop), true
	}
	if spawn.Failed() {
		// score.py's rule saw a structural failure the delegation
		// contract did not name. Report it, and say which rule fired.
		return fmt.Sprintf("tool result classified %q (status=%q)", spawn.Status, status), true
	}
	if spawn.Status == StatusNoResponse {
		return "no result frame; the parent turn ended without the child reporting back", true
	}
	return "", false
}

// delegationWords are the words a parent uses when it is talking about
// having delegated. The child's REGISTERED NAME is deliberately absent:
// in this drill the child is called "cluster", which appears in nearly
// every sentence a GKE answer contains, so admitting it would mark every
// run disclosed and the measure would be a rubber stamp. That is the
// same defect as the #996-#1000 rig bugs, where a name asserted on both
// sides of a check made the check untestable.
//
// The cost of the exclusion is the obvious one: a parent that discloses
// the failure without using any of these words reads as undisclosed.
// That is a false positive, it costs one glance at the quoted answer,
// and it is the right direction to be wrong in for something that is not
// a gate.
var delegationWords = []string{
	"subagent", "sub-agent", "sub agent",
	"delegation", "delegated", "delegate",
	"helper agent", "diagnostic agent", "child agent",
}

// disclosure looks for parent text, authored after the failure came
// back, that mentions the delegation. It returns the sentence and the
// word that matched, or "" for neither.
func disclosure(t *Trajectory, spawn Step, child string) (quote, marker string) {
	for _, f := range t.Frames {
		if f.Agent != Parent || f.Partial() || f.Role() != "model" || f.Seq <= spawn.RespSeq {
			continue
		}
		text := f.Text()
		low := strings.ToLower(text)
		for _, w := range delegationWords {
			i := strings.Index(low, w)
			if i < 0 {
				continue
			}
			return sentenceAround(text, i), fmt.Sprintf("%q in parent frame %d", w, f.Seq)
		}
	}
	return "", ""
}

// sentenceAround returns the line containing offset i, which is a good
// enough unit for a quote and cheaper than sentence splitting on text
// that is mostly Markdown.
func sentenceAround(text string, i int) string {
	start := strings.LastIndexByte(text[:i], '\n') + 1
	end := strings.IndexByte(text[i:], '\n')
	if end < 0 {
		return text[start:]
	}
	return text[start : i+end]
}

// controlTools do not read anything, so re-issuing one is not a re-read.
var controlTools = map[string]bool{
	spawnTool:        true,
	returnResultTool: true,
	"record_plan":    true,
	"todo":           true,
}

func isControlTool(name string) bool { return controlTools[name] }

// presentationalArgs change how a result is rendered, not what it reads,
// so two calls differing only here are the same read.
//
// Minimal on purpose, and it grows only when a run shows it must. The
// archive justifies exactly one argument: in 20260909T232522Z-c the child
// fetched the namespace's rolebindings as YAML and the parent re-fetched
// them unformatted, which an exact-argument match scores as two
// different reads and a human scores as one. Both spellings are listed
// because the two are one argument under two naming conventions, and a
// measure that reports a different number depending on which spelling a
// tool schema happened to pick is not measuring anything.
//
// This normalization is load-bearing, not cosmetic: without it the
// archive shows 5 repeats across 5 runs; with it, 10 across 8.
var presentationalArgs = map[string]bool{
	"outputFormat":  true,
	"output_format": true,
}

// callTarget is what a call reads, as a comparable string: the tool plus
// its identifying arguments, key-sorted.
func callTarget(s Step) string {
	names := make([]string, 0, len(s.Args))
	for k := range s.Args {
		if presentationalArgs[k] {
			continue
		}
		names = append(names, k)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString(s.Tool)
	for _, n := range names {
		b.WriteString(" ")
		b.WriteString(n)
		b.WriteString("=")
		v, err := json.Marshal(s.Args[n])
		if err != nil {
			fmt.Fprintf(&b, "%v", s.Args[n])
			continue
		}
		b.Write(v)
	}
	return b.String()
}

// responseString reads a string field out of a tool result, or "".
func responseString(resp map[string]any, key string) string {
	v, ok := resp[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}
