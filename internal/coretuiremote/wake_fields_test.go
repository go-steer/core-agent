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

package coretuiremote

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// TestSubagentsToCoreTui_CarriesWake is the attach-mode half of #1283:
// a sleeping subagent's wake survives the /agents wire and reaches
// core-tui's roster, so the bar counts down to it. Round-tripped through
// JSON because the wire is where a field gets lost.
func TestSubagentsToCoreTui_CarriesWake(t *testing.T) {
	t.Parallel()
	wake := time.Date(2026, 10, 9, 15, 30, 0, 0, time.UTC)
	sent := []attach.AgentInfo{
		{ID: "monitor", Name: "monitor", Status: attach.AgentStatusRunning, NextWakeAt: wake, WakeDetail: "polling cluster-A"},
		{ID: "cluster", Name: "cluster", Status: attach.AgentStatusRunning, LastReport: "reading events"},
	}
	b, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	var got []attach.AgentInfo
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}

	rows := subagentsToCoreTui(got)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	if !rows[0].NextWakeAt.Equal(wake) || rows[0].WakeDetail != "polling cluster-A" {
		t.Errorf("sleeping row = %+v, want NextWakeAt %v and its reason — the bar would count up as if it were working", rows[0], wake)
	}
	if !rows[1].NextWakeAt.IsZero() || rows[1].WakeDetail != "" {
		t.Errorf("working row = %+v, want no wake — zero is how core-tui knows it is not asleep", rows[1])
	}
}
