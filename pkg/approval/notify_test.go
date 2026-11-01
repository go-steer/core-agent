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

package approval

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/plugin"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/permissions"
)

type announcement struct {
	ctx    context.Context
	notice ParkNotice
}

type notifyProbe struct {
	mu      sync.Mutex
	got     []announcement
	entered chan struct{}
	release chan struct{}
	runTook time.Duration
	once    sync.Once
}

const releaseAfter = 2 * time.Second

func (p *notifyProbe) hook(ctx context.Context, n ParkNotice) {
	p.mu.Lock()
	p.got = append(p.got, announcement{ctx: ctx, notice: n})
	p.mu.Unlock()
	p.once.Do(func() { close(p.entered) })
	if p.release != nil {
		<-p.release
	}
}

func (p *notifyProbe) announcements() []announcement {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]announcement(nil), p.got...)
}

func (p *notifyProbe) waitForAnnouncement(t *testing.T) {
	t.Helper()
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the gate parked and never announced it")
	}
}

type notifyProbeConfig struct {
	policy  OnMutation
	release bool
	noHook  bool
}

func runNotifyProbe(t *testing.T, cfg notifyProbeConfig) (*notifyProbe, []string, []scaleArgs) {
	t.Helper()
	probe := &notifyProbe{entered: make(chan struct{})}
	if cfg.release {
		probe.release = make(chan struct{})
	}

	var executed []scaleArgs
	scale, err := functiontool.New(functiontool.Config{
		Name:        "scale_deployment",
		Description: "changes a deployment's replica count",
	}, func(_ adkagent.Context, args scaleArgs) (map[string]any, error) {
		executed = append(executed, args)
		return map[string]any{"scaled": args.Replicas}, nil
	})
	if err != nil {
		t.Fatalf("functiontool.New: %v", err)
	}

	root, err := llmagent.New(llmagent.Config{
		Name:        "notify_agent",
		Description: "park-announcement probe",
		Instruction: "act",
		Model:       &proposingModel{calls: []map[string]any{proposeScale("api", 10)}},
		Tools:       []tool.Tool{scale},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}

	policy := cfg.policy
	if policy == "" {
		policy = OnMutationRequireApproval
	}
	gcfg := Config{
		Policy:   policy,
		Mutating: alwaysMutating,
		Gate:     permissions.New(permissions.Options{}),
		Workload: "triage",
	}
	if !cfg.noHook {
		gcfg.NotifyPark = probe.hook
	}
	wg, err := New(gcfg)
	if err != nil {
		t.Fatalf("approval.New: %v", err)
	}

	svc := sqliteService(t)
	r, err := runner.New(runner.Config{
		AppName:           testApp,
		Agent:             root,
		SessionService:    svc,
		AutoCreateSession: true,
		PluginConfig:      runner.PluginConfig{Plugins: []*plugin.Plugin{wg}},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}

	if cfg.release {
		time.AfterFunc(releaseAfter, func() { close(probe.release) })
	}

	runCtx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	for _, err := range r.Run(runCtx, testUser, sid, genai.NewContentFromText("scale api to 10", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	probe.runTook = time.Since(started)
	cancel()

	ids, _, _ := confirmationRequests(t, svc)
	return probe, ids, executed
}

func TestAParkAnnouncesItself(t *testing.T) {
	probe, ids, executed := runNotifyProbe(t, notifyProbeConfig{})
	probe.waitForAnnouncement(t)

	if len(ids) != 1 {
		t.Fatalf("parks = %d, want 1", len(ids))
	}
	if len(executed) != 0 {
		t.Fatalf("the tool ran %d time(s) while parked", len(executed))
	}
	got := probe.announcements()
	if len(got) != 1 {
		t.Fatalf("announcements = %d, want exactly 1", len(got))
	}
	n := got[0].notice
	if n.Session != sid {
		t.Errorf("Session = %q, want %q", n.Session, sid)
	}
	if n.Invocation == "" {
		t.Error("Invocation is empty")
	}
	if n.Workload != "triage" {
		t.Errorf("Workload = %q, want %q", n.Workload, "triage")
	}
	if n.Agent != "notify_agent" {
		t.Errorf("Agent = %q, want %q", n.Agent, "notify_agent")
	}
	if n.Tool != "scale_deployment" {
		t.Errorf("Tool = %q, want %q", n.Tool, "scale_deployment")
	}
	if want := CallKey("scale_deployment", proposeScale("api", 10)); n.Key != want {
		t.Errorf("Key = %q, want %q", n.Key, want)
	}
	if !strings.Contains(n.Hint, "Approve mutating call") {
		t.Errorf("Hint = %q, want the operator-facing park hint", n.Hint)
	}
	if n.ParkedAt.IsZero() {
		t.Error("ParkedAt is zero")
	}
}

func TestTheAnnouncementOutlivesTheTurnThatRaisedIt(t *testing.T) {
	probe, _, _ := runNotifyProbe(t, notifyProbeConfig{release: true})
	probe.waitForAnnouncement(t)

	got := probe.announcements()
	if len(got) != 1 {
		t.Fatalf("announcements = %d, want 1", len(got))
	}
	if err := got[0].ctx.Err(); err != nil {
		t.Fatalf("the announcement's context is already done (%v)", err)
	}
	if _, ok := got[0].ctx.Deadline(); !ok {
		t.Error("the announcement's context has no deadline")
	}
}

func TestTheGateDoesNotWaitForTheAnnouncement(t *testing.T) {
	probe, ids, _ := runNotifyProbe(t, notifyProbeConfig{release: true})
	probe.waitForAnnouncement(t)
	if probe.runTook >= releaseAfter {
		t.Errorf("the turn took %s, waiting for the announcement", probe.runTook)
	}
	if len(ids) != 1 {
		t.Fatalf("parks = %d, want 1", len(ids))
	}
}

func TestACallThatDoesNotParkAnnouncesNothing(t *testing.T) {
	probe, ids, executed := runNotifyProbe(t, notifyProbeConfig{policy: OnMutationApply})
	if len(ids) != 0 {
		t.Fatalf("parks = %d, want 0 under apply", len(ids))
	}
	if len(executed) != 1 {
		t.Fatalf("executions = %d, want 1 under apply", len(executed))
	}
	if got := probe.announcements(); len(got) != 0 {
		t.Fatalf("announcements = %d, want 0", len(got))
	}
}

func TestNoNotifierIsNotAnError(t *testing.T) {
	probe, ids, executed := runNotifyProbe(t, notifyProbeConfig{noHook: true})
	if len(ids) != 1 {
		t.Fatalf("parks = %d, want 1", len(ids))
	}
	if len(executed) != 0 {
		t.Fatalf("the tool ran %d time(s) while parked", len(executed))
	}
	if got := probe.announcements(); len(got) != 0 {
		t.Fatalf("announcements = %d, want 0", len(got))
	}
}
