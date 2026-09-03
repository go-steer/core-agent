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

package background

import (
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/internal/testutil"
)

// The rationale for the ban list lives with the list itself, in
// internal/testutil.ModelFacingBans (#909). This package owns both
// halves of the delegation contract — the parent's spawn tools and the
// per-subagent report_alert — so a drift here desynchronises the two
// sides of a conversation the operator cannot see.
func TestBackgroundToolTextIsDeploymentNeutral(t *testing.T) {
	t.Parallel()
	mgr := &Manager{}
	tools := append(NewSpawnTools(mgr), newReportAlertTool(mgr, "probe"))
	if len(tools) < 3 {
		t.Fatalf("expected spawn_agent + stop_agent + report_alert, got %d", len(tools))
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
		// spawn_agent's description names `agent` and `wait` inline, so
		// this package is where an arg rename would silently orphan the
		// prose that explains it.
		refs, checked := testutil.UndeclaredArgRefs(tl)
		if !checked {
			continue
		}
		for _, ref := range refs {
			t.Errorf("tool %q description tells the model to set %q, which it does not declare:\n  %s", tl.Name(), ref, tl.Description())
		}
	}
}

// droppedAlert is not a tool description, but it is model-facing text
// on exactly the same footing: the parent's model reads it verbatim in
// its next prompt. It used to end "check its status (list_agents)".
// #625 removed list_agents as a model tool — its content is served by
// the pre-turn push digest — so this named a tool the model does not
// have, inside the one message it reads precisely when it is confused
// about a subagent's state. That is the #215/#758 defect in a third
// form (#909).
func TestDroppedAlertNamesNoUnregisteredTool(t *testing.T) {
	t.Parallel()
	text := droppedAlert(3).Text
	registered := map[string]bool{}
	for _, tl := range NewSpawnTools(&Manager{}) {
		registered[tl.Name()] = true
	}
	for _, name := range []string{"list_agents", "check_agent"} {
		if registered[name] {
			continue // re-registered since; the advice would be sound again
		}
		if strings.Contains(text, name) {
			t.Errorf("droppedAlert points the model at unregistered %q:\n  %s", name, text)
		}
	}
	// The rest of the sentence is the part that was always correct, and
	// dropping it would leave "some reports are missing" unactionable.
	if !strings.Contains(text, "ask it again") {
		t.Errorf("droppedAlert lost its actionable advice:\n  %s", text)
	}
}

// The two return tools in this repo must agree. background's is the
// exemplar the audit named; assert it keeps the properties that made it
// one, so a future edit here cannot quietly re-open the disagreement
// that autonomous's default had.
func TestSubagentDoneDescriptionAsksForFindings(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"findings", "status line"} {
		if !strings.Contains(strings.ToLower(subagentDoneToolDescription), want) {
			t.Errorf("subagent done description lost %q:\n  %s", want, subagentDoneToolDescription)
		}
	}
}
