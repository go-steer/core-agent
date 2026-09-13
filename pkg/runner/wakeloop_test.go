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

package runner

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

func TestWakeLoop_ReturnsOnContextCancel(t *testing.T) {
	t.Parallel()
	a := newEchoWakeAgent(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		WakeLoop(ctx, a, WakeLoopOptions{})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WakeLoop did not return after ctx cancel")
	}
}

// #566: when the loop's ctx is cancelled (daemon shutdown or per-session
// eviction), WakeLoop must close the agent's inbox on the way out so a
// post-death inject fails loudly with ErrInboxClosed instead of being
// acknowledged and dropped into a mailbox nobody drains.
func TestWakeLoop_ClosesInboxOnExit(t *testing.T) {
	t.Parallel()
	a := newEchoWakeAgent(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		WakeLoop(ctx, a, WakeLoopOptions{})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("WakeLoop did not return after ctx cancel")
	}

	if err := a.Inject("post-eviction inject"); !errors.Is(err, agent.ErrInboxClosed) {
		t.Errorf("Inject after WakeLoop exit = %v, want ErrInboxClosed", err)
	}
}

func TestWakeLoop_InjectDrivesATurn(t *testing.T) {
	t.Parallel()
	a := newEchoWakeAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	turnErrs := make(chan error, 4)
	debugLines := make(chan string, 16)
	go WakeLoop(ctx, a, WakeLoopOptions{
		OnTurnError: func(err error) { turnErrs <- err },
		Debugf: func(format string, args ...any) {
			select {
			case debugLines <- format:
			default:
			}
		},
	})

	// Inject queues the message on the inbox AND fires WakeRequested;
	// the loop must pick it up and complete a Run without errors.
	if err := a.Inject("hello from the operator"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case line := <-debugLines:
			if strings.HasPrefix(line, "Run finished") {
				select {
				case err := <-turnErrs:
					t.Fatalf("turn error: %v", err)
				default:
				}
				return
			}
		case err := <-turnErrs:
			t.Fatalf("turn error: %v", err)
		case <-deadline:
			t.Fatal("WakeLoop never completed a turn after Inject")
		}
	}
}

// #978: a loop whose turns keep failing must hold off between them
// instead of burning one turn per arriving event, and must say so
// somewhere an operator can read. Before the fix this loop logged the
// error and went straight back to blocking, so nothing ever slept and
// the health handle stayed idle.
func TestWakeLoop_RepeatedFailuresBackOffAndShowUpAsUnhealthy(t *testing.T) {
	t.Parallel()
	llm := &flakyLLM{}
	llm.set(llmFail)
	a, err := agent.New(llm)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var set LoopHealthSet
	health, unregister := set.Register("s-978")
	t.Cleanup(unregister)

	// The sleep seam stands in for the wall clock the way
	// models.RetryPolicy's does: record the interval, return
	// immediately, keep the test at memory speed.
	sleeps := make(chan time.Duration, 8)
	turnDone := make(chan struct{}, 8)
	go WakeLoop(ctx, a, WakeLoopOptions{
		Health:            health,
		FailureBackoff:    10 * time.Millisecond,
		MaxFailureBackoff: 25 * time.Millisecond,
		OnTurnError:       func(error) {},
		sleep: func(_ context.Context, d time.Duration) bool {
			sleeps <- d
			return true
		},
		Debugf: func(format string, _ ...any) {
			if strings.HasPrefix(format, "Run finished") {
				select {
				case turnDone <- struct{}{}:
				default:
				}
			}
		},
	})

	drive := func(msg string) {
		t.Helper()
		if err := a.Inject(msg); err != nil {
			t.Fatalf("Inject: %v", err)
		}
		select {
		case <-turnDone:
		case <-time.After(10 * time.Second):
			t.Fatalf("no turn ran after injecting %q", msg)
		}
	}
	nextSleep := func() time.Duration {
		t.Helper()
		select {
		case d := <-sleeps:
			return d
		case <-time.After(10 * time.Second):
			t.Fatal("the loop never held off between failing turns")
			return 0
		}
	}

	// The first failure is free — a single blip must not add latency to
	// the operator's next message.
	drive("first")
	select {
	case d := <-sleeps:
		t.Fatalf("held off %s after a single failure; the first one is free", d)
	case <-time.After(50 * time.Millisecond):
	}

	drive("second")
	if got := nextSleep(); got != 10*time.Millisecond {
		t.Errorf("backoff after 2 failures = %s, want 10ms", got)
	}
	drive("third")
	if got := nextSleep(); got != 20*time.Millisecond {
		t.Errorf("backoff after 3 failures = %s, want 20ms", got)
	}
	drive("fourth")
	if got := nextSleep(); got != 25*time.Millisecond {
		t.Errorf("backoff after 4 failures = %s, want the 25ms cap", got)
	}

	st := health.Snapshot()
	if st.State != LoopFailing {
		t.Errorf("State = %q, want %q", st.State, LoopFailing)
	}
	if st.ConsecutiveFailures != 4 {
		t.Errorf("ConsecutiveFailures = %d, want 4", st.ConsecutiveFailures)
	}
	if st.LastErrorKind == "" {
		t.Error("LastErrorKind is empty; the aggregate has nothing to log")
	}
	if st.Session != "s-978" {
		t.Errorf("Session = %q, want s-978", st.Session)
	}
	if err := set.Err(); err == nil {
		t.Error("the aggregate health check is green while every loop is failing")
	}

	// An obeyed stop in the middle of an outage is not a recovery and
	// not an extra failure. It must neither clear the reading — the
	// fault is still there and the operator who pressed stop did not
	// fix it — nor earn a hold-off, which would rate-limit the turn
	// right after a guardrail is reset.
	llm.set(llmCanceled)
	drive("interrupted")
	select {
	case d := <-sleeps:
		t.Errorf("held off %s after an obeyed stop", d)
	case <-time.After(50 * time.Millisecond):
	}
	if st := health.Snapshot(); st.ConsecutiveFailures != 4 || st.State != LoopFailing {
		t.Errorf("after an obeyed stop State=%q failures=%d, want failing/4", st.State, st.ConsecutiveFailures)
	}
	if err := set.Err(); err == nil {
		t.Error("an operator interrupt turned the aggregate green mid-outage")
	}

	// Recovery is the other half: one clean turn clears the rate limit
	// and the unhealthy reading, so a fixed credential does not leave
	// the session throttled.
	llm.set(llmOK)
	drive("recovered")
	select {
	case d := <-sleeps:
		t.Errorf("held off %s after a clean turn", d)
	case <-time.After(50 * time.Millisecond):
	}
	if st := health.Snapshot(); st.State != LoopIdle || st.ConsecutiveFailures != 0 {
		t.Errorf("after recovery State=%q failures=%d, want idle/0", st.State, st.ConsecutiveFailures)
	}
	if err := set.Err(); err != nil {
		t.Errorf("aggregate still unhealthy after recovery: %v", err)
	}
}

