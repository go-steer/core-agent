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

package scionremote

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	scionmessages "github.com/GoogleCloudPlatform/scion/pkg/messages"

	"github.com/go-steer/core-agent/agent"
)

// fakeAgentService implements hubclient.AgentService with just the
// methods Spawner uses (Create, Get, Stop, StreamCloudLogs). The
// other methods return errors via panic — calling them in a test
// is a programmer mistake we want to surface loudly.
type fakeAgentService struct {
	mu sync.Mutex

	createCalls   int32
	stopCalls     int32
	streamCalls   int32
	lastCreateReq *hubclient.CreateAgentRequest
	createResp    *hubclient.CreateAgentResponse
	createErr     error
	getResp       *hubclient.Agent
	getErr        error
	streamEntries []hubclient.CloudLogEntry
	streamErr     error
	streamBlockCh chan struct{} // when non-nil, stream waits on it
	streamHandler func(hubclient.CloudLogEntry)
}

func (f *fakeAgentService) Create(_ context.Context, req *hubclient.CreateAgentRequest) (*hubclient.CreateAgentResponse, error) {
	atomic.AddInt32(&f.createCalls, 1)
	f.mu.Lock()
	f.lastCreateReq = req
	f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createResp != nil {
		return f.createResp, nil
	}
	return &hubclient.CreateAgentResponse{
		Agent: &hubclient.Agent{ID: "agent-id-1", Name: req.Name, Phase: "running"},
	}, nil
}

func (f *fakeAgentService) Get(_ context.Context, _ string) (*hubclient.Agent, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getResp != nil {
		return f.getResp, nil
	}
	return &hubclient.Agent{ID: "agent-id-1", Phase: "running"}, nil
}

func (f *fakeAgentService) Stop(_ context.Context, _ string) error {
	atomic.AddInt32(&f.stopCalls, 1)
	return nil
}

