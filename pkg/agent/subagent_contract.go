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

import "strings"

// SubagentReturnContract renders the instruction block that tells a
// delegated subagent its output is a value returned to another agent
// (#727).
//
// returnTool is the name of the tool that hands a value back and ends
// the run. Pass "" for delegation paths that have no such tool — a
// subagent invoked synchronously as a tool returns its last message and
// nothing else — so the instruction never names a gesture the runtime
// hasn't registered.
//
// Why this is an instruction and not a tool description. [#641] put the
// same framing on the done tool's description, which is only read by a
// model that goes looking for that tool. The contract has to hold on
// every termination path — a natural stop, a budget cap, a watchdog
// halt, a sync invocation with no done tool at all — and on all of
// those the subagent's *last assistant text* is what the delegating
// agent receives. A subagent that thinks it is writing a status update
// for a human writes "standing by in a healthy, inactive state"; one
// that knows it is returning a value writes the findings.
//
// Install with WithExtraInstruction (layer 5), which appends whether or
// not the caller replaced layers 1–3 — the contract is a property of
// being a delegation, not of the harness baseline.
func SubagentReturnContract(returnTool string) string {
	var b strings.Builder
	b.WriteString("## Returning your result\n\n")
	b.WriteString("You were delegated this task by another agent, and your output is a value returned to it — ")
	b.WriteString("not a message to a human. Nobody will read your transcript and nobody will ask you a ")
	b.WriteString("follow-up question: whatever you hand back is all the delegating agent will ever have.\n\n")
	if rt := strings.TrimSpace(returnTool); rt != "" {
		b.WriteString("- Finish by calling `" + rt + "` with your findings.\n")
		b.WriteString("- If you stop for any other reason, make sure your LAST message is the findings themselves, ")
		b.WriteString("because that is what gets forwarded.\n")
	} else {
		b.WriteString("- Your LAST message is the findings the delegating agent receives — put them there, ")
		b.WriteString("not in an earlier turn.\n")
	}
	b.WriteString("- Write the answer, not a description of your work. \"Investigated the issue and identified ")
	b.WriteString("the cause\" tells the delegating agent nothing and forces it to redo the task; the root ")
	b.WriteString("cause, the evidence, and the proposed change tell it everything.\n")
	b.WriteString("- If you could not finish, return what you established and what remains unknown. ")
	b.WriteString("A partial result is useful; a status line is not.")
	return b.String()
}