func TestWakeLoopOptions_FailureBackoff(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		opts        WakeLoopOptions
		consecutive int
		want        time.Duration
	}{
		{"first failure is free", WakeLoopOptions{}, 1, 0},
		{"zero and negative counts are free", WakeLoopOptions{}, 0, 0},
		{"second failure waits the base", WakeLoopOptions{}, 2, defaultFailureBackoff},
		{"third doubles", WakeLoopOptions{}, 3, 2 * defaultFailureBackoff},
		{"fourth doubles again", WakeLoopOptions{}, 4, 4 * defaultFailureBackoff},
		{"a long outage sits at the cap", WakeLoopOptions{}, 40, defaultMaxFailureBackoff},
		{
			"a base past the cap is clamped, not doubled past it",
			WakeLoopOptions{FailureBackoff: time.Hour, MaxFailureBackoff: time.Minute},
			2, time.Minute,
		},
		{
			"negative base disables the hold-off entirely",
			WakeLoopOptions{FailureBackoff: -1},
			9, 0,
		},
		{
			"custom base and cap",
			WakeLoopOptions{FailureBackoff: time.Second, MaxFailureBackoff: 3 * time.Second},
			4, 3 * time.Second,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.opts.failureBackoff(tc.consecutive); got != tc.want {
				t.Errorf("failureBackoff(%d) = %s, want %s", tc.consecutive, got, tc.want)
			}
		})
	}
}

// The exclusions are the load-bearing part: obeying an operator or a
// guardrail is not a fault, and counting it would rate-limit the turn
// that runs right after the halt is cleared.
func TestCountsAsFailure(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"", attach.TurnErrorCanceled, attach.TurnErrorWatchdog, attach.TurnErrorCostCeiling} {
		if countsAsFailure(kind) {
			t.Errorf("countsAsFailure(%q) = true, want false", kind)
		}
	}
	for _, kind := range []string{
		attach.TurnErrorAuth, attach.TurnErrorConfig, attach.TurnErrorRateLimited,
		attach.TurnErrorTransientNet, attach.TurnErrorModelNotFound, attach.TurnErrorUnknown,
	} {
		if !countsAsFailure(kind) {
			t.Errorf("countsAsFailure(%q) = false, want true", kind)
		}
	}
}

// flakyLLM fails, obeys a stop, or echoes on demand, so one test can
// walk a loop from a persistent fault through to recovery.
type flakyLLM struct{ mode atomic.Int32 }

const (
	llmOK int32 = iota
	llmFail
	llmCanceled
)

func (l *flakyLLM) set(mode int32) { l.mode.Store(mode) }

func (*flakyLLM) Name() string { return "flaky" }

func (l *flakyLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		switch l.mode.Load() {
		case llmFail:
			yield(nil, errors.New("flaky: the cluster hung up"))
			return
		case llmCanceled:
			// Not the loop's ctx — an operator interrupt or a guardrail
			// cutting THIS turn short, which the loop must read as an
			// obeyed stop rather than a fault.
			yield(nil, fmt.Errorf("model call: %w", context.Canceled))
			return
		}
		content := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}}
		yield(&adkmodel.LLMResponse{
			Content:      content,
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

func newEchoWakeAgent(t *testing.T) *agent.Agent {
	t.Helper()
	provider := mock.NewEcho()
	m, err := provider.Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	a, err := agent.New(m)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return a
}
