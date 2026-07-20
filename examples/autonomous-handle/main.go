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

// Example: drive an autonomous run via the AutonomousHandle API.
// Demonstrates Pause / Resume / Inject / Stop without LLM credentials
// by using the echo mock provider as the model.
//
// The "agent" just echoes whatever prompt it sees, so the trace shows:
// - Initial turn with the goal
// - Pause + Resume (visible as a gap in the activity log)
// - Inject() arriving on the post-pause turn as an [Inbox] block
// - Stop tearing the goroutine down
//
//	go run ./examples/autonomous-handle
package main

import (
	"context"
	"fmt"
	"iter"
	"log"
	"time"

	adkmodel "google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// slowLLM wraps an inner LLM and adds a fixed delay before each
// response. Used here so the example has a wide-enough window for
// the test goroutine to call Pause / Inject / Resume between turns.
// In real usage, network latency to the LLM provider gives you a
// similar window automatically.
type slowLLM struct {
	inner adkmodel.LLM
	delay time.Duration
}

func (s *slowLLM) Name() string { return s.inner.Name() }

func (s *slowLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	inner := s.inner.GenerateContent(ctx, req, stream)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			yield(nil, ctx.Err())
			return
		}
		for r, err := range inner {
			if !yield(r, err) {
				return
			}
		}
	}
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	prov := mock.NewEcho()
	inner, err := prov.Model(ctx, "echo")
	if err != nil {
		return err
	}
	llm := &slowLLM{inner: inner, delay: 400 * time.Millisecond}

	build := func(extras []adktool.Tool) (*agent.Agent, error) {
		return agent.New(llm,
			agent.WithName("autonomous-handle-demo"),
			agent.WithInstruction("you are an echo agent; this example demonstrates the handle API"),
			agent.WithTools(extras),
		)
	}

	fmt.Println("== StartAutonomous ==")
	h, err := agent.StartAutonomous(ctx, build, "first goal",
		agent.WithMaxTurns(3),                  // bounded so the echo loop terminates
		agent.WithMaxWallclock(10*time.Second), // safety net
	)
	if err != nil {
		return fmt.Errorf("StartAutonomous: %w", err)
	}
	fmt.Printf("  status: %s\n", h.Status())

	// Let turn 1 begin, then pause.
	time.Sleep(50 * time.Millisecond)
	fmt.Println("\n== Pause ==")
	if err := h.Pause(); err != nil {
		return err
	}
	// Wait briefly for the loop to reach the pre-turn check.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.Status() == agent.AutonomousPaused {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	fmt.Printf("  status: %s\n", h.Status())

	// Queue a message; it'll land on the next turn's prompt as
	// [Inbox] when we Resume.
	fmt.Println("\n== Inject ==")
	if err := h.Inject("priority changed: hello from the example"); err != nil {
		return err
	}
	fmt.Println("  queued: \"priority changed: hello from the example\"")

	fmt.Println("\n== Resume ==")
	if err := h.Resume(); err != nil {
		return err
	}
	fmt.Printf("  status: %s\n", h.Status())

	fmt.Println("\n== Wait ==")
	res, err := h.Wait()
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	fmt.Printf("  reason:    %s\n", res.Reason)
	fmt.Printf("  turns:     %d\n", res.Turns)
	fmt.Printf("  finalText: %q\n", res.FinalText)
	fmt.Printf("  status:    %s\n", h.Status())

	// Stop is idempotent + safe to call after terminal.
	if err := h.Stop(); err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	return nil
}
