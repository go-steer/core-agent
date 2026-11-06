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
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/models/mock"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

func TestParseOutputFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw     string
		want    OutputFormat
		wantErr bool
	}{
		{"", OutputFormatText, false},
		{"text", OutputFormatText, false},
		{" text ", OutputFormatText, false},
		{"json", OutputFormatJSON, false},
		{"stream-json", OutputFormatStreamJSON, false},
		{"yaml", "", true},
	}
	for _, tc := range cases {
		got, err := ParseOutputFormat(tc.raw)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseOutputFormat(%q) err = %v, wantErr %v", tc.raw, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseOutputFormat(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestHeadless_NonTTYOmitsPrefix(t *testing.T) {
	t.Parallel()
	m, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock model: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code, err := Headless(
		context.Background(),
		m,
		"piped-hello",
		&stdout,
		&stderr,
		usage.NewTracker(),
		usage.Pricing{},
		nil,
	)
	if err != nil || code != ExitOK {
		t.Fatalf("Headless = (%d, %v), want (ExitOK, nil)", code, err)
	}
	out := stdout.String()
	if strings.Contains(out, "asst ›") {
		t.Errorf("non-TTY Headless stdout contains 'asst ›' prefix: %q", out)
	}
	if !strings.Contains(out, "piped-hello") {
		t.Errorf("Headless stdout %q missing response text", out)
	}

	// Explicit WithPrefix(true) restores the prefix even on a non-TTY writer.
	stdout.Reset()
	stderr.Reset()
	code, err = Headless(
		context.Background(),
		m,
		"prefixed-hello",
		&stdout,
		&stderr,
		usage.NewTracker(),
		usage.Pricing{},
		nil,
		WithPrefix(true),
	)
	if err != nil || code != ExitOK {
		t.Fatalf("Headless(WithPrefix(true)) = (%d, %v), want (ExitOK, nil)", code, err)
	}
	if !strings.HasPrefix(stdout.String(), "asst › ") {
		t.Errorf("WithPrefix(true) stdout = %q, want 'asst › ' prefix", stdout.String())
	}
}

type meteredLLM struct{}

func (meteredLLM) Name() string { return "metered-llm" }

func (meteredLLM) GenerateContent(_ context.Context, req *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	text := "echo: "
	if len(req.Contents) > 0 && len(req.Contents[len(req.Contents)-1].Parts) > 0 {
		text += req.Contents[len(req.Contents)-1].Parts[0].Text
	}
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		if !yield(&adkmodel.LLMResponse{
			Content: genai.NewContentFromText(text, genai.RoleModel),
			Partial: true,
		}, nil) {
			return
		}
		yield(&adkmodel.LLMResponse{
			Content:      genai.NewContentFromText(text, genai.RoleModel),
			TurnComplete: true,
			FinishReason: genai.FinishReasonStop,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     1000,
				CandidatesTokenCount: 500,
			},
		}, nil)
	}
}

func TestHeadless_OutputFormatJSON(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	tracker := usage.NewTracker()
	rate := usage.Pricing{InputPerMTok: 1.0, OutputPerMTok: 2.0}
	code, err := Headless(
		context.Background(),
		meteredLLM{},
		"json-prompt",
		&stdout,
		&stderr,
		tracker,
		rate,
		[]agent.Option{agent.WithUsageTracker(tracker)},
		WithOutputFormat(OutputFormatJSON),
	)
	if err != nil || code != ExitOK {
		t.Fatalf("Headless(JSON) = (%d, %v), want (ExitOK, nil)", code, err)
	}
	var res HeadlessJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("stdout is not valid JSON (%v): %q", err, stdout.String())
	}
	if !strings.Contains(res.Text, "json-prompt") {
		t.Errorf("res.Text = %q, want 'json-prompt'", res.Text)
	}
	if res.Model != "metered-llm" {
		t.Errorf("res.Model = %q, want 'metered-llm'", res.Model)
	}
	if res.Usage.Turns != 1 {
		t.Errorf("res.Usage.Turns = %d, want 1", res.Usage.Turns)
	}
	if res.Usage.InputTokens != 1000 || res.Usage.OutputTokens != 500 {
		t.Errorf("res.Usage tokens = (%d, %d), want (1000, 500)", res.Usage.InputTokens, res.Usage.OutputTokens)
	}
	if res.CostUSD <= 0 {
		t.Errorf("res.CostUSD = %f, want > 0", res.CostUSD)
	}
	if res.StopReason != "stop" {
		t.Errorf("res.StopReason = %q, want 'stop'", res.StopReason)
	}
	if res.Error != "" {
		t.Errorf("res.Error = %q, want empty", res.Error)
	}
}

