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

package hooks

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{name: "nil is fine", cfg: nil, wantErr: ""},
		{
			name:    "known event, valid handler",
			cfg:     Config{"tool-start": {{Command: "echo hi", TimeoutSeconds: 5}}},
			wantErr: "",
		},
		{
			name:    "unknown event rejected",
			cfg:     Config{"typo-event": {{Command: "echo"}}},
			wantErr: `unknown event "typo-event"`,
		},
		{
			name:    "empty command rejected",
			cfg:     Config{"tool-end": {{Command: ""}}},
			wantErr: `command is required`,
		},
		{
			name:    "negative timeout rejected",
			cfg:     Config{"agent-end": {{Command: "echo", TimeoutSeconds: -1}}},
			wantErr: `timeout_seconds must be non-negative`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate: unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate: expected error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate: error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

// recorder is a captured-envelope fake for runCmd. Records every
// (command, envelope) pair the dispatcher would have spawned.
type recorder struct {
	mu      sync.Mutex
	entries []recorderEntry
}

type recorderEntry struct {
	command  string
	envelope map[string]any
}

func (r *recorder) run(_ context.Context, command string, envelope []byte) error {
	var payload map[string]any
	if err := json.Unmarshal(envelope, &payload); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, recorderEntry{command: command, envelope: payload})
	return nil
}

func (r *recorder) events() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.entries))
	for i, e := range r.entries {
		out[i] = e.envelope["hook_event_name"].(string)
	}
	return out
}

func newDispatcher(t *testing.T, cfg Config) (*Dispatcher, *recorder) {
	t.Helper()
	rec := &recorder{}
	d := New(cfg, "sess-abc", io.Discard)
	d.runCmd = rec.run
	return d, rec
}

// allEventsCfg wires every KnownEvents entry to the same fake command
// so the dispatcher fires everything it knows how to fire.
func allEventsCfg() Config {
	cfg := Config{}
	for _, e := range KnownEvents {
		cfg[e] = []Handler{{Command: "echo"}}
	}
	return cfg
}

func TestOnEvent_ToolCallFiresToolStart(t *testing.T) {
	d, rec := newDispatcher(t, allEventsCfg())
	d.OnEvent(&session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{
				Name: "read_file",
				Args: map[string]any{"path": "/tmp/x"},
			},
		}}},
	}})
	if got := rec.events(); len(got) != 1 || got[0] != "tool-start" {
		t.Fatalf("events = %v, want [tool-start]", got)
	}
	env := rec.entries[0].envelope
	if env["tool_name"] != "read_file" {
		t.Errorf("tool_name = %v, want read_file", env["tool_name"])
	}
	if env["session_id"] != "sess-abc" {
		t.Errorf("session_id = %v, want sess-abc", env["session_id"])
	}
	input, ok := env["tool_input"].(map[string]any)
	if !ok || input["path"] != "/tmp/x" {
		t.Errorf("tool_input = %v, want {path: /tmp/x}", env["tool_input"])
	}
}

func TestOnEvent_FunctionResponseFiresToolEnd(t *testing.T) {
	d, rec := newDispatcher(t, allEventsCfg())
	d.OnEvent(&session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{
				Name:     "read_file",
				Response: map[string]any{"bytes": 42},
			},
		}}},
	}})
	if got := rec.events(); len(got) != 1 || got[0] != "tool-end" {
		t.Fatalf("events = %v, want [tool-end]", got)
	}
	env := rec.entries[0].envelope
	if env["tool_name"] != "read_file" {
		t.Errorf("tool_name = %v, want read_file", env["tool_name"])
	}
	out, ok := env["tool_output"].(map[string]any)
	if !ok || out["bytes"] != float64(42) {
		t.Errorf("tool_output = %v, want {bytes: 42}", env["tool_output"])
	}
}

