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
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/agent"
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
