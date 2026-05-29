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

func TestNewBackgroundSpawnTools_RegistersAll(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	tools := NewBackgroundSpawnTools(mgr)
	if len(tools) != 4 {
		t.Fatalf("expected 4 tools (spawn/list/check/stop); got %d", len(tools))
	}
	got := []string{}
	for _, t := range tools {
		got = append(got, t.Name())
	}
	want := []string{"spawn_agent", "list_agents", "check_agent", "stop_agent"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("tools[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestSpawnTool_NameAndDescription(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	tool := NewSpawnAgentTool(mgr)
	if tool.Name() != "spawn_agent" {
		t.Errorf("tool name = %q, want spawn_agent", tool.Name())
	}
	if !strings.Contains(tool.Description(), "background subagent") {
		t.Errorf("description should mention background subagent; got %q", tool.Description())
	}
}

func TestReportTool_ConstructorsReturnNonNil(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	if newReportAlertTool(mgr, "x").Name() != "report_alert" {
		t.Errorf("report_alert name mismatch")
	}
	if newReportCompletedTool(mgr, "x").Name() != "report_completed" {
		t.Errorf("report_completed name mismatch")
	}
}