func TestOnEvent_ModelStartFiresOncePerWindow(t *testing.T) {
	d, rec := newDispatcher(t, allEventsCfg())
	textEv := func(s string) *session.Event {
		return &session.Event{LLMResponse: adkmodel.LLMResponse{
			Content: &genai.Content{Parts: []*genai.Part{{Text: s}}},
			Partial: true,
		}}
	}
	// First model chunk of the turn → model-start.
	d.OnEvent(textEv("he"))
	// Follow-up chunks in the same window → nothing new.
	d.OnEvent(textEv("llo"))
	d.OnEvent(textEv(" world"))
	if got := rec.events(); len(got) != 1 || got[0] != "model-start" {
		t.Fatalf("events after streaming = %v, want single model-start", got)
	}

	// Tool boundary resets the window; the next text chunk fires model-start again.
	d.OnEvent(&session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{Name: "read_file"},
		}}},
	}})
	d.OnEvent(&session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{{
			FunctionResponse: &genai.FunctionResponse{Name: "read_file"},
		}}},
	}})
	d.OnEvent(textEv("post-tool"))
	got := rec.events()
	want := []string{"model-start", "tool-start", "tool-end", "model-start"}
	if !equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestOnTurnEnd_FiresAgentEndAndResetsState(t *testing.T) {
	d, rec := newDispatcher(t, allEventsCfg())
	textEv := &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{{Text: "hi"}}},
		Partial: true,
	}}
	d.OnEvent(textEv)
	d.OnTurnEnd()
	// Model-start should fire again on the next turn's first text chunk,
	// even though the flag was set within the previous turn.
	d.OnEvent(textEv)
	got := rec.events()
	want := []string{"model-start", "agent-end", "model-start"}
	if !equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestOnEvent_UnknownEventTypeIsSilent(t *testing.T) {
	d, rec := newDispatcher(t, allEventsCfg())
	// Non-partial text: model turn output past streaming, not a boundary
	// we care about. Should not fire anything.
	d.OnEvent(&session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{{Text: "final answer"}}},
	}})
	// Nil parts, nil content, nil event.
	d.OnEvent(&session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{nil, {}}},
	}})
	d.OnEvent(&session.Event{})
	d.OnEvent(nil)
	if got := rec.events(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
}

func TestEmpty(t *testing.T) {
	d := New(nil, "", io.Discard)
	if !d.Empty() {
		t.Fatal("empty config: Empty() = false, want true")
	}
	d2 := New(Config{"tool-start": {{Command: "true"}}}, "", io.Discard)
	if d2.Empty() {
		t.Fatal("non-empty config: Empty() = true, want false")
	}
}

func TestFire_SkipsEventsWithoutHandlers(t *testing.T) {
	// Only tool-start is configured; tool-end fires but should be silently
	// dropped because no handler is registered for it.
	d, rec := newDispatcher(t, Config{"tool-start": {{Command: "echo"}}})
	d.OnEvent(&session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{
			{FunctionCall: &genai.FunctionCall{Name: "t"}},
			{FunctionResponse: &genai.FunctionResponse{Name: "t"}},
		}},
	}})
	if got := rec.events(); len(got) != 1 || got[0] != "tool-start" {
		t.Fatalf("events = %v, want [tool-start]", got)
	}
}

func TestExecCommand_RunsRealShell(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "envelope.json")
	cfg := Config{
		// Piping stdin to a file exercises the /bin/sh -c path (the whole
		// point of shelling out — direct exec.Command wouldn't parse '>').
		"tool-start": {{Command: "cat > " + target}},
	}
	d := New(cfg, "sess-real", os.Stderr)
	d.OnEvent(&session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{Name: "read_file"},
		}}},
	}})
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal envelope: %v (body=%q)", err, string(body))
	}
	if got["hook_event_name"] != "tool-start" {
		t.Errorf("hook_event_name = %v, want tool-start", got["hook_event_name"])
	}
	if got["tool_name"] != "read_file" {
		t.Errorf("tool_name = %v, want read_file", got["tool_name"])
	}
}

func TestExecCommand_FailureLoggedNotPropagated(t *testing.T) {
	// A non-zero-exit command must not crash the agent; error goes to
	// stderr and OnEvent returns normally.
	var stderr strings.Builder
	d := New(Config{"agent-end": {{Command: "sh -c 'exit 7'"}}}, "", &stderr)
	// Doesn't panic; doesn't hang.
	d.OnTurnEnd()
	if stderr.Len() == 0 {
		t.Fatal("expected stderr to record the hook failure, got nothing")
	}
	if !strings.Contains(stderr.String(), "agent-end") {
		t.Errorf("stderr = %q, want mention of agent-end", stderr.String())
	}
}

// equal compares two string slices element-by-element. Kept local to
// this test file to avoid a dependency for a one-line helper.
func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
