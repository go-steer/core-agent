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

package attach

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-steer/core-agent/pkg/permissions"
)

func TestPromptBroker_RoundTripsDecision(t *testing.T) {
	t.Parallel()

	b := NewPromptBroker()
	defer b.Close()

	frames, cleanup := b.Subscribe(context.Background())
	defer cleanup()

	decided := make(chan struct {
		d   permissions.Decision
		err error
	}, 1)
	go func() {
		d, err := b.AskApproval(context.Background(), permissions.PromptRequest{
			Kind:     permissions.PromptKindBash,
			ToolName: "bash",
			Detail:   "echo hi",
			Verb:     "echo",
		})
		decided <- struct {
			d   permissions.Decision
			err error
		}{d, err}
	}()

	// Subscriber must see the frame within a beat.
	var frame PromptFrame
	select {
	case frame = <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("frame never reached subscriber")
	}
	if frame.Kind != "bash" || frame.ToolName != "bash" || frame.Detail != "echo hi" || frame.Verb != "echo" {
		t.Fatalf("frame = %+v, want bash/echo hi", frame)
	}
	if frame.ID == "" {
		t.Fatal("frame.ID is empty")
	}

	if err := b.Respond(frame.ID, permissions.DecisionAllowOnce); err != nil {
		t.Fatalf("Respond: %v", err)
	}

	select {
	case got := <-decided:
		if got.err != nil {
			t.Fatalf("AskApproval err = %v, want nil", got.err)
		}
		if got.d != permissions.DecisionAllowOnce {
			t.Errorf("decision = %v, want DecisionAllowOnce", got.d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AskApproval never unblocked")
	}
}

func TestPromptBroker_CtxCancelDropsPending(t *testing.T) {
	t.Parallel()

	b := NewPromptBroker()
	defer b.Close()
	_, cleanup := b.Subscribe(context.Background())
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		d   permissions.Decision
		err error
	}, 1)
	go func() {
		d, err := b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash"})
		done <- struct {
			d   permissions.Decision
			err error
		}{d, err}
	}()
	// Allow the goroutine to register the pending entry.
	time.Sleep(20 * time.Millisecond)

	if got := len(b.Pending()); got != 1 {
		t.Fatalf("Pending() = %d, want 1", got)
	}

	cancel()
	select {
	case got := <-done:
		if !errors.Is(got.err, context.Canceled) {
			t.Errorf("AskApproval after cancel: err = %v, want context.Canceled", got.err)
		}
		if got.d != permissions.DecisionDeny {
			t.Errorf("decision on cancel = %v, want DecisionDeny", got.d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AskApproval did not return after ctx cancel")
	}
	if got := len(b.Pending()); got != 0 {
		t.Errorf("Pending() after cancel = %d, want 0", got)
	}
}

func TestPromptBroker_RespondUnknownID(t *testing.T) {
	t.Parallel()

	b := NewPromptBroker()
	defer b.Close()
	err := b.Respond("nope", permissions.DecisionAllowOnce)
	if !errors.Is(err, ErrPromptNotFound) {
		t.Errorf("Respond unknown id: err = %v, want ErrPromptNotFound", err)
	}
}

func TestPromptBroker_LateSubscriberSeesPending(t *testing.T) {
	t.Parallel()

	b := NewPromptBroker()
	defer b.Close()

	go func() {
		_, _ = b.AskApproval(context.Background(), permissions.PromptRequest{ToolName: "bash", Detail: "ls"})
	}()
	// Wait for the pending entry to register.
	time.Sleep(20 * time.Millisecond)

	frames, cleanup := b.Subscribe(context.Background())
	defer cleanup()

	select {
	case f := <-frames:
		if f.Detail != "ls" {
			t.Errorf("late subscriber: frame = %+v, want detail=ls", f)
		}
	case <-time.After(time.Second):
		t.Fatal("late subscriber never received pending frame")
	}
}

func TestPromptBroker_CloseUnblocksPending(t *testing.T) {
	t.Parallel()

	b := NewPromptBroker()
	done := make(chan error, 1)
	go func() {
		_, err := b.AskApproval(context.Background(), permissions.PromptRequest{ToolName: "bash"})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)

	b.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("AskApproval after Close: err = nil, want closed-broker error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AskApproval did not unblock after Close")
	}
}

func TestDecisionFromWire(t *testing.T) {
	t.Parallel()
	cases := map[string]permissions.Decision{
		"deny":               permissions.DecisionDeny,
		"allow-once":         permissions.DecisionAllowOnce,
		"allow-session":      permissions.DecisionAllowSession,
		"allow-session-verb": permissions.DecisionAllowSessionVerb,
		"allow-session-tool": permissions.DecisionAllowSessionTool,
		"allow-always":       permissions.DecisionAllowAlways,
	}
	for s, want := range cases {
		got, ok := DecisionFromWire(s)
		if !ok || got != want {
			t.Errorf("DecisionFromWire(%q) = (%v, %v), want (%v, true)", s, got, ok, want)
		}
	}
	if _, ok := DecisionFromWire("bogus"); ok {
		t.Error("DecisionFromWire(bogus) accepted; want rejected")
	}
}
