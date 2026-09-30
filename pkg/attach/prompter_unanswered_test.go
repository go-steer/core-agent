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

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// askWithSubscriber opens one prompt on b with a draining subscriber
// attached and returns the prompt's id once the subscriber has it.
func askWithSubscriber(t *testing.T, b *PromptBroker) string {
	t.Helper()
	subCtx, subCancel := context.WithCancel(context.Background())
	t.Cleanup(subCancel)
	frames, cleanup := b.Subscribe(subCtx)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		_, _ = b.AskApproval(ctx, permissions.PromptRequest{ToolName: "spawn_agent", Detail: "reviewer"})
	}()
	select {
	case f := <-frames:
		return f.ID
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber never received the prompt")
		return ""
	}
}

// TestUnansweredPromptOnAnAttachedClientIsAnnounced is #1167. In the
// #1116 T2 run the operator left the TUI attached and walked away, and
// a spawn_agent prompt sat for 55 minutes while the approval channel
// heard nothing, because the fan-out had delivered it to a terminal
// nobody was looking at. With SetUnansweredAfter, the prompt is
// announced once it has gone that long without an answer, and only
// once.
func TestUnansweredPromptOnAnAttachedClientIsAnnounced(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	// Close after the subscriber's cleanup, as the stream handler's
	// ordering has it; t.Cleanup runs last-registered first.
	t.Cleanup(b.Close)
	rec := newRecorder()
	b.SetUnwatchedNotifier(rec.fn)
	const after = 50 * time.Millisecond
	b.SetUnansweredAfter(after)

	askWithSubscriber(t, b)

	p := rec.await(t)
	if p.Unanswered != after {
		t.Errorf("Unanswered = %v, want %v: the notifier words an attached-but-unanswered prompt differently", p.Unanswered, after)
	}
	if p.Frame.ToolName != "spawn_agent" {
		t.Errorf("notification carried the wrong prompt: %+v", p.Frame)
	}

	time.Sleep(4 * after)
	if n := rec.count(); n != 1 {
		t.Fatalf("notified %d times for one unanswered prompt, want exactly 1", n)
	}
}

// An answer that arrives before the delay is the operator watching, and
// nothing is announced.
func TestAnsweredPromptIsNotAnnounced(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	// Close after the subscriber's cleanup, as the stream handler's
	// ordering has it; t.Cleanup runs last-registered first.
	t.Cleanup(b.Close)
	rec := newRecorder()
	b.SetUnwatchedNotifier(rec.fn)
	b.SetUnansweredAfter(150 * time.Millisecond)

	id := askWithSubscriber(t, b)
	if err := b.RespondAs(id, permissions.DecisionAllowOnce, "operator"); err != nil {
		t.Fatalf("RespondAs: %v", err)
	}

	time.Sleep(400 * time.Millisecond)
	if n := rec.count(); n != 0 {
		t.Fatalf("notified %d times for a prompt answered before the delay", n)
	}
}

// A prompt that reached nobody is announced at once, as before, and the
// delay does not add a second notification for it.
func TestUnwatchedPromptIsAnnouncedOnceWithTheDelaySet(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	// Close after the subscriber's cleanup, as the stream handler's
	// ordering has it; t.Cleanup runs last-registered first.
	t.Cleanup(b.Close)
	rec := newRecorder()
	b.SetUnwatchedNotifier(rec.fn)
	const after = 50 * time.Millisecond
	b.SetUnansweredAfter(after)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash", Detail: "kubectl apply"})
	}()

	if p := rec.await(t); p.Unanswered != 0 {
		t.Errorf("Unanswered = %v for a prompt that reached nobody, want 0", p.Unanswered)
	}
	time.Sleep(4 * after)
	if n := rec.count(); n != 1 {
		t.Fatalf("notified %d times for one prompt that reached nobody, want exactly 1", n)
	}
}

// A prompt whose turn ends while the delay is still running goes the
// old ctx.Done way — tombstoned, so a late answer hears the turn ended —
// and is never announced: there is nothing left to answer.
func TestPromptCanceledBeforeTheDelayIsTombstonedNotAnnounced(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	t.Cleanup(b.Close)
	rec := newRecorder()
	b.SetUnwatchedNotifier(rec.fn)
	const after = 200 * time.Millisecond
	b.SetUnansweredAfter(after)

	subCtx, subCancel := context.WithCancel(context.Background())
	t.Cleanup(subCancel)
	frames, cleanup := b.Subscribe(subCtx)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash", Detail: "x"})
		done <- err
	}()
	var id string
	select {
	case f := <-frames:
		id = f.ID
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber never received the prompt")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AskApproval did not return when its context was cancelled with the delay armed")
	}
	if err := b.RespondAs(id, permissions.DecisionAllowOnce, ""); !errors.Is(err, ErrPromptCanceled) {
		t.Errorf("late answer: err = %v, want ErrPromptCanceled", err)
	}

	time.Sleep(2 * after)
	if n := rec.count(); n != 0 {
		t.Fatalf("notified %d times for a prompt whose turn had already ended", n)
	}
}
