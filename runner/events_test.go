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
	"bytes"
	"errors"
	"iter"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/session"
	"google.golang.org/genai"
)

// eventSeq builds an iter.Seq2 from a fixed list of events and an
// optional terminating error.
func eventSeq(events []*session.Event, finalErr error) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
		if finalErr != nil {
			yield(nil, finalErr)
		}
	}
}

func partialText(s string) *session.Event {
	return &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: s}},
		},
		Partial: true,
	}}
}

func turnComplete(s string) *session.Event {
	return &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: s}},
		},
		TurnComplete: true,
	}}
}

func toolCall(name string, args map[string]any) *session.Event {
	return &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{{
				FunctionCall: &genai.FunctionCall{Name: name, Args: args},
			}},
		},
	}}
}

func toolResult(name string, resp map[string]any) *session.Event {
	return &session.Event{LLMResponse: adkmodel.LLMResponse{
		Content: &genai.Content{
			Role: genai.RoleUser,
			Parts: []*genai.Part{{
				FunctionResponse: &genai.FunctionResponse{Name: name, Response: resp},
			}},
		},
	}}
}

func TestWriteEvents_StreamsPartialTextOnlyToOut(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	err := WriteEvents(eventSeq([]*session.Event{
		partialText("hel"),
		partialText("lo"),
		turnComplete("hello"), // skipped — already streamed
	}, nil), &out, &info)
	if err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	if got := out.String(); got != "hello\n" {
		t.Errorf("out = %q, want %q (streamed text + trailing newline)", got, "hello\n")
	}
	if info.Len() != 0 {
		t.Errorf("info should be empty for text-only events, got %q", info.String())
	}
}

func TestWriteEvents_FormatsToolCallsToInfo(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	err := WriteEvents(eventSeq([]*session.Event{
		toolCall("bash", map[string]any{"command": "ls -la"}),
		toolResult("bash", map[string]any{"exit_code": float64(0), "stdout": "main.go\nREADME.md\n"}),
		partialText("Done."),
	}, nil), &out, &info)
	if err != nil {
		t.Fatalf("WriteEvents: %v", err)
	}
	got := info.String()
	if !strings.Contains(got, "→ bash(command=") {
		t.Errorf("missing call line. info = %q", got)
	}
	if !strings.Contains(got, `"ls -la"`) {
		t.Errorf("call args not formatted as expected. info = %q", got)
	}
	if !strings.Contains(got, "← bash(") {
		t.Errorf("missing response line. info = %q", got)
	}
	if !strings.Contains(got, "exit_code=0") {
		t.Errorf("response args not formatted as expected. info = %q", got)
	}
}

func TestWriteEvents_KeyOrderingIsStable(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	args := map[string]any{"zeta": "z", "alpha": "a", "mid": "m"}
	_ = WriteEvents(eventSeq([]*session.Event{toolCall("t", args)}, nil), &out, &info)
	got := info.String()
	// Sorted keys: alpha, mid, zeta
	if got != "→ t(alpha=\"a\", mid=\"m\", zeta=\"z\")\n" {
		t.Errorf("keys not in stable sort order. info = %q", got)
	}
}

func TestWriteEvents_LongValueTruncated(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	bigVal := strings.Repeat("x", 500)
	_ = WriteEvents(eventSeq([]*session.Event{
		toolCall("t", map[string]any{"k": bigVal}),
	}, nil), &out, &info)
	got := info.String()
	if len(got) > 200 {
		t.Errorf("output should be truncated, got %d bytes: %q", len(got), got)
	}
	if !strings.Contains(got, "...") {
		t.Errorf("expected ellipsis in truncated value, got %q", got)
	}
}

func TestWriteEvents_ErrorPropagates(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	wantErr := errors.New("boom")
	err := WriteEvents(eventSeq([]*session.Event{
		partialText("partial"),
	}, wantErr), &out, &info)
	if !errors.Is(err, wantErr) {
		t.Errorf("expected boom error, got %v", err)
	}
	// Trailing newline should still get written so a downstream
	// terminal renders cleanly even on error.
	if !strings.HasSuffix(out.String(), "\n") {
		t.Errorf("expected trailing newline on error after partial, got %q", out.String())
	}
}

func TestWriteEvents_SharedWriterCombined(t *testing.T) {
	t.Parallel()
	// Caller wants one combined stream (e.g., for tmux capture).
	var combined bytes.Buffer
	_ = WriteEvents(eventSeq([]*session.Event{
		toolCall("read_file", map[string]any{"path": "main.go"}),
		toolResult("read_file", map[string]any{"content": "package main"}),
		partialText("Read it."),
	}, nil), &combined, &combined)
	got := combined.String()
	for _, want := range []string{"→ read_file(", "← read_file(", "Read it."} {
		if !strings.Contains(got, want) {
			t.Errorf("combined output missing %q. got %q", want, got)
		}
	}
}

func TestWriteEvents_NoArgsCallShowsParens(t *testing.T) {
	t.Parallel()
	var out, info bytes.Buffer
	_ = WriteEvents(eventSeq([]*session.Event{
		toolCall("ping", nil),
	}, nil), &out, &info)
	if got := info.String(); got != "→ ping()\n" {
		t.Errorf("got %q", got)
	}
}
