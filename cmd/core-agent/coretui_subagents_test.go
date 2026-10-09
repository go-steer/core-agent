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

//go:build !no_tui

package main

import (
	"context"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/background"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// TestSubagentInfos_CarriesLastReport pins the in-process TUI's half of
// #1283. This adapter reads the handles directly rather than going
// through ListSubagents, and before the fix it set LastReport only from
// a run error, so the running-tasks bar showed no text for any
// subagent that did not fail.
func TestSubagentInfos_CarriesLastReport(t *testing.T) {
	t.Parallel()
	mgr, err := background.NewManager(background.WithProvider(mock.NewEcho(), "echo"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	if _, err := agent.New(mustEchoModel(t), agent.WithBackgroundManager(mgr)); err != nil {
		t.Fatalf("agent.New(parent): %v", err)
	}

	h, err := mgr.Spawn(context.Background(), "", background.Spec{Name: "kid", SystemPrompt: "echo", Goal: "say the word lighthouse"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("subagent never finished")
	}

	infos := subagentInfos(mgr.List())
	if len(infos) != 1 {
		t.Fatalf("infos = %+v, want one row", infos)
	}
	want := h.LastReport()
	if want == "" {
		t.Fatalf("the echo subagent finished with no report at all; this test can't tell the adapter from the handle")
	}
	if infos[0].LastReport != want {
		t.Errorf("LastReport = %q, want the handle's report %q", infos[0].LastReport, want)
	}
}
