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

package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-steer/core-agent/models/mock"
)

func TestInbox_PushDrainOrder(t *testing.T) {
	t.Parallel()
	q := newInbox()
	for _, m := range []string{"one", "two", "three"} {
		if err := q.push(m); err != nil {
			t.Fatalf("push %q: %v", m, err)
		}
	}
	got := q.drain()
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("drain count = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("drain[%d] = %q, want %q", i, got[i], w)
		}
	}
	if again := q.drain(); len(again) != 0 {
		t.Errorf("second drain should be empty; got %v", again)
	}
}

func TestInbox_NotifyFiresOnPush(t *testing.T) {
	t.Parallel()
	q := newInbox()
	// Channel should be empty initially.
	select {
	case <-q.arrived():
		t.Fatal("notify channel should be empty before any push")
	default:
	}
	if err := q.push("hello"); err != nil {
		t.Fatalf("push: %v", err)
	}
	select {
	case <-q.arrived():
	case <-time.After(time.Second):
		t.Fatal("notify channel didn't fire after push")
	}
}

func TestInbox_NotifyCoalesces(t *testing.T) {
	t.Parallel()
	// Multiple pushes between consumer wake-ups should coalesce into
	// one notification; the consumer drains the queue and sees them
	// all. Otherwise the notify channel would back up and a "1
	// notification per push" semantic would deadlock.
	q := newInbox()
	for i := 0; i < 5; i++ {
		_ = q.push("x")
	}
	// First consumer drain sees one notification.
	<-q.arrived()
	// No further notifications because we haven't drained the queue.
	select {
	case <-q.arrived():
		t.Errorf("unexpected second notification before drain")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
	if got := q.drain(); len(got) != 5 {
		t.Errorf("drain count = %d, want 5", len(got))
	}
}

func TestInbox_DropOldestWhenCapExceeded(t *testing.T) {
	t.Parallel()
	q := newInbox()
	// Push one over the cap to trigger drop-oldest.
	for i := 0; i < defaultInboxCap+1; i++ {
		_ = q.push("msg")
	}
	got := q.drain()
	if len(got) != defaultInboxCap {
		t.Errorf("expected drain to return exactly defaultInboxCap (%d) after drop-oldest; got %d",
			defaultInboxCap, len(got))
	}
}

func TestInbox_PushAfterCloseErrors(t *testing.T) {
	t.Parallel()
	q := newInbox()
	q.close()
	err := q.push("hello")
	if !errors.Is(err, ErrInboxClosed) {
		t.Errorf("expected ErrInboxClosed after close; got %v", err)
	}
}

func TestPrependInboxMessages_FormatsBlock(t *testing.T) {
	t.Parallel()
	got := prependInboxMessages("what's next?", []string{
		"deadline moved up to 14:00",
		"pause file writes until further notice",
	})
	want := "[Inbox]\n" +
		"- deadline moved up to 14:00\n" +
		"- pause file writes until further notice" +
		"\n\n---\n\n" +
		"what's next?"
	if got != want {
		t.Errorf("unexpected prepended block:\n got:  %q\n want: %q", got, want)
	}
}

func TestPrependInboxMessages_EmptyIsPassthrough(t *testing.T) {
	t.Parallel()
	got := prependInboxMessages("hello", nil)
	if got != "hello" {
		t.Errorf("empty inbox should pass prompt through unchanged; got %q", got)
	}
}

func TestAgent_Inject_RoundtripsToNextRun(t *testing.T) {
	t.Parallel()
	// Use the echo mock to make the model's first response observable:
	// echo returns the user's last message verbatim, so we can verify
	// the prompt was rewritten with the inbox block.
	prov := mock.NewEcho()
	llm, err := prov.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("provider.Model: %v", err)
	}
	a, err := New(llm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.Inject("priority changed: focus on Q4 review"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	var saw string
	for ev, err := range a.Run(context.Background(), "what should I do?") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.Text != "" && !ev.Partial {
				saw += p.Text
			}
		}
	}
	if !strings.Contains(saw, "[Inbox]") {
		t.Errorf("model response should echo the [Inbox] header; got %q", saw)
	}
	if !strings.Contains(saw, "priority changed: focus on Q4 review") {
		t.Errorf("model response should echo the injected message; got %q", saw)
	}
	if !strings.Contains(saw, "what should I do?") {
		t.Errorf("model response should still contain the original prompt; got %q", saw)
	}
}

func TestAgent_Inject_DrainedOnceAcrossTurns(t *testing.T) {
	t.Parallel()
	prov := mock.NewEcho()
	llm, _ := prov.Model(context.Background(), "echo")
	a, _ := New(llm)
	if err := a.Inject("once"); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	for range a.Run(context.Background(), "first") {
		// drain
	}
	// Second turn — inbox should be empty now.
	var second string
	for ev, err := range a.Run(context.Background(), "second") {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.Text != "" && !ev.Partial {
				second += p.Text
			}
		}
	}
	if strings.Contains(second, "[Inbox]") {
		t.Errorf("second turn should not have any inbox block; got %q", second)
	}
}

func TestAgent_InboxArrived_FiresOnInject(t *testing.T) {
	t.Parallel()
	prov := mock.NewEcho()
	llm, _ := prov.Model(context.Background(), "echo")
	a, _ := New(llm)
	select {
	case <-a.InboxArrived():
		t.Fatal("should not have arrived before any Inject")
	default:
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = a.Inject("ping")
	}()
	select {
	case <-a.InboxArrived():
	case <-time.After(2 * time.Second):
		t.Fatal("InboxArrived didn't fire")
	}
}

func TestInject_ConcurrentProducers(t *testing.T) {
	t.Parallel()
	// Many goroutines pushing concurrently — all messages should
	// arrive (or, near the cap, get drop-oldest'd cleanly with no
	// data race). Run with -race.
	prov := mock.NewEcho()
	llm, _ := prov.Model(context.Background(), "echo")
	a, _ := New(llm)

	const goroutines = 50
	const perGoroutine = 10
	var wg sync.WaitGroup
	var errCount atomic.Int64
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if err := a.Inject("m"); err != nil {
					errCount.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if errCount.Load() > 0 {
		t.Errorf("expected no Inject errors under concurrent load; got %d", errCount.Load())
	}
	got := a.inbox.drain()
	// Total pushes = 500, cap = 256 → at most cap entries remain.
	if len(got) == 0 {
		t.Errorf("expected the queue to retain at least some messages; got 0")
	}
	if len(got) > defaultInboxCap {
		t.Errorf("queue retained %d > cap %d (drop-oldest didn't fire correctly)",
			len(got), defaultInboxCap)
	}
}

func TestAgent_Inject_NilReceiver(t *testing.T) {
	t.Parallel()
	var a *Agent
	if err := a.Inject("x"); err == nil {
		t.Errorf("nil Agent.Inject should error, not panic")
	}
}
