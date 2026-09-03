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

package attachadapter

import (
	"context"
	"iter"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/attach"
)

// #896. AttachStatus() never produced AgentStateRunning, so GET /status
// and the SSE status seed reported "idle" for a session that was
// mid-turn. Pre-fix both tests below see State="idle" and
// TurnInFlight=false while the model call is parked.

// blockingModel holds its turn open until release is closed, so the
// test can observe the agent from outside while a turn is genuinely in
// flight rather than racing a fast one.
type blockingModel struct {
	entered chan struct{}
	release chan struct{}
}

func (m *blockingModel) Name() string { return "blocking-fake" }

func (m *blockingModel) GenerateContent(ctx context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		select {
		case m.entered <- struct{}{}:
		default:
		}
		select {
		case <-m.release:
		case <-ctx.Done():
			return
		}
		yield(&adkmodel.LLMResponse{TurnComplete: true}, nil)
	}
}

// newBlockedTurn starts a turn and returns once the model call is
// parked inside it. The returned func releases the model and waits for
// the turn to unwind.
func newBlockedTurn(t *testing.T) (*agent.Agent, func()) {
	t.Helper()
	m := &blockingModel{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a, err := agent.New(m)
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range a.Run(ctx, "hi") { //nolint:revive // draining is the point
		}
	}()
	select {
	case <-m.entered:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("model call never started; the turn never got in flight")
	}
	return a, func() {
		close(m.release)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("turn did not unwind after release")
		}
		cancel()
	}
}

func TestAttachStatus_MidTurnReportsRunning(t *testing.T) {
	a, finish := newBlockedTurn(t)
	defer finish()

	got := New(a).AttachStatus()
	if !got.TurnInFlight {
		t.Errorf("TurnInFlight = false during an in-flight turn, want true")
	}
	if got.State != attach.AgentStateRunning {
		t.Errorf("State = %q mid-turn, want %q", got.State, attach.AgentStateRunning)
	}
}

// The window #896 was actually filed on: an operator parks the session,
// the turn the park interrupted keeps executing. State stays "paused"
// so hold banners keep working; TurnInFlight is the only field that can
// say the turn is still alive.
func TestAttachStatus_PausedMidTurnKeepsPausedButReportsTurnInFlight(t *testing.T) {
	a, finish := newBlockedTurn(t)
	defer finish()

	a.Pause("operator hold")

	got := New(a).AttachStatus()
	if got.State != attach.AgentStatePaused {
		t.Errorf("State = %q, want %q — pause must keep outranking running so existing hold banners don't regress",
			got.State, attach.AgentStatePaused)
	}
	if !got.TurnInFlight {
		t.Errorf("TurnInFlight = false while parked mid-turn, want true — this is the only field that can distinguish a quiet hold from a hold over a turn that is still running")
	}
	if got.PausedSince.IsZero() {
		t.Errorf("PausedSince zero, want the pause fields still populated alongside TurnInFlight")
	}
}

func TestAttachStatus_IdleAgentReportsNoTurnInFlight(t *testing.T) {
	t.Parallel()
	got := New(newEchoAgent(t)).AttachStatus()
	if got.TurnInFlight {
		t.Errorf("TurnInFlight = true on an idle agent, want false")
	}
	if got.State != attach.AgentStateIdle {
		t.Errorf("State = %q, want %q", got.State, attach.AgentStateIdle)
	}
}
