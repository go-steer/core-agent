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

package skills

import (
	"context"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// TestToolInfos_NamesTheToolsNotTheSkills is the distinction the
// method exists to draw. Two skills are installed, but the model does
// not call them by name — it calls the toolset's fixed tools and names
// a skill as an argument. An operator surface that reported Infos as
// the tool list (which /tools had no way to avoid before #767, since
// it reported neither) would show tools the agent cannot call.
func TestToolInfos_NamesTheToolsNotTheSkills(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSkill(t, dir, "cli-setup", "set up the CLI")
	writeSkill(t, dir, "triage", "triage an incident")

	got, err := LoadAll(context.Background(), dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Infos) != 2 {
		t.Fatalf("expected 2 skills, got %d", len(got.Infos))
	}

	toolInfos := got.ToolInfos()
	if len(toolInfos) == 0 {
		t.Fatal("ToolInfos() empty for a bundle with two skills")
	}
	for _, ti := range toolInfos {
		if ti.Name == "cli-setup" || ti.Name == "triage" {
			t.Errorf("ToolInfos() reported the SKILL %q as a tool", ti.Name)
		}
		if ti.Name == "" {
			t.Error("ToolInfos() reported a nameless tool")
		}
	}
	// The set is the ADK skill toolset's, so pin only the one tool
	// whose absence would mean the toolset changed shape underneath us
	// rather than the exact roster.
	var found bool
	for _, ti := range toolInfos {
		if ti.Name == "list_skills" {
			found = true
		}
	}
	if !found {
		var names []string
		for _, ti := range toolInfos {
			names = append(names, ti.Name)
		}
		t.Errorf("list_skills absent from ToolInfos(); got %v", names)
	}
}

// TestToolInfos_EmptyBundleHasNoTools — no skills means no toolset,
// and no toolset means no tools. A host that reported the three skill
// tools regardless would advertise a capability that isn't wired,
// which is the #759 failure mode in a different surface.
func TestToolInfos_EmptyBundleHasNoTools(t *testing.T) {
	t.Parallel()
	got, err := LoadAll(context.Background(), t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Empty() {
		t.Fatalf("expected an empty bundle, got %+v", got.Infos)
	}
	if ti := got.ToolInfos(); ti != nil {
		t.Errorf("ToolInfos() = %+v on an empty bundle, want nil", ti)
	}
}

// TestToolInfos_SurvivesTheGateWrapper — LoadAll wraps the toolset in
// tools.GateToolset when a gate is wired, and enumeration has to see
// through it. A wrapper that dropped Tools() would silently empty the
// skill section of /tools on every gated (i.e. every real) daemon.
func TestToolInfos_SurvivesTheGateWrapper(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSkill(t, dir, "cli-setup", "set up the CLI")

	gate := permissions.New(permissions.Options{Mode: permissions.ModeAsk})
	got, err := LoadAll(context.Background(), dir, "", gate)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ToolInfos()) == 0 {
		t.Fatal("ToolInfos() empty behind the gate wrapper")
	}
}
