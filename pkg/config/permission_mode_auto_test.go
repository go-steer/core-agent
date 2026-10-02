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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// permissions.mode "auto" is selectable (#1175 phase 4), and needs
// what makes it auto: an approver (permissions.auto), and an
// approval_timeout so an escalated call cannot wait forever (decision
// 12). Missing either is a startup error naming the field.
func TestValidate_PermissionModeAuto(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		auto    *AutoApproverConfig
		timeout string
		want    string // "" = valid
	}{
		{"approver and timeout", &AutoApproverConfig{Eligible: []string{"read_file:*"}}, "5m", ""},
		{"no approver", nil, "5m", "permissions.auto"},
		{"no timeout", &AutoApproverConfig{}, "", "permissions.approval_timeout"},
		{"bad timeout", &AutoApproverConfig{}, "soon", "approval_timeout"},
	}
	for _, tc := range cases {
		c := DefaultConfig()
		c.Permissions.Mode = PermissionModeAuto
		c.Permissions.Auto = tc.auto
		c.Permissions.ApprovalTimeout = tc.timeout
		err := c.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: Validate() = %v, want nil", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: Validate() = %v, want an error naming %q", tc.name, err, tc.want)
		}
	}
}

// The auto block is accepted, and validated, in any mode: a runtime
// switch to auto (phase 4) uses the approver configured here.
func TestValidate_PermissionsAuto(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		auto *AutoApproverConfig
		want string // "" = valid
	}{
		{"absent", nil, ""},
		{"empty block", &AutoApproverConfig{}, ""},
		{"full", &AutoApproverConfig{Model: "m", Timeout: "45s", Eligible: []string{"bash:go test *"}, TaskFrom: []string{"alice@example.com"}, InstructionsFile: "approver.md"}, ""},
		{"bad timeout", &AutoApproverConfig{Timeout: "soon"}, "permissions.auto.timeout"},
		{"zero timeout", &AutoApproverConfig{Timeout: "0s"}, "must be positive"},
		{"negative timeout", &AutoApproverConfig{Timeout: "-30s"}, "must be positive"},
		{"blank eligible entry", &AutoApproverConfig{Eligible: []string{"read_file:*", " "}}, "permissions.auto.eligible[1]"},
		{"blank task_from entry", &AutoApproverConfig{TaskFrom: []string{"alice", ""}}, "permissions.auto.task_from[1]"},
		{"padded task_from entry", &AutoApproverConfig{TaskFrom: []string{" alice"}}, "match exactly"},
		{"blank instructions file", &AutoApproverConfig{InstructionsFile: "  "}, "instructions_file"},
	}
	for _, tc := range cases {
		c := DefaultConfig()
		c.Permissions.Auto = tc.auto
		err := c.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: Validate() = %v, want nil", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: Validate() = %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestAutoApproverConfig_ResolvedTimeout(t *testing.T) {
	t.Parallel()
	if d, err := (AutoApproverConfig{}).ResolvedTimeout(); err != nil || d != DefaultAutoApproverTimeout {
		t.Errorf("unset timeout = %v, %v; want %v", d, err, DefaultAutoApproverTimeout)
	}
	if d, err := (AutoApproverConfig{Timeout: "2m"}).ResolvedTimeout(); err != nil || d != 2*time.Minute {
		t.Errorf("2m timeout = %v, %v", d, err)
	}
}

func TestAutoApproverConfig_InstructionsPath(t *testing.T) {
	t.Parallel()
	abs := filepath.Join(string(filepath.Separator), "etc", "approver.md")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	cases := []struct{ file, agentsDir, want string }{
		{"", "/proj/.agents", ""},
		{"approver.md", "/proj/.agents", filepath.Join("/proj/.agents", "approver.md")},
		{"../policy/approver.md", "/proj/.agents", filepath.Join("/proj", "policy", "approver.md")},
		{abs, "/proj/.agents", abs},
		{"approver.md", "", "approver.md"},
		{"~/policy/approver.md", "/proj/.agents", filepath.Join(home, "policy", "approver.md")},
	}
	for _, tc := range cases {
		if got := (AutoApproverConfig{InstructionsFile: tc.file}).InstructionsPath(tc.agentsDir); got != tc.want {
			t.Errorf("InstructionsPath(%q, %q) = %q, want %q", tc.file, tc.agentsDir, got, tc.want)
		}
	}
}
