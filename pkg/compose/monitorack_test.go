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
	"errors"
	"strings"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"

	"github.com/go-steer/core-agent/v3/internal/monitor"
	"github.com/go-steer/core-agent/v3/pkg/attach"
	"github.com/go-steer/core-agent/v3/pkg/config"
)

func TestMonitorAcker_ForwardsWithCallerIdentityAndRecordsDurably(t *testing.T) {
	t.Parallel()
	hLog := openMonitorTestEventLog(t)
	ctx := context.Background()

	var sawName string
	var sawArgs map[string]any
	var sawCtx adkagent.Context
	acker := NewMonitorAcker(MonitorAckerConfig{
		WorkloadName: "gke-triage",
		AppName:      "core-agent",
		UserID:       "daemon",
		EventLog:     hLog,
		Monitor: &config.MonitorConfig{
			Ack: &config.MonitorAckConfig{Tool: "findings_ack"},
		},
		AckArgs: map[string]any{
			"window":              "4h",
			"cluster":             "prod",
			monitor.AckByArg:      "spoofed-by-bundle",
			monitor.AckSubjectArg: "spoofed-subject",
		},
		RunTool: func(cctx adkagent.Context, name string, args map[string]any) (map[string]any, error) {
			sawCtx = cctx
			sawName = name
			sawArgs = args
			return map[string]any{"ok": true}, nil
		},
		Now: func() time.Time { return time.Date(2026, 8, 21, 9, 15, 0, 0, time.UTC) },
	})

	res, err := acker.Ack(ctx, attach.MonitorAckRequest{
		Subject: "ns/checkout/oom",
		Reason:  "known",
		By:      "alice@example.com",
		ProxyBy: "sa:switchboard",
	})
	if err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if sawName != "findings_ack" {
		t.Fatalf("sawName = %q, want findings_ack", sawName)
	}
	if sawArgs[monitor.AckSubjectArg] != "ns/checkout/oom" {
		t.Errorf("%s = %v, want ns/checkout/oom", monitor.AckSubjectArg, sawArgs[monitor.AckSubjectArg])
	}
	if sawArgs[monitor.AckByArg] != "alice@example.com" {
		t.Errorf("%s = %v, want alice@example.com", monitor.AckByArg, sawArgs[monitor.AckByArg])
	}
	if sawArgs["window"] != "4h" || sawArgs["cluster"] != "prod" {
		t.Errorf("sawArgs = %v, want bundle literals preserved", sawArgs)
	}
	if sawCtx.SessionID() != "" || sawCtx.InvocationID() != "" {
		t.Errorf("SessionID=%q InvocationID=%q, want both empty", sawCtx.SessionID(), sawCtx.InvocationID())
	}
	if !res.Forwarded || res.By != "alice@example.com" || res.ProxyBy != "sa:switchboard" {
		t.Errorf("res = %+v, want Forwarded=true, By=alice@example.com, ProxyBy=sa:switchboard", res)
	}

	got, err := hLog.Ack(ctx, "core-agent", "gke-triage", "ns/checkout/oom")
	if err != nil || got == nil {
		t.Fatalf("hLog.Ack: %v, %+v", err, got)
	}
	if !got.Forwarded || got.By != "alice@example.com" || got.ProxyBy != "sa:switchboard" {
		t.Errorf("persisted AckRecord = %+v, want Forwarded=true and both By/ProxyBy recorded", got)
	}

	// Repeat ack reports PreviouslyAckedBy and PreviouslyAckedAt.
	res2, err := acker.Ack(ctx, attach.MonitorAckRequest{
		Subject: "ns/checkout/oom",
		By:      "bob@example.com",
	})
	if err != nil {
		t.Fatalf("second Ack: %v", err)
	}
	if res2.PreviouslyAckedBy != "alice@example.com" || res2.PreviouslyAckedAt == "" {
		t.Errorf("res2 = %+v, want PreviouslyAckedBy=alice@example.com and non-empty PreviouslyAckedAt", res2)
	}
}

func TestMonitorAcker_KeepsUnforwardedRecordWhenUpstreamFails(t *testing.T) {
	t.Parallel()
	hLog := openMonitorTestEventLog(t)
	ctx := context.Background()

	acker := NewMonitorAcker(MonitorAckerConfig{
		WorkloadName: "gke-triage",
		AppName:      "core-agent",
		EventLog:     hLog,
		Monitor: &config.MonitorConfig{
			Ack: &config.MonitorAckConfig{Tool: "findings_ack"},
		},
		RunTool: func(_ adkagent.Context, _ string, _ map[string]any) (map[string]any, error) {
			return nil, errors.New("producer down")
		},
	})

	_, err := acker.Ack(ctx, attach.MonitorAckRequest{
		Subject: "ns/checkout/oom",
		By:      "alice@example.com",
	})
	if err == nil {
		t.Fatal("Ack succeeded when upstream tool failed")
	}

	got, err := hLog.Ack(ctx, "core-agent", "gke-triage", "ns/checkout/oom")
	if err != nil || got == nil {
		t.Fatalf("hLog.Ack: %v, %+v (expected unforwarded durable record)", err, got)
	}
	if got.Forwarded {
		t.Errorf("got.Forwarded = true, want false when upstream tool failed")
	}
}

func TestMonitorAcker_RefusesWrongWorkloadAndUnattributedCaller(t *testing.T) {
	t.Parallel()
	acker := NewMonitorAcker(MonitorAckerConfig{
		WorkloadName: "gke-triage",
		AppName:      "core-agent",
		Monitor: &config.MonitorConfig{
			Ack: &config.MonitorAckConfig{Tool: "findings_ack"},
		},
		RunTool: func(_ adkagent.Context, _ string, _ map[string]any) (map[string]any, error) {
			return map[string]any{"ok": true}, nil
		},
	})

	if _, err := acker.Ack(context.Background(), attach.MonitorAckRequest{
		Subject:  "s",
		Workload: "cost-sweep",
		By:       "alice@example.com",
	}); err == nil || !strings.Contains(err.Error(), "gke-triage") || !strings.Contains(err.Error(), "cost-sweep") {
		t.Errorf("wrong workload err = %v, want error naming both workloads", err)
	}

	if _, err := acker.Ack(context.Background(), attach.MonitorAckRequest{
		Subject: "s",
	}); err == nil {
		t.Error("unattributed Ack succeeded, want error")
	}
}
