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
	"fmt"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
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

// Out-of-band approval means slow humans. Somebody reads a
// notification, thinks about it, and approves at minute eleven of a
// ten-minute window — and "unknown request id" leaves them unable to
// tell whether the write went ahead on somebody else's answer. The
// broker remembers the expiry so it can say what actually happened.
func TestPromptBroker_LateAnswerToExpiredPrompt(t *testing.T) {
	t.Parallel()

	b := NewPromptBroker()
	defer b.Close()

	// The gate marks its own timeout as the context cause; that is the
	// only signal distinguishing an expiry from a cancelled turn once
	// the wait is over.
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash"})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)

	pending := b.Pending()
	if len(pending) != 1 {
		t.Fatalf("Pending() = %d, want 1", len(pending))
	}
	id := pending[0].ID

	cancel(permissions.ErrPromptExpired)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AskApproval did not return after the prompt expired")
	}

	err := b.Respond(id, permissions.DecisionAllowOnce)
	if !errors.Is(err, ErrPromptExpired) {
		t.Fatalf("late answer to an expired prompt: err = %v, want ErrPromptExpired", err)
	}
	if errors.Is(err, ErrPromptNotFound) {
		t.Fatal("an expired prompt must not read as never having existed")
	}
}

// A prompt cut down with its turn gets a receipt too, and it is not the
// expiry one (#1088). This test used to assert the opposite — that only
// an expiry leaves a tombstone — on the reasoning that "a turn somebody
// stopped on purpose has an answer already". Run 13 of
// dev/uat/approval-gate/ showed what that costs: the gate's refusal-storm
// arm cut leg 2's turn with a prompt still open, and the late approval
// the drill posts one leg later came back `404 attach: prompt id not
// found (already responded, cancelled, or never issued)`. Every branch
// of that sentence is wrong for this caller except the middle one, and
// the one they will read first — never issued — sends them looking for a
// write that no part of the system attempted. The turn had not been
// "stopped on purpose" by them or by anybody: a guardrail ended it.
func TestPromptBroker_LateAnswerToACancelledPrompt(t *testing.T) {
	t.Parallel()

	b := NewPromptBroker()
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash"})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)

	pending := b.Pending()
	if len(pending) != 1 {
		t.Fatalf("Pending() = %d, want 1", len(pending))
	}
	id := pending[0].ID

	cancel()
	<-done

	err := b.Respond(id, permissions.DecisionAllowOnce)
	if !errors.Is(err, ErrPromptCanceled) {
		t.Fatalf("late answer to a cancelled prompt: err = %v, want ErrPromptCanceled", err)
	}
	// The two reasons must stay apart in both directions. Reading as an
	// expiry would tell the operator to answer faster next time, which
	// would not have helped; reading as not-found is the #1088 defect.
	if errors.Is(err, ErrPromptExpired) {
		t.Error("a cancelled prompt must not read as having expired — nothing timed out")
	}
	if errors.Is(err, ErrPromptNotFound) {
		t.Error("a cancelled prompt must not read as never having existed")
	}
}

// The tombstone ring is a courtesy, not a record, so it must not grow
// without bound on a daemon that prompts on a cycle all night.
func TestPromptBroker_GoneTombstonesAreBounded(t *testing.T) {
	t.Parallel()

	b := NewPromptBroker()
	defer b.Close()

	for i := 0; i < maxGoneRemembered+10; i++ {
		b.mu.Lock()
		b.rememberGone(fmt.Sprintf("id-%d", i), context.Canceled)
		b.mu.Unlock()
	}

	b.mu.Lock()
	got := len(b.gone)
	oldestKept := b.gone[0].id
	b.mu.Unlock()

	if got != maxGoneRemembered {
		t.Fatalf("tombstone ring holds %d, want %d", got, maxGoneRemembered)
	}
	// It keeps the NEWEST ones: a late approver is answering a prompt
	// from minutes ago, not from the start of the shift.
	if oldestKept != "id-10" {
		t.Fatalf("oldest kept tombstone = %q, want id-10 (ring dropped the wrong end)", oldestKept)
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
