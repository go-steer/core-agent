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
	"strings"
	"testing"
)

// A misspelled target name would otherwise be discovered by the first
// prompt nobody answers — which is precisely the event it exists to
// report. Same reasoning as approval_timeout's eager parse, one field
// over.
func TestApprovalNotifyMustNameARegisteredTarget(t *testing.T) {
	t.Parallel()

	t.Run("unset is fine", func(t *testing.T) {
		c := DefaultConfig()
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("a registered name passes", func(t *testing.T) {
		c := DefaultConfig()
		c.Alerts = AlertsConfig{Targets: []AlertTarget{
			{Name: "oncall", URL: "https://example.test/hook", Template: AlertTemplateGeneric},
		}}
		c.Permissions.ApprovalNotify = "oncall"
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	t.Run("a typo is rejected and the real names are offered", func(t *testing.T) {
		c := DefaultConfig()
		c.Alerts = AlertsConfig{Targets: []AlertTarget{
			{Name: "oncall", URL: "https://example.test/hook", Template: AlertTemplateGeneric},
			{Name: "audit", URL: "https://example.test/audit", Template: AlertTemplateGeneric},
		}}
		c.Permissions.ApprovalNotify = "onkall"
		err := c.Validate()
		if err == nil {
			t.Fatal("want an error for an unregistered target")
		}
		for _, want := range []string{"onkall", "oncall", "audit"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should mention %q so the operator can see the typo: %v", want, err)
			}
		}
	})

	t.Run("an empty registry says so rather than listing nothing", func(t *testing.T) {
		c := DefaultConfig()
		c.Permissions.ApprovalNotify = "oncall"
		err := c.Validate()
		if err == nil {
			t.Fatal("want an error when there are no targets at all")
		}
		// "(have: )" would read as a mystery. The operator's mistake here
		// is a missing registry, not a wrong name.
		if !strings.Contains(err.Error(), "alerts.targets is empty") {
			t.Errorf("error should name the empty registry: %v", err)
		}
	})
}
