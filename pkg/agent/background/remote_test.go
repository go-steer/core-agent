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

package background

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRemoteSpawner records Spawn calls and returns a handle with a
// caller-controlled event channel so tests can drive the fan-in.
type fakeRemoteSpawner struct {
	mu       sync.Mutex
	spawnErr error
	handles  []*fakeRemoteHandle
}

func (s *fakeRemoteSpawner) Spawn(_ context.Context, spec RemoteAgentSpec) (RemoteAgentHandle, error) {
	if s.spawnErr != nil {
		return nil, s.spawnErr
	}
	h := &fakeRemoteHandle{
		id:     spec.ID,
		events: make(chan RemoteAgentEvent, 8),
	}
	s.mu.Lock()
	s.handles = append(s.handles, h)
	s.mu.Unlock()
	return h, nil
}

type fakeRemoteHandle struct {
	id        string
	events    chan RemoteAgentEvent
	stopErr   error
	statusErr error
	stopped   bool
	mu        sync.Mutex
	stat      RemoteAgentStatus
	stopOnce  sync.Once
}

func (h *fakeRemoteHandle) ID() string { return h.id }

func (h *fakeRemoteHandle) Status(_ context.Context) (RemoteAgentStatus, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.statusErr != nil {
		return RemoteStatusPending, h.statusErr
	}
	return h.stat, nil
}

func (h *fakeRemoteHandle) Stop(_ context.Context) error {
	h.stopOnce.Do(func() {
		h.mu.Lock()
		h.stopped = true
		h.mu.Unlock()
	})
	return h.stopErr
}

func (h *fakeRemoteHandle) Events() <-chan RemoteAgentEvent { return h.events }

func TestRefuseRemoteAgentSpawner_Errors(t *testing.T) {
	t.Parallel()
	s := RefuseRemoteAgentSpawner("nope")
	_, err := s.Spawn(context.Background(), RemoteAgentSpec{})
	if err == nil {
		t.Errorf("expected error from refusing spawner")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("expected reason in error; got %v", err)
	}
}

func TestRefuseRemoteAgentSpawner_DefaultReason(t *testing.T) {
	t.Parallel()
	_, err := RefuseRemoteAgentSpawner("").Spawn(context.Background(), RemoteAgentSpec{})
	if err == nil || err.Error() == "" {
		t.Errorf("expected non-empty error from default-reason refusing spawner")
	}
}

func TestNewSpawnRemoteAgentTool_NilSpawner(t *testing.T) {
	t.Parallel()
	_, err := NewSpawnRemoteAgentTool(nil, nil)
	if !errors.Is(err, ErrNoSpawner) {
		t.Errorf("expected ErrNoSpawner; got %v", err)
	}
}

func TestNewSpawnRemoteAgentTool_NameAndDescription(t *testing.T) {
	t.Parallel()
	spawner := RefuseRemoteAgentSpawner("test")
	tool, err := NewSpawnRemoteAgentTool(spawner, nil)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	if tool.Name() != "spawn_remote_agent" {
		t.Errorf("name = %q, want spawn_remote_agent", tool.Name())
	}
	if !strings.Contains(tool.Description(), "out-of-process") {
		t.Errorf("description should mention out-of-process; got %q", tool.Description())
	}
}

