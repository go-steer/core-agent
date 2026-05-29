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

package tools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/tool"
)

func TestNewLifecycleTool_RequiresHandler(t *testing.T) {
	t.Parallel()
	_, err := NewLifecycleTool(LifecycleOptions{})
	if err == nil || !strings.Contains(err.Error(), "Handler is required") {
		t.Fatalf("expected Handler-required error, got %v", err)
	}
}

func TestNewLifecycleTool_DefaultsNameAndDescription(t *testing.T) {
	t.Parallel()
	tl, err := NewLifecycleTool(LifecycleOptions{
		Handler: func(_ context.Context, _ LifecycleEvent) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewLifecycleTool: %v", err)
	}
	if tl.Name() != "set_status" {
		t.Errorf("default name = %q, want set_status", tl.Name())
	}
	if tl.Description() == "" {
		t.Errorf("default description should be non-empty")
	}
}

func TestNewLifecycleTool_NameAndDescriptionOverrides(t *testing.T) {
	t.Parallel()
	tl, err := NewLifecycleTool(LifecycleOptions{
		Handler:     func(_ context.Context, _ LifecycleEvent) error { return nil },
		Name:        "report_done",
		Description: "signal that the goal is complete",
	})
	if err != nil {
		t.Fatalf("NewLifecycleTool: %v", err)
	}
	if tl.Name() != "report_done" {
		t.Errorf("name override didn't take, got %q", tl.Name())
	}
	if tl.Description() != "signal that the goal is complete" {
		t.Errorf("description override didn't take, got %q", tl.Description())
	}
}

func TestNewLifecycleTool_AllowedStatesInDescription(t *testing.T) {
	t.Parallel()
	tl, err := NewLifecycleTool(LifecycleOptions{
		Handler:       func(_ context.Context, _ LifecycleEvent) error { return nil },
		AllowedStates: []string{"thinking", "blocked", "done"},
	})
	if err != nil {
		t.Fatalf("NewLifecycleTool: %v", err)
	}
	desc := tl.Description()
	for _, s := range []string{"thinking", "blocked", "done"} {
		if !strings.Contains(desc, s) {
			t.Errorf("allowed state %q should appear in default description, got %q", s, desc)
		}
	}
}

func TestNewLifecycleTool_RejectsEmptyAllowedState(t *testing.T) {
	t.Parallel()
	_, err := NewLifecycleTool(LifecycleOptions{
		Handler:       func(_ context.Context, _ LifecycleEvent) error { return nil },
		AllowedStates: []string{"done", ""},
	})
	if err == nil || !strings.Contains(err.Error(), "empty entry") {
		t.Fatalf("expected empty-entry error, got %v", err)
	}
}

func TestLifecycleFunc_DeliversToHandler(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		received []LifecycleEvent
	)
	fn := lifecycleFunc(func(_ context.Context, ev LifecycleEvent) error {
		mu.Lock()
		defer mu.Unlock()
		received = append(received, ev)
		return nil
	}, nil)

	res, err := fn(tool.Context(nil), lifecycleArgs{State: "thinking", Detail: "looking around"})
	if err != nil {
		t.Fatalf("fn: %v", err)
	}
	if res.Ack != "ok" {
		t.Errorf("ack = %q, want ok", res.Ack)
	}
	if len(received) != 1 {
		t.Fatalf("handler not invoked: got %d events", len(received))
	}
	if received[0].State != "thinking" || received[0].Detail != "looking around" {
		t.Errorf("event mismatch: %+v", received[0])
	}
	if received[0].Time.IsZero() {
		t.Errorf("event time should be set")
	}
}

func TestLifecycleFunc_TrimsArgs(t *testing.T) {
	t.Parallel()
	var got LifecycleEvent
	fn := lifecycleFunc(func(_ context.Context, ev LifecycleEvent) error {
		got = ev
		return nil
	}, nil)
	if _, err := fn(tool.Context(nil), lifecycleArgs{State: "  done  ", Detail: "  finished  "}); err != nil {
		t.Fatalf("fn: %v", err)
	}
	if got.State != "done" {
		t.Errorf("state = %q, want done", got.State)
	}
	if got.Detail != "finished" {
		t.Errorf("detail = %q, want finished", got.Detail)
	}
}

func TestLifecycleFunc_RejectsEmptyState(t *testing.T) {
	t.Parallel()
	called := false
	fn := lifecycleFunc(func(_ context.Context, _ LifecycleEvent) error {
		called = true
		return nil
	}, nil)
	res, err := fn(tool.Context(nil), lifecycleArgs{State: "   "})
	if err != nil {
		t.Fatalf("fn: %v", err)
	}
	if !strings.Contains(res.Ack, "rejected") || !strings.Contains(res.Ack, "state is required") {
		t.Errorf("expected rejection ack, got %q", res.Ack)
	}
	if called {
		t.Errorf("handler should not be invoked when state is empty")
	}
}

func TestLifecycleFunc_AllowedStatesRejection(t *testing.T) {
	t.Parallel()
	called := false
	fn := lifecycleFunc(func(_ context.Context, _ LifecycleEvent) error {
		called = true
		return nil
	}, []string{"thinking", "done"})

	// Allowed state passes through.
	res, err := fn(tool.Context(nil), lifecycleArgs{State: "done"})
	if err != nil || res.Ack != "ok" {
		t.Fatalf("allowed state should pass: ack=%q err=%v", res.Ack, err)
	}
	if !called {
		t.Errorf("handler should fire for allowed state")
	}

	// Disallowed state is rejected without invoking the handler.
	called = false
	res, err = fn(tool.Context(nil), lifecycleArgs{State: "frozen"})
	if err != nil {
		t.Fatalf("fn: %v", err)
	}
	if !strings.Contains(res.Ack, "rejected") || !strings.Contains(res.Ack, "frozen") {
		t.Errorf("expected rejection mentioning the bad state, got %q", res.Ack)
	}
	if called {
		t.Errorf("handler must not be invoked for disallowed state")
	}
}

func TestLifecycleFunc_HandlerErrorBecomesAck(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("not yet")
	fn := lifecycleFunc(func(_ context.Context, _ LifecycleEvent) error {
		return wantErr
	}, nil)
	res, err := fn(tool.Context(nil), lifecycleArgs{State: "done"})
	if err != nil {
		t.Fatalf("fn returned err; should surface via Ack: %v", err)
	}
	if !strings.Contains(res.Ack, "not yet") {
		t.Errorf("expected handler error in Ack, got %q", res.Ack)
	}
}
