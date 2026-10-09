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

//go:build !no_tui

package main

import (
	"context"
	"iter"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/agent"
	"github.com/go-steer/core-agent/v2/pkg/agent/background"
	"github.com/go-steer/core-agent/v2/pkg/models/mock"
)

// scheduleOnceLLM schedules its next turn on the first call and answers
// with text after that, so a subagent built on it goes to sleep.
type scheduleOnceLLM struct{ calls atomic.Int32 }

func (*scheduleOnceLLM) Name() string { return "schedule-once" }

func (l *scheduleOnceLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	part := &genai.Part{Text: "scheduled"}
	if l.calls.Add(1) == 1 {
		part = &genai.Part{FunctionCall: &genai.FunctionCall{
			Name: "schedule_next_turn",
			Args: map[string]any{"wake_in_sec": 60, "detail": "polling cluster-A"},
		}}
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(&adkmodel.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}},
			FinishReason: genai.FinishReasonStop, TurnComplete: true,
		}, nil)
	}
}

type scheduleOnceProvider struct{ llm *scheduleOnceLLM }

func (scheduleOnceProvider) Name() string { return "schedule-once" }
func (p scheduleOnceProvider) Model(context.Context, string) (adkmodel.LLM, error) {
	return p.llm, nil
}

// TestSubagentInfos_CarriesPendingWake is the in-process half of #1283
// part B: a subagent asleep on schedule_next_turn reaches core-tui's
// roster with its wake, so the local bar counts down to it, and loses
// it once the run is over.
func TestSubagentInfos_CarriesPendingWake(t *testing.T) {
	t.Parallel()
	mgr, err := background.NewManager(background.WithProvider(scheduleOnceProvider{llm: &scheduleOnceLLM{}}, "m"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	if _, err := agent.New(mustEchoModel(t), agent.WithBackgroundManager(mgr)); err != nil {
		t.Fatalf("agent.New(parent): %v", err)
	}
	h, err := mgr.Spawn(context.Background(), "", background.Spec{
		Name: "monitor", SystemPrompt: "watch", Goal: "watch cluster-A", Scheduler: "sleep",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		infos := subagentInfos(mgr.List())
		if len(infos) == 1 && !infos[0].NextWakeAt.IsZero() {
			if infos[0].WakeDetail != "polling cluster-A" {
				t.Errorf("WakeDetail = %q, want the model's reason", infos[0].WakeDetail)
			}
			if infos[0].Status != "running" {
				t.Errorf("Status = %q while asleep, want running", infos[0].Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no NextWakeAt on the roster while the subagent sleeps: %+v", infos)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := mgr.Stop(h.Name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("stopped subagent never finished")
	}
	infos := subagentInfos(mgr.List())
	if len(infos) != 1 {
		t.Fatalf("infos after Stop = %+v, want the one stopped subagent", infos)
	}
	if !infos[0].NextWakeAt.IsZero() {
		t.Errorf("stopped subagent still advertises a wake at %v", infos[0].NextWakeAt)
	}
}

// TestSubagentInfos_CarriesLastReport pins the in-process TUI's half of
// #1283. This adapter reads the handles directly rather than going
// through ListSubagents, and before the fix it set LastReport only from
// a run error, so the running-tasks bar showed no text for any
// subagent that did not fail.
func TestSubagentInfos_CarriesLastReport(t *testing.T) {
	t.Parallel()
	mgr, err := background.NewManager(background.WithProvider(mock.NewEcho(), "echo"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	if _, err := agent.New(mustEchoModel(t), agent.WithBackgroundManager(mgr)); err != nil {
		t.Fatalf("agent.New(parent): %v", err)
	}

	h, err := mgr.Spawn(context.Background(), "", background.Spec{Name: "kid", SystemPrompt: "echo", Goal: "say the word lighthouse"})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("subagent never finished")
	}

	infos := subagentInfos(mgr.List())
	if len(infos) != 1 {
		t.Fatalf("infos = %+v, want one row", infos)
	}
	want := h.LastReport()
	if want == "" {
		t.Fatalf("the echo subagent finished with no report at all; this test can't tell the adapter from the handle")
	}
	if infos[0].LastReport != want {
		t.Errorf("LastReport = %q, want the handle's report %q", infos[0].LastReport, want)
	}
}
