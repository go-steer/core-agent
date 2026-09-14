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
	"sync"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// recorder collects unwatched-prompt callbacks.
type recorder struct {
	mu    sync.Mutex
	got   []UnwatchedPrompt
	ctxs  []context.Context
	fired chan struct{}
}

func newRecorder() *recorder { return &recorder{fired: make(chan struct{}, 8)} }

func (r *recorder) fn(ctx context.Context, p UnwatchedPrompt) {
	r.mu.Lock()
	r.got = append(r.got, p)
	r.ctxs = append(r.ctxs, ctx)
	r.mu.Unlock()
	r.fired <- struct{}{}
}

func (r *recorder) await(t *testing.T) UnwatchedPrompt {
	t.Helper()
	select {
	case <-r.fired:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the unwatched-prompt notification")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got[len(r.got)-1]
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

func TestUnwatchedNotifierFiresWhenNobodyIsAttached(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	defer b.Close()
	rec := newRecorder()
	b.SetUnwatchedNotifier(rec.fn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = b.AskApproval(ctx, permissions.PromptRequest{
			Kind: permissions.PromptKindBash, ToolName: "bash", Detail: "kubectl apply -f x.yaml",
		})
	}()

	p := rec.await(t)
	if p.Frame.ToolName != "bash" || p.Frame.Detail != "kubectl apply -f x.yaml" {
		t.Errorf("notification carried the wrong prompt: %+v", p.Frame)
	}
	if p.Frame.ID == "" {
		t.Error("notification must carry the request id; without it the recipient cannot answer")
	}
	if !p.Deadline.IsZero() {
		t.Errorf("no approval timeout was set, so Deadline should be zero, got %v", p.Deadline)
	}
}

func TestUnwatchedNotifierStaysQuietWhenSomebodyIsWatching(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	defer b.Close()
	rec := newRecorder()
	b.SetUnwatchedNotifier(rec.fn)

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	frames, cleanup := b.Subscribe(subCtx)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_, _ = b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash", Detail: "rm -rf /tmp/x"})
	}()

	select {
	case f := <-frames:
		if f.ToolName != "bash" {
			t.Fatalf("subscriber got the wrong frame: %+v", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber never received the prompt")
	}

	// Give a stray notification time to land before declaring silence.
	time.Sleep(200 * time.Millisecond)
	if n := rec.count(); n != 0 {
		t.Fatalf("notified %d times with a live subscriber attached; out-of-band escalation on every prompt of an interactive session is noise that trains the recipient to ignore the channel", n)
	}
}

// The pre-fix behaviour checked len(b.subs) and would have stayed silent
// here. A subscriber whose buffer is full is one whose reader stopped
// draining — the operator sees nothing, but the broker's roster says
// somebody is watching. That is strictly worse than no subscriber,
// because it is the case an operator would never think to check.
func TestUnwatchedNotifierFiresWhenTheOnlySubscriberIsNotDraining(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	defer b.Close()
	rec := newRecorder()
	b.SetUnwatchedNotifier(rec.fn)

	subCtx, subCancel := context.WithCancel(context.Background())
	defer subCancel()
	frames, cleanup := b.Subscribe(subCtx)
	defer cleanup()

	// Wedge the subscriber: fill its buffer and never read.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for i := 0; i < cap(frames)+1; i++ {
		go func() {
			_, _ = b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash", Detail: "filler"})
		}()
	}

	// The one that matters: raised once the buffer cannot take it.
	deadline := time.After(3 * time.Second)
	for rec.count() == 0 {
		select {
		case <-deadline:
			t.Fatal("a prompt that reached no reader was never escalated; the broker counted subscribers rather than deliveries")
		default:
		}
		go func() {
			_, _ = b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash", Detail: "the real one"})
		}()
		time.Sleep(50 * time.Millisecond)
	}
}

func TestUnwatchedNotifierCarriesTheDeadline(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	defer b.Close()
	rec := newRecorder()
	b.SetUnwatchedNotifier(rec.fn)

	want := time.Now().Add(10 * time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), want)
	defer cancel()
	go func() {
		_, _ = b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash", Detail: "x"})
	}()

	p := rec.await(t)
	if p.Deadline.IsZero() {
		t.Fatal("the prompt had a deadline; a notification that omits it cannot tell the recipient whether they have ten minutes or forever")
	}
	if d := p.Deadline.Sub(want); d > time.Second || d < -time.Second {
		t.Errorf("Deadline = %v, want ~%v", p.Deadline, want)
	}
}

// The notification's whole purpose is to reach somebody before the
// prompt expires, so its context must outlive the prompt's. Parenting
// the send on the prompt context would kill it at exactly the moment it
// became worth sending.
func TestUnwatchedNotificationOutlivesTheExpiredPrompt(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	defer b.Close()

	released := make(chan struct{})
	alive := make(chan error, 1)
	b.SetUnwatchedNotifier(func(ctx context.Context, _ UnwatchedPrompt) {
		<-released // hold until the prompt has already expired
		alive <- ctx.Err()
	})

	ctx, cancel := context.WithTimeoutCause(context.Background(), 100*time.Millisecond, permissions.ErrPromptExpired)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash", Detail: "x"})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("AskApproval never returned after its context expired")
	}

	close(released)
	select {
	case err := <-alive:
		if err != nil {
			t.Fatalf("the notification's context died with the prompt (%v); the escalation would never have been sent", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("notifier never reported")
	}
}

// A prompt must stay answerable while its notification is still in
// flight: the gate is not allowed to depend on a webhook.
func TestASlowNotifierDoesNotBlockThePrompt(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	defer b.Close()

	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	b.SetUnwatchedNotifier(func(context.Context, UnwatchedPrompt) {
		close(entered)
		<-release
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		a   permissions.Approval
		err error
	}
	out := make(chan result, 1)
	go func() {
		a, err := b.AskApprovalAttributed(ctx, permissions.PromptRequest{ToolName: "bash", Detail: "x"})
		out <- result{a, err}
	}()

	<-entered

	// Find the id and answer it while the notifier is still stuck.
	var id string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if p := b.Pending(); len(p) == 1 {
			id = p[0].ID
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("prompt never appeared in Pending")
	}
	if err := b.RespondAs(id, permissions.DecisionAllowOnce, "operator"); err != nil {
		t.Fatalf("RespondAs: %v", err)
	}

	select {
	case r := <-out:
		if r.err != nil {
			t.Fatalf("AskApprovalAttributed: %v", r.err)
		}
		if r.a.Decision != permissions.DecisionAllowOnce {
			t.Errorf("decision = %v", r.a.Decision)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stuck notifier blocked the prompt; the gate must not wait on the escalation channel")
	}
}

func TestUnwatchedNotifierIsOptional(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	defer b.Close()
	rec := newRecorder()
	b.SetUnwatchedNotifier(rec.fn)
	b.SetUnwatchedNotifier(nil) // removed again

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := b.AskApproval(ctx, permissions.PromptRequest{ToolName: "bash", Detail: "x"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("notified %d times after the notifier was removed", n)
	}
}

// Regression: a subscriber disconnecting while a prompt opens used to be
// a send on a closed channel, because the fan-out sent to a snapshot of
// b.subs taken under the lock unsubscribe closes them under. Under
// -race this fails as a data race; without it, it panics the daemon.
//
// The two events are an operator's SSE handler returning and the agent
// asking for permission — which on an unattended deployment is not a
// coincidence, it is the ordinary case.
func TestPromptFanOutRacesSubscriberTeardown(t *testing.T) {
	t.Parallel()
	b := NewPromptBroker()
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				subCtx, subCancel := context.WithCancel(context.Background())
				_, cleanup := b.Subscribe(subCtx)
				cleanup()
				subCancel()
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				pctx, pcancel := context.WithTimeout(ctx, time.Millisecond)
				_, _ = b.AskApproval(pctx, permissions.PromptRequest{ToolName: "bash", Detail: "x"})
				pcancel()
			}
		}()
	}
	wg.Wait()
}
