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

package autonomous

import (
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/internal/testutil"
)

// The rationale for the ban list lives with the list itself, in
// internal/testutil.ModelFacingBans (#909). This package owns the
// SECOND of the two return tools in the repo, and it is the one that
// was wrong: the default report_done description asked for "a
// one-sentence detail explaining what you accomplished" — an
// accomplishment summary — while the sibling return tool one package
// over (background.subagentDoneToolDescription) explicitly forbids the
// status-line genre. Two return tools, one repo, opposite instructions.
//
// Both branches of buildDoneTools are swept: the lifecycle path (no
// WithReturnTool, which is what every library consumer of
// autonomous.Run gets by default, including dev/uat/scheduled-monitor)
// and the result path.
func TestDoneToolTextIsDeploymentNeutral(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  autoConfig
	}{
		{"lifecycle default", defaultAutoConfig()},
		{"result path default", func() autoConfig {
			c := defaultAutoConfig()
			c.returnTool = &ReturnToolConfig{Aliases: []string{"report_done"}}
			return c
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			tools, err := buildDoneTools(&cfg, make(chan string, 1))
			if err != nil {
				t.Fatalf("buildDoneTools: %v", err)
			}
			if len(tools) == 0 {
				t.Fatal("no done tool built: the sweep would pass vacuously")
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
		})
	}
}

// The ban list catches the genre words but cannot assert the positive
// obligation, and the positive obligation is the whole fix: the two
// return tools must agree that the argument carries findings, not a
// description of work done.
func TestDefaultDoneToolDescriptionAsksForFindings(t *testing.T) {
	t.Parallel()
	desc := defaultAutoConfig().doneToolDescription
	if !strings.Contains(desc, "findings") {
		t.Errorf("report_done's default description does not name a content obligation:\n  %s", desc)
	}
	if !strings.Contains(strings.ToLower(desc), "status line") {
		t.Errorf("report_done's default description does not name the failure mode it prevents:\n  %s", desc)
	}
	// The content obligation is the only thing #909 changed here. The
	// ARGUMENT NAMES had to stay, and the first draft of the fix got
	// this wrong: it borrowed `result` from the sibling return tool, but
	// this branch builds a coretools.NewLifecycleTool whose args are
	// {state, detail}. `state` is required and restricted to {"done"}
	// (lifecycle.go:124), and NewLifecycleTool's " Allowed states: ..."
	// clause is appended only when the description is empty — which this
	// one is not, so this string is the model's only source for the
	// required value. UndeclaredArgRefs is the structural guard;
	// these two are the positive assertions it cannot make.
	if !strings.Contains(desc, `state="done"`) {
		t.Errorf("report_done's description dropped the only mention of the required state value:\n  %s", desc)
	}
	if !strings.Contains(desc, "detail") {
		t.Errorf("report_done's description does not name the argument the findings go in:\n  %s", desc)
	}
}

// The result path is where `result` lives, and it is a different tool
// with a different arg struct. Asserting the two do NOT share argument
// names is what stops the next edit from harmonising the wording of two
// tools that only look alike.
func TestDoneToolArgNamesDifferByBranch(t *testing.T) {
	t.Parallel()
	if desc := defaultAutoConfig().doneToolDescription; strings.Contains(desc, "result argument") {
		t.Errorf("lifecycle report_done points at `result`, which only the WithReturnTool path declares:\n  %s", desc)
	}
	if !strings.Contains(defaultReturnToolDescription, "result") {
		t.Errorf("return tool's default description does not name its own `result` argument:\n  %s", defaultReturnToolDescription)
	}
}
