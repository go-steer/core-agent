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

package main

import (
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/memory"
	"github.com/go-steer/core-agent/v3/pkg/memory/memtools"
)

func TestDescribePromptLayers(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		model    string
		replaced bool
		appended bool
		hasTools bool
		sources  int
		want     string
	}{
		{
			name:     "gemini with tools includes parallelism quirk",
			model:    "gemini-3.8-flash",
			hasTools: true,
			sources:  2,
			want:     "core + quirks(gemini-parallelism) + interactive-overlay + memory(2 sources)",
		},
		{
			name:     "gemini without tools omits parallelism quirk",
			model:    "gemini-3.8-flash",
			hasTools: false,
			sources:  1,
			want:     "core + interactive-overlay + memory(1 sources)",
		},
		{
			name:     "claude with tools has no quirks",
			model:    "claude-sonnet-4-6",
			hasTools: true,
			appended: true,
			sources:  0,
			want:     "core + interactive-overlay + memory(0 sources) + append",
		},
		{
			name:     "replaced prompt skips layers 1-3",
			model:    "gemini-3.8-flash",
			replaced: true,
			appended: true,
			hasTools: true,
			sources:  1,
			want:     "REPLACED(system_prompt_file) + memory(1 sources) + append",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := describePromptLayers(tc.model, tc.replaced, tc.appended, tc.hasTools, tc.sources)
			if got != tc.want {
				t.Errorf("describePromptLayers() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunState_HasModelTools(t *testing.T) {
	t.Parallel()

	rwRouter, err := memory.NewRouter(memory.NewInMemory(), []memory.MountTemplate{
		memory.AgentMount("prod", "triage", memory.AccessReadWrite),
	})
	if err != nil {
		t.Fatalf("NewRouter(rw): %v", err)
	}
	roRouter, err := memory.NewRouter(memory.NewInMemory(), []memory.MountTemplate{
		memory.AgentMount("prod", "triage", memory.AccessReadOnly),
	})
	if err != nil {
		t.Fatalf("NewRouter(ro): %v", err)
	}
	toollessAgent, err := agent.New(mustEchoModel(t))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}

	cases := []struct {
		name  string
		state *runState
		want  bool
	}{
		{
			name:  "no tools and checkpoint off",
			state: &runState{ckpt: checkpointResolution{Mode: config.CheckpointModeOff}},
			want:  false,
		},
		{
			name:  "no tools and checkpoint operator",
			state: &runState{ckpt: checkpointResolution{Mode: config.CheckpointModeOperator}},
			want:  false,
		},
		{
			name:  "checkpoint model registers mark_task_done",
			state: &runState{ckpt: checkpointResolution{Mode: config.CheckpointModeModel}},
			want:  true,
		},
		{
			name: "memory with --no-builtin-tools disables all memory tools",
			state: &runState{
				ckpt:      checkpointResolution{Mode: config.CheckpointModeOff},
				memRouter: rwRouter,
				memSettings: agent.MemorySettings{
					DisabledTools: []string{memtools.RecallToolName, memtools.PersistToolName, memtools.ForgetToolName},
				},
			},
			want: false,
		},
		{
			name: "read-only memory with recall_memory disabled has zero tools",
			state: &runState{
				ckpt:      checkpointResolution{Mode: config.CheckpointModeOff},
				memRouter: roRouter,
				memSettings: agent.MemorySettings{
					DisabledTools: []string{memtools.RecallToolName},
				},
			},
			want: false,
		},
		{
			name: "writable memory with only recall_memory disabled still registers persist/forget",
			state: &runState{
				ckpt:      checkpointResolution{Mode: config.CheckpointModeOff},
				memRouter: rwRouter,
				memSettings: agent.MemorySettings{
					DisabledTools: []string{memtools.RecallToolName},
				},
			},
			want: true,
		},
		{
			name: "post-construct agentRef with zero tools is authoritative",
			state: &runState{
				agentRef: toollessAgent,
				ckpt:     checkpointResolution{Mode: config.CheckpointModeModel},
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.state.hasModelTools(); got != tc.want {
				t.Errorf("hasModelTools() = %v, want %v", got, tc.want)
			}
		})
	}
}