func TestFanInRemote_AlertsFlowToManagerChannel(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	spawner := &fakeRemoteSpawner{}
	mgr.registerRemote(must(spawner.Spawn(context.Background(), RemoteAgentSpec{ID: "r1", Name: "r1"})), RemoteAgentSpec{ID: "r1", Name: "r1"})

	// Push an alert through the remote's event channel.
	h := spawner.handles[0]
	h.events <- RemoteAgentEvent{Kind: "alert", Text: "hello"}

	select {
	case a := <-mgr.Alerts():
		if a.From != "r1" || a.Text != "hello" || a.Kind != "alert" {
			t.Errorf("unexpected alert: %+v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("no alert arrived through fan-in")
	}
}

func TestFanInRemote_TerminalEventClosesHandle(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	spawner := &fakeRemoteSpawner{}
	mgr.registerRemote(must(spawner.Spawn(context.Background(), RemoteAgentSpec{ID: "r2", Name: "r2"})), RemoteAgentSpec{ID: "r2", Name: "r2"})
	h := spawner.handles[0]

	h.events <- RemoteAgentEvent{Kind: "completed", Text: "done"}
	close(h.events)

	bh, ok := mgr.Get("r2")
	if !ok {
		t.Fatalf("handle not registered")
	}
	select {
	case <-bh.Done():
	case <-time.After(time.Second):
		t.Fatal("background handle did not close after terminal event")
	}
	if bh.Status() != StatusCompleted {
		t.Errorf("status = %v, want StatusCompleted", bh.Status())
	}
}

func TestStop_OnRemoteHandle_CallsRemoteStop(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	spawner := &fakeRemoteSpawner{}
	mgr.registerRemote(must(spawner.Spawn(context.Background(), RemoteAgentSpec{ID: "r3", Name: "r3"})), RemoteAgentSpec{ID: "r3", Name: "r3"})
	h := spawner.handles[0]

	if err := mgr.Stop("r3"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// Close the remote's event channel so the fan-in goroutine
	// exits and the handle's done channel closes.
	close(h.events)

	bh, _ := mgr.Get("r3")
	select {
	case <-bh.Done():
	case <-time.After(time.Second):
		t.Fatal("handle not closed after Stop")
	}
	if !h.stopped {
		t.Errorf("remote Stop was not called")
	}
}

func TestFanInRemote_UnknownKindStillSurfacesAsAlert(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t)
	spawner := &fakeRemoteSpawner{}
	mgr.registerRemote(must(spawner.Spawn(context.Background(), RemoteAgentSpec{ID: "rx", Name: "rx"})), RemoteAgentSpec{ID: "rx", Name: "rx"})
	h := spawner.handles[0]

	h.events <- RemoteAgentEvent{Kind: "weird", Text: "hmm"}

	select {
	case a := <-mgr.Alerts():
		if a.Kind != "alert" {
			t.Errorf("expected unknown kind to map to 'alert'; got %q", a.Kind)
		}
		if !strings.Contains(a.Text, "weird") {
			t.Errorf("expected kind tag in text; got %q", a.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("unknown-kind alert never arrived")
	}
}

func TestFanInRemote_StreamClosedWithoutTerminalEvent_ConsultsStatusAndFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		remoteStat RemoteAgentStatus
		statusErr  error
		wantStatus Status
		wantKind   string
		wantErr    bool
	}{
		{
			name:       "running_on_close_fails_closed",
			remoteStat: RemoteStatusRunning,
			wantStatus: StatusFailed,
			wantKind:   "failed",
			wantErr:    true,
		},
		{
			name:       "pending_on_close_fails_closed",
			remoteStat: RemoteStatusPending,
			wantStatus: StatusFailed,
			wantKind:   "failed",
			wantErr:    true,
		},
		{
			name:       "failed_on_close_fails_closed",
			remoteStat: RemoteStatusFailed,
			wantStatus: StatusFailed,
			wantKind:   "failed",
			wantErr:    true,
		},
		{
			name:       "unknown_status_on_close_fails_closed",
			remoteStat: RemoteAgentStatus(99),
			wantStatus: StatusFailed,
			wantKind:   "failed",
			wantErr:    true,
		},
		{
			name:       "status_error_fails_closed",
			remoteStat: RemoteStatusCompleted,
			statusErr:  errors.New("rpc unavailable"),
			wantStatus: StatusFailed,
			wantKind:   "failed",
			wantErr:    true,
		},
		{
			name:       "completed_status_on_close",
			remoteStat: RemoteStatusCompleted,
			wantStatus: StatusCompleted,
			wantKind:   "completed",
			wantErr:    false,
		},
		{
			name:       "stopped_status_on_close",
			remoteStat: RemoteStatusStopped,
			wantStatus: StatusStopped,
			wantKind:   "stopped",
			wantErr:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mgr, _ := newFakeManager(t)
			spawner := &fakeRemoteSpawner{}
			rh := must(spawner.Spawn(context.Background(), RemoteAgentSpec{ID: "rc", Name: "rc"}))
			fh := spawner.handles[0]
			fh.mu.Lock()
			fh.stat = tc.remoteStat
			fh.statusErr = tc.statusErr
			fh.mu.Unlock()

			mgr.registerRemote(rh, RemoteAgentSpec{ID: "rc", Name: "rc"})
			close(fh.events)

			bh, ok := mgr.Get("rc")
			if !ok {
				t.Fatalf("handle not registered")
			}
			select {
			case <-bh.Done():
			case <-time.After(time.Second):
				t.Fatal("background handle did not close")
			}
			if bh.Status() != tc.wantStatus {
				t.Errorf("status = %v, want %v", bh.Status(), tc.wantStatus)
			}
			if tc.wantErr && bh.Err() == nil {
				t.Errorf("expected non-nil handle.Err() on unexpected stream close")
			}
			if !tc.wantErr && bh.Err() != nil {
				t.Errorf("unexpected handle.Err(): %v", bh.Err())
			}
			select {
			case a := <-mgr.Alerts():
				if a.From != "rc" || a.Kind != tc.wantKind {
					t.Errorf("unexpected alert: %+v (want kind %q)", a, tc.wantKind)
				}
			case <-time.After(time.Second):
				t.Fatal("expected terminal alert on stream close")
			}
		})
	}
}

func TestManager_TerminalHandlesCappedAtMaxTerminalHandles(t *testing.T) {
	t.Parallel()
	mgr, _ := newFakeManager(t, WithMaxTerminalHandles(3))
	spawner := &fakeRemoteSpawner{}

	// Spawn one long-running subagent first; it must never be evicted
	// while still StatusRunning.
	rhRunning := must(spawner.Spawn(context.Background(), RemoteAgentSpec{ID: "stay-running", Name: "stay-running"}))
	mgr.registerRemote(rhRunning, RemoteAgentSpec{ID: "stay-running", Name: "stay-running"})

	// Spawn 5 subagents that complete in order.
	names := []string{"term-1", "term-2", "term-3", "term-4", "term-5"}
	for i, name := range names {
		rh := must(spawner.Spawn(context.Background(), RemoteAgentSpec{ID: name, Name: name}))
		fh := spawner.handles[i+1]
		mgr.registerRemote(rh, RemoteAgentSpec{ID: name, Name: name})
		fh.events <- RemoteAgentEvent{Kind: "completed", Text: "done " + name}
		close(fh.events)
		bh, ok := mgr.Get(name)
		if !ok {
			t.Fatalf("handle %q not found immediately after spawn", name)
		}
		select {
		case <-bh.Done():
		case <-time.After(time.Second):
			t.Fatalf("handle %q did not finish", name)
		}
	}

	// Trigger one more eviction pass by finishing the 5th or checking List().
	got := mgr.List()
	// Expect 1 running ("stay-running") + 3 most recent terminal ("term-3", "term-4", "term-5") = 4 handles.
	if len(got) != 4 {
		var gotNames []string
		for _, h := range got {
			gotNames = append(gotNames, h.Name+"("+h.Status().String()+")")
		}
		t.Fatalf("len(List()) = %d (%v), want 4 (1 running + 3 terminal)", len(got), gotNames)
	}
	if _, ok := mgr.Get("stay-running"); !ok {
		t.Errorf("running subagent 'stay-running' was evicted")
	}
	for _, evicted := range []string{"term-1", "term-2"} {
		if _, ok := mgr.Get(evicted); ok {
			t.Errorf("expected oldest terminal subagent %q to be evicted", evicted)
		}
	}
	for _, retained := range []string{"term-3", "term-4", "term-5"} {
		if _, ok := mgr.Get(retained); !ok {
			t.Errorf("expected recent terminal subagent %q to be retained", retained)
		}
	}
	close(spawner.handles[0].events)
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