func TestHeadless_OutputFormatStreamJSON(t *testing.T) {
	t.Parallel()
	m, err := mock.NewEcho().Model(context.Background(), "echo")
	if err != nil {
		t.Fatalf("mock model: %v", err)
	}
	var stdout, stderr bytes.Buffer
	tracker := usage.NewTracker()
	code, err := Headless(
		context.Background(),
		m,
		"stream-hello",
		&stdout,
		&stderr,
		tracker,
		usage.Pricing{},
		[]agent.Option{agent.WithUsageTracker(tracker)},
		WithOutputFormat(OutputFormatStreamJSON),
	)
	if err != nil || code != ExitOK {
		t.Fatalf("Headless(StreamJSON) = (%d, %v), want (ExitOK, nil)", code, err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 NDJSON lines (text_delta + result), got %d: %q", len(lines), stdout.String())
	}
	var sawTextDelta bool
	for i := 0; i < len(lines)-1; i++ {
		var ev HeadlessStreamEvent
		if err := json.Unmarshal([]byte(lines[i]), &ev); err != nil {
			t.Fatalf("line %d is not valid JSON (%v): %q", i, err, lines[i])
		}
		if ev.Type == "text_delta" {
			sawTextDelta = true
		}
	}
	if !sawTextDelta {
		t.Error("stream-json output did not include a 'text_delta' event")
	}
	var finalRes HeadlessJSONResult
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &finalRes); err != nil {
		t.Fatalf("final line is not valid JSON (%v): %q", err, lines[len(lines)-1])
	}
	if finalRes.Type != "result" {
		t.Errorf("finalRes.Type = %q, want 'result'", finalRes.Type)
	}
	if !strings.Contains(finalRes.Text, "stream-hello") {
		t.Errorf("finalRes.Text = %q, want 'stream-hello'", finalRes.Text)
	}
}

type failingLLM struct{}

func (failingLLM) Name() string { return "failing-llm" }

func (failingLLM) GenerateContent(_ context.Context, _ *adkmodel.LLMRequest, _ bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(nil, fmt.Errorf("upstream provider boom"))
	}
}

func TestHeadless_OutputFormatJSON_OnError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code, err := Headless(
		context.Background(),
		failingLLM{},
		"fail-me",
		&stdout,
		&stderr,
		usage.NewTracker(),
		usage.Pricing{},
		nil,
		WithOutputFormat(OutputFormatJSON),
	)
	if err == nil || code != ExitAgentError {
		t.Fatalf("Headless(failingLLM, JSON) = (%d, %v), want (ExitAgentError, error)", code, err)
	}
	var res HeadlessJSONResult
	if jerr := json.Unmarshal(stdout.Bytes(), &res); jerr != nil {
		t.Fatalf("error stdout is not valid JSON (%v): %q", jerr, stdout.String())
	}
	if res.StopReason != "error" {
		t.Errorf("res.StopReason = %q, want 'error'", res.StopReason)
	}
	if !strings.Contains(res.Error, "upstream provider boom") {
		t.Errorf("res.Error = %q, want 'upstream provider boom'", res.Error)
	}
}

func TestHeadless_OutputFormatJSON_NilTrackerStillMeters(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code, err := Headless(
		context.Background(),
		meteredLLM{},
		"nil-tracker-prompt",
		&stdout,
		&stderr,
		nil,
		usage.Pricing{InputPerMTok: 1.0, OutputPerMTok: 2.0},
		nil,
		WithOutputFormat(OutputFormatJSON),
	)
	if err != nil || code != ExitOK {
		t.Fatalf("Headless(JSON, nil tracker) = (%d, %v), want (ExitOK, nil)", code, err)
	}
	var res HeadlessJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("stdout is not valid JSON (%v): %q", err, stdout.String())
	}
	if res.Usage.Turns != 1 || res.Usage.InputTokens != 1000 || res.Usage.OutputTokens != 500 {
		t.Errorf("res.Usage = %+v, want Turns=1 InputTokens=1000 OutputTokens=500", res.Usage)
	}
	if res.CostUSD <= 0 {
		t.Errorf("res.CostUSD = %f, want > 0", res.CostUSD)
	}
}