func (f *fakeAgentService) StreamCloudLogs(ctx context.Context, _ string, _ *hubclient.GetCloudLogsOptions, handler func(hubclient.CloudLogEntry)) error {
	atomic.AddInt32(&f.streamCalls, 1)
	f.mu.Lock()
	f.streamHandler = handler
	entries := f.streamEntries
	blockCh := f.streamBlockCh
	err := f.streamErr
	f.mu.Unlock()
	for _, e := range entries {
		handler(e)
	}
	if blockCh != nil {
		select {
		case <-blockCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// The remaining AgentService methods aren't used by Spawn / handle
// but we have to implement them for the interface to compile. They
// panic so a test that accidentally touches them fails loudly.
func (f *fakeAgentService) List(context.Context, *hubclient.ListAgentsOptions) (*hubclient.ListAgentsResponse, error) {
	panic("fakeAgentService: List not implemented")
}
func (f *fakeAgentService) Update(context.Context, string, *hubclient.UpdateAgentRequest) (*hubclient.Agent, error) {
	panic("fakeAgentService: Update not implemented")
}
func (f *fakeAgentService) Delete(context.Context, string, *hubclient.DeleteAgentOptions) error {
	panic("fakeAgentService: Delete not implemented")
}
func (f *fakeAgentService) Start(context.Context, string) error {
	panic("fakeAgentService: Start not implemented")
}
func (f *fakeAgentService) Suspend(context.Context, string) error {
	panic("fakeAgentService: Suspend not implemented")
}
func (f *fakeAgentService) Restart(context.Context, string) error {
	panic("fakeAgentService: Restart not implemented")
}
func (f *fakeAgentService) StopAll(context.Context) (*hubclient.StopAllResponse, error) {
	panic("fakeAgentService: StopAll not implemented")
}
func (f *fakeAgentService) SendMessage(context.Context, string, string, bool) error {
	panic("fakeAgentService: SendMessage not implemented")
}
func (f *fakeAgentService) SendStructuredMessage(context.Context, string, *scionmessages.StructuredMessage, bool, bool, bool) error {
	panic("fakeAgentService: SendStructuredMessage not implemented")
}
func (f *fakeAgentService) BroadcastMessage(context.Context, *scionmessages.StructuredMessage, bool) error {
	panic("fakeAgentService: BroadcastMessage not implemented")
}
func (f *fakeAgentService) SubmitEnv(context.Context, string, *hubclient.SubmitEnvRequest) (*hubclient.CreateAgentResponse, error) {
	panic("fakeAgentService: SubmitEnv not implemented")
}
func (f *fakeAgentService) Restore(context.Context, string) (*hubclient.Agent, error) {
	panic("fakeAgentService: Restore not implemented")
}
func (f *fakeAgentService) Exec(context.Context, string, []string, int) (*hubclient.ExecResponse, error) {
	panic("fakeAgentService: Exec not implemented")
}
func (f *fakeAgentService) GetLogs(context.Context, string, *hubclient.GetLogsOptions) (string, error) {
	panic("fakeAgentService: GetLogs not implemented")
}
func (f *fakeAgentService) SendOutboundMessage(context.Context, string, *hubclient.OutboundMessageRequest) error {
	panic("fakeAgentService: SendOutboundMessage not implemented")
}
func (f *fakeAgentService) GetCloudLogs(context.Context, string, *hubclient.GetCloudLogsOptions) (*hubclient.CloudLogsResponse, error) {
	panic("fakeAgentService: GetCloudLogs not implemented")
}

// newTestSpawner constructs a Spawner backed by the supplied fake
// service. Project is hard-coded since Spawn passes it via the
// CreateAgentRequest.
func newTestSpawner(t *testing.T, svc *fakeAgentService) *Spawner {
	t.Helper()
	s, err := New(
		WithProjectID("test-project"),
		WithTemplate("test-template"),
		WithAgentService(svc),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestNew_RequiresProjectID(t *testing.T) {
	// Uses t.Setenv, so cannot run in parallel.
	// No env, no opts — should refuse.
	t.Setenv("SCION_AGENT_TOKEN", "")
	t.Setenv("SCION_HUB_ENDPOINT", "")
	t.Setenv("SCION_PROJECT_ID", "")
	_, err := New()
	if !errors.Is(err, ErrNotInsideScion) {
		t.Errorf("expected ErrNotInsideScion; got %v", err)
	}
}

func TestNew_EnvOnly(t *testing.T) {
	// Uses t.Setenv, so cannot run in parallel.
	t.Setenv("SCION_AGENT_TOKEN", "tok")
	t.Setenv("SCION_HUB_ENDPOINT", "http://hub.example/")
	t.Setenv("SCION_PROJECT_ID", "p1")
	s, err := New()
	if err != nil {
		t.Fatalf("New from env: %v", err)
	}
	if s.projectID != "p1" {
		t.Errorf("projectID = %q, want p1", s.projectID)
	}
	if s.template != defaultTemplate {
		t.Errorf("template = %q, want default %q", s.template, defaultTemplate)
	}
}

func TestNew_OptionsOverrideEnv(t *testing.T) {
	// Uses t.Setenv, so cannot run in parallel.
	t.Setenv("SCION_AGENT_TOKEN", "tok")
	t.Setenv("SCION_HUB_ENDPOINT", "http://hub.example/")
	t.Setenv("SCION_PROJECT_ID", "from-env")
	t.Setenv("SCION_DEFAULT_TEMPLATE", "from-env-tpl")
	s, err := New(
		WithProjectID("from-opts"),
		WithTemplate("from-opts-tpl"),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.projectID != "from-opts" {
		t.Errorf("projectID = %q, want from-opts", s.projectID)
	}
	if s.template != "from-opts-tpl" {
		t.Errorf("template = %q, want from-opts-tpl", s.template)
	}
}

func TestSpawn_CreatesAgent(t *testing.T) {
	t.Parallel()
	svc := &fakeAgentService{}
	s := newTestSpawner(t, svc)
	h, err := s.Spawn(context.Background(), agent.RemoteAgentSpec{
		Name: "research-1",
		Goal: "look at recent commits in agent/",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if h.ID() != "agent-id-1" {
		t.Errorf("handle ID = %q, want agent-id-1", h.ID())
	}
	if atomic.LoadInt32(&svc.createCalls) != 1 {
		t.Errorf("Create call count = %d, want 1", atomic.LoadInt32(&svc.createCalls))
	}
	svc.mu.Lock()
	req := svc.lastCreateReq
	svc.mu.Unlock()
	if req.Name != "research-1" {
		t.Errorf("CreateAgentRequest.Name = %q, want research-1", req.Name)
	}
	if req.ProjectID != "test-project" {
		t.Errorf("ProjectID = %q, want test-project", req.ProjectID)
	}
	if req.Template != "test-template" {
		t.Errorf("Template = %q, want test-template", req.Template)
	}
	if req.Task != "look at recent commits in agent/" {
		t.Errorf("Task = %q, want goal verbatim", req.Task)
	}
	if !req.Notify {
		t.Errorf("Notify should be true so the spawner subscribes to status updates")
	}
	if req.Labels["spawned-by"] != "core-agent" {
		t.Errorf("Labels missing spawned-by=core-agent: %v", req.Labels)
	}
}

func TestSpawn_PropagatesCreateError(t *testing.T) {
	t.Parallel()
	svc := &fakeAgentService{createErr: errors.New("hub said no")}
	s := newTestSpawner(t, svc)
	_, err := s.Spawn(context.Background(), agent.RemoteAgentSpec{
		Name: "x", Goal: "x",
	})
	if err == nil {
		t.Fatalf("expected error from Spawn")
	}
}

func TestHandle_StatusMapsScionPhases(t *testing.T) {
	t.Parallel()
	cases := []struct {
		phase string
		want  agent.RemoteAgentStatus
	}{
		{"running", agent.RemoteStatusRunning},
		{"created", agent.RemoteStatusPending},
		{"provisioning", agent.RemoteStatusPending},
		{"stopped", agent.RemoteStatusStopped},
		{"error", agent.RemoteStatusFailed},
		{"failed", agent.RemoteStatusFailed},
		{"suspended", agent.RemoteStatusRunning}, // documented choice
		{"unknown-future-phase", agent.RemoteStatusRunning},
	}
	for _, tc := range cases {
		svc := &fakeAgentService{getResp: &hubclient.Agent{Phase: tc.phase}}
		s := newTestSpawner(t, svc)
		h, err := s.Spawn(context.Background(), agent.RemoteAgentSpec{
			Name: "x", Goal: "x",
		})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		got, err := h.Status(context.Background())
		if err != nil {
			t.Errorf("phase %q: Status error: %v", tc.phase, err)
			continue
		}
		if got != tc.want {
			t.Errorf("phase %q → %v, want %v", tc.phase, got, tc.want)
		}
	}
}

func TestHandle_Stop_IsIdempotent(t *testing.T) {
	t.Parallel()
	svc := &fakeAgentService{}
	s := newTestSpawner(t, svc)
	h, err := s.Spawn(context.Background(), agent.RemoteAgentSpec{
		Name: "x", Goal: "x",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := h.Stop(context.Background()); err != nil {
		t.Errorf("second Stop should be no-op; got %v", err)
	}
	// Hub only sees one Stop call regardless of how many times the
	// handle's Stop was invoked (matches the "idempotent" contract).
	if got := atomic.LoadInt32(&svc.stopCalls); got != 1 {
		t.Errorf("hub Stop calls = %d, want 1", got)
	}
}

func TestHandle_EventsFromStream(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	svc := &fakeAgentService{
		streamEntries: []hubclient.CloudLogEntry{
			{Timestamp: now, Message: "boring log line"}, // dropped
			{Timestamp: now, Message: "[REPORT_ALERT] found something"},
			{Timestamp: now, JSONPayload: map[string]interface{}{
				"kind": "completed",
				"text": "done!",
			}},
		},
	}
	s := newTestSpawner(t, svc)
	h, err := s.Spawn(context.Background(), agent.RemoteAgentSpec{
		Name: "x", Goal: "x",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Wait for the goroutine to drain stream entries and close the
	// channel (StreamCloudLogs returns immediately when streamEntries
	// is consumed and streamBlockCh is nil).
	var got []agent.RemoteAgentEvent
	for ev := range h.Events() {
		got = append(got, ev)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 classified events; got %d (%v)", len(got), got)
	}
	if got[0].Kind != "alert" || got[0].Text != "found something" {
		t.Errorf("event[0] = %+v, want alert/found something", got[0])
	}
	if got[1].Kind != "completed" || got[1].Text != "done!" {
		t.Errorf("event[1] = %+v, want completed/done!", got[1])
	}
}

func TestHandle_StreamErrorBecomesFailedEvent(t *testing.T) {
	t.Parallel()
	svc := &fakeAgentService{streamErr: errors.New("SSE connection died")}
	s := newTestSpawner(t, svc)
	h, err := s.Spawn(context.Background(), agent.RemoteAgentSpec{
		Name: "x", Goal: "x",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	var got []agent.RemoteAgentEvent
	for ev := range h.Events() {
		got = append(got, ev)
	}
	if len(got) != 1 || got[0].Kind != "failed" {
		t.Errorf("expected one failed event; got %v", got)
	}
}

func TestSpawn_RespectsCustomClassifier(t *testing.T) {
	t.Parallel()
	svc := &fakeAgentService{
		streamEntries: []hubclient.CloudLogEntry{
			{Message: "literally anything"},
		},
	}
	s, err := New(
		WithProjectID("p"),
		WithAgentService(svc),
		WithClassifier(Verbose), // emits every entry
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h, err := s.Spawn(context.Background(), agent.RemoteAgentSpec{
		Name: "x", Goal: "x",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	got := drainEvents(t, h)
	// Verbose passes everything, so the one log entry plus no
	// terminal failed (no streamErr) means we expect 1 event.
	if len(got) != 1 || got[0].Kind != "log" {
		t.Errorf("Verbose classifier should produce 1 log event; got %v", got)
	}
}

func drainEvents(t *testing.T, h agent.RemoteAgentHandle) []agent.RemoteAgentEvent {
	t.Helper()
	var got []agent.RemoteAgentEvent
	timeout := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-timeout:
			t.Fatal("timed out waiting for events channel to close")
			return got
		}
	}
}
