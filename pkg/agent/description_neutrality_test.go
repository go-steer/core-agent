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
	"strings"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/internal/testutil"
)

// The rationale for the ban list lives with the list itself, in
// internal/testutil.ModelFacingBans (#909). This package is the FIFTH
// that registers model-facing tools, and #909 swept only four — so the
// one tool with a confirmed live incident behind it, mark_task_done,
// was the one tool with no re-drift guard (#919). Its strings were
// rewritten carefully in #905 and are clean; nothing was checking that
// they stayed clean, which is the wrong way round for the tool that
// motivated the list.
//
// Both tools this package registers are swept. mark_task_done is the
// obvious one. The subagent tool is here for its DEFAULT description
// and its arg schema: a caller supplying Name/Description gets its own
// text (and owns it), but a caller that supplies neither gets in-tree
// prose, and the `request` arg schema is in-tree on every path.
func TestAgentToolTextIsDeploymentNeutral(t *testing.T) {
	t.Parallel()
	inner, err := New(minimalLLM{}, WithName("research"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// No Name/Description override, and Inner carries no description
	// either, so this is the defaultSubagentDesc path.
	sub, err := NewSubagentTool(SubagentOptions{Inner: inner})
	if err != nil {
		t.Fatalf("NewSubagentTool: %v", err)
	}
	tools := []tool.Tool{
		NewMarkTaskDoneTool(func() *Agent { return nil }),
		sub,
	}
	if len(tools) < 2 {
		t.Fatalf("swept %d tools, want the 2 this package registers: the sweep would pass vacuously", len(tools))
	}
	for _, tl := range tools {
		texts, scanned := testutil.ModelFacingText(tl)
		if !scanned {
			t.Errorf("tool %q exposes no arg schema to scan", tl.Name())
		}
		for _, text := range texts {
			for _, bad := range testutil.ModelFacingBanViolations(text) {
				t.Errorf("tool %q: %s\n  %s", tl.Name(), bad, text)
			}
		}
		refs, checked := testutil.UndeclaredArgRefs(tl)
		if !checked {
			t.Errorf("tool %q exposes no arg schema to cross-check its description against", tl.Name())
		}
		for _, ref := range refs {
			t.Errorf("tool %q description tells the model to set %q, which it does not declare — ADK validates with additionalProperties:false, so obeying is a hard error:\n  %s", tl.Name(), ref, tl.Description())
		}
	}
}

// Tool descriptions are not the only baked prose this package puts in
// front of a model, and the other two kinds are subject to the same
// rule for the same reason: a recipe author can neither see nor
// override them.
//
//   - Tool RESULT text. mark_task_done's repeat status is the one that
//     had to be written carefully — it is what a looping model reads
//     nine times in a row — and subagentPartial's header is read by a
//     parent deciding what to do with a truncated delegation.
//   - The delegation contract (#727), injected as instruction layer 5
//     on every subagent. It is the text that decides whether a subagent
//     returns findings or a status line, which is exactly what the ban
//     list is about.
//
// Deliberately NOT swept: the checkpoint and compaction instructions
// (defaultCheckpointHeader and friends). Those are summarizer prompts
// whose output the RUNTIME consumes, not the operator, so prescribing a
// document shape — "one paragraph max", the six fixed headings — is the
// job rather than the defect. The ban on "one-paragraph" is a ban on a
// TOOL telling the model what genre to write for its caller; a
// runtime-owned prompt for a runtime-owned artifact is the one place
// that shape belongs. Sweeping them would flag correct text and teach
// the next reader to disable the check.
func TestAgentBakedModelFacingProseIsDeploymentNeutral(t *testing.T) {
	t.Parallel()
	a, err := New(minimalLLM{}, WithName("worker"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first := markTaskDone(a, "shut the incident")
	repeat := markTaskDone(a, "shut the incident, again")
	texts := map[string]string{
		"mark_task_done status":            first.Status,
		"mark_task_done repeat status":     repeat.Status,
		"mark_task_done unbound status":    markTaskDone(nil, "").Status,
		"subagent partial header":          subagentPartial("findings", "research", "turn budget of 4"),
		"subagent contract (return tool)":  SubagentReturnContract("return_result"),
		"subagent contract (no done tool)": SubagentReturnContract(""),
	}
	if len(texts) < 6 {
		t.Fatalf("swept %d strings: the sweep would pass vacuously", len(texts))
	}
	for what, text := range texts {
		if strings.TrimSpace(text) == "" {
			t.Errorf("%s is empty — nothing was scanned", what)
			continue
		}
		for _, bad := range testutil.ModelFacingBanViolations(text) {
			t.Errorf("%s: %s\n  %s", what, bad, text)
		}
	}
}