func TestWriteJSONTurn_FiltersThoughtsAndEmitsToolAndGroundingStreamEvents(t *testing.T) {
	t.Parallel()
	events := func(yield func(*session.Event, error) bool) {
		// 1. Partial thought + partial FunctionCall (both must be ignored in output).
		ev1 := session.NewEvent(context.Background(), "inv-1")
		ev1.Partial = true
		ev1.Content = &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{Text: "internal chain-of-thought", Thought: true},
				{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": "partial.go"}}},
			},
		}
		if !yield(ev1, nil) {
			return
		}
		// 2. Aggregated tool call + grounding metadata.
		ev2 := session.NewEvent(context.Background(), "inv-1")
		ev2.Content = &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{FunctionCall: &genai.FunctionCall{Name: "read_file", Args: map[string]any{"path": "main.go"}}},
			},
		}
		ev2.GroundingMetadata = &genai.GroundingMetadata{
			WebSearchQueries: []string{"golang iter.Seq2"},
		}
		if !yield(ev2, nil) {
			return
		}
		// 3. Tool response + duplicate grounding metadata (must be deduplicated).
		ev3 := session.NewEvent(context.Background(), "inv-1")
		ev3.Content = &genai.Content{
			Role: genai.RoleUser,
			Parts: []*genai.Part{
				{FunctionResponse: &genai.FunctionResponse{Name: "read_file", Response: map[string]any{"content": "package main"}}},
			},
		}
		ev3.GroundingMetadata = &genai.GroundingMetadata{
			WebSearchQueries: []string{"golang iter.Seq2"},
		}
		if !yield(ev3, nil) {
			return
		}
		// 4. Non-streaming final reply (exercises fallbackBuilder when sawPartial == false) + non-partial thought.
		ev4 := session.NewEvent(context.Background(), "inv-1")
		ev4.FinishReason = genai.FinishReasonStop
		ev4.Content = &genai.Content{
			Role: genai.RoleModel,
			Parts: []*genai.Part{
				{Text: "final thought", Thought: true},
				{Text: "Here is main.go."},
			},
		}
		yield(ev4, nil)
	}

	var buf bytes.Buffer
	if err := writeJSONTurn(events, &buf, nil, "test-model", OutputFormatStreamJSON); err != nil {
		t.Fatalf("writeJSONTurn(StreamJSON): %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var toolCalls, toolResults, groundings, textDeltas int
	for i := 0; i < len(lines)-1; i++ {
		var ev HeadlessStreamEvent
		if err := json.Unmarshal([]byte(lines[i]), &ev); err != nil {
			t.Fatalf("line %d invalid JSON: %v", i, err)
		}
		switch ev.Type {
		case "tool_call":
			toolCalls++
			if ev.Name != "read_file" || ev.Args["path"] != "main.go" {
				t.Errorf("unexpected tool_call: %+v", ev)
			}
		case "tool_result":
			toolResults++
			if ev.Name != "read_file" || ev.Response["content"] != "package main" {
				t.Errorf("unexpected tool_result: %+v", ev)
			}
		case "grounding":
			groundings++
		case "text_delta":
			textDeltas++
		}
	}
	if toolCalls != 1 || toolResults != 1 || groundings != 1 || textDeltas != 0 {
		t.Errorf("counts = (tool_call=%d, tool_result=%d, grounding=%d, text_delta=%d), want (1, 1, 1, 0)",
			toolCalls, toolResults, groundings, textDeltas)
	}
	var finalRes HeadlessJSONResult
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &finalRes); err != nil {
		t.Fatalf("final line invalid JSON: %v", err)
	}
	if finalRes.Text != "Here is main.go." {
		t.Errorf("finalRes.Text = %q, want 'Here is main.go.' (thoughts must be excluded)", finalRes.Text)
	}
}

func TestWriteEvents_WithPrefixFalse(t *testing.T) {
	t.Parallel()
	events := func(yield func(*session.Event, error) bool) {
		ev := session.NewEvent(context.Background(), "inv-1")
		ev.Partial = true
		ev.Content = genai.NewContentFromText("hello unprefixed", genai.RoleModel)
		yield(ev, nil)
	}
	var out, info bytes.Buffer
	if err := WriteEvents(events, &out, &info, WithPrefix(false)); err != nil {
		t.Fatalf("WriteEvents(WithPrefix(false)): %v", err)
	}
	if got := out.String(); got != "hello unprefixed\n" {
		t.Errorf("out = %q, want %q", got, "hello unprefixed\n")
	}
}
