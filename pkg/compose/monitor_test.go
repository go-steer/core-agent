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

// Originally derived from go-steer/mast.

package compose

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
)

type stubCollectTool struct {
	name       string
	runFn      func(ctx adkagent.Context, args any) (map[string]any, error)
	sawAgent   string
	sawSession string
	confirmErr error
	stateErr   error
}

func (s *stubCollectTool) Name() string                            { return s.name }
func (s *stubCollectTool) Description() string                     { return s.name }
func (s *stubCollectTool) IsLongRunning() bool                     { return false }
func (s *stubCollectTool) Declaration() *genai.FunctionDeclaration { return nil }

func (s *stubCollectTool) Run(ctx adkagent.Context, args any) (map[string]any, error) {
	s.sawAgent = ctx.AgentName()
	s.sawSession = ctx.SessionID()
	_ = ctx.ToolConfirmation()
	ctx.Actions().StateDelta["seen"] = true
	s.confirmErr = ctx.RequestConfirmation("approve?", nil)
	s.stateErr = ctx.State().Set("k", "v")
	if s.runFn != nil {
		return s.runFn(ctx, args)
	}
	return map[string]any{"output": `{"transitions":[],"degraded":false}`}, nil
}

type stubDigestWrapperTool struct {
	inner tool.Tool
}

func (w stubDigestWrapperTool) Name() string                            { return w.inner.Name() }
func (w stubDigestWrapperTool) Description() string                     { return w.inner.Description() }
func (w stubDigestWrapperTool) IsLongRunning() bool                     { return w.inner.IsLongRunning() }
func (w stubDigestWrapperTool) Declaration() *genai.FunctionDeclaration { return nil }
func (w stubDigestWrapperTool) Unwrap() tool.Tool                       { return w.inner }

func (w stubDigestWrapperTool) Run(_ adkagent.Context, _ any) (map[string]any, error) {
	return map[string]any{"digest": "TRUNCATED_DIGEST"}, nil
}

type stubCollectToolset struct {
	name  string
	tools []tool.Tool
}

func (ts *stubCollectToolset) Name() string { return ts.name }
func (ts *stubCollectToolset) Tools(_ adkagent.ReadonlyContext) ([]tool.Tool, error) {
	return ts.tools, nil
}

