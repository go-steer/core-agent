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

package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// permissions.plan_mode (#215) decouples the plan artifact from gate
// enforcement. ResolvedPlanMode is the single reader every consumer
// must go through; the two raw fields disagreeing is the failure this
// file exists to prevent.
func TestResolvedPlanMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		mode         string
		bool_        bool
		want         string
		wantTool     bool
		wantGateArmd bool
	}{
		{"both unset", "", false, PlanModeOff, false, false},
		{"deprecated bool only", "", true, PlanModeRequired, true, true},
		{"advisory", PlanModeAdvisory, false, PlanModeAdvisory, true, false},
		{"required", PlanModeRequired, false, PlanModeRequired, true, true},
		{"explicit off", PlanModeOff, false, PlanModeOff, false, false},
		{
			// plan_mode outranks the deprecated bool, so a config
			// mid-migration resolves to the field the operator most
			// recently wrote. The genuinely contradictory pair
			// (off + true) is rejected by Validate instead.
			"advisory outranks the deprecated bool",
			PlanModeAdvisory, true, PlanModeAdvisory, true, false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := PermissionsConfig{PlanMode: tc.mode, RequirePlanArtifact: tc.bool_}
			if got := p.ResolvedPlanMode(); got != tc.want {
				t.Errorf("ResolvedPlanMode() = %q, want %q", got, tc.want)
			}
			if got := p.PlanToolRegistered(); got != tc.wantTool {
				t.Errorf("PlanToolRegistered() = %v, want %v", got, tc.wantTool)
			}
			if got := p.PlanGateArmed(); got != tc.wantGateArmd {
				t.Errorf("PlanGateArmed() = %v, want %v", got, tc.wantGateArmd)
			}
		})
	}
}

// The property the whole issue turns on: advisory registers the tool
// WITHOUT arming the gate. Every other mode has the two moving
// together, which is why the bool was sufficient before and isn't now.
func TestResolvedPlanMode_AdvisoryIsTheOnlyModeThatSplitsThem(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{PlanModeOff, PlanModeAdvisory, PlanModeRequired} {
		p := PermissionsConfig{PlanMode: mode}
		split := p.PlanToolRegistered() != p.PlanGateArmed()
		if want := mode == PlanModeAdvisory; split != want {
			t.Errorf("plan_mode=%q: tool-registered(%v) != gate-armed(%v) is %v, want %v",
				mode, p.PlanToolRegistered(), p.PlanGateArmed(), split, want)
		}
	}
}

func TestValidate_PlanMode(t *testing.T) {
	t.Parallel()
	cases := []struct {
		mode    string
		wantErr bool
	}{
		{"", false},
		{PlanModeOff, false},
		{PlanModeAdvisory, false},
		{PlanModeRequired, false},
		{"Advisory", true},
		{"warn", true},
		{"true", true},
	}
	for _, tc := range cases {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			t.Parallel()
			c := DefaultConfig()
			c.Permissions.PlanMode = tc.mode
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate() with plan_mode=%q: got nil, want error", tc.mode)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate() with plan_mode=%q: got %v, want nil", tc.mode, err)
			}
		})
	}
}

// A config that says both "off" and "true" is a half-done migration.
// Silently picking a winner is how a gate an operator believes is armed
// turns out not to be, so Validate makes them say it once.
func TestValidate_PlanModeOffContradictsTheDeprecatedBool(t *testing.T) {
	t.Parallel()
	c := DefaultConfig()
	c.Permissions.PlanMode = PlanModeOff
	c.Permissions.RequirePlanArtifact = true
	err := c.Validate()
	if err == nil {
		t.Fatal("plan_mode=off + require_plan_artifact=true must be rejected")
	}
	if !strings.Contains(err.Error(), "require_plan_artifact") || !strings.Contains(err.Error(), "plan_mode") {
		t.Errorf("error should name both fields so the operator knows what to delete: %v", err)
	}

	// The non-contradictory overlaps stay legal — advisory/required
	// alongside the old bool is a normal mid-migration config.
	for _, mode := range []string{PlanModeAdvisory, PlanModeRequired} {
		c := DefaultConfig()
		c.Permissions.PlanMode = mode
		c.Permissions.RequirePlanArtifact = true
		if err := c.Validate(); err != nil {
			t.Errorf("plan_mode=%q + require_plan_artifact=true should be legal: %v", mode, err)
		}
	}
}

func TestPlanMode_UnmarshalsFromJSON(t *testing.T) {
	t.Parallel()
	var c Config
	if err := json.Unmarshal([]byte(`{"permissions":{"plan_mode":"advisory"}}`), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Permissions.PlanMode != PlanModeAdvisory {
		t.Errorf("permissions.plan_mode: got %q, want %q", c.Permissions.PlanMode, PlanModeAdvisory)
	}
	if c.Permissions.ResolvedPlanMode() != PlanModeAdvisory {
		t.Errorf("ResolvedPlanMode() = %q, want %q", c.Permissions.ResolvedPlanMode(), PlanModeAdvisory)
	}
}

// omitempty on both fields: a default config must not start writing a
// plan_mode key into every .agents/config.json the persist helpers
// rewrite.
func TestPlanMode_DefaultConfigMarshalsClean(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(DefaultConfig().Permissions)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "plan_mode") {
		t.Errorf("default permissions block emits plan_mode: %s", body)
	}
}
