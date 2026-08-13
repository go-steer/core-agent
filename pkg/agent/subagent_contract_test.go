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
)

// TestSubagentReturnContract_NamesOnlyToolsThatExist is the property
// that makes this a renderer rather than a constant: a subagent invoked
// synchronously as a parent tool has no return tool, and telling it to
// "call return_result" would state a gesture the runtime never
// registered — the unenforced-claim pattern this milestone exists to
// remove. Both renderings must still say the thing that is always true:
// the last message is what the delegating agent receives.
func TestSubagentReturnContract_NamesOnlyToolsThatExist(t *testing.T) {
	t.Parallel()

	withTool := SubagentReturnContract("return_result")
	if !strings.Contains(withTool, "`return_result`") {
		t.Errorf("async rendering does not name its return tool:\n%s", withTool)
	}

	noTool := SubagentReturnContract("")
	if strings.Contains(noTool, "return_result") {
		t.Errorf("sync rendering names a tool that isn't registered on that path:\n%s", noTool)
	}
	if strings.Contains(noTool, "call `") {
		t.Errorf("sync rendering tells the model to call something:\n%s", noTool)
	}

	// Shared invariants — the parts that hold on every delegation path.
	for name, got := range map[string]string{"with-tool": withTool, "no-tool": noTool} {
		for _, want := range []string{
			"## Returning your result",
			"value returned to it",
			"LAST message is the findings",
			"not a description of your work",
			"A partial result is useful",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("%s rendering missing %q:\n%s", name, want, got)
			}
		}
	}
}

// TestSubagentReturnContract_TrimsToolName guards the empty-vs-blank
// distinction: a caller threading a config value through can pass
// whitespace, and " " must read as "no return tool", not as a tool
// literally named " ".
func TestSubagentReturnContract_TrimsToolName(t *testing.T) {
	t.Parallel()
	if SubagentReturnContract("   ") != SubagentReturnContract("") {
		t.Error("a blank tool name must render as the no-tool contract")
	}
	if !strings.Contains(SubagentReturnContract("  finish_up  "), "`finish_up`") {
		t.Error("tool name should be trimmed, not embedded with its padding")
	}
}