func openMonitorTestEventLog(t *testing.T) *eventlog.Handle {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "monitor.db")
	h, err := eventlog.Open(context.Background(), sqlite.Open(dbPath))
	if err != nil {
		t.Fatalf("eventlog.Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

func TestMonitorCollector_UnwrapsDigestWrapperAndDrivesCollectContext(t *testing.T) {
	t.Parallel()
	rawJSON := `{"transitions":[{"kind":"oom","edge":"opened","subject_key":"pod/prod/api/CrashLoopBackOff","summary":"OOMKilled"}],"degraded":false}`
	inner := &stubCollectTool{
		name: "lookout_diff",
		runFn: func(_ adkagent.Context, _ any) (map[string]any, error) {
			return map[string]any{"output": rawJSON}, nil
		},
	}
	// Wrap twice to verify unwrapTool loops through nested Unwrap() layers.
	wrapped := stubDigestWrapperTool{inner: stubDigestWrapperTool{inner: inner}}
	ts := &stubCollectToolset{name: "lookout", tools: []tool.Tool{wrapped}}

	collector := NewMonitorCollector(MonitorCollectorConfig{
		WorkloadName: "gke-triage",
		AppName:      "core-agent",
		UserID:       "daemon",
		BasePrompt:   "Triage transitions.",
		Monitor: &config.MonitorConfig{
			Collect:         &config.MonitorCollectConfig{Tool: "lookout_diff"},
			TransitionsFrom: "lookout_diff",
		},
		Toolsets: []tool.Toolset{ts},
	})

	tick := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	prompt, shouldWake := collector.PreFire(context.Background(), tick, 1)
	if !shouldWake {
		t.Fatal("PreFire shouldWake = false, want true when edge=opened is present")
	}
	if !strings.Contains(prompt, "pod/prod/api/CrashLoopBackOff") {
		t.Errorf("prompt = %q, want raw untruncated subject_key", prompt)
	}
	if inner.sawAgent != "mast:monitor" {
		t.Errorf("sawAgent = %q, want mast:monitor", inner.sawAgent)
	}
	if inner.confirmErr == nil {
		t.Error("RequestConfirmation succeeded; want error on collectContext")
	}
	if inner.stateErr == nil {
		t.Error("State().Set succeeded; want error on collectContext")
	}
}

func TestMonitorCollector_QuietCycleSkipsTurnWithZeroModelCalls(t *testing.T) {
	t.Parallel()
	collector := NewMonitorCollector(MonitorCollectorConfig{
		WorkloadName: "gke-triage",
		BasePrompt:   "Triage transitions.",
		Monitor: &config.MonitorConfig{
			Collect:         &config.MonitorCollectConfig{Tool: "lookout_diff"},
			TransitionsFrom: "lookout_diff",
		},
		RunTool: func(_ adkagent.Context, _ string, _ map[string]any) (map[string]any, error) {
			return map[string]any{"output": `{"transitions":[],"degraded":false}`}, nil
		},
	})

	tick := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	prompt, shouldWake := collector.PreFire(context.Background(), tick, 1)
	if shouldWake || prompt != "" {
		t.Fatalf("PreFire on quiet cycle = (%q, %v), want (\"\", false)", prompt, shouldWake)
	}
	parsed, failed := collector.LastCycle()
	if failed {
		t.Fatal("LastCycle failed = true on a valid quiet cycle")
	}
	if parsed == nil || len(parsed.Transitions) != 0 || parsed.Degraded {
		t.Errorf("LastCycle parsed = %+v, want empty transitions and Degraded=false", parsed)
	}
}

func TestMonitorCollector_AckedSubjectSuppressesWake(t *testing.T) {
	t.Parallel()
	hLog := openMonitorTestEventLog(t)
	ctx := context.Background()

	if err := hLog.RecordAck(ctx, "core-agent", eventlog.AckRecord{
		Workload:  "gke-triage",
		Subject:   "pod/prod/api/CrashLoopBackOff",
		By:        "alice@example.com",
		At:        time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC),
		Forwarded: true,
	}); err != nil {
		t.Fatalf("RecordAck: %v", err)
	}

	collector := NewMonitorCollector(MonitorCollectorConfig{
		WorkloadName: "gke-triage",
		AppName:      "core-agent",
		BasePrompt:   "Triage transitions.",
		EventLog:     hLog,
		Monitor: &config.MonitorConfig{
			Collect:         &config.MonitorCollectConfig{Tool: "lookout_diff"},
			TransitionsFrom: "lookout_diff",
		},
		RunTool: func(_ adkagent.Context, _ string, _ map[string]any) (map[string]any, error) {
			return map[string]any{
				"output": `{"transitions":[{"kind":"oom","edge":"opened","subject_key":"pod/prod/api/CrashLoopBackOff"}],"degraded":false}`,
			}, nil
		},
	})

	tick := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	prompt, shouldWake := collector.PreFire(ctx, tick, 1)
	if shouldWake || prompt != "" {
		t.Fatalf("PreFire with acked subject = (%q, %v), want (\"\", false)", prompt, shouldWake)
	}
}

func TestMonitorCollector_WakeOnFilter(t *testing.T) {
	t.Parallel()
	collector := NewMonitorCollector(MonitorCollectorConfig{
		WorkloadName: "gke-triage",
		BasePrompt:   "Triage transitions.",
		Monitor: &config.MonitorConfig{
			Collect:         &config.MonitorCollectConfig{Tool: "lookout_diff"},
			TransitionsFrom: "lookout_diff",
			WakeOn:          []string{"opened"},
		},
		RunTool: func(_ adkagent.Context, _ string, _ map[string]any) (map[string]any, error) {
			return map[string]any{
				"output": `{"transitions":[{"kind":"oom","edge":"closed","subject_key":"pod/prod/web/Unhealthy"}],"degraded":false}`,
			}, nil
		},
	})

	tick := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	prompt, shouldWake := collector.PreFire(context.Background(), tick, 1)
	if shouldWake || prompt != "" {
		t.Fatalf("PreFire with only closed transition when wake_on=[opened] = (%q, %v), want (\"\", false)", prompt, shouldWake)
	}
}

func TestMonitorCollector_MalformedStreamOrToolFailureAbortsCycle(t *testing.T) {
	t.Parallel()
	fake := &fakeSwitchboard{channel: "#sre-alerts"}
	notifier, err := NewNotifier(NotifierConfig{
		WorkloadName: "gke-triage",
		Notify: &config.MonitorNotifyConfig{
			Channel: "#sre-alerts",
		},
		Client: fake,
	})
	if err != nil {
		t.Fatalf("NewNotifier: %v", err)
	}

	// 1. Malformed JSON (missing transitions field) aborts the cycle and posts one degraded notice.
	collector := NewMonitorCollector(MonitorCollectorConfig{
		WorkloadName: "gke-triage",
		BasePrompt:   "Triage transitions.",
		Notifier:     notifier,
		Monitor: &config.MonitorConfig{
			Collect:         &config.MonitorCollectConfig{Tool: "lookout_diff"},
			TransitionsFrom: "lookout_diff",
		},
		RunTool: func(_ adkagent.Context, _ string, _ map[string]any) (map[string]any, error) {
			return map[string]any{
				"output": `{"not_transitions":123}`,
			}, nil
		},
	})

	tick := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	prompt, shouldWake := collector.PreFire(context.Background(), tick, 1)
	if shouldWake || prompt != "" {
		t.Fatalf("PreFire on malformed JSON = (%q, %v), want (\"\", false)", prompt, shouldWake)
	}
	_, failed := collector.LastCycle()
	if !failed {
		t.Error("LastCycle failed = false, want true on malformed transition stream")
	}
	if len(fake.posts) != 1 {
		t.Fatalf("degraded posts = %d, want 1", len(fake.posts))
	}

	// 2. Second consecutive failure (tool error) does not spam another degraded notice.
	collector.runTool = func(_ adkagent.Context, _ string, _ map[string]any) (map[string]any, error) {
		return nil, errors.New("mcp server unreachable")
	}
	_, shouldWake = collector.PreFire(context.Background(), tick.Add(15*time.Minute), 2)
	if shouldWake {
		t.Fatal("PreFire on tool error shouldWake = true, want false")
	}
	if len(fake.posts) != 1 {
		t.Errorf("degraded posts after consecutive failure = %d, want still 1 (edge-triggered)", len(fake.posts))
	}
}

func TestMonitorCollector_RawCollectWithoutTransitionsFrom(t *testing.T) {
	t.Parallel()
	collector := NewMonitorCollector(MonitorCollectorConfig{
		WorkloadName: "gke-triage",
		BasePrompt:   "Check cluster health.",
		Monitor: &config.MonitorConfig{
			Collect: &config.MonitorCollectConfig{
				Tool: "k8s_cluster_health",
				Args: map[string]any{"cluster": "prod"},
			},
		},
		RunTool: func(_ adkagent.Context, _ string, args map[string]any) (map[string]any, error) {
			b, _ := json.Marshal(args)
			return map[string]any{"output": "cluster healthy: " + string(b)}, nil
		},
	})

	tick := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	prompt, shouldWake := collector.PreFire(context.Background(), tick, 1)
	if !shouldWake {
		t.Fatal("PreFire in raw collect mode = false, want true")
	}
	if !strings.Contains(prompt, "Check cluster health.") || !strings.Contains(prompt, `"cluster":"prod"`) {
		t.Errorf("prompt = %q, want base prompt and collected output", prompt)
	}
}
