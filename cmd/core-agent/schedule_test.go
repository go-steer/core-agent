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

package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkagent "google.golang.org/adk/v2/agent"
	adktool "google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-agent/v3/internal/effects"
	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/tools"
)

func TestStartDaemonSchedule_SeedsAndRunsBehindLease(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dbPath := filepath.Join(t.TempDir(), "sched.db")
	h, err := eventlog.Open(ctx, sqlite.Open(dbPath))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	defer func() { _ = h.Close() }()

	fired := make(chan compose.ScheduledFire, 1)
	sh, err := startDaemonSchedule(ctx, daemonScheduleDeps{
		EventLog: h,
		AppName:  "core-agent",
		Workload: "nightly-audit",
		Schedule: &config.ScheduleConfig{
			Every:  "1s",
			Jitter: "0s",
			Prompt: "Run audit.",
		},
		Fire: func(_ context.Context, f compose.ScheduledFire) error {
			select {
			case fired <- f:
			default:
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("startDaemonSchedule: %v", err)
	}
	defer sh.Stop()

	if !sh.Drives || sh.Trigger == nil {
		t.Fatalf("handle = %+v, want Drives=true and non-nil Trigger", sh)
	}
	rec, err := h.Schedule(ctx, "core-agent", "nightly-audit")
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if rec == nil || rec.Interval != time.Second {
		t.Fatalf("Schedule record = %+v, want seeded 1s interval", rec)
	}

	select {
	case f := <-fired:
		if f.FireNum != 1 {
			t.Errorf("FireNum = %d, want 1", f.FireNum)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for scheduled fire")
	}
}

type fakeHintedToolset struct {
	tools []adktool.Tool
}

func (f fakeHintedToolset) Name() string { return "fake-mcp" }
func (f fakeHintedToolset) Tools(adkagent.ReadonlyContext) ([]adktool.Tool, error) {
	return f.tools, nil
}

type roWrappedTool struct {
	name string
	ro   bool
}

func (w roWrappedTool) Name() string        { return w.name }
func (w roWrappedTool) Description() string { return w.name }
func (w roWrappedTool) IsLongRunning() bool { return false }
func (w roWrappedTool) ReadOnlyHint() bool  { return w.ro }

var _ adktool.Tool = roWrappedTool{}
var _ tools.ReadOnlyHinter = roWrappedTool{}

func TestBuildDaemonEffectControls_HonorsMCPReadOnlyHintsAndPrefixedPolicies(t *testing.T) {
	t.Parallel()
	ts := fakeHintedToolset{
		tools: []adktool.Tool{roWrappedTool{name: "k8s_list_pods", ro: true}},
	}

	f := false
	tr := true
	cfg := &config.Config{
		Tools: config.ToolsConfig{
			Policies: []config.ToolPolicyConfig{
				{Name: "get_nodes", Mutating: &f},
				{Name: "scale_deployment", Mutating: &tr},
				{Name: "file", Mutating: &f},
				{Name: "confirmation", Mutating: &tr},
			},
		},
		Subagents: []config.SubagentSpec{{Name: "diagnoser"}},
	}

	pred, subNames := buildDaemonEffectControls(cfg, ts)
	if !subNames["diagnoser"] {
		t.Errorf("subNames = %v, want diagnoser=true", subNames)
	}
	if got := pred("k8s_list_pods"); got != effects.ClassReadOnly {
		t.Errorf("pred(k8s_list_pods) = %v, want ClassReadOnly from MCP ReadOnlyHint", got)
	}
	if got := pred("k8s_get_nodes"); got != effects.ClassReadOnly {
		t.Errorf("pred(k8s_get_nodes) = %v, want ClassReadOnly from unprefixed policy match", got)
	}
	if got := pred("k8s_scale_deployment"); got != effects.ClassMutating {
		t.Errorf("pred(k8s_scale_deployment) = %v, want ClassMutating from unprefixed policy match", got)
	}
	if got := pred("k8s_unknown_write"); got != effects.ClassMutating {
		t.Errorf("pred(k8s_unknown_write) = %v, want ClassMutating default-deny", got)
	}
	if got := pred("write_file"); got != effects.ClassMutating {
		t.Errorf("pred(write_file) = %v, want ClassMutating (builtin must not suffix-match policy \"file\")", got)
	}
	if got := pred("adk_request_confirmation"); got != effects.ClassReadOnly {
		t.Errorf("pred(adk_request_confirmation) = %v, want ClassReadOnly (control call must not suffix-match policy \"confirmation\")", got)
	}
}
