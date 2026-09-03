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

package tools

import (
	"strings"
	"testing"

	"google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/internal/testutil"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// The rationale for the ban list lives with the list itself, in
// internal/testutil.ModelFacingBans (#909). This is the sweep over the
// built-in catalog — the largest of the four model-facing tool
// surfaces, and the only one whose descriptions are composed at
// registration time (bashDescription, recordPlanDescRequired,
// alert.buildDescription), which is why it has to read the built
// registry rather than the source constants.
func neutralityRegistry(t *testing.T) *Registry {
	t.Helper()
	cfg := config.DefaultConfig()
	// Everything on, so a phrase cannot hide behind a tool this build
	// happened not to register: fetch_url and alert are gated on their
	// own config, and record_plan on plan mode.
	cfg.URLScope.Allow = []string{"example.com"}
	cfg.Permissions.PlanMode = config.PlanModeRequired
	gate := permissions.New(permissions.Options{
		Mode:                permissions.ModeYolo,
		RequirePlanArtifact: cfg.Permissions.PlanGateArmed(),
	})
	reg, err := Build(cfg, gate, t.TempDir(), Default())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(reg.Tools) == 0 {
		t.Fatal("no tools registered: the sweep would pass vacuously")
	}
	return reg
}

func TestToolDescriptionsAreDeploymentNeutral(t *testing.T) {
	t.Parallel()
	for _, tl := range neutralityRegistry(t).Tools {
		texts, scanned := testutil.ModelFacingText(tl)
		if !scanned {
			t.Errorf("tool %q exposes no arg schema to scan; the sweep is blind to it", tl.Name())
		}
		for _, text := range texts {
			for _, bad := range testutil.ModelFacingBanViolations(text) {
				t.Errorf("tool %q: %s\n  %s", tl.Name(), bad, text)
			}
		}
	}
}

// A description that tells the model to set an argument the tool does
// not declare is worse than a badly-worded one: ADK validates with
// additionalProperties:false, so a model that obeys gets a hard
// validation error. Found by the #909 adversarial review, on a
// first-draft rewrite that borrowed the wrong return tool's arg name.
func TestToolDescriptionsNameOnlyDeclaredArgs(t *testing.T) {
	t.Parallel()
	for _, tl := range neutralityRegistry(t).Tools {
		refs, checked := testutil.UndeclaredArgRefs(tl)
		if !checked {
			continue // no arg schema; nothing to cross-check against
		}
		for _, ref := range refs {
			t.Errorf("tool %q description tells the model to set %q, which it does not declare:\n  %s", tl.Name(), ref, tl.Description())
		}
	}
}

// The sweep is only worth anything if it reaches arg schemas, which is
// where the confirmed damage lived: mark_task_done's `detail` tag, not
// its description, is what shaped the visible output (#905). A
// description-only walk would have passed clean on the tool that broke
// a live deployment.
func TestModelFacingTextReachesArgSchemas(t *testing.T) {
	t.Parallel()
	var plan tool.Tool
	for _, tl := range neutralityRegistry(t).Tools {
		if tl.Name() == "record_plan" {
			plan = tl
		}
	}
	if plan == nil {
		t.Fatal("record_plan not registered")
	}
	texts, scanned := testutil.ModelFacingText(plan)
	if !scanned {
		t.Fatal("record_plan's arg schema was not scanned")
	}
	// The `plan` arg's schema tag, which appears nowhere in the
	// description. If this stops being found the sweep has gone blind to
	// the half of the surface that matters most.
	const argTag = "the operator picks the shape"
	for _, s := range texts {
		if strings.Contains(s, argTag) {
			return
		}
	}
	t.Errorf("ModelFacingText(record_plan) never reached the plan arg schema; got %d strings:\n  %q", len(texts), texts)
}

// record_plan's description used to hand the model a document template
// — "typical shape: goal, files to change, approach, risks, test plan,
// out of scope" — while its own arg schema one screen up already
// deferred the shape to the recipe author. The description was
// overriding the very person it defers to, and an agent proposing an
// RBAC change has no "files to change" and no "test plan", so it
// fabricates them or pads the slots by restating the incident (#909).
//
// Asserted against the constant rather than the ban list because these
// slot names are wrong HERE and unremarkable anywhere else — a bash
// description may legitimately say "test".
func TestRecordPlanDescriptionDoesNotTemplateThePlan(t *testing.T) {
	t.Parallel()
	for _, slot := range []string{"typical shape", "files to change", "test plan", "out of scope"} {
		if strings.Contains(recordPlanDescCommon, slot) {
			t.Errorf("record_plan description supplies the slot %q; the arg schema defers the shape to the operator's AGENTS.md:\n  %s",
				slot, recordPlanDescCommon)
		}
	}
	if !strings.Contains(recordPlanDescCommon, "the operator's AGENTS.md picks the shape") {
		t.Errorf("record_plan description lost the sentence that defers the shape:\n  %s", recordPlanDescCommon)
	}
}
